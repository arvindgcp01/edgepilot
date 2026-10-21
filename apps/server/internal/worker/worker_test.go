package worker_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
	"github.com/visionify/edgepilot/apps/server/internal/worker"
)

// These tests run against a real PostgreSQL where the behaviour under test is
// in the SQL — "never move last_seen_at backwards" and "only devices that
// actually transitioned come back" are properties of the statements, not of the
// Go around them, and a fake repository would assert nothing.
//
// The buffering rules, by contrast, are entirely in Go, so those tests use a
// stub repository: it is the only way to make a write fail on demand.

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

type workerFixture struct {
	store *postgres.Store
	org   *domain.Organization
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN, MaxConns: 2, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	org, err := store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Acme", Slug: "acme-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return &workerFixture{store: store, org: org}
}

func (f *workerFixture) ctx(t *testing.T) context.Context {
	return domain.WithOrg(t.Context(), f.org.ID)
}

// enrolOnline registers a device and puts it in the state the sweeper cares
// about. A freshly enrolled device is 'pending' and is deliberately not swept:
// it has never been online, so there is nothing to transition.
func (f *workerFixture) enrolOnline(t *testing.T, serial string) uuid.UUID {
	t.Helper()
	id := f.enrol(t, serial)
	if err := f.store.Devices().SetStatus(f.ctx(t), id, domain.DeviceStatusOnline); err != nil {
		t.Fatalf("set status online: %v", err)
	}
	return id
}

func (f *workerFixture) enrol(t *testing.T, serial string) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Devices().Enroll(f.ctx(t), domain.EnrollDeviceInput{
		DeviceID: id, OrgID: f.org.ID, SerialNumber: serial,
		Hostname: "mac-" + serial, AutoApprove: true,
	}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return id
}

func (f *workerFixture) device(t *testing.T, id uuid.UUID) *domain.Device {
	t.Helper()
	device, err := f.store.Devices().Get(f.ctx(t), id)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	return device
}

// --- the presence flusher --------------------------------------------------

func TestFlusherWritesBufferedHeartbeats(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "FLUSH-1")

	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: f.store.Devices(), Logger: quiet(),
	})

	seen := time.Now().UTC().Truncate(time.Millisecond)
	flusher.Record(deviceID, seen)
	if flusher.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", flusher.Pending())
	}

	flusher.Flush(t.Context())

	if flusher.Pending() != 0 {
		t.Errorf("pending = %d after flush, want 0", flusher.Pending())
	}
	if got := flusher.Flushed(); got != 1 {
		t.Errorf("flushed = %d, want 1", got)
	}

	device := f.device(t, deviceID)
	if device.LastSeenAt == nil {
		t.Fatal("last_seen_at is still null after a flush")
	}
	if !device.LastSeenAt.Equal(seen) {
		t.Errorf("last_seen_at = %s, want %s", device.LastSeenAt, seen)
	}
}

// A device that was marked offline must come back online when its heartbeats
// resume. Without this the sweeper's transition would be one-way and a
// recovered device would sit there looking dead until something else touched
// its row.
func TestFlushBringsAnOfflineDeviceBackOnline(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "REVIVE-1")
	if err := f.store.Devices().SetStatus(f.ctx(t), deviceID, domain.DeviceStatusOffline); err != nil {
		t.Fatalf("set status offline: %v", err)
	}

	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: f.store.Devices(), Logger: quiet(),
	})
	flusher.Record(deviceID, time.Now().UTC())
	flusher.Flush(t.Context())

	if got := f.device(t, deviceID).Status; got != domain.DeviceStatusOnline {
		t.Errorf("status = %s after a heartbeat, want online", got)
	}
}

// A device whose operator marked it degraded stays degraded through a
// heartbeat: liveness and health are different questions, and the flusher only
// answers the first.
func TestFlushDoesNotClearADegradedStatus(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "DEGRADED-1")
	if err := f.store.Devices().SetStatus(f.ctx(t), deviceID, domain.DeviceStatusDegraded); err != nil {
		t.Fatalf("set status degraded: %v", err)
	}

	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: f.store.Devices(), Logger: quiet(),
	})
	flusher.Record(deviceID, time.Now().UTC())
	flusher.Flush(t.Context())

	if got := f.device(t, deviceID).Status; got != domain.DeviceStatusDegraded {
		t.Errorf("status = %s, want degraded", got)
	}
}

func TestRepeatedHeartbeatsCollapseToOneWrite(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "COLLAPSE-1")

	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: f.store.Devices(), Logger: quiet(),
	})

	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := range 10 {
		flusher.Record(deviceID, base.Add(time.Duration(i)*time.Second))
	}
	if flusher.Pending() != 1 {
		t.Fatalf("pending = %d after 10 heartbeats from one device, want 1",
			flusher.Pending())
	}

	flusher.Flush(t.Context())

	want := base.Add(9 * time.Second)
	if got := f.device(t, deviceID).LastSeenAt; got == nil || !got.Equal(want) {
		t.Errorf("last_seen_at = %v, want the latest heartbeat %s", got, want)
	}
}

// Two streams for one device overlap during a reconnect, and the older one's
// heartbeat must not win. Guarded twice — in the buffer and in the SQL — and
// this exercises the buffer.
func TestRecordNeverMovesATimestampBackwards(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "BACKWARDS-1")

	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: f.store.Devices(), Logger: quiet(),
	})

	newer := time.Now().UTC().Truncate(time.Millisecond)
	flusher.Record(deviceID, newer)
	flusher.Record(deviceID, newer.Add(-30*time.Second))
	flusher.Flush(t.Context())

	if got := f.device(t, deviceID).LastSeenAt; got == nil || !got.Equal(newer) {
		t.Errorf("last_seen_at = %v, want the newer timestamp %s", got, newer)
	}
}

// The same guard in the database, which is what protects against two replicas
// flushing out of order — the in-memory one only sees its own process.
func TestAStaleFlushDoesNotOverwriteANewerOne(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "STALE-FLUSH-1")

	devices := f.store.Devices()
	newer := time.Now().UTC().Truncate(time.Millisecond)

	if err := devices.TouchLastSeen(t.Context(), map[uuid.UUID]time.Time{deviceID: newer}); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// A slow replica's flush, arriving late with an older timestamp.
	if err := devices.TouchLastSeen(t.Context(), map[uuid.UUID]time.Time{
		deviceID: newer.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	if got := f.device(t, deviceID).LastSeenAt; got == nil || !got.Equal(newer) {
		t.Errorf("last_seen_at = %v, want %s", got, newer)
	}
}

func TestFlushingIsFreeWhenNothingIsBuffered(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{}
	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{Devices: devices, Logger: quiet()})

	flusher.Flush(t.Context())

	if got := devices.touches.Load(); got != 0 {
		t.Errorf("TouchLastSeen called %d times with an empty buffer, want 0", got)
	}
}

// A failed write must not accumulate. The next heartbeat is at most one
// interval away and carries a newer timestamp, so retaining the batch would
// grow the buffer without bound for a database outage while writing a stale
// value if it ever succeeded.
func TestAFailedFlushDiscardsTheBatchRatherThanGrowing(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{touchErr: errors.New("connection refused")}
	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{Devices: devices, Logger: quiet()})

	for range 3 {
		flusher.Record(uuid.New(), time.Now())
	}
	flusher.Flush(t.Context())

	if got := flusher.Pending(); got != 0 {
		t.Errorf("pending = %d after a failed flush, want 0 — the buffer must not grow", got)
	}
	if got := flusher.Failures(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
	if got := flusher.Flushed(); got != 0 {
		t.Errorf("flushed = %d after a failure, want 0", got)
	}
}

// Every rolling deploy cancels this context. Dropping the buffer there would
// make a routine restart look like the fleet briefly going quiet.
func TestShutdownFlushesWhatIsBuffered(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{}
	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: devices, Logger: quiet(), Interval: time.Hour,
	})

	deviceID := uuid.New()
	flusher.Record(deviceID, time.Now())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); flusher.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if got := devices.touches.Load(); got != 1 {
		t.Fatalf("TouchLastSeen called %d times on shutdown, want 1", got)
	}
	if _, ok := devices.lastBatch()[deviceID]; !ok {
		t.Error("the shutdown flush did not carry the buffered device")
	}
}

func TestRunFlushesOnItsInterval(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{}
	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{
		Devices: devices, Logger: quiet(), Interval: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go flusher.Run(ctx)

	flusher.Record(uuid.New(), time.Now())
	waitFor(t, 5*time.Second, func() bool { return devices.touches.Load() >= 1 })
}

// Record is called from every stream goroutine on this replica. The race
// detector is the point of this test.
func TestRecordIsSafeFromManyStreams(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{}
	flusher := worker.NewPresenceFlusher(worker.FlusherOptions{Devices: devices, Logger: quiet()})

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := uuid.New()
			for range 20 {
				flusher.Record(id, time.Now())
			}
		}()
	}
	go func() {
		for range 10 {
			flusher.Flush(context.Background())
		}
	}()
	wg.Wait()
	flusher.Flush(t.Context())

	if got := flusher.Pending(); got != 0 {
		t.Errorf("pending = %d after the final flush, want 0", got)
	}
}

// --- the offline sweeper ---------------------------------------------------

func TestSweeperMarksSilentDevicesOffline(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrolOnline(t, "SWEEP-1")

	// Last heard from five minutes ago, against a 90-second threshold.
	if err := f.store.Devices().TouchLastSeen(t.Context(), map[uuid.UUID]time.Time{
		deviceID: time.Now().UTC().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	var offline []domain.StaleDevice
	var mu sync.Mutex
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: f.store.Devices(), Logger: quiet(), OfflineAfter: 90 * time.Second,
	})
	sweeper.OnOffline = func(_ context.Context, d domain.StaleDevice) {
		mu.Lock()
		defer mu.Unlock()
		offline = append(offline, d)
	}

	sweeper.Sweep(t.Context())

	if got := f.device(t, deviceID).Status; got != domain.DeviceStatusOffline {
		t.Errorf("status = %s, want offline", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(offline) != 1 {
		t.Fatalf("OnOffline called %d times, want 1", len(offline))
	}
	if offline[0].DeviceID != deviceID {
		t.Errorf("OnOffline device = %s, want %s", offline[0].DeviceID, deviceID)
	}
	if offline[0].OrgID != f.org.ID {
		t.Errorf("OnOffline org = %s, want %s", offline[0].OrgID, f.org.ID)
	}
	if offline[0].Hostname != "mac-SWEEP-1" {
		t.Errorf("OnOffline hostname = %q, want mac-SWEEP-1", offline[0].Hostname)
	}
}

// The alert must fire once, not on every sweep for as long as the device stays
// away. That is what returning only the rows that changed buys.
func TestASecondSweepDoesNotReAlert(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrolOnline(t, "SWEEP-IDEMPOTENT-1")
	if err := f.store.Devices().TouchLastSeen(t.Context(), map[uuid.UUID]time.Time{
		deviceID: time.Now().UTC().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	var alerts atomic.Int64
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: f.store.Devices(), Logger: quiet(), OfflineAfter: 90 * time.Second,
	})
	sweeper.OnOffline = func(context.Context, domain.StaleDevice) { alerts.Add(1) }

	sweeper.Sweep(t.Context())
	sweeper.Sweep(t.Context())
	sweeper.Sweep(t.Context())

	if got := alerts.Load(); got != 1 {
		t.Errorf("alerted %d times for one device going offline, want 1", got)
	}
}

func TestSweeperLeavesLiveDevicesAlone(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrolOnline(t, "SWEEP-LIVE-1")
	if err := f.store.Devices().TouchLastSeen(t.Context(), map[uuid.UUID]time.Time{
		deviceID: time.Now().UTC().Add(-10 * time.Second),
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: f.store.Devices(), Logger: quiet(), OfflineAfter: 90 * time.Second,
	})
	sweeper.Sweep(t.Context())

	if got := f.device(t, deviceID).Status; got != domain.DeviceStatusOnline {
		t.Errorf("status = %s for a device seen 10s ago, want online", got)
	}
}

// A device awaiting approval has never been online. Sweeping it would show
// operators a queue of "offline" devices that are actually just new.
func TestSweeperIgnoresDevicesThatWereNeverOnline(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrol(t, "SWEEP-PENDING-1")

	var alerts atomic.Int64
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: f.store.Devices(), Logger: quiet(), OfflineAfter: time.Nanosecond,
	})
	sweeper.OnOffline = func(context.Context, domain.StaleDevice) { alerts.Add(1) }
	sweeper.Sweep(t.Context())

	if got := f.device(t, deviceID).Status; got != domain.DeviceStatusPending {
		t.Errorf("status = %s for a never-connected device, want pending", got)
	}
	if got := alerts.Load(); got != 0 {
		t.Errorf("alerted %d times for a pending device, want 0", got)
	}
}

// A decommissioned device is gone. It must not resurface in the sweep as
// something to page about.
func TestSweeperIgnoresDecommissionedDevices(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	deviceID := f.enrolOnline(t, "SWEEP-GONE-1")
	if err := f.store.Devices().Decommission(f.ctx(t), deviceID); err != nil {
		t.Fatalf("decommission: %v", err)
	}

	var alerts atomic.Int64
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: f.store.Devices(), Logger: quiet(), OfflineAfter: time.Nanosecond,
	})
	sweeper.OnOffline = func(context.Context, domain.StaleDevice) { alerts.Add(1) }
	sweeper.Sweep(t.Context())

	if got := alerts.Load(); got != 0 {
		t.Errorf("alerted %d times for a decommissioned device, want 0", got)
	}
}

// The sweep is fleet-wide by design: it is a singleton worker, not a
// tenant-scoped request, and a device in one organization going quiet must be
// noticed regardless of which organization the replica last served.
func TestSweeperCrossesOrganizations(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t)
	other, err := f.store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Globex", Slug: "globex-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	first := f.enrolOnline(t, "SWEEP-ORG-A")

	secondID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	otherCtx := domain.WithOrg(t.Context(), other.ID)
	if _, err := f.store.Devices().Enroll(otherCtx, domain.EnrollDeviceInput{
		DeviceID: secondID, OrgID: other.ID, SerialNumber: "SWEEP-ORG-B",
		Hostname: "mac-b", AutoApprove: true,
	}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := f.store.Devices().SetStatus(otherCtx, secondID, domain.DeviceStatusOnline); err != nil {
		t.Fatalf("set status: %v", err)
	}

	stale := time.Now().UTC().Add(-5 * time.Minute)
	if err := f.store.Devices().TouchLastSeen(t.Context(), map[uuid.UUID]time.Time{
		first: stale, secondID: stale,
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	var mu sync.Mutex
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: f.store.Devices(), Logger: quiet(), OfflineAfter: 90 * time.Second,
	})
	sweeper.OnOffline = func(_ context.Context, d domain.StaleDevice) {
		mu.Lock()
		defer mu.Unlock()
		seen[d.DeviceID] = true
	}
	sweeper.Sweep(t.Context())

	mu.Lock()
	defer mu.Unlock()
	if !seen[first] || !seen[secondID] {
		t.Errorf("swept %v, want both organizations' devices", seen)
	}
}

// A single-replica deployment has no Redis. The sweeper must still run rather
// than waiting forever for a lease nobody can grant.
func TestSweeperRunsWithoutABus(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{stale: []domain.StaleDevice{
		{DeviceID: uuid.New(), OrgID: uuid.New(), Hostname: "mac-1"},
	}}
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: devices, Logger: quiet(), Interval: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go sweeper.Run(ctx)

	waitFor(t, 5*time.Second, func() bool { return devices.sweeps.Load() >= 1 })
}

// With a bus, the sweep happens under a lease — and on acquiring it, at once
// rather than after a full interval. Leadership usually moves because the
// previous holder died, which usually means its devices need marking offline.
func TestSweeperSweepsImmediatelyOnAcquiringTheLease(t *testing.T) {
	t.Parallel()

	r := testsupport.NewRedis(t)
	b, err := bus.New(t.Context(), bus.Options{
		Client: r.Client, Prefix: r.Prefix, ReplicaID: "replica-a", Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}

	devices := &stubDevices{}
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: devices, Bus: b, Logger: quiet(), Interval: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go sweeper.Run(ctx)

	// One-hour interval: anything that arrives here came from the acquisition
	// path, not from the ticker.
	waitFor(t, 10*time.Second, func() bool { return devices.sweeps.Load() >= 1 })
}

// A database outage must not stop the loop or produce phantom alerts.
func TestAFailedSweepAlertsNobody(t *testing.T) {
	t.Parallel()

	devices := &stubDevices{
		sweepErr: errors.New("connection refused"),
		stale:    []domain.StaleDevice{{DeviceID: uuid.New(), Hostname: "mac-1"}},
	}
	var alerts atomic.Int64
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: devices, Logger: quiet(), OfflineAfter: 90 * time.Second,
	})
	sweeper.OnOffline = func(context.Context, domain.StaleDevice) { alerts.Add(1) }

	sweeper.Sweep(t.Context())

	if got := alerts.Load(); got != 0 {
		t.Errorf("alerted %d times when the sweep failed, want 0", got)
	}
}

// The threshold is derived from the injected clock, so the sweep window is
// testable and does not drift with the wall clock during a test.
func TestTheThresholdComesFromTheInjectedClock(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	devices := &stubDevices{}
	sweeper := worker.NewOfflineSweeper(worker.SweeperOptions{
		Devices: devices, Logger: quiet(), OfflineAfter: 90 * time.Second,
		Now: func() time.Time { return fixed },
	})

	sweeper.Sweep(t.Context())

	want := fixed.Add(-90 * time.Second)
	if got := devices.lastThreshold(); !got.Equal(want) {
		t.Errorf("threshold = %s, want %s", got, want)
	}
}

// --- stubs -----------------------------------------------------------------

// stubDevices implements only the two methods the workers call. The embedded
// interface is nil on purpose: anything else these workers start calling should
// panic loudly in a test rather than be silently absorbed.
type stubDevices struct {
	domain.DeviceRepository

	touchErr error
	sweepErr error
	stale    []domain.StaleDevice

	touches atomic.Int64
	sweeps  atomic.Int64

	mu        sync.Mutex
	batch     map[uuid.UUID]time.Time
	threshold time.Time
}

func (s *stubDevices) TouchLastSeen(_ context.Context, seen map[uuid.UUID]time.Time) error {
	s.touches.Add(1)
	s.mu.Lock()
	s.batch = seen
	s.mu.Unlock()
	return s.touchErr
}

func (s *stubDevices) MarkStaleOffline(_ context.Context, threshold time.Time) ([]domain.StaleDevice, error) {
	s.sweeps.Add(1)
	s.mu.Lock()
	s.threshold = threshold
	s.mu.Unlock()
	if s.sweepErr != nil {
		return nil, s.sweepErr
	}
	return s.stale, nil
}

func (s *stubDevices) lastBatch() map[uuid.UUID]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batch
}

func (s *stubDevices) lastThreshold() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threshold
}

// waitFor polls until cond holds, because the workers are timer-driven and a
// fixed sleep would either be flaky or slow.
func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", limit)
}
