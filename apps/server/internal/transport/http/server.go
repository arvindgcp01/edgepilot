package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
	"github.com/visionify/edgepilot/apps/server/internal/service/enrollment"
)

// Server is the user-plane HTTP API.
type Server struct {
	auth        AuthService
	authn       Authenticator
	enrollment  EnrollmentService
	store       domain.Store
	tenants     *TenantResolver
	log         *slog.Logger
	now         func() time.Time
	refreshTTL  time.Duration
	agentLimits *RateLimiter

	// offlineAfter is how long without a heartbeat counts as offline. It has to
	// match the sweeper's threshold, or the fleet summary and the device list
	// disagree about the same device.
	offlineAfter time.Duration

	// devicePlaneEndpoint is what a freshly enrolled agent is told to dial.
	devicePlaneEndpoint string

	// sessions ends live SSH relays. Nil when the bastion is disabled.
	sessions SessionTerminator

	// The browser terminal. All three are set together or not at all: without a
	// signer there is nothing to authenticate with, and without a tunnel
	// registry there is nothing to connect over.
	terminalSigner ssh.Signer
	tunnels        TerminalOpener
	// sealer unseals a device's VNC password for the screen relay, and seals a
	// freshly generated one when screen sharing is enabled. Nil disables remote
	// screen entirely.
	sealer *secret.Sealer
	// notifyConfig asks the device's owning replica to push fresh policy, so a
	// screen-sharing toggle reaches the agent within a second rather than at its
	// next reconnect. Nil degrades to "applies on reconnect".
	notifyConfig   func(ctx context.Context, deviceID uuid.UUID)
	recordingDir   string
	sshIdleTimeout time.Duration
	replicaID      string

	// hub pushes realtime events to dashboards.
	hub *Hub
	// wsOrigins is the allowlist a WebSocket upgrade's Origin is checked
	// against. Separate from the CORS list only in shape: coder/websocket wants
	// host patterns, not full origins.
	wsOrigins []string

	loginLimiter *RateLimiter

	// insecureCookies drops the Secure attribute. Development only, and the
	// server refuses to start with it set unless the listener is loopback.
	insecureCookies bool

	handler         http.Handler
	http            *http.Server
	shutdownTimeout time.Duration
}

// EnrollmentService is what the agent-enrollment handler needs.
type EnrollmentService interface {
	Enroll(ctx context.Context, req enrollment.Request) (*enrollment.Result, error)
	Approve(ctx context.Context, deviceID uuid.UUID, approve bool) (*domain.Device, error)
}

// Options configures the Server.
type Options struct {
	Auth AuthService
	// Authenticator verifies access tokens. Normally the same object as Auth;
	// separate because the two are different jobs and a test may want only one.
	Authenticator Authenticator
	Enrollment    EnrollmentService
	Store         domain.Store
	Logger        *slog.Logger

	// Sessions ends live SSH relays, for the terminate endpoint. Optional: a
	// deployment without a bastion host key has none to end.
	Sessions SessionTerminator

	// TerminalSigner is the SSH identity the browser terminal logs into devices
	// with. Nil disables the browser terminal; `ssh -J` is unaffected.
	TerminalSigner ssh.Signer
	// Tunnels opens a relay for the browser terminal and the remote screen.
	Tunnels TerminalOpener
	// Sealer seals and unseals per-device VNC passwords for remote screen.
	Sealer *secret.Sealer
	// NotifyConfig pushes fresh policy to a device's agent immediately. Nil
	// means the change applies at the agent's next reconnect.
	NotifyConfig func(ctx context.Context, deviceID uuid.UUID)
	// RecordingDir is where terminal sessions are recorded. Empty disables it.
	RecordingDir string
	// SSHIdleTimeout bounds an idle terminal, matching the bastion's.
	SSHIdleTimeout time.Duration
	// ReplicaID is recorded on each session so an operator can tell which
	// server held it.
	ReplicaID string

	// OfflineAfter must match the offline sweeper's threshold.
	OfflineAfter time.Duration
	// DevicePlaneEndpoint is the address agents are told to dial after
	// enrollment, when the enrollment service does not supply one.
	DevicePlaneEndpoint string

	// Addr is the listen address.
	Addr string
	// BaseDomain enables per-tenant subdomain resolution at login. Empty
	// disables it.
	BaseDomain string
	// AllowedOrigins is the CORS allowlist for the dashboard.
	AllowedOrigins []string
	// TrustedProxies may set X-Forwarded-For and X-Forwarded-Proto.
	TrustedProxies []net.IPNet

	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	MaxRequestBytes int64
	// HandlerTimeout bounds one handler. Zero derives it from WriteTimeout.
	HandlerTimeout time.Duration

	// RefreshTTL is how long the refresh cookie lives; it must match the
	// session lifetime the auth service issues.
	RefreshTTL time.Duration

	// LoginAttempts and LoginWindow bound authentication attempts per source and
	// per account.
	LoginAttempts int
	LoginWindow   time.Duration

	// EnrollAttempts and EnrollWindow bound agent enrollment per source. A
	// leaked token is worth far less if it cannot be redeemed at speed.
	EnrollAttempts int
	EnrollWindow   time.Duration

	InsecureCookies bool

	Now func() time.Time
}

// New builds the server.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Auth == nil:
		return nil, errors.New("http: an Auth service is required")
	case opts.Store == nil:
		return nil, errors.New("http: a Store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Addr == "" {
		opts.Addr = ":8080"
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 15 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 30 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 120 * time.Second
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 30 * time.Second
	}
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = 1 << 20
	}
	if opts.HandlerTimeout <= 0 {
		// Just inside the write timeout, so a handler that overruns is
		// cancelled with time left to render the failure — rather than having
		// its connection cut mid-response, which the client sees as a truncated
		// body rather than an error.
		opts.HandlerTimeout = opts.WriteTimeout - time.Second
		if opts.HandlerTimeout <= 0 {
			opts.HandlerTimeout = opts.WriteTimeout
		}
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = 30 * 24 * time.Hour
	}
	if opts.LoginAttempts <= 0 {
		opts.LoginAttempts = 10
	}
	if opts.LoginWindow <= 0 {
		opts.LoginWindow = 15 * time.Minute
	}
	if opts.EnrollAttempts <= 0 {
		opts.EnrollAttempts = 30
	}
	if opts.EnrollWindow <= 0 {
		opts.EnrollWindow = time.Minute
	}
	if opts.OfflineAfter <= 0 {
		opts.OfflineAfter = 90 * time.Second
	}
	if opts.Authenticator == nil {
		// The auth service satisfies both interfaces in production. Falling back
		// to it keeps the common wiring to one field.
		authn, ok := opts.Auth.(Authenticator)
		if !ok {
			return nil, errors.New("http: an Authenticator is required when " +
				"the Auth service does not provide one")
		}
		opts.Authenticator = authn
	}

	s := &Server{
		auth:            opts.Auth,
		authn:           opts.Authenticator,
		enrollment:      opts.Enrollment,
		store:           opts.Store,
		tenants:         NewTenantResolver(opts.Store.Organizations(), opts.BaseDomain),
		log:             opts.Logger,
		sessions:        opts.Sessions,
		terminalSigner:  opts.TerminalSigner,
		tunnels:         opts.Tunnels,
		sealer:          opts.Sealer,
		notifyConfig:    opts.NotifyConfig,
		recordingDir:    opts.RecordingDir,
		sshIdleTimeout:  opts.SSHIdleTimeout,
		replicaID:       opts.ReplicaID,
		now:             opts.Now,
		refreshTTL:      opts.RefreshTTL,
		loginLimiter:    NewRateLimiter(opts.LoginAttempts, opts.LoginWindow, opts.Now),
		agentLimits:     NewRateLimiter(opts.EnrollAttempts, opts.EnrollWindow, opts.Now),
		insecureCookies: opts.InsecureCookies,

		offlineAfter:        opts.OfflineAfter,
		devicePlaneEndpoint: opts.DevicePlaneEndpoint,
		hub:                 NewHub(opts.Logger),
		wsOrigins:           websocketOrigins(opts.AllowedOrigins),
	}

	// CaptureRoute wraps the mux directly; see its comment for why it cannot sit
	// anywhere else in this chain.
	s.handler = Chain(CaptureRoute()(s.routes()),
		// Outermost first. Recovery wraps everything so a panic anywhere still
		// produces a response and an access-log line; the request id is set
		// before logging so every line carries it.
		RequestID(),
		RealIP(opts.TrustedProxies),
		Recover(opts.Logger),
		AccessLog(opts.Logger),
		SecurityHeaders(),
		CORS(opts.AllowedOrigins),
		MaxBody(opts.MaxRequestBytes),
		Timeout(opts.HandlerTimeout),
	)

	s.http = &http.Server{
		Addr:              opts.Addr,
		Handler:           s.handler,
		ReadTimeout:       opts.ReadTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      opts.WriteTimeout,
		IdleTimeout:       opts.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
	}
	s.shutdownTimeout = opts.ShutdownTimeout

	return s, nil
}

// Handler exposes the routed handler, for tests that drive it without a
// listener.
func (s *Server) Handler() http.Handler { return s.handler }

// Hub returns the realtime event hub, so the workers that detect a change can
// publish it.
func (s *Server) Hub() *Hub { return s.hub }

// websocketOrigins converts the CORS allowlist into the host patterns a
// WebSocket upgrade is checked against.
//
// The check matters more here than for CORS: a WebSocket is not subject to the
// same-origin policy, so without it any site a signed-in operator visits could
// open a socket to this API with their cookies attached.
func websocketOrigins(allowed []string) []string {
	origins := make([]string, 0, len(allowed))
	for _, origin := range allowed {
		trimmed := strings.TrimRight(origin, "/")
		if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
			origins = append(origins, u.Host)
			continue
		}
		origins = append(origins, trimmed)
	}
	return origins
}

// Serve listens and serves until the context is cancelled.
//
// Shutdown is graceful: in-flight requests finish, and only then does the
// listener close. An operator's device approval should not be lost because a
// deploy landed mid-request.
func (s *Server) Serve(ctx context.Context) error {
	// ListenConfig rather than net.Listen: binding can block — on a port in
	// TIME_WAIT, or behind a slow DNS lookup for a named address — and a
	// process being shut down should not have to wait it out.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("http: listen on %s: %w", s.http.Addr, err)
	}

	s.log.InfoContext(ctx, "the API is listening",
		slog.String("addr", listener.Addr().String()))

	serveErr := make(chan error, 1)
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	s.log.InfoContext(ctx, "the API is draining",
		slog.Duration("timeout", s.shutdownTimeout))

	// Dashboards are disconnected before the HTTP shutdown waits on them: a
	// WebSocket is a long-lived request, and Shutdown would otherwise block for
	// its full timeout on every connected browser.
	s.hub.Close()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// The deadline passed with requests still running. Closing is the only
		// remaining option, and saying so beats a silent hang.
		s.log.WarnContext(ctx, "the API did not drain in time; closing anyway",
			slog.Any("error", err))
		return errors.Join(err, s.http.Close())
	}
	return <-serveErr
}
