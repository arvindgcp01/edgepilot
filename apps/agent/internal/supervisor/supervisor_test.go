package supervisor_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/visionify/edgepilot/apps/agent/internal/client"
	"github.com/visionify/edgepilot/apps/agent/internal/collect"
	"github.com/visionify/edgepilot/apps/agent/internal/config"
	"github.com/visionify/edgepilot/apps/agent/internal/enroll"
	"github.com/visionify/edgepilot/apps/agent/internal/identity"
	"github.com/visionify/edgepilot/apps/agent/internal/supervisor"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// These exercise the whole first-boot path: no identity on disk, enroll, persist,
// connect. The enrollment endpoint and the device plane are both stood up here,
// so what is being tested is the sequence and its failure handling rather than
// any one component.

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testCA issues the certificates the enrollment response carries.
type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "EdgePilot Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{
		key: key, cert: cert,
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// sign issues a leaf for the CSR, as the real CA does — reading only the public
// key from it.
func (ca *testCA) sign(t *testing.T, csrDER []byte, deviceID uuid.UUID, notAfter time.Time) []byte {
	t.Helper()
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: deviceID.String()},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue leaf: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// fakeEnroller stands in for the server's enrollment endpoint.
type fakeEnroller struct {
	t  *testing.T
	ca *testCA

	mu       sync.Mutex
	requests []enroll.Request

	// err is returned instead of a certificate.
	err error
	// failuresBeforeSuccess makes the first N attempts fail with a retryable
	// error, as a site with a flapping uplink would.
	failuresBeforeSuccess int
	approved              bool
	notAfter              time.Time
	endpoint              string
}

func (f *fakeEnroller) Enroll(_ context.Context, req enroll.Request) (*enroll.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	attempt := len(f.requests)
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	if attempt <= f.failuresBeforeSuccess {
		return nil, enroll.ErrUnavailable
	}

	notAfter := f.notAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(90 * 24 * time.Hour)
	}
	return &enroll.Result{
		DeviceID:            req.DeviceID,
		OrgID:               uuid.New(),
		CertificatePEM:      f.ca.sign(f.t, req.CSRDer, req.DeviceID, notAfter),
		CABundlePEM:         f.ca.pem,
		NotAfter:            notAfter,
		Approved:            f.approved,
		DevicePlaneEndpoint: f.endpoint,
	}, nil
}

func (f *fakeEnroller) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeEnroller) lastRequest() enroll.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return enroll.Request{}
	}
	return f.requests[len(f.requests)-1]
}

// devicePlane is a minimal AgentService that accepts and holds streams.
type devicePlane struct {
	v1.UnimplementedAgentServiceServer

	mu          sync.Mutex
	connections int
	hellos      []*v1.Hello

	// disconnect ends every stream with this reason, when set.
	disconnect v1.Disconnect_Reason
}

func (d *devicePlane) Connect(stream v1.AgentService_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errors.New("the first message must be Hello")
	}

	d.mu.Lock()
	d.connections++
	d.hellos = append(d.hellos, hello)
	d.mu.Unlock()

	if err := stream.Send(&v1.ServerMessage{
		Seq: 1, SentAt: timestamppb.Now(),
		Payload: &v1.ServerMessage_Welcome{Welcome: &v1.Welcome{
			ServerTime: timestamppb.Now(), Approved: true,
			Config: &v1.ConfigUpdate{HeartbeatIntervalS: 1},
		}},
	}); err != nil {
		return err
	}

	if d.disconnect != v1.Disconnect_REASON_UNSPECIFIED {
		return stream.Send(&v1.ServerMessage{
			Seq: 2, SentAt: timestamppb.Now(),
			Payload: &v1.ServerMessage_Disconnect{Disconnect: &v1.Disconnect{
				Reason: d.disconnect, Message: "scripted by the test",
			}},
		})
	}

	for {
		if _, err := stream.Recv(); err != nil {
			// The agent hung up, which ends this handler normally.
			return nil //nolint:nilerr // the client closing the stream ends the handler
		}
	}
}

func (d *devicePlane) connectionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connections
}

// fixture wires a supervisor to both fakes.
type fixture struct {
	cfg       *config.Config
	enroller  *fakeEnroller
	plane     *devicePlane
	sv        *supervisor.Supervisor
	stateDir  string
	tokenPath string
}

func newFixture(t *testing.T, tune func(*fixture, *supervisor.Options)) *fixture {
	t.Helper()

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	tokenPath := filepath.Join(dir, "enroll")
	if err := os.WriteFile(tokenPath, []byte("ep_enroll_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	plane := &devicePlane{}
	grpcServer := grpc.NewServer()
	v1.RegisterAgentServiceServer(grpcServer, plane)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	cfg := &config.Config{
		ServerURL:      "https://edgepilot.test",
		Endpoint:       "passthrough:///bufnet",
		TokenFile:      tokenPath,
		StateDir:       stateDir,
		SpoolDir:       filepath.Join(stateDir, "spool"),
		LogLevel:       "info",
		EnrollRetryMax: 20 * time.Millisecond,
	}

	enroller := &fakeEnroller{
		t: t, ca: newTestCA(t), approved: true, endpoint: "passthrough:///bufnet",
	}

	f := &fixture{
		cfg: cfg, enroller: enroller, plane: plane,
		stateDir: stateDir, tokenPath: tokenPath,
	}

	opts := supervisor.Options{
		Config:    cfg,
		Logger:    quiet(),
		Version:   "1.0.0-test",
		Collector: collect.New(),
		NewEnrollClient: func(enroll.Options) (supervisor.EnrollClient, error) {
			return enroller, nil
		},
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}
	if tune != nil {
		tune(f, &opts)
	}

	sv, err := supervisor.New(opts)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	f.sv = sv
	return f
}

func (f *fixture) run(t *testing.T) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.sv.Run(ctx) }()

	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("Run did not return after its context was cancelled")
		}
	}
}

func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

// --- first boot ------------------------------------------------------------

// The whole path a new Mac takes: no identity, enroll, persist, connect.
func TestAFreshMacEnrollsAndConnects(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	stop := f.run(t)

	waitFor(t, 30*time.Second, "the device to connect", func() bool {
		return f.plane.connectionCount() >= 1
	})

	if got := f.enroller.attempts(); got != 1 {
		t.Errorf("enrolled %d times, want 1", got)
	}

	// The enrollment carried this machine's real facts, not placeholders.
	req := f.enroller.lastRequest()
	if req.SerialNumber == "" {
		t.Error("enrollment sent no serial number, which is what makes it idempotent")
	}
	if req.Hostname == "" {
		t.Error("enrollment sent no hostname")
	}
	if req.AgentVersion != "1.0.0-test" {
		t.Errorf("agent version = %q, want 1.0.0-test", req.AgentVersion)
	}

	// The identity landed on disk, so a restart does not enroll again.
	store := identity.NewStore(f.stateDir)
	if !store.Exists() {
		t.Fatal("no identity was written")
	}
	id, err := store.Load()
	if err != nil {
		t.Fatalf("load the stored identity: %v", err)
	}
	if id.DeviceID != req.DeviceID {
		t.Errorf("stored device id %s, enrolled %s", id.DeviceID, req.DeviceID)
	}

	if err := stop(); err != nil {
		t.Errorf("stop: %v", err)
	}
}

// A single-use token left on disk is a live credential on a machine that may be
// resold, imaged, or backed up.
func TestTheEnrollmentTokenIsRemovedOnceUsed(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 30*time.Second, "the token file to be removed", func() bool {
		_, err := os.Stat(f.tokenPath)
		return errors.Is(err, os.ErrNotExist)
	})
}

// Restarting must not enroll again: a second enrollment would register a second
// device for one Mac and leave the first looking permanently offline.
func TestARestartReusesTheStoredIdentity(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	stop := f.run(t)
	waitFor(t, 30*time.Second, "the first connection", func() bool {
		return f.plane.connectionCount() >= 1
	})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	firstID := f.enroller.lastRequest().DeviceID

	// A second supervisor over the same state directory, as launchd would start
	// after a reboot.
	second, err := supervisor.New(supervisor.Options{
		Config: f.cfg, Logger: quiet(), Version: "1.0.0-test",
		Collector: collect.New(),
		NewEnrollClient: func(enroll.Options) (supervisor.EnrollClient, error) {
			t.Error("a restart must not enroll again")
			return f.enroller, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	store := identity.NewStore(f.stateDir)
	id, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id.DeviceID != firstID {
		t.Errorf("device id changed across a restart: %s then %s", firstID, id.DeviceID)
	}
	if f.enroller.attempts() != 1 {
		t.Errorf("enrolled %d times across a restart, want 1", f.enroller.attempts())
	}
	_ = second
}

// A site whose uplink is down at install time is the normal case, not an
// exception.
func TestEnrollmentIsRetriedWhileTheServerIsUnreachable(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.enroller.failuresBeforeSuccess = 3
	})
	stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 30*time.Second, "enrollment to succeed after retrying", func() bool {
		return f.plane.connectionCount() >= 1
	})
	if got := f.enroller.attempts(); got != 4 {
		t.Errorf("made %d attempts, want 4 (three failures then a success)", got)
	}
}

// A rejected token will produce the same answer forever. Retrying makes it a
// load problem on the server and a silent failure on the Mac.
func TestARejectedTokenStopsRatherThanRetrying(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.enroller.err = enroll.ErrTokenRejected
	})

	done := make(chan error, 1)
	go func() { done <- f.sv.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, supervisor.ErrEnrollmentImpossible) {
			t.Fatalf("Run returned %v, want ErrEnrollmentImpossible", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the agent kept retrying a rejected token")
	}

	if got := f.enroller.attempts(); got != 1 {
		t.Errorf("attempted enrollment %d times with a rejected token, want 1", got)
	}
	// And the token file survives, because it was never successfully used —
	// deleting it would destroy the evidence an operator needs.
	if _, err := os.Stat(f.tokenPath); err != nil {
		t.Errorf("the token file was removed after a failed enrollment: %v", err)
	}
}

func TestASerialAlreadyEnrolledStopsRatherThanRetrying(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.enroller.err = enroll.ErrAlreadyEnrolled
	})

	done := make(chan error, 1)
	go func() { done <- f.sv.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, supervisor.ErrEnrollmentImpossible) {
			t.Fatalf("Run returned %v, want ErrEnrollmentImpossible", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the agent kept retrying an already-enrolled serial")
	}
}

// Without a server to enroll against, the agent has to say so rather than
// looping on a condition only an installer can fix.
func TestAnUnconfiguredAgentSaysWhatIsMissing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.cfg.ServerURL = ""
	})

	err := f.sv.Run(t.Context())
	if err == nil {
		t.Fatal("an agent with no server URL should refuse to start")
	}
	if f.enroller.attempts() != 0 {
		t.Error("it tried to enroll without a server URL")
	}
}

func TestAMissingTokenFileIsReported(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.cfg.TokenFile = filepath.Join(t.TempDir(), "absent")
	})

	if err := f.sv.Run(t.Context()); err == nil {
		t.Fatal("an agent with no enrollment token should refuse to start")
	}
}

// --- being told to stop ----------------------------------------------------

// A decommissioned Mac may be resold or repurposed, and the key it holds would
// still be a valid credential.
func TestADecommissionedDeviceWipesItsIdentity(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.plane.disconnect = v1.Disconnect_REASON_DEVICE_DELETED
	})

	done := make(chan error, 1)
	go func() { done <- f.sv.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, client.ErrDeleted) {
			t.Fatalf("Run returned %v, want ErrDeleted", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a decommissioned device kept running")
	}

	store := identity.NewStore(f.stateDir)
	if store.Exists() {
		t.Error("the identity survived decommissioning")
	}
	entries, err := os.ReadDir(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "device.key" {
			t.Error("the private key is still on disk after decommissioning")
		}
	}
}

// A revoked device stops, but keeps its identity: revocation is reversible by an
// administrator, and wiping the key would turn a re-approval into a re-install.
func TestARevokedDeviceStopsButKeepsItsIdentity(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.plane.disconnect = v1.Disconnect_REASON_DEVICE_REVOKED
	})

	done := make(chan error, 1)
	go func() { done <- f.sv.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, client.ErrRevoked) {
			t.Fatalf("Run returned %v, want ErrRevoked", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a revoked device kept running")
	}

	if !identity.NewStore(f.stateDir).Exists() {
		t.Error("a revoked device lost its identity; re-approval would need a re-install")
	}
}

// --- a damaged identity ----------------------------------------------------

// Re-enrolling past a corrupt identity would register a second device for this
// Mac and leave the first looking permanently offline. Refusing to start is the
// safe direction.
func TestACorruptIdentityIsNotEnrolledPast(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	stop := f.run(t)
	waitFor(t, 30*time.Second, "the first connection", func() bool {
		return f.plane.connectionCount() >= 1
	})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// A restored backup that mixed up the key and the certificate.
	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := identity.EncodePrivateKey(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir, "device.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	attemptsBefore := f.enroller.attempts()
	second, err := supervisor.New(supervisor.Options{
		Config: f.cfg, Logger: quiet(), Version: "1.0.0-test", Collector: collect.New(),
		NewEnrollClient: func(enroll.Options) (supervisor.EnrollClient, error) {
			return f.enroller, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := second.Run(t.Context()); err == nil {
		t.Fatal("an agent with a mismatched key and certificate should refuse to start")
	}
	if f.enroller.attempts() != attemptsBefore {
		t.Error("it re-enrolled past a damaged identity, duplicating the device")
	}
}

// --- awaiting approval -----------------------------------------------------

// A device the server has not approved still connects and reports. It simply
// receives no commands — which is what makes an approval queue usable rather
// than a list of devices nobody can see.
func TestAnUnapprovedDeviceStillConnects(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(f *fixture, _ *supervisor.Options) {
		f.enroller.approved = false
	})
	stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 30*time.Second, "an unapproved device to connect", func() bool {
		return f.plane.connectionCount() >= 1
	})

	id, err := identity.NewStore(f.stateDir).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id.Approved {
		t.Error("the stored identity claims approval the server did not grant")
	}
}
