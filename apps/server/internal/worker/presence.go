// Package worker holds the background loops: flushing presence, sweeping
// offline devices, rolling up metrics, retention.
//
// Two kinds live here, and the distinction matters operationally:
//
//   - **Per-replica** workers run on every replica and only touch that
//     replica's own state. The presence flusher is one: it batches heartbeats
//     from the streams this process holds.
//
//   - **Singleton** workers must run on exactly one replica, coordinated by a
//     lease. The offline sweeper is one. Everything behind a lease must be
//     idempotent, because a lease is not mutual exclusion under arbitrary
//     failure — see the bus package.
package worker

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// DefaultFlushInterval is how often buffered heartbeats reach PostgreSQL.
//
// One minute, against a 15-second heartbeat: each device contributes at most
// one write per flush regardless of how often it checked in, so the write rate
// is bounded by the fleet size rather than by the heartbeat rate.
const DefaultFlushInterval = time.Minute

// PresenceFlusher batches liveness updates into PostgreSQL.
//
// Redis holds the live answer to "is this device connected right now". This
// keeps the durable column roughly current so that "when did we last hear from
// this device" is still answerable once it is offline and its Redis key has
// lapsed.
//
// Writing last_seen_at per heartbeat instead would be 10,000 row updates every
// 15 seconds against the table the dashboard reads most — pure write
// amplification for a column nobody reads at that resolution.
type PresenceFlusher struct {
	devices  domain.DeviceRepository
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	pending map[uuid.UUID]time.Time

	// Counters for metrics. Written from the flush goroutine and read from
	// wherever metrics are scraped, so they are genuinely atomic rather than
	// merely looking like it.
	flushed  atomic.Int64
	failures atomic.Int64
}

// FlusherOptions configures the PresenceFlusher.
type FlusherOptions struct {
	Devices  domain.DeviceRepository
	Logger   *slog.Logger
	Interval time.Duration
}

// NewPresenceFlusher builds the flusher.
func NewPresenceFlusher(opts FlusherOptions) *PresenceFlusher {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultFlushInterval
	}
	return &PresenceFlusher{
		devices:  opts.Devices,
		log:      opts.Logger,
		interval: opts.Interval,
		pending:  map[uuid.UUID]time.Time{},
	}
}

// Record buffers a heartbeat.
//
// Called from the stream handler on the hot path, so it does no I/O and holds
// the mutex for a single map write. Repeated heartbeats from one device between
// flushes collapse into the latest timestamp, which is all the column needs.
func (f *PresenceFlusher) Record(deviceID uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.pending[deviceID]; ok && existing.After(at) {
		// Never move a timestamp backwards. Two streams for one device can
		// briefly overlap during a reconnect, and the older one's heartbeat
		// must not overwrite the newer one's.
		return
	}
	f.pending[deviceID] = at
}

// Run flushes on a timer until the context is cancelled.
func (f *PresenceFlusher) Run(ctx context.Context) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A final flush on shutdown, with a fresh context: the one we were
			// given is already cancelled, and skipping this would discard up to
			// a full interval of liveness on every deploy — making a rolling
			// restart look like a fleet that briefly went quiet.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			f.Flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, f.interval)
			f.Flush(flushCtx)
			cancel()
		}
	}
}

// Flush writes the buffered heartbeats.
func (f *PresenceFlusher) Flush(ctx context.Context) {
	f.mu.Lock()
	if len(f.pending) == 0 {
		f.mu.Unlock()
		return
	}
	batch := maps.Clone(f.pending)
	// Cleared before the write, not after. Holding the mutex across a database
	// round trip would block every heartbeat on this replica for its duration.
	f.pending = map[uuid.UUID]time.Time{}
	f.mu.Unlock()

	if err := f.devices.TouchLastSeen(ctx, batch); err != nil {
		f.failures.Add(1)
		// Not retried and not restored to the buffer. The next heartbeat is at
		// most 15 seconds away and carries a newer timestamp, so retrying would
		// write a stale value while risking unbounded growth if the database
		// stays down.
		f.log.WarnContext(ctx, "flushing device liveness failed; "+
			"the next heartbeat will carry a newer timestamp",
			slog.Int("devices", len(batch)), slog.Any("error", err))
		return
	}

	f.flushed.Add(int64(len(batch)))
	f.log.DebugContext(ctx, "flushed device liveness", slog.Int("devices", len(batch)))
}

// Flushed and Failures report counters for metrics.
func (f *PresenceFlusher) Flushed() int64  { return f.flushed.Load() }
func (f *PresenceFlusher) Failures() int64 { return f.failures.Load() }

// Pending returns how many devices are waiting to be flushed.
func (f *PresenceFlusher) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}
