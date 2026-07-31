package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Leases coordinate the singleton workers: the offline sweeper, the metric
// rollup, partition maintenance, retention. Each must run on exactly one
// replica, and must move to another replica when that one dies.
//
// This is a lease, not a distributed lock, and the difference matters. A lease
// expires on its own, so a replica that is killed or partitioned stops holding
// it without needing to release it. What it does NOT provide is mutual
// exclusion under arbitrary failure — a replica that stalls past its lease
// (a long GC pause, a suspended VM) can believe it still holds one that another
// replica has taken.
//
// That is acceptable here because every workload behind these leases is
// idempotent: marking a device offline twice, computing the same rollup twice,
// or creating a partition that already exists are all harmless. Nothing behind
// a lease may be an operation where running twice is worse than not running.

// LeaseTTL and renewal interval.
const (
	// LeaseTTL is how long a lease survives without renewal.
	LeaseTTL = 30 * time.Second
	// LeaseRenewInterval is comfortably shorter, so an ordinary scheduling
	// delay does not drop a lease that is being renewed normally.
	LeaseRenewInterval = 10 * time.Second
)

// ErrLeaseLost means the lease was taken by another replica, or expired.
var ErrLeaseLost = errors.New("lease lost")

// Lease is a held, self-renewing lease.
type Lease struct {
	bus   *Bus
	name  string
	token string
	log   *slog.Logger

	mu       sync.Mutex
	released bool

	// lost is closed when renewal fails, so the work can stop promptly rather
	// than continuing to act as leader after another replica has taken over.
	lost   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

// AcquireLease tries to take a named lease.
//
// Returns nil without error when another replica holds it — not holding a lease
// is the normal state for most replicas most of the time, and treating it as an
// error would fill the logs with noise about the system working correctly.
func (b *Bus) AcquireLease(ctx context.Context, name string) (*Lease, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("bus: generate lease token: %w", err)
	}
	// The token identifies this *acquisition*, not this replica. A replica that
	// loses and re-takes a lease gets a new token, so a renewal from the old
	// acquisition cannot extend the new one.
	token := b.replicaID + ":" + hex.EncodeToString(tokenBytes)

	ok, err := b.client.SetNX(ctx, b.leaseKey(name), token, LeaseTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("bus: acquire lease %s: %w", name, err)
	}
	if !ok {
		return nil, nil
	}

	renewCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	lease := &Lease{
		bus:    b,
		name:   name,
		token:  token,
		log:    b.log,
		lost:   make(chan struct{}),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go lease.renew(renewCtx)

	b.log.InfoContext(ctx, "lease acquired", slog.String("lease", name))
	return lease, nil
}

// Lost returns a channel closed when the lease is no longer held.
//
// Work behind a lease should select on this, so a replica that loses leadership
// stops acting as leader promptly rather than at the end of its current cycle.
func (l *Lease) Lost() <-chan struct{} { return l.lost }

// Release gives up the lease.
//
// The delete is conditional on still holding it. An unconditional delete would
// let a replica whose lease had already expired and been taken by another
// delete the *new* holder's lease on its way out — handing leadership to a
// third replica while the second still believes it is leader.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	l.mu.Unlock()

	l.cancel()
	<-l.done

	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0`
	if err := l.bus.client.Eval(ctx, script, []string{l.bus.leaseKey(l.name)}, l.token).Err(); err != nil &&
		!errors.Is(err, redis.Nil) {
		return fmt.Errorf("bus: release lease %s: %w", l.name, err)
	}
	l.log.InfoContext(ctx, "lease released", slog.String("lease", l.name))
	return nil
}

// renew extends the lease until the context is cancelled or renewal fails.
func (l *Lease) renew(ctx context.Context) {
	defer close(l.done)

	ticker := time.NewTicker(LeaseRenewInterval)
	defer ticker.Stop()

	// Extends only if this acquisition still owns the key, so a lease that
	// expired and was taken by another replica is not silently stolen back.
	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(ctx, LeaseRenewInterval/2)
			result, err := l.bus.client.Eval(renewCtx, script,
				[]string{l.bus.leaseKey(l.name)},
				l.token, LeaseTTL.Milliseconds()).Int64()
			cancel()

			if err != nil {
				// A transient Redis error is not proof the lease is gone. Keep
				// trying until it actually expires — dropping leadership on
				// every blip would make the singleton workers flap.
				l.log.WarnContext(ctx, "renewing lease failed; will retry",
					slog.String("lease", l.name), slog.Any("error", err))
				continue
			}
			if result == 0 {
				// Another replica holds it now. Stop immediately.
				l.log.WarnContext(ctx, "lease lost to another replica",
					slog.String("lease", l.name))
				l.markLost()
				return
			}
		}
	}
}

func (l *Lease) markLost() {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.lost:
	default:
		close(l.lost)
	}
}

// RunWithLease runs fn on whichever replica holds the named lease.
//
// Every replica calls this; one wins and runs the work, and the rest retry
// periodically so that leadership moves within a lease TTL when the holder
// dies. fn must return promptly when its context is cancelled — that context is
// cancelled both on shutdown and on losing the lease.
func (b *Bus) RunWithLease(ctx context.Context, name string, retry time.Duration, fn func(context.Context)) {
	if retry <= 0 {
		retry = LeaseRenewInterval
	}
	ticker := time.NewTicker(retry)
	defer ticker.Stop()

	for {
		lease, err := b.AcquireLease(ctx, name)
		if err != nil {
			b.log.WarnContext(ctx, "acquiring lease failed",
				slog.String("lease", name), slog.Any("error", err))
		}
		if lease != nil {
			runCtx, cancel := context.WithCancel(ctx)
			go func() {
				select {
				case <-lease.Lost():
					cancel()
				case <-runCtx.Done():
				}
			}()

			fn(runCtx)
			cancel()

			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := lease.Release(releaseCtx); err != nil {
				b.log.WarnContext(ctx, "releasing lease failed",
					slog.String("lease", name), slog.Any("error", err))
			}
			releaseCancel()
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bus) leaseKey(name string) string { return b.key("lease", name) }
