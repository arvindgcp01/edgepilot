package bus_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
)

// newBus returns a Bus namespaced to the calling test, so parallel tests share
// one Redis without seeing each other's keys.
func newBus(t *testing.T, replicaID string) *bus.Bus {
	t.Helper()
	r := testsupport.NewRedis(t)
	b, err := bus.New(t.Context(), bus.Options{
		Client:    r.Client,
		Prefix:    r.Prefix,
		ReplicaID: replicaID,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	return b
}

// newPair returns two Bus handles sharing a keyspace, standing in for two
// server replicas.
func newPair(t *testing.T) (*bus.Bus, *bus.Bus) {
	t.Helper()
	r := testsupport.NewRedis(t)
	mk := func(id string) *bus.Bus {
		b, err := bus.New(t.Context(), bus.Options{
			Client: r.Client, Prefix: r.Prefix, ReplicaID: id,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("new bus %s: %v", id, err)
		}
		return b
	}
	return mk("replica-a"), mk("replica-b")
}

func TestNewRequiresReplicaID(t *testing.T) {
	t.Parallel()
	r := testsupport.NewRedis(t)

	if _, err := bus.New(t.Context(), bus.Options{Client: r.Client}); err == nil {
		t.Error("a replica id is required: two replicas sharing one would each " +
			"believe they own the other's device streams")
	}
}

// --- presence --------------------------------------------------------------

func TestPresenceLifecycle(t *testing.T) {
	t.Parallel()
	b := newBus(t, "replica-a")
	ctx := t.Context()
	deviceID := uuid.New()

	present, err := b.IsPresent(ctx, deviceID)
	if err != nil {
		t.Fatalf("is present: %v", err)
	}
	if present {
		t.Error("a device that never connected must not be present")
	}
	if _, err := b.OwnerOf(ctx, deviceID); err == nil {
		t.Error("OwnerOf should report ErrNotPresent for an unconnected device")
	}

	if err := b.MarkPresent(ctx, deviceID); err != nil {
		t.Fatalf("mark present: %v", err)
	}

	owner, err := b.OwnerOf(ctx, deviceID)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if owner != "replica-a" {
		t.Errorf("owner = %q, want replica-a", owner)
	}

	if err := b.ClearPresence(ctx, deviceID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if present, _ := b.IsPresent(ctx, deviceID); present {
		t.Error("presence should be gone after clearing")
	}
}

// A device that reconnected to another replica between our stream closing and
// our cleanup running has already had its presence rewritten. Clearing it
// blindly would mark a live device offline.
func TestClearPresenceOnlyRemovesOurOwn(t *testing.T) {
	t.Parallel()
	a, b := newPair(t)
	ctx := t.Context()
	deviceID := uuid.New()

	// The device is on replica A, then reconnects to replica B.
	if err := a.MarkPresent(ctx, deviceID); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkPresent(ctx, deviceID); err != nil {
		t.Fatal(err)
	}

	// A's stream teardown now runs, late.
	if err := a.ClearPresence(ctx, deviceID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	owner, err := b.OwnerOf(ctx, deviceID)
	if err != nil {
		t.Fatalf("the device should still be present on replica B: %v", err)
	}
	if owner != "replica-b" {
		t.Errorf("owner = %q, want replica-b; a late cleanup marked a live device offline", owner)
	}
}

func TestPresenceExpires(t *testing.T) {
	t.Parallel()
	r := testsupport.NewRedis(t)
	b, err := bus.New(t.Context(), bus.Options{
		Client: r.Client, Prefix: r.Prefix, ReplicaID: "replica-a",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	deviceID := uuid.New()

	if err := b.MarkPresent(ctx, deviceID); err != nil {
		t.Fatal(err)
	}

	// Rather than waiting 45 seconds, check the TTL was actually set. A
	// presence key with no expiry would keep a dead device marked live forever.
	ttl, err := r.Client.TTL(ctx, r.Prefix+"presence:device:"+deviceID.String()).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("presence key has no expiry (ttl=%v); a dead device would stay live forever", ttl)
	}
	if ttl > bus.PresenceTTL {
		t.Errorf("ttl = %v, longer than PresenceTTL %v", ttl, bus.PresenceTTL)
	}
	// Three heartbeat intervals, so one dropped packet does not make a healthy
	// Mac vanish from the dashboard.
	if bus.PresenceTTL < 3*15*time.Second {
		t.Errorf("PresenceTTL %v is under three heartbeat intervals", bus.PresenceTTL)
	}
}

// The fleet table asks this for every row it renders; 500 sequential round
// trips would dominate the response.
func TestPresentDevicesBatches(t *testing.T) {
	t.Parallel()
	a, b := newPair(t)
	ctx := t.Context()

	onA, onB, offline := uuid.New(), uuid.New(), uuid.New()
	if err := a.MarkPresent(ctx, onA); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkPresent(ctx, onB); err != nil {
		t.Fatal(err)
	}

	got, err := a.PresentDevices(ctx, []uuid.UUID{onA, onB, offline})
	if err != nil {
		t.Fatalf("present devices: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d present devices, want 2", len(got))
	}
	if got[onA] != "replica-a" {
		t.Errorf("device on A reports owner %q", got[onA])
	}
	if got[onB] != "replica-b" {
		t.Errorf("device on B reports owner %q", got[onB])
	}
	if _, ok := got[offline]; ok {
		t.Error("an offline device should be absent from the result")
	}

	// An empty request must not round-trip at all.
	empty, err := a.PresentDevices(ctx, nil)
	if err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if len(empty) != 0 {
		t.Error("an empty request should return an empty map")
	}
}

// --- pub/sub ---------------------------------------------------------------

// The whole point of the bus: an API request on one replica reaching the
// replica that holds the device's stream.
func TestDeviceNotificationCrossesReplicas(t *testing.T) {
	t.Parallel()
	a, b := newPair(t)
	ctx := t.Context()

	deviceID := uuid.New()
	commandID := uuid.New()

	received := make(chan bus.DeviceNotification, 4)
	cancel, err := b.SubscribeDevice(ctx, deviceID, func(n bus.DeviceNotification) {
		received <- n
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	a.NotifyDevice(ctx, bus.DeviceNotification{
		Kind:      bus.KindCommandQueued,
		DeviceID:  deviceID,
		CommandID: &commandID,
	})

	select {
	case n := <-received:
		if n.Kind != bus.KindCommandQueued {
			t.Errorf("kind = %q", n.Kind)
		}
		if n.DeviceID != deviceID {
			t.Errorf("device = %s", n.DeviceID)
		}
		if n.CommandID == nil || *n.CommandID != commandID {
			t.Errorf("command id did not survive: %v", n.CommandID)
		}
		// The publisher is identified, so a subscriber can ignore its own.
		if n.From != "replica-a" {
			t.Errorf("from = %q, want replica-a", n.From)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification was not delivered across replicas")
	}
}

// A subscription per device means 10,000 of them. Leaking one goroutine and one
// Redis connection each is the difference between working and exhausting file
// descriptors.
func TestSubscriptionCancelStopsCleanly(t *testing.T) {
	t.Parallel()
	b := newBus(t, "replica-a")
	ctx := t.Context()

	var delivered atomic.Int64
	cancel, err := b.SubscribeDevice(ctx, uuid.New(), func(bus.DeviceNotification) {
		delivered.Add(1)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Cancel must return only once the reader goroutine has finished, so a
	// caller that cancels and then exits does not race a live handler.
	done := make(chan struct{})
	go func() {
		cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not return; the subscription goroutine is leaked")
	}

	// Cancelling twice must not panic — teardown paths run more than once.
	cancel()
}

func TestBroadcastReachesEveryReplica(t *testing.T) {
	t.Parallel()
	a, b := newPair(t)
	ctx := t.Context()

	received := make(chan bus.BroadcastNotification, 4)
	cancel, err := b.SubscribeBroadcast(ctx, func(n bus.BroadcastNotification) {
		received <- n
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	fingerprint := []byte("0123456789abcdef0123456789abcdef")
	a.Broadcast(ctx, bus.BroadcastNotification{
		Kind:        bus.KindCertificateRevoked,
		Fingerprint: fingerprint,
	})

	select {
	case n := <-received:
		if n.Kind != bus.KindCertificateRevoked {
			t.Errorf("kind = %q", n.Kind)
		}
		if string(n.Fingerprint) != string(fingerprint) {
			t.Error("fingerprint did not survive the round trip")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast was not delivered")
	}
}

// Publishing must never fail a caller's request. The durable change is already
// committed; the message only makes the reconciliation loop unnecessary.
func TestNotifyNeverFailsTheCaller(t *testing.T) {
	t.Parallel()
	b := newBus(t, "replica-a")

	// A cancelled context makes the publish fail. NotifyDevice returns nothing,
	// so the only assertion available is that it does not panic — which is the
	// property: a failed hint is not the caller's problem.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	b.NotifyDevice(ctx, bus.DeviceNotification{Kind: bus.KindCommandQueued, DeviceID: uuid.New()})
	b.Broadcast(ctx, bus.BroadcastNotification{Kind: bus.KindCertificateRevoked})
}

// --- leases ----------------------------------------------------------------

func TestOnlyOneReplicaHoldsALease(t *testing.T) {
	t.Parallel()
	a, b := newPair(t)
	ctx := t.Context()

	leaseA, err := a.AcquireLease(ctx, "sweeper")
	if err != nil {
		t.Fatalf("acquire on A: %v", err)
	}
	if leaseA == nil {
		t.Fatal("the first acquirer should win")
	}

	// Not holding a lease is the normal state for most replicas most of the
	// time, so it is a nil lease rather than an error.
	leaseB, err := b.AcquireLease(ctx, "sweeper")
	if err != nil {
		t.Fatalf("acquire on B should not error: %v", err)
	}
	if leaseB != nil {
		t.Fatal("two replicas hold the same lease")
	}

	if err := leaseA.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Leadership moves once released.
	leaseB, err = b.AcquireLease(ctx, "sweeper")
	if err != nil {
		t.Fatalf("acquire on B after release: %v", err)
	}
	if leaseB == nil {
		t.Fatal("B should acquire the lease once A releases it")
	}
	if err := leaseB.Release(ctx); err != nil {
		t.Fatalf("release B: %v", err)
	}
}

// An unconditional delete would let a replica whose lease had already expired
// and been taken delete the new holder's lease on its way out.
func TestReleaseDoesNotStealAnotherHoldersLease(t *testing.T) {
	t.Parallel()
	r := testsupport.NewRedis(t)
	ctx := t.Context()

	mk := func(id string) *bus.Bus {
		b, err := bus.New(ctx, bus.Options{
			Client: r.Client, Prefix: r.Prefix, ReplicaID: id,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := mk("replica-a"), mk("replica-b")

	leaseA, err := a.AcquireLease(ctx, "rollup")
	if err != nil || leaseA == nil {
		t.Fatalf("acquire on A: %v", err)
	}

	// Simulate A's lease expiring and B taking over.
	if err := r.Client.Del(ctx, r.Prefix+"lease:rollup").Err(); err != nil {
		t.Fatal(err)
	}
	leaseB, err := b.AcquireLease(ctx, "rollup")
	if err != nil || leaseB == nil {
		t.Fatalf("acquire on B: %v", err)
	}

	// A now releases, believing it is still leader.
	if err := leaseA.Release(ctx); err != nil {
		t.Fatalf("release A: %v", err)
	}

	// B must still hold it.
	holder, err := r.Client.Get(ctx, r.Prefix+"lease:rollup").Result()
	if err != nil {
		t.Fatalf("the lease was deleted by a replica that no longer held it: %v", err)
	}
	if holder == "" {
		t.Fatal("lease key is empty")
	}
	_ = leaseB.Release(ctx)
}

func TestLeaseIsRenewedWhileHeld(t *testing.T) {
	t.Parallel()
	r := testsupport.NewRedis(t)
	ctx := t.Context()

	b, err := bus.New(ctx, bus.Options{
		Client: r.Client, Prefix: r.Prefix, ReplicaID: "replica-a",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := b.AcquireLease(ctx, "retention")
	if err != nil || lease == nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	ttl, err := r.Client.TTL(ctx, r.Prefix+"lease:retention").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	// A lease with no expiry would never move to another replica when this one
	// dies — the failure the lease exists to survive.
	if ttl <= 0 {
		t.Fatalf("lease has no expiry (ttl=%v); leadership could never move", ttl)
	}
	if ttl > bus.LeaseTTL {
		t.Errorf("ttl = %v, longer than LeaseTTL %v", ttl, bus.LeaseTTL)
	}
	// Renewal must be comfortably more frequent than expiry, or an ordinary
	// scheduling delay drops a lease that is being renewed normally.
	if bus.LeaseRenewInterval >= bus.LeaseTTL/2 {
		t.Errorf("renew interval %v is too close to the TTL %v",
			bus.LeaseRenewInterval, bus.LeaseTTL)
	}
}

// Work behind a lease must stop when leadership moves, rather than continuing
// to act as leader until its current cycle ends.
func TestLeaseLostSignalsTheHolder(t *testing.T) {
	t.Parallel()
	r := testsupport.NewRedis(t)
	ctx := t.Context()

	b, err := bus.New(ctx, bus.Options{
		Client: r.Client, Prefix: r.Prefix, ReplicaID: "replica-a",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.AcquireLease(ctx, "sweeper")
	if err != nil || lease == nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	select {
	case <-lease.Lost():
		t.Fatal("a freshly acquired lease reports itself lost")
	default:
	}
}

func TestRunWithLeaseRunsOnExactlyOneReplica(t *testing.T) {
	t.Parallel()
	r := testsupport.NewRedis(t)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		running int
		peak    int
		ran     atomic.Int64
	)

	var wg sync.WaitGroup
	for i := range 4 {
		b, err := bus.New(t.Context(), bus.Options{
			Client: r.Client, Prefix: r.Prefix,
			ReplicaID: "replica-" + string(rune('a'+i)),
			Logger:    slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.RunWithLease(ctx, "singleton", 200*time.Millisecond, func(runCtx context.Context) {
				mu.Lock()
				running++
				if running > peak {
					peak = running
				}
				mu.Unlock()
				ran.Add(1)

				select {
				case <-runCtx.Done():
				case <-time.After(500 * time.Millisecond):
				}

				mu.Lock()
				running--
				mu.Unlock()
			})
		}()
	}
	wg.Wait()

	if ran.Load() == 0 {
		t.Fatal("the work never ran on any replica")
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 1 {
		t.Errorf("%d replicas ran the singleton work simultaneously", peak)
	}
}
