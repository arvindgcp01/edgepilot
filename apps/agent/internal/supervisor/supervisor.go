// Package supervisor is the agent's top level: it brings the process from
// "launchd started me" to "connected and reporting", and decides what to do when
// that fails.
//
// # The order of operations
//
//	load the identity
//	 ├─ absent  → collect facts, generate a key, enroll, persist, remove the token
//	 └─ present → use it
//	open the spool
//	connect, and stay connected
//
// # What it will not do
//
// It will not loop forever on a condition an operator has to fix. A revoked
// device, a decommissioned device, and an expired certificate all stop the
// process, because launchd will restart it and the restart will fail the same
// way — and a Mac quietly retrying a hopeless operation every second for a month
// is worse than one that is visibly stopped.
package supervisor

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/visionify/edgepilot/apps/agent/internal/backoff"
	"github.com/visionify/edgepilot/apps/agent/internal/client"
	"github.com/visionify/edgepilot/apps/agent/internal/collect"
	"github.com/visionify/edgepilot/apps/agent/internal/config"
	"github.com/visionify/edgepilot/apps/agent/internal/enroll"
	"github.com/visionify/edgepilot/apps/agent/internal/identity"
	"github.com/visionify/edgepilot/apps/agent/internal/spool"
)

// ErrEnrollmentImpossible reports a condition an operator has to resolve: a
// rejected token, a serial number already enrolled, a request the server refuses.
// Retrying cannot help, and pretending otherwise hides the problem.
var ErrEnrollmentImpossible = errors.New("supervisor: this device cannot enroll")

// Supervisor runs the agent.
type Supervisor struct {
	cfg     *config.Config
	log     *slog.Logger
	version string

	store     *identity.Store
	collector collect.Collector

	// bootID distinguishes a restart from a reconnect, which is what makes the
	// server's agent_stopped alert accurate rather than firing on every blip.
	bootID string

	// newEnrollClient is overridden by tests to reach a local server.
	newEnrollClient func(opts enroll.Options) (enrollClient, error)
	// dialOptions are passed through to the device-plane client, so a test can
	// reach a local listener without the production mTLS path.
	dialOptions []grpc.DialOption

	now func() time.Time
}

// enrollClient is the part of enroll.Client the supervisor uses.
type enrollClient interface {
	Enroll(ctx context.Context, req enroll.Request) (*enroll.Result, error)
}

// Options configures a Supervisor.
type Options struct {
	Config    *config.Config
	Logger    *slog.Logger
	Version   string
	Collector collect.Collector
	Now       func() time.Time

	// DialOptions are appended to the device-plane client's own options.
	DialOptions []grpc.DialOption
	// NewEnrollClient overrides how the enrollment client is built.
	NewEnrollClient func(opts enroll.Options) (EnrollClient, error)
}

// EnrollClient is the part of enroll.Client the supervisor needs. Exported so a
// test can supply one that reaches a local server.
type EnrollClient = enrollClient

// New builds a Supervisor.
func New(opts Options) (*Supervisor, error) {
	if opts.Config == nil {
		return nil, errors.New("supervisor: a Config is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Collector == nil {
		opts.Collector = collect.New()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	bootID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("supervisor: generate a boot id: %w", err)
	}

	sv := &Supervisor{
		cfg:         opts.Config,
		log:         opts.Logger,
		version:     opts.Version,
		store:       identity.NewStore(opts.Config.StateDir),
		collector:   opts.Collector,
		bootID:      bootID.String(),
		now:         opts.Now,
		dialOptions: opts.DialOptions,
	}
	if opts.NewEnrollClient != nil {
		sv.newEnrollClient = opts.NewEnrollClient
	}
	return sv, nil
}

// newClient builds the device-plane client.
func (s *Supervisor) newClient(id *identity.Identity, sp *spool.Spool) (*client.Client, error) {
	return client.New(client.Options{
		Identity:    id,
		Endpoint:    s.cfg.Endpoint,
		Spool:       sp,
		Facts:       s.collector,
		Version:     s.version,
		BootID:      s.bootID,
		Logger:      s.log,
		DialOptions: s.dialOptions,
		Now:         s.now,
	})
}

// Run brings the agent up and keeps it there until the context is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	id, err := s.identity(ctx)
	if err != nil {
		return err
	}

	sp, err := spool.Open(spool.Options{
		Dir:      s.cfg.SpoolDir,
		MaxBytes: s.cfg.SpoolMaxBytes,
		Logger:   s.log,
	})
	if err != nil {
		return fmt.Errorf("supervisor: open the spool: %w", err)
	}
	defer func() {
		if closeErr := sp.Close(); closeErr != nil {
			s.log.Warn("closing the spool", slog.Any("error", closeErr))
		}
	}()

	if n := sp.Len(); n > 0 {
		s.log.Info("resuming with buffered messages from a previous run",
			slog.Int("messages", n), slog.Int64("bytes", sp.Bytes()))
	}

	c, err := s.newClient(id, sp)
	if err != nil {
		return err
	}

	s.log.Info("agent starting",
		slog.String("version", s.version),
		slog.String("device_id", id.DeviceID.String()),
		slog.String("endpoint", s.endpoint(id)),
		slog.Bool("approved", id.Approved),
		slog.Time("certificate_expires", id.NotAfter))

	runErr := c.Run(ctx)

	switch {
	case errors.Is(runErr, client.ErrDeleted):
		// The device was decommissioned. Leaving a key and certificate on a Mac
		// that may be resold or repurposed is exactly the credential nobody
		// remembers to clean up.
		s.log.Warn("this device has been decommissioned; removing its identity")
		if wipeErr := s.store.Wipe(); wipeErr != nil {
			s.log.Error("removing the identity failed", slog.Any("error", wipeErr))
			return errors.Join(runErr, wipeErr)
		}
		return runErr

	case errors.Is(runErr, client.ErrRevoked):
		s.log.Error("this device's enrollment has been revoked; " +
			"an operator must re-enroll it")
		return runErr

	case errors.Is(runErr, client.ErrCertificateExpired):
		s.log.Error("this device's certificate has expired; " +
			"it was offline for longer than the certificate's life and must be re-enrolled")
		return runErr
	}
	return runErr
}

// identity loads the device's identity, enrolling first if there is none.
func (s *Supervisor) identity(ctx context.Context) (*identity.Identity, error) {
	if s.store.Exists() {
		id, err := s.store.Load()
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, identity.ErrNotEnrolled) {
			// A corrupt or insecure identity is not something to enroll past:
			// re-enrolling would register a second device for this Mac and
			// leave the first one looking permanently offline.
			return nil, fmt.Errorf("supervisor: this device's stored identity "+
				"cannot be used, and re-enrolling would duplicate it: %w", err)
		}
	}

	if err := s.cfg.RequireEnrollment(); err != nil {
		return nil, err
	}
	return s.enroll(ctx)
}

// enroll performs first-boot enrollment, retrying while retrying can help.
func (s *Supervisor) enroll(ctx context.Context) (*identity.Identity, error) {
	token, tokenPath, err := s.token()
	if err != nil {
		return nil, err
	}

	facts, err := s.collector.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("supervisor: collect this machine's facts: %w", err)
	}
	if facts.SerialNumber == "" {
		// Enrollment is idempotent on the serial number. Without one, every
		// retry would register another device.
		return nil, fmt.Errorf("supervisor: %w", collect.ErrNoSerialNumber)
	}

	key, err := identity.GenerateKey()
	if err != nil {
		return nil, err
	}
	deviceID, err := identity.NewDeviceID()
	if err != nil {
		return nil, err
	}
	csr, err := identity.CreateCSR(key, deviceID)
	if err != nil {
		return nil, err
	}

	var caPEM []byte
	if s.cfg.ServerCAFile != "" {
		if caPEM, err = os.ReadFile(s.cfg.ServerCAFile); err != nil {
			return nil, fmt.Errorf("supervisor: read %s: %w", s.cfg.ServerCAFile, err)
		}
	}

	newClient := s.newEnrollClient
	if newClient == nil {
		newClient = func(opts enroll.Options) (enrollClient, error) { return enroll.New(opts) }
	}
	ec, err := newClient(enroll.Options{
		BaseURL: s.cfg.ServerURL, Token: token, ServerCAPEM: caPEM, Logger: s.log,
	})
	if err != nil {
		return nil, err
	}

	req := enroll.Request{
		DeviceID: deviceID, CSRDer: csr,
		SerialNumber: facts.SerialNumber, HardwareUUID: facts.HardwareUUID,
		Hostname: facts.Hostname, ModelIdentifier: facts.ModelIdentifier,
		Chip: facts.Chip, Arch: facts.Arch, CPUCores: facts.CPUCores,
		MemoryBytes: facts.MemoryBytes, DiskTotalBytes: facts.DiskTotalBytes,
		OSVersion: facts.OSVersion, OSBuild: facts.OSBuild,
		AgentVersion: s.version, LocalIP: facts.LocalIP,
	}

	b := &backoff.Backoff{Initial: 2 * time.Second, Max: s.cfg.EnrollRetryMax}
	for attempt := 1; ; attempt++ {
		result, enrollErr := ec.Enroll(ctx, req)
		if enrollErr == nil {
			return s.persist(key, result, tokenPath)
		}

		if !enroll.Retryable(enrollErr) {
			// A rejected token or an already-enrolled serial will produce the
			// same answer forever. Stopping makes it visible; retrying makes it
			// a load problem on the server and a silent failure here.
			s.log.Error("enrollment was refused and retrying cannot help",
				slog.Any("error", enrollErr))
			return nil, fmt.Errorf("%w: %w", ErrEnrollmentImpossible, enrollErr)
		}

		delay := b.Next()
		s.log.Warn("enrollment failed; retrying",
			slog.Int("attempt", attempt), slog.Duration("in", delay),
			slog.Any("error", enrollErr))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// persist stores the enrolled identity and disposes of the used token.
func (s *Supervisor) persist(key crypto.Signer, result *enroll.Result, tokenPath string) (*identity.Identity, error) {
	id := &identity.Identity{
		DeviceID:            result.DeviceID,
		OrgID:               result.OrgID,
		Key:                 key,
		CertificatePEM:      result.CertificatePEM,
		CABundlePEM:         result.CABundlePEM,
		NotAfter:            result.NotAfter,
		Approved:            result.Approved,
		DevicePlaneEndpoint: result.DevicePlaneEndpoint,
		SigningPublicKeys:   result.SigningPublicKeys,
	}
	if err := s.store.Save(id); err != nil {
		return nil, err
	}

	s.log.Info("this device is enrolled",
		slog.String("device_id", id.DeviceID.String()),
		slog.Bool("approved", id.Approved),
		slog.Time("certificate_expires", id.NotAfter))
	if !id.Approved {
		s.log.Warn("this device is awaiting approval; it will connect and " +
			"report, but will be dispatched no commands until an administrator approves it")
	}

	// The token has done its job. A single-use credential left on disk outlives
	// its usefulness immediately and outlives the machine's ownership for
	// years.
	if tokenPath != "" {
		if err := enroll.RemoveTokenFile(tokenPath); err != nil {
			// Worth surfacing, not worth failing: the device is enrolled, and
			// refusing to start over a leftover file would be worse than the
			// file.
			s.log.Warn("the used enrollment token could not be removed; "+
				"delete it by hand", slog.String("path", tokenPath), slog.Any("error", err))
		}
	}
	return id, nil
}

// token returns the enrollment token and the path it came from, if any.
func (s *Supervisor) token() (token, path string, err error) {
	if s.cfg.Token != "" {
		// Injected directly, so there is no file to clean up afterwards.
		return s.cfg.Token, "", nil
	}
	token, err = enroll.ReadTokenFile(s.cfg.TokenFile)
	if err != nil {
		return "", "", err
	}
	return token, s.cfg.TokenFile, nil
}

func (s *Supervisor) endpoint(id *identity.Identity) string {
	if s.cfg.Endpoint != "" {
		return s.cfg.Endpoint
	}
	return id.DevicePlaneEndpoint
}
