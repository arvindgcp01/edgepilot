// Package revocation answers "is this certificate revoked?" during the TLS
// handshake.
//
// The constraint that shapes everything here: this runs on every mTLS handshake,
// synchronously, before any application code. At 10,000 devices reconnecting
// after a rolling deploy that is 10,000 handshakes in a burst, so it cannot
// touch the database or Redis on the hot path. It is an in-process set.
//
// # The two failure modes, and why they are treated differently
//
// **Startup** fails closed. If the revoked set cannot be loaded, the process
// refuses to start. Serving with an empty set would silently honour every
// revoked certificate in the fleet, and an operator watching a deploy would see
// nothing wrong.
//
// **Refresh** fails open — it keeps the previous set and alarms. The
// alternative, treating a failed refresh as "revoke everything", would
// disconnect the entire fleet on a transient database blip and then face 10,000
// simultaneous reconnections. A briefly stale set is a far smaller problem, and
// the pub/sub invalidation below closes the window that actually matters:
// a certificate revoked *now* propagates to every replica within milliseconds,
// without waiting for the next refresh.
package revocation

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// DefaultRefreshInterval is the backstop for anything pub/sub missed — a
// revocation issued while a replica was starting, or a dropped message.
const DefaultRefreshInterval = 60 * time.Second

// Set is the in-process revoked-certificate set.
type Set struct {
	certs domain.DeviceCertificateRepository
	bus   *bus.Bus
	log   *slog.Logger

	interval time.Duration

	// fingerprints is replaced wholesale on refresh rather than mutated, so
	// readers never see a partially-rebuilt set. Individual revocations arriving
	// by pub/sub are added under the mutex.
	mu           sync.RWMutex
	fingerprints map[string]struct{}

	lastRefresh   atomic.Int64 // unix nanos
	refreshErrors atomic.Int64
	loaded        atomic.Bool
}

// Options configures the Set.
type Options struct {
	Certificates    domain.DeviceCertificateRepository
	Bus             *bus.Bus
	Logger          *slog.Logger
	RefreshInterval time.Duration
}

// New builds the Set and performs the initial load.
//
// The load is not optional. Returning a usable Set with an empty map would mean
// starting a replica that honours every revoked certificate until its first
// successful refresh — a window an operator has no way to notice.
func New(ctx context.Context, opts Options) (*Set, error) {
	if opts.Certificates == nil {
		return nil, errors.New("revocation: Certificates repository is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}

	s := &Set{
		certs:        opts.Certificates,
		bus:          opts.Bus,
		log:          opts.Logger,
		interval:     opts.RefreshInterval,
		fingerprints: map[string]struct{}{},
	}

	if err := s.refresh(ctx); err != nil {
		return nil, fmt.Errorf("revocation: initial load failed, refusing to start "+
			"(serving with an empty set would honour every revoked certificate): %w", err)
	}
	return s, nil
}

// IsRevoked reports whether a certificate fingerprint has been revoked.
//
// A read-locked map lookup: roughly 100 ns, which is what makes it viable on
// every handshake.
func (s *Set) IsRevoked(fingerprint []byte) bool {
	if len(fingerprint) == 0 {
		// A certificate with no fingerprint is not something to wave through.
		return true
	}
	key := hex.EncodeToString(fingerprint)

	s.mu.RLock()
	defer s.mu.RUnlock()
	_, revoked := s.fingerprints[key]
	return revoked
}

// Size returns the number of revoked certificates currently tracked.
func (s *Set) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.fingerprints)
}

// Stale reports whether the set has gone too long without a successful refresh.
//
// Exposed so readiness and monitoring can surface it: a replica quietly serving
// a set that stopped updating hours ago is exactly the condition that turns a
// revocation into a non-event.
func (s *Set) Stale(now time.Time, tolerance time.Duration) bool {
	last := s.lastRefresh.Load()
	if last == 0 {
		return true
	}
	return now.Sub(time.Unix(0, last)) > tolerance
}

// LastRefresh returns when the set was last rebuilt from the database.
func (s *Set) LastRefresh() time.Time {
	last := s.lastRefresh.Load()
	if last == 0 {
		return time.Time{}
	}
	return time.Unix(0, last).UTC()
}

// Revoke adds a fingerprint immediately and tells every other replica.
//
// Called after the database write, so the durable state is already committed.
// The broadcast is what makes revocation take effect fleet-wide in milliseconds
// rather than at the next refresh — which is the difference between revoking a
// compromised device and revoking it in a minute.
func (s *Set) Revoke(ctx context.Context, fingerprint []byte) {
	s.add(fingerprint)
	if s.bus != nil {
		s.bus.Broadcast(ctx, bus.BroadcastNotification{
			Kind:        bus.KindCertificateRevoked,
			Fingerprint: fingerprint,
		})
	}
}

// Run keeps the set current until the context is cancelled.
//
// It both subscribes to revocation broadcasts and refreshes periodically. The
// subscription is the fast path; the refresh is the backstop for whatever the
// subscription missed, since Redis pub/sub has no delivery guarantee.
func (s *Set) Run(ctx context.Context) {
	if s.bus != nil {
		cancel, err := s.bus.SubscribeBroadcast(ctx, func(n bus.BroadcastNotification) {
			if n.Kind != bus.KindCertificateRevoked || len(n.Fingerprint) == 0 {
				return
			}
			s.add(n.Fingerprint)
			s.log.DebugContext(ctx, "certificate revoked via broadcast",
				slog.String("fingerprint", hex.EncodeToString(n.Fingerprint)[:16]))
		})
		if err != nil {
			// Not fatal: the periodic refresh still bounds staleness. But the
			// window between a revocation and it taking effect just widened
			// from milliseconds to a refresh interval, which is worth saying
			// loudly.
			s.log.ErrorContext(ctx, "subscribing to revocation broadcasts failed; "+
				"revocations will take up to one refresh interval to apply",
				slog.Duration("refresh_interval", s.interval), slog.Any("error", err))
		} else {
			defer cancel()
		}
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, s.interval)
			err := s.refresh(refreshCtx)
			cancel()

			if err != nil {
				n := s.refreshErrors.Add(1)
				// Keep the previous set. Treating a failed refresh as "revoke
				// everything" would disconnect the fleet on a transient blip
				// and then face 10,000 simultaneous reconnections.
				s.log.ErrorContext(ctx, "refreshing the revocation set failed; "+
					"continuing with the previous set",
					slog.Int64("consecutive_failures", n),
					slog.Time("last_successful_refresh", s.LastRefresh()),
					slog.Any("error", err))
				continue
			}
			s.refreshErrors.Store(0)
		}
	}
}

// refresh rebuilds the set from the database.
func (s *Set) refresh(ctx context.Context) error {
	fingerprints, err := s.certs.ListRevokedFingerprints(ctx)
	if err != nil {
		return err
	}

	next := make(map[string]struct{}, len(fingerprints))
	for _, fp := range fingerprints {
		next[hex.EncodeToString(fp)] = struct{}{}
	}

	s.mu.Lock()
	// Anything added by broadcast since the query started must survive: the
	// database read may predate a revocation that arrived while it was in
	// flight, and dropping it would un-revoke a certificate.
	for k := range s.fingerprints {
		next[k] = struct{}{}
	}
	s.fingerprints = next
	s.mu.Unlock()

	s.lastRefresh.Store(time.Now().UnixNano())
	s.loaded.Store(true)
	return nil
}

func (s *Set) add(fingerprint []byte) {
	if len(fingerprint) == 0 {
		return
	}
	key := hex.EncodeToString(fingerprint)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fingerprints[key] = struct{}{}
}
