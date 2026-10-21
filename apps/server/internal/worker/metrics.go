package worker

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// DefaultMetricsFlushInterval is how often buffered samples reach PostgreSQL.
//
// Agents report every 30 seconds, so a 15-second flush means the fleet table
// is never more than one sample behind while the database sees one batched
// insert per replica per flush rather than one row per device per report.
const DefaultMetricsFlushInterval = 15 * time.Second

// DefaultMaxPendingSamples bounds the buffer. At 10,000 devices reporting
// every 30 seconds it is about two minutes of samples: enough to ride out a
// database hiccup, small enough that a database outage cannot grow the
// process without limit.
const DefaultMaxPendingSamples = 50_000

// partitionCheckInterval is how often the flusher makes sure the coming
// month's partition exists. Creating one that already exists is a no-op, and
// the cost of checking is a single round trip.
const partitionCheckInterval = 6 * time.Hour

// MetricsFlusher batches device health samples into PostgreSQL.
//
// It is the metrics counterpart of PresenceFlusher and follows the same rule:
// Record runs on the stream hot path and does no I/O; Flush does one round
// trip for every device on this replica. It also keeps the "last known"
// utilisation on the devices row current, which is what lets the fleet table
// show CPU, memory and disk for every device without touching the partitioned
// samples table.
type MetricsFlusher struct {
	metrics  domain.MetricsRepository
	log      *slog.Logger
	interval time.Duration
	maxPend  int
	now      func() time.Time

	mu      sync.Mutex
	pending []domain.MetricSample
	last    map[uuid.UUID]domain.LastMetrics
	dropped int

	flushed  atomic.Int64
	failures atomic.Int64
}

// MetricsFlusherOptions configures a MetricsFlusher.
type MetricsFlusherOptions struct {
	Metrics    domain.MetricsRepository
	Logger     *slog.Logger
	Interval   time.Duration
	MaxPending int
	Now        func() time.Time
}

// NewMetricsFlusher builds the flusher.
func NewMetricsFlusher(opts MetricsFlusherOptions) *MetricsFlusher {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultMetricsFlushInterval
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = DefaultMaxPendingSamples
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &MetricsFlusher{
		metrics:  opts.Metrics,
		log:      opts.Logger,
		interval: opts.Interval,
		maxPend:  opts.MaxPending,
		now:      opts.Now,
		last:     map[uuid.UUID]domain.LastMetrics{},
	}
}

// Record buffers one sample.
//
// Called from the stream handler, so it holds the mutex for an append and a
// map write. The per-device "last" entry keeps only the newest sample by its
// own timestamp, so two overlapping streams during a reconnect cannot make the
// fleet table go backwards.
func (f *MetricsFlusher) Record(sample domain.MetricSample) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.pending) >= f.maxPend {
		// Drop the oldest rather than the newest: the newest is what the
		// dashboard is about to ask for. Counted, and reported at flush.
		f.pending = f.pending[1:]
		f.dropped++
	}
	f.pending = append(f.pending, sample)

	if existing, ok := f.last[sample.DeviceID]; ok && existing.At.After(sample.Time) {
		return
	}
	last := domain.LastMetrics{
		CPUPercent:    sample.CPUPercent,
		MemoryPercent: sample.MemoryPercent(),
		DiskPercent:   sample.DiskPercent(),
		At:            sample.Time,
	}
	if sample.UptimeSeconds != nil && *sample.UptimeSeconds > 0 {
		booted := sample.Time.Add(-time.Duration(*sample.UptimeSeconds) * time.Second)
		last.BootedAt = &booted
	}
	f.last[sample.DeviceID] = last
}

// Run flushes on a timer until the context is cancelled, and keeps the
// samples table's monthly partitions one month ahead.
func (f *MetricsFlusher) Run(ctx context.Context) {
	f.ensurePartitions(ctx)

	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	partitions := time.NewTicker(partitionCheckInterval)
	defer partitions.Stop()

	for {
		select {
		case <-ctx.Done():
			// A final flush with a fresh context, as PresenceFlusher does: the
			// samples in the buffer are real readings and a deploy should not
			// leave a gap in every chart.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			f.Flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, f.interval)
			f.Flush(flushCtx)
			cancel()
		case <-partitions.C:
			f.ensurePartitions(ctx)
		}
	}
}

// Flush writes the buffered samples and the per-device latest values.
func (f *MetricsFlusher) Flush(ctx context.Context) {
	f.mu.Lock()
	if len(f.pending) == 0 && len(f.last) == 0 {
		f.mu.Unlock()
		return
	}
	samples := f.pending
	last := f.last
	dropped := f.dropped
	// Cleared before the write so the stream handlers are never blocked on a
	// database round trip.
	f.pending = nil
	f.last = map[uuid.UUID]domain.LastMetrics{}
	f.dropped = 0
	f.mu.Unlock()

	if dropped > 0 {
		f.log.WarnContext(ctx, "the metrics buffer overflowed; oldest samples were dropped",
			slog.Int("dropped", dropped), slog.Int("max_pending", f.maxPend))
	}

	if len(samples) > 0 {
		if err := f.metrics.InsertSamples(ctx, samples); err != nil {
			f.failures.Add(1)
			// Not retried and not restored. A sample is a point on a chart;
			// losing fifteen seconds of them under a database outage is a
			// visible gap, which is the honest rendering of an outage. Growing
			// the buffer until the database returns is how a short outage
			// becomes a replica that cannot restart.
			f.log.WarnContext(ctx, "writing device metrics failed; this flush's samples are lost",
				slog.Int("samples", len(samples)), slog.Any("error", err))
		} else {
			f.flushed.Add(int64(len(samples)))
		}
	}

	if len(last) > 0 {
		if err := f.metrics.UpdateLast(ctx, last); err != nil {
			f.failures.Add(1)
			f.log.WarnContext(ctx, "updating last-known device utilisation failed",
				slog.Int("devices", len(last)), slog.Any("error", err))
		}
	}

	f.log.DebugContext(ctx, "flushed device metrics",
		slog.Int("samples", len(samples)), slog.Int("devices", len(last)))
}

// ensurePartitions creates this month's and next month's partitions.
//
// The migration seeds four months ahead, so a fresh install writes immediately;
// this is what keeps a long-running one writing. Idempotent, so every replica
// may run it without coordination.
func (f *MetricsFlusher) ensurePartitions(ctx context.Context) {
	now := f.now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	months := []time.Time{thisMonth, thisMonth.AddDate(0, 1, 0)}

	ensureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := f.metrics.EnsurePartitions(ensureCtx, months); err != nil {
		// Not fatal: the migration's four-month runway is almost certainly
		// still covering us. Loud, because it will not cover us forever.
		f.log.ErrorContext(ctx, "ensuring metrics partitions failed; "+
			"inserts will start failing when the seeded partitions run out",
			slog.Any("error", err))
	}
}

// Flushed reports how many samples have been written.
func (f *MetricsFlusher) Flushed() int64 { return f.flushed.Load() }

// Failures reports how many flushes failed.
func (f *MetricsFlusher) Failures() int64 { return f.failures.Load() }

// Pending reports how many samples are buffered, for tests and health.
func (f *MetricsFlusher) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}
