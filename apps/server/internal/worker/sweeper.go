package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// SweeperLease names the lease the offline sweeper holds.
const SweeperLease = "offline-sweeper"

// DefaultSweepInterval is how often stale devices are looked for.
const DefaultSweepInterval = 30 * time.Second

// OfflineSweeper marks devices offline once their heartbeats stop.
//
// # Why this exists at all
//
// A device that disconnects cleanly clears its own presence. A device whose
// network vanishes does not — the TCP connection may stay half-open on the
// server for minutes, and the Mac has no opportunity to say goodbye. So
// something has to notice the absence rather than waiting to be told about it.
//
// # Why it is a singleton
//
// Not for correctness — the update is idempotent, and two replicas running it
// would produce the same result. It is a singleton because it raises alerts:
// four replicas sweeping would page four times for one device going offline.
type OfflineSweeper struct {
	devices domain.DeviceRepository
	bus     *bus.Bus
	log     *slog.Logger

	// offlineAfter is how long without a heartbeat marks a device offline. It
	// must exceed the heartbeat interval by enough to absorb a missed beat,
	// which config validation enforces.
	offlineAfter time.Duration
	interval     time.Duration
	now          func() time.Time

	// OnOffline is called for each device that transitioned, so alerting can
	// hook in without this package depending on it.
	OnOffline func(ctx context.Context, device domain.StaleDevice)
}

// SweeperOptions configures the OfflineSweeper.
type SweeperOptions struct {
	Devices      domain.DeviceRepository
	Bus          *bus.Bus
	Logger       *slog.Logger
	OfflineAfter time.Duration
	Interval     time.Duration
	Now          func() time.Time
}

// NewOfflineSweeper builds the sweeper.
func NewOfflineSweeper(opts SweeperOptions) *OfflineSweeper {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OfflineAfter <= 0 {
		opts.OfflineAfter = 90 * time.Second
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultSweepInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &OfflineSweeper{
		devices:      opts.Devices,
		bus:          opts.Bus,
		log:          opts.Logger,
		offlineAfter: opts.OfflineAfter,
		interval:     opts.Interval,
		now:          opts.Now,
	}
}

// Run acquires the lease and sweeps until the context is cancelled.
//
// Every replica calls this. One wins the lease and sweeps; the rest retry, so
// leadership moves within a lease TTL when the holder dies.
func (s *OfflineSweeper) Run(ctx context.Context) {
	if s.bus == nil {
		// Single-replica deployments have no bus. Running unguarded is correct
		// there — there is nobody to duplicate against.
		s.loop(ctx)
		return
	}
	s.bus.RunWithLease(ctx, SweeperLease, s.interval, s.loop)
}

func (s *OfflineSweeper) loop(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// Sweep immediately on acquiring the lease rather than waiting a full
	// interval: leadership has just moved, which usually means the previous
	// holder died, which usually means its devices need marking offline.
	s.Sweep(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep marks stale devices offline once.
func (s *OfflineSweeper) Sweep(ctx context.Context) {
	threshold := s.now().Add(-s.offlineAfter)

	stale, err := s.devices.MarkStaleOffline(ctx, threshold)
	if err != nil {
		s.log.WarnContext(ctx, "sweeping for offline devices failed",
			slog.Time("threshold", threshold), slog.Any("error", err))
		return
	}
	if len(stale) == 0 {
		return
	}

	s.log.InfoContext(ctx, "devices marked offline",
		slog.Int("count", len(stale)),
		slog.Duration("offline_after", s.offlineAfter))

	for _, device := range stale {
		s.log.InfoContext(ctx, "device offline",
			slog.String("device_id", device.DeviceID.String()),
			slog.String("org_id", device.OrgID.String()),
			slog.String("hostname", device.Hostname))

		if s.OnOffline != nil {
			s.OnOffline(ctx, device)
		}
	}
}
