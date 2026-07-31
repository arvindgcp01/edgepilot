// Package bus is the Redis-backed coordination layer between server replicas.
//
// An agent's gRPC stream terminates on exactly one replica. An operator's API
// request lands on any replica. Something has to bridge that, and this is it.
//
// # What is authoritative here, and what is not
//
// Redis holds *ephemeral* state only: which replica currently owns a device's
// stream, whether a device is live right now, which certificates are revoked,
// and who holds a singleton worker's lease. None of it is a source of truth —
// every value can be rebuilt from PostgreSQL or from the agents reconnecting.
//
// That is deliberate, and it is what makes the failure mode acceptable: losing
// Redis costs live dashboard updates and adds latency to command dispatch,
// because the owning replica also reconciles the commands table every few
// seconds. It does not lose a command, a metric, or a device.
//
// Pub/sub in particular is treated as a *notification*, never a queue. Redis
// pub/sub has no delivery guarantee, so anything that relied on it would be
// silently unreliable. Everything published here has a durable counterpart that
// a reconciliation loop will find anyway; the message only makes it faster.
package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Bus wraps the Redis client with EdgePilot's keyspace conventions.
type Bus struct {
	client *redis.Client
	prefix string
	log    *slog.Logger

	// replicaID identifies this process in the presence map. It must be unique
	// per replica: two replicas sharing an id would each believe they own the
	// other's device streams.
	replicaID string
}

// Options configures the Bus.
type Options struct {
	// URL is a redis:// or rediss:// DSN.
	URL string
	// Client is used instead of URL when supplied, for tests.
	Client *redis.Client
	// Prefix namespaces every key. Tests use it for isolation; production
	// leaves it empty unless one Redis is shared between environments, which it
	// should not be.
	Prefix    string
	ReplicaID string
	Logger    *slog.Logger

	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// New opens a Bus and verifies Redis is reachable.
func New(ctx context.Context, opts Options) (*Bus, error) {
	if opts.ReplicaID == "" {
		return nil, errors.New("bus: ReplicaID is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	client := opts.Client
	if client == nil {
		if opts.URL == "" {
			return nil, errors.New("bus: URL or Client is required")
		}
		cfg, err := redis.ParseURL(opts.URL)
		if err != nil {
			return nil, fmt.Errorf("bus: parse URL: %w", err)
		}
		if opts.PoolSize > 0 {
			cfg.PoolSize = opts.PoolSize
		}
		if opts.DialTimeout > 0 {
			cfg.DialTimeout = opts.DialTimeout
		}
		if opts.ReadTimeout > 0 {
			cfg.ReadTimeout = opts.ReadTimeout
		}
		if opts.WriteTimeout > 0 {
			cfg.WriteTimeout = opts.WriteTimeout
		}
		client = redis.NewClient(cfg)
	}

	// Verify at construction. A lazy pool would first report a bad DSN as a
	// failed request, long after the deploy that introduced it.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("bus: ping: %w", err)
	}

	return &Bus{
		client:    client,
		prefix:    opts.Prefix,
		log:       opts.Logger,
		replicaID: opts.ReplicaID,
	}, nil
}

// Close releases the client.
func (b *Bus) Close() error { return b.client.Close() }

// ReplicaID returns this process's identifier.
func (b *Bus) ReplicaID() string { return b.replicaID }

// Client exposes the underlying client for health checks.
func (b *Bus) Client() *redis.Client { return b.client }

// Ping reports whether Redis is reachable, for readiness.
func (b *Bus) Ping(ctx context.Context) error { return b.client.Ping(ctx).Err() }

// key namespaces a key.
func (b *Bus) key(parts ...string) string {
	out := b.prefix
	for i, p := range parts {
		if i > 0 {
			out += ":"
		}
		out += p
	}
	return out
}
