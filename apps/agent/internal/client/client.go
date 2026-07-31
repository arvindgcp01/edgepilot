// Package client is the agent's side of the device plane.
//
// One long-lived bidirectional gRPC stream, held open for the life of the
// process, reconnected when it drops. Everything the agent says and everything
// the server tells it travels over this stream.
//
// # The shape of the loop
//
//	Run
//	 └─ for {
//	      dial (mTLS, server pinned to the CA bundle from enrollment)
//	        └─ Hello  →  Welcome
//	             └─ steady state: heartbeat, drain the spool, answer pings
//	      wait out the backoff
//	    }
//
// # What is deliberately not here
//
// Command execution, file transfer, and SSH tunnels are Phase 2. Messages for
// them are counted and logged rather than silently dropped: an agent that
// quietly ignores a command the server believes it dispatched is worse than one
// that says it cannot do it yet.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/visionify/edgepilot/apps/agent/internal/backoff"
	"github.com/visionify/edgepilot/apps/agent/internal/collect"
	"github.com/visionify/edgepilot/apps/agent/internal/identity"
	"github.com/visionify/edgepilot/apps/agent/internal/spool"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// Terminal conditions: the server has told the agent to stop, and reconnecting
// would only produce the same answer.
var (
	// ErrRevoked means this device's enrollment was revoked. The agent stops
	// and stays stopped; an operator has to re-enroll it.
	ErrRevoked = errors.New("client: this device's enrollment has been revoked")

	// ErrDeleted means the device was decommissioned. The identity is wiped, so
	// the Mac holds no usable credential afterwards.
	ErrDeleted = errors.New("client: this device has been decommissioned")

	// ErrCertificateExpired means the certificate is past its validity. There
	// is no point dialling: renewal happens over an authenticated stream this
	// agent can no longer establish.
	ErrCertificateExpired = errors.New("client: the device certificate has expired")
)

// healthyFor is how long a connection must hold before the backoff resets.
//
// Resetting on connect alone would hammer a server that accepts connections and
// immediately closes them, which is exactly how an overloaded server behaves.
const healthyFor = 60 * time.Second

// Message kinds, stored with each spooled record so a backlog can be described
// without decoding it.
const (
	KindEvent uint16 = iota + 1
	KindCommandResult
	KindDeploymentProgress
	KindInventory
)

// Client maintains the stream.
type Client struct {
	identity *identity.Identity
	endpoint string
	spool    *spool.Spool
	facts    collect.Collector
	log      *slog.Logger

	version string
	bootID  string

	dialOptions []grpc.DialOption
	now         func() time.Time
	newBackoff  func() *backoff.Backoff

	// policy is the last ConfigUpdate the server sent, applied atomically.
	// Persisted by the supervisor so a restart while offline keeps it.
	policy atomic.Pointer[Policy]

	// lastSeqSeen is the highest server sequence durably processed. Sent in
	// Hello so the server can resume rather than restart.
	lastSeqSeen atomic.Uint64

	connected atomic.Bool
	approved  atomic.Bool

	// bootedAt is the machine's boot time, learned when facts are collected for
	// Hello. Held so heartbeats can report uptime without shelling out to
	// sysctl every fifteen seconds.
	bootedAt atomic.Pointer[time.Time]

	// Counters, surfaced by Stats.
	connects      atomic.Int64
	sends         atomic.Int64
	receives      atomic.Int64
	unhandled     atomic.Int64
	lastConnected atomic.Int64 // unix nanos

	// onPolicy is called whenever the server changes policy, so the supervisor
	// can persist it without this package knowing where.
	onPolicy func(Policy)
}

// Policy is the server-supplied configuration from Welcome and ConfigUpdate.
type Policy struct {
	HeartbeatInterval     time.Duration
	MetricsInterval       time.Duration
	InventoryInterval     time.Duration
	AllowCustomScripts    bool
	EnabledCommandSlugs   []string
	MaxConcurrentCommands int
	SpoolMaxBytes         int64
	SigningPublicKeys     [][]byte
	LogLevel              string
}

// Options configures a Client.
type Options struct {
	// Identity carries the device key, certificate, and the CA bundle the
	// server is verified against.
	Identity *identity.Identity
	// Endpoint overrides the endpoint recorded at enrollment.
	Endpoint string
	// Spool holds durable messages while disconnected.
	Spool *spool.Spool
	// Facts supplies the hostname and OS version sent in Hello, which change
	// without an inventory refresh.
	Facts collect.Collector

	Version string
	BootID  string
	Logger  *slog.Logger

	// DialOptions are appended to the defaults. Tests use this to reach a
	// local listener; production leaves it empty so the mTLS credentials below
	// apply.
	DialOptions []grpc.DialOption

	// OnPolicy is called when the server changes policy.
	OnPolicy func(Policy)

	Now        func() time.Time
	NewBackoff func() *backoff.Backoff
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	switch {
	case opts.Identity == nil:
		return nil, errors.New("client: an Identity is required")
	case opts.Spool == nil:
		return nil, errors.New("client: a Spool is required")
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = opts.Identity.DevicePlaneEndpoint
	}
	if endpoint == "" {
		return nil, errors.New("client: no device-plane endpoint; " +
			"enrollment should have supplied one")
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewBackoff == nil {
		opts.NewBackoff = backoff.New
	}
	if opts.BootID == "" {
		return nil, errors.New("client: a boot id is required; it is what " +
			"distinguishes a restart from a reconnect")
	}

	c := &Client{
		identity: opts.Identity,
		endpoint: endpoint,
		spool:    opts.Spool,
		facts:    opts.Facts,
		log: opts.Logger.With(
			slog.String("device_id", opts.Identity.DeviceID.String())),
		version:     opts.Version,
		bootID:      opts.BootID,
		dialOptions: opts.DialOptions,
		now:         opts.Now,
		newBackoff:  opts.NewBackoff,
		onPolicy:    opts.OnPolicy,
	}
	c.policy.Store(defaultPolicy())
	c.approved.Store(opts.Identity.Approved)
	return c, nil
}

func defaultPolicy() *Policy {
	return &Policy{
		HeartbeatInterval:     15 * time.Second,
		MetricsInterval:       30 * time.Second,
		InventoryInterval:     6 * time.Hour,
		MaxConcurrentCommands: 4,
		SpoolMaxBytes:         spool.DefaultMaxBytes,
	}
}

// Run holds the stream open until the context is cancelled or the server says
// to stop for good.
//
// Returns nil on a clean shutdown, or one of the terminal errors above. Every
// other failure is retried: a customer site's uplink coming back six hours later
// is the normal case, not an exception.
func (c *Client) Run(ctx context.Context) error {
	b := c.newBackoff()

	for {
		if ctx.Err() != nil {
			// A cancelled context is a shutdown, not a failure: launchd asked
			// the agent to stop and it did.
			return nil //nolint:nilerr // a cancelled context is a clean stop
		}

		if c.identity.Expired(c.now()) {
			// Dialling would fail the handshake anyway, and retrying forever
			// would hide the one thing an operator needs to see.
			c.log.Error("the device certificate has expired; "+
				"this device must be re-enrolled",
				slog.Time("not_after", c.identity.NotAfter))
			return ErrCertificateExpired
		}

		start := c.now()
		err := c.connectOnce(ctx)
		uptime := c.now().Sub(start)

		switch {
		case err == nil, errors.Is(err, context.Canceled):
			if ctx.Err() != nil {
				// Shutting down: the stream ended because this agent is
				// stopping, which is not something to report as a failure.
				return nil //nolint:nilerr // a cancelled context is a clean stop
			}
		case errors.Is(err, ErrRevoked), errors.Is(err, ErrDeleted):
			return err
		}

		if uptime >= healthyFor {
			// The connection was healthy; the next failure starts from the
			// bottom rather than from wherever the last outage left off.
			b.Reset()
		}

		delay := c.reconnectDelay(b, err)
		if err != nil {
			c.log.Warn("the connection to the server ended; reconnecting",
				slog.Duration("after", delay),
				slog.Int("attempt", b.Attempt()),
				slog.Duration("connection_lasted", uptime.Truncate(time.Millisecond)),
				slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// reconnectDelay turns the reason a stream ended into a wait.
//
// A replica that says it is draining should be reconnected to at once: it is
// going away by design, another is ready, and backing off would make a rolling
// deploy look like an outage across the fleet.
func (c *Client) reconnectDelay(b *backoff.Backoff, err error) time.Duration {
	var stop *serverStop
	if errors.As(err, &stop) {
		switch stop.reason {
		case v1.Disconnect_REASON_SHUTTING_DOWN:
			// Immediate, but not synchronised: ten thousand agents reconnecting
			// in the same millisecond is the storm the backoff exists to
			// prevent, so a short jittered window still applies.
			jitter := &backoff.Backoff{Initial: 2 * time.Second, Max: 2 * time.Second}
			return jitter.Next()
		case v1.Disconnect_REASON_REBALANCE:
			jitter := &backoff.Backoff{Initial: 30 * time.Second, Max: 30 * time.Second}
			return jitter.Next()
		}
		if stop.retryAfter > 0 {
			return stop.retryAfter
		}
	}
	return b.Next()
}

// serverStop carries a deliberate server-side disconnect.
type serverStop struct {
	reason     v1.Disconnect_Reason
	message    string
	retryAfter time.Duration
}

func (s *serverStop) Error() string {
	return fmt.Sprintf("the server closed the stream: %s (%s)",
		s.message, s.reason.String())
}

// connectOnce dials, runs the stream, and returns when it ends.
func (c *Client) connectOnce(ctx context.Context) error {
	creds, err := c.transportCredentials()
	if err != nil {
		return err
	}

	options := append([]grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		// Keepalives are what turn a silently dead NAT binding into a
		// detectable disconnect. Without them an agent behind a router that
		// dropped its state sits happily on a socket nothing is listening to.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: false,
		}),
	}, c.dialOptions...)

	conn, err := grpc.NewClient(c.endpoint, options...)
	if err != nil {
		return fmt.Errorf("client: dial %s: %w", c.endpoint, err)
	}
	defer func() { _ = conn.Close() }()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := v1.NewAgentServiceClient(conn).Connect(streamCtx)
	if err != nil {
		return fmt.Errorf("client: open the stream: %w", err)
	}

	welcome, err := c.handshake(streamCtx, stream)
	if err != nil {
		return err
	}

	c.connected.Store(true)
	c.connects.Add(1)
	c.lastConnected.Store(c.now().UnixNano())
	defer c.connected.Store(false)

	c.log.Info("connected to the server",
		slog.String("endpoint", c.endpoint),
		slog.Bool("approved", welcome.GetApproved()),
		slog.Uint64("resume_from_seq", welcome.GetResumeFromSeq()),
		slog.Int("spooled_messages", c.spool.Len()))

	return c.serve(streamCtx, stream)
}

// transportCredentials builds the mTLS configuration.
//
// The server is verified against the CA bundle issued at enrollment, not the
// system roots. That is what stops a device being redirected to another server
// by a compromised trust store or a DNS answer: the only server this device will
// talk to is one holding a certificate from its own CA.
func (c *Client) transportCredentials() (credentials.TransportCredentials, error) {
	keyPEM, err := identity.EncodePrivateKey(c.identity.Key)
	if err != nil {
		return nil, fmt.Errorf("client: encode the device key: %w", err)
	}
	cert, err := tls.X509KeyPair(c.identity.CertificatePEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("client: build the client certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.identity.CABundlePEM) {
		return nil, errors.New("client: the CA bundle contains no usable certificate")
	}

	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
		ServerName:   hostFromEndpoint(c.endpoint),
	}), nil
}

// handshake sends Hello and waits for Welcome.
func (c *Client) handshake(ctx context.Context, stream v1.AgentService_ConnectClient) (*v1.Welcome, error) {
	hello := &v1.Hello{
		AgentVersion:    c.version,
		BootId:          c.bootID,
		LastSeqSeen:     c.lastSeqSeen.Load(),
		SpooledMessages: uint32(c.spool.Len()),
	}

	// Hostname and OS version travel in every Hello because they change without
	// an inventory refresh — a renamed Mac or a point upgrade would otherwise
	// show stale values until the next six-hourly report.
	if c.facts != nil {
		factsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		facts, err := c.facts.Collect(factsCtx)
		cancel()
		if err != nil {
			// Not fatal: connecting without a hostname is better than not
			// connecting.
			c.log.Warn("collecting facts for Hello failed; connecting without them",
				slog.Any("error", err))
		} else {
			hello.Hostname = facts.Hostname
			hello.OsVersion = facts.OSVersion
			hello.OsBuild = facts.OSBuild
			hello.LocalIp = facts.LocalIP
			if !facts.BootedAt.IsZero() {
				hello.BootedAt = timestamppb.New(facts.BootedAt)
				booted := facts.BootedAt
				c.bootedAt.Store(&booted)
			}
		}
	}

	if err := stream.Send(&v1.AgentMessage{
		SentAt:  timestamppb.New(c.now()),
		Payload: &v1.AgentMessage_Hello{Hello: hello},
	}); err != nil {
		return nil, fmt.Errorf("client: send Hello: %w", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		return nil, c.classifyStreamError(err)
	}
	welcome := msg.GetWelcome()
	if welcome == nil {
		return nil, fmt.Errorf("client: expected Welcome, got %T", msg.GetPayload())
	}

	c.applyWelcome(welcome)
	return welcome, nil
}

func (c *Client) applyWelcome(welcome *v1.Welcome) {
	c.approved.Store(welcome.GetApproved())
	if cfg := welcome.GetConfig(); cfg != nil {
		c.applyConfig(cfg)
	}
	// resume_from_seq is where the server wants the agent to continue from. It
	// is the server's own outbound sequence, so adopting it is what makes a
	// reconnect a resume rather than a restart.
	if resume := welcome.GetResumeFromSeq(); resume > 0 {
		c.lastSeqSeen.Store(resume)
	}
}

func (c *Client) applyConfig(cfg *v1.ConfigUpdate) {
	policy := &Policy{
		HeartbeatInterval:     secondsOr(cfg.GetHeartbeatIntervalS(), 15*time.Second),
		MetricsInterval:       secondsOr(cfg.GetMetricsIntervalS(), 30*time.Second),
		InventoryInterval:     secondsOr(cfg.GetInventoryIntervalS(), 6*time.Hour),
		AllowCustomScripts:    cfg.GetAllowCustomScripts(),
		EnabledCommandSlugs:   cfg.GetEnabledCommandSlugs(),
		MaxConcurrentCommands: int(cfg.GetMaxConcurrentCommands()),
		SpoolMaxBytes:         int64(cfg.GetSpoolMaxBytes()),
		SigningPublicKeys:     cfg.GetSigningPublicKeys(),
		LogLevel:              cfg.GetLogLevel(),
	}
	if policy.MaxConcurrentCommands <= 0 {
		policy.MaxConcurrentCommands = 4
	}
	c.policy.Store(policy)

	if c.onPolicy != nil {
		c.onPolicy(*policy)
	}
}

func secondsOr(s uint32, fallback time.Duration) time.Duration {
	if s == 0 {
		return fallback
	}
	return time.Duration(s) * time.Second
}

// classifyStreamError turns a gRPC status into something the loop can act on.
func (c *Client) classifyStreamError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.PermissionDenied:
		// The server rejected the device itself, not the request.
		return fmt.Errorf("%w: %s", ErrRevoked, st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrDeleted, st.Message())
	default:
		return err
	}
}

// Stats reports the client's state, for logging and the local status command.
func (c *Client) Stats() Stats {
	var lastConnected time.Time
	if n := c.lastConnected.Load(); n != 0 {
		lastConnected = time.Unix(0, n).UTC()
	}
	return Stats{
		Connected:     c.connected.Load(),
		Approved:      c.approved.Load(),
		Connects:      c.connects.Load(),
		Sent:          c.sends.Load(),
		Received:      c.receives.Load(),
		Unhandled:     c.unhandled.Load(),
		Spooled:       c.spool.Len(),
		SpoolBytes:    c.spool.Bytes(),
		SpoolDropped:  c.spool.Dropped(),
		LastSeqSeen:   c.lastSeqSeen.Load(),
		LastConnected: lastConnected,
	}
}

// Stats is a point-in-time view of the client.
type Stats struct {
	Connected     bool
	Approved      bool
	Connects      int64
	Sent          int64
	Received      int64
	Unhandled     int64
	Spooled       int
	SpoolBytes    int64
	SpoolDropped  uint64
	LastSeqSeen   uint64
	LastConnected time.Time
}

// Policy returns the configuration currently in force.
func (c *Client) Policy() Policy { return *c.policy.Load() }

// Connected reports whether the stream is up.
func (c *Client) Connected() bool { return c.connected.Load() }

// Approved reports whether the server will dispatch commands to this device.
func (c *Client) Approved() bool { return c.approved.Load() }

// Report buffers a durable message for delivery.
//
// Durable, not perishable: this goes to disk and survives a restart. Metrics and
// heartbeats do not come through here — see the spool package for why.
func (c *Client) Report(kind uint16, msg proto.Message) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("client: encode a durable message: %w", err)
	}
	if _, err := c.spool.Append(kind, payload); err != nil {
		return fmt.Errorf("client: buffer a durable message: %w", err)
	}
	return nil
}

// hostFromEndpoint strips the port for TLS server-name verification.
func hostFromEndpoint(endpoint string) string {
	for i := len(endpoint) - 1; i >= 0; i-- {
		switch endpoint[i] {
		case ':':
			return endpoint[:i]
		case ']':
			return endpoint
		}
	}
	return endpoint
}
