// Command edgepilot-server is the EdgePilot management server.
//
// It runs four things in one process:
//
//	:8080  the REST API the dashboard and epctl speak
//	:9443  the mTLS device plane every agent holds a stream to
//	:9090  health, readiness, and metrics for the orchestrator
//	       background workers — presence flushing, the offline sweeper
//
// One process rather than four services, because they share the same database
// pool, the same Redis connection, and the same in-memory stream registry.
// Splitting them would mean the API asking another process which replica holds a
// device's stream, which is the routing problem the bus already solves between
// replicas.
//
// # Startup order
//
// Configuration, then the database, then migrations, then everything that
// depends on them. Anything that can fail is failed before a listener opens, so
// a misconfigured deploy never accepts a request it cannot serve.
//
// # Shutdown order
//
// The reverse, and it matters: the device plane drains first so agents are told
// to reconnect elsewhere while the API is still up to serve the dashboard, and
// the workers flush last so the final presence batch reaches PostgreSQL.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/config"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
	"github.com/visionify/edgepilot/apps/server/internal/platform/migrate"
	"github.com/visionify/edgepilot/apps/server/internal/platform/password"
	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/service/auth"
	"github.com/visionify/edgepilot/apps/server/internal/service/enrollment"
	"github.com/visionify/edgepilot/apps/server/internal/service/revocation"
	epgrpc "github.com/visionify/edgepilot/apps/server/internal/transport/grpc"
	ephttp "github.com/visionify/edgepilot/apps/server/internal/transport/http"
	"github.com/visionify/edgepilot/apps/server/internal/worker"
	"github.com/visionify/edgepilot/apps/server/migrations"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// Set by the linker; see the build-server target in the Makefile.
var (
	version = "dev"
	commit  = "none"
)

// Exit codes. An operator reading `kubectl get pods` should be able to tell a
// misconfiguration from a crash without opening the log.
const (
	exitOK      = 0
	exitConfig  = 78 // EX_CONFIG
	exitFailure = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Fprintf(stdout, "edgepilot-server %s (%s)\n", version, commit)
			return exitOK
		default:
			fmt.Fprintf(stderr, "edgepilot-server: unexpected argument %q\n"+
				"Configuration is environment-only; see docs/Deployment.md §5.\n", args[0])
			return exitConfig
		}
	}

	cfg, err := config.Load()
	if err != nil {
		// Before the logger exists. Every problem is listed at once, so fixing a
		// deploy is one pass rather than one restart per mistake.
		fmt.Fprintf(stderr, "edgepilot-server: %v\n", err)
		return exitConfig
	}

	log := logging.New(logging.Options{
		Level: cfg.Log.Level, Format: cfg.Log.Format, Output: stderr,
	}).With(
		slog.String("version", version),
		slog.String("replica_id", cfg.ReplicaID))

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := serve(ctx, cfg, log); err != nil {
		log.Error("the server stopped", slog.Any("error", err))
		return exitFailure
	}
	log.Info("the server stopped cleanly")
	return exitOK
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	// --- migrations --------------------------------------------------------
	//
	// Every replica runs this. One takes the advisory lock and migrates; the
	// rest wait and find nothing to do. See internal/platform/migrate for why
	// the wait is bounded in two places.
	runner, err := migrate.New(migrate.Options{
		DatabaseURL: cfg.Database.URL.Reveal(),
		Source:      migrations.FS,
		Logger:      log,
	})
	if err != nil {
		return fmt.Errorf("open the migration runner: %w", err)
	}
	if err := runner.Up(ctx); err != nil {
		_ = runner.Close()
		return fmt.Errorf("apply migrations: %w", err)
	}
	if err := runner.Close(); err != nil {
		log.Warn("closing the migration runner", slog.Any("error", err))
	}

	// --- database ----------------------------------------------------------

	store, err := postgres.New(ctx, postgres.Options{
		DatabaseURL:      cfg.Database.URL.Reveal(),
		MaxConns:         int32(cfg.Database.MaxOpenConns),
		MinConns:         int32(cfg.Database.MaxIdleConns),
		MaxConnLifetime:  cfg.Database.ConnMaxLifetime,
		MaxConnIdleTime:  cfg.Database.ConnMaxIdleTime,
		StatementTimeout: cfg.Database.StatementTimeout,
		Logger:           log,
	})
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Warn("closing the database", slog.Any("error", err))
		}
	}()

	// --- Redis and the bus -------------------------------------------------

	redisOpts, err := redis.ParseURL(cfg.Redis.URL.Reveal())
	if err != nil {
		return fmt.Errorf("parse the Redis URL: %w", err)
	}
	redisOpts.PoolSize = cfg.Redis.PoolSize
	redisOpts.DialTimeout = cfg.Redis.DialTimeout
	redisOpts.ReadTimeout = cfg.Redis.ReadTimeout
	redisOpts.WriteTimeout = cfg.Redis.WriteTimeout

	redisClient := redis.NewClient(redisOpts)
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Warn("closing Redis", slog.Any("error", err))
		}
	}()

	messageBus, err := bus.New(ctx, bus.Options{
		Client: redisClient, ReplicaID: cfg.ReplicaID, Logger: log,
	})
	if err != nil {
		return fmt.Errorf("open the bus: %w", err)
	}

	// --- the device CA -----------------------------------------------------

	caCertPEM, err := os.ReadFile(cfg.CA.CertFile)
	if err != nil {
		return fmt.Errorf("read the CA certificate %s: %w", cfg.CA.CertFile, err)
	}
	caKeyPEM, err := os.ReadFile(cfg.CA.KeyFile)
	if err != nil {
		return fmt.Errorf("read the CA key %s: %w", cfg.CA.KeyFile, err)
	}
	authority, err := ca.New(ca.Options{
		SigningCertPEM: caCertPEM,
		SigningKeyPEM:  caKeyPEM,
		DeviceTTL:      cfg.CA.DeviceCertTTL,
	})
	if err != nil {
		return fmt.Errorf("open the device CA: %w", err)
	}

	// --- services ----------------------------------------------------------

	hasher, err := password.New(password.Params{
		MemoryKiB:   cfg.Auth.ArgonMemoryKiB,
		Iterations:  cfg.Auth.ArgonIterations,
		Parallelism: cfg.Auth.ArgonParallel,
	})
	if err != nil {
		return fmt.Errorf("configure the password hasher: %w", err)
	}

	issuer, err := token.NewIssuer(token.Options{
		PrivateKey: cfg.Auth.JWTPrivateKey,
		PublicKey:  cfg.Auth.JWTPublicKey,
		Issuer:     "edgepilot",
		Audience:   "edgepilot-api",
		TTL:        cfg.Auth.AccessTokenTTL,
	})
	if err != nil {
		return fmt.Errorf("configure the token issuer: %w", err)
	}

	sealer, err := secret.New(cfg.Auth.DataKey)
	if err != nil {
		return fmt.Errorf("configure the secret sealer: %w", err)
	}

	authService, err := auth.New(auth.Options{
		Store: store, Hasher: hasher, Issuer: issuer, Sealer: sealer, Logger: log,
		RefreshTTL:      cfg.Auth.RefreshTokenTTL,
		LockoutDuration: cfg.Auth.LockoutDuration,
		MaxFailedLogins: cfg.Auth.MaxFailedLogins,
	})
	if err != nil {
		return fmt.Errorf("configure authentication: %w", err)
	}

	enrollService, err := enrollment.New(enrollment.Options{
		Store: store, CA: authority, Logger: log,
		DevicePlaneEndpoint: cfg.Agent.DevicePlaneEndpoint,
		SigningPublicKeys:   signingKeyBytes(cfg),
		RenewBefore:         cfg.CA.RenewBefore,
	})
	if err != nil {
		return fmt.Errorf("configure enrollment: %w", err)
	}

	// The revocation set fails closed at startup: a server that cannot load it
	// would accept certificates it should refuse.
	revoked, err := revocation.New(ctx, revocation.Options{
		Certificates: store.DeviceCertificates(), Bus: messageBus, Logger: log,
	})
	if err != nil {
		return fmt.Errorf("load the certificate revocation set: %w", err)
	}

	// --- workers -----------------------------------------------------------

	presence := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: store.Devices(), Logger: log,
	})
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: store.Devices(), Bus: messageBus, Logger: log,
		OfflineAfter: cfg.Agent.OfflineAfter,
	})

	// --- the device plane --------------------------------------------------

	serverCert, err := tls.LoadX509KeyPair(cfg.GRPC.CertFile, cfg.GRPC.KeyFile)
	if err != nil {
		return fmt.Errorf("load the device-plane certificate: %w", err)
	}

	manager := epgrpc.NewManager(log)
	agentService, err := epgrpc.NewAgentService(epgrpc.ServiceOptions{
		Manager: manager, Store: store, Bus: messageBus, Presence: presence, Logger: log,
		Policy: epgrpc.AgentPolicy{
			HeartbeatInterval:     cfg.Agent.HeartbeatInterval,
			MetricsInterval:       cfg.Agent.MetricsInterval,
			InventoryInterval:     cfg.Agent.InventoryInterval,
			MaxConcurrentCommands: cfg.Agent.MaxConcurrentCommands,
			SpoolMaxBytes:         cfg.Agent.SpoolMaxBytes,
			SigningPublicKeys:     signingKeyBytes(cfg),
			LogLevel:              cfg.Log.Level,
		},
	})
	if err != nil {
		return fmt.Errorf("configure the device plane: %w", err)
	}

	interceptor := epgrpc.NewAuthInterceptor(authority, log)
	grpcServer := grpc.NewServer(
		grpc.Creds(epgrpc.Credentials(
			epgrpc.TLSConfig(authority, serverCert, revoked, log))),
		//nolint:contextcheck // the interceptor receives the stream's own context
		grpc.ChainStreamInterceptor(interceptor.Stream()),
		grpc.ChainUnaryInterceptor(interceptor.Unary()),
		// Keepalives are what turn a silently dead NAT binding into a
		// detectable disconnect. Without them a replica holds sockets for
		// agents that are long gone, and its device count drifts upward
		// forever.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    cfg.GRPC.KeepaliveInterval,
			Timeout: cfg.GRPC.KeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// Agents ping on their own schedule; anything faster than half the
			// server's interval is a misbehaving client, not a network problem.
			MinTime:             cfg.GRPC.KeepaliveInterval / 2,
			PermitWithoutStream: false,
		}),
	)
	v1.RegisterAgentServiceServer(grpcServer, agentService)

	// --- the API -----------------------------------------------------------

	api, err := ephttp.New(ephttp.Options{
		Auth: authService, Authenticator: authService,
		Enrollment: enrollService, Store: store, Logger: log,
		Addr:                cfg.HTTP.Addr,
		BaseDomain:          cfg.PublicHost,
		AllowedOrigins:      cfg.HTTP.AllowedOrigins,
		ReadTimeout:         cfg.HTTP.ReadTimeout,
		WriteTimeout:        cfg.HTTP.WriteTimeout,
		IdleTimeout:         cfg.HTTP.IdleTimeout,
		ShutdownTimeout:     cfg.HTTP.ShutdownTimeout,
		MaxRequestBytes:     cfg.HTTP.MaxRequestBytes,
		RefreshTTL:          cfg.Auth.RefreshTokenTTL,
		OfflineAfter:        cfg.Agent.OfflineAfter,
		DevicePlaneEndpoint: cfg.Agent.DevicePlaneEndpoint,
		InsecureCookies:     cfg.Env == config.EnvDevelopment,
	})
	if err != nil {
		return fmt.Errorf("configure the API: %w", err)
	}

	// --- run ---------------------------------------------------------------

	return runAll(ctx, log, cfg, components{
		api: api, grpc: grpcServer, manager: manager,
		presence: presence, sweeper: sweeper, revoked: revoked,
		bus: messageBus, store: store,
	})
}

// components is everything with a lifecycle.
type components struct {
	api      *ephttp.Server
	grpc     *grpc.Server
	manager  *epgrpc.Manager
	presence *worker.PresenceFlusher
	sweeper  *worker.OfflineSweeper
	revoked  *revocation.Set
	bus      *bus.Bus
	store    domain.Store
}

// runAll starts everything and shuts it down in the right order.
func runAll(ctx context.Context, log *slog.Logger, cfg *config.Config, c components) error {
	// A failure in any one of these is a failure of the process: a server whose
	// device plane died but whose API is still answering reports a healthy fleet
	// that is not connected to anything.
	failed := make(chan error, 4)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		if err := c.api.Serve(ctx); err != nil {
			failed <- fmt.Errorf("the API: %w", err)
		}
	}()

	go func() {
		if err := serveGRPC(ctx, log, cfg.GRPC.Addr, c.grpc); err != nil {
			failed <- fmt.Errorf("the device plane: %w", err)
		}
	}()

	go c.presence.Run(ctx)
	go c.sweeper.Run(ctx)
	go c.revoked.Run(ctx)

	log.Info("edgepilot is running",
		slog.String("api", cfg.HTTP.Addr),
		slog.String("device_plane", cfg.GRPC.Addr),
		slog.String("environment", string(cfg.Env)))

	select {
	case err := <-failed:
		cancel()
		return err
	case <-ctx.Done():
	}

	// Drain the device plane first. Agents are told to reconnect immediately —
	// they go to another replica while this one is still serving the dashboard,
	// so a rolling deploy is invisible rather than a fleet-wide blink.
	drainCtx, drainCancel := context.WithTimeout(
		context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
	defer drainCancel()

	log.Info("draining agent streams")
	if err := c.manager.Drain(drainCtx); err != nil {
		log.Warn("some agent streams did not drain in time", slog.Any("error", err))
	}
	c.grpc.GracefulStop()

	// The API's own Serve returns once its context is cancelled, which already
	// happened. The workers flush on the way out — the presence flusher's final
	// batch is what keeps a rolling restart from looking like a fleet that went
	// quiet.
	return nil
}

// signingKeyBytes renders the configured Ed25519 keys as raw bytes.
//
// The config holds them as typed keys so a malformed one fails at boot; the
// wire and the agent want the bytes.
func signingKeyBytes(cfg *config.Config) [][]byte {
	out := make([][]byte, 0, len(cfg.Signing.PublicKeys))
	for _, key := range cfg.Signing.PublicKeys {
		out = append(out, []byte(key))
	}
	return out
}

// serveGRPC listens and serves the device plane.
func serveGRPC(ctx context.Context, log *slog.Logger, addr string, server *grpc.Server) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	log.InfoContext(ctx, "the device plane is listening", slog.String("addr", addr))

	errc := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return nil
	}
}
