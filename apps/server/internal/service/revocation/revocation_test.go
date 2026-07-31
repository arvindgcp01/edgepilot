package revocation_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/service/revocation"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
)

// fakeCerts stands in for the certificate repository.
//
// A fake rather than a real database here: the properties under test are about
// what the *set* does when the repository succeeds, fails, or changes — and
// making the repository fail on demand is the whole point. The repository's own
// behaviour is covered by its integration tests.
type fakeCerts struct {
	domain.DeviceCertificateRepository

	mu           sync.Mutex
	fingerprints [][]byte
	err          error
	calls        int
}

func (f *fakeCerts) ListRevokedFingerprints(context.Context) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]byte, len(f.fingerprints))
	copy(out, f.fingerprints)
	return out, nil
}

func (f *fakeCerts) set(fingerprints [][]byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fingerprints, f.err = fingerprints, err
}

func (f *fakeCerts) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func fp(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// --- startup ---------------------------------------------------------------

func TestLoadsAtStartup(t *testing.T) {
	t.Parallel()

	certs := &fakeCerts{fingerprints: [][]byte{fp(1), fp(2)}}
	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates: certs, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if set.Size() != 2 {
		t.Errorf("size = %d, want 2", set.Size())
	}
	if !set.IsRevoked(fp(1)) || !set.IsRevoked(fp(2)) {
		t.Error("known revoked certificates should be reported as revoked")
	}
	if set.IsRevoked(fp(9)) {
		t.Error("an unrevoked certificate must not be reported as revoked")
	}
}

// Serving with an empty set would silently honour every revoked certificate in
// the fleet, and an operator watching a deploy would see nothing wrong.
func TestStartupFailsClosed(t *testing.T) {
	t.Parallel()

	certs := &fakeCerts{err: errors.New("database unreachable")}
	_, err := revocation.New(t.Context(), revocation.Options{
		Certificates: certs, Logger: quiet(),
	})
	if err == nil {
		t.Fatal("the process must refuse to start when the revocation set cannot be loaded")
	}
	if !errors.Is(err, certs.err) {
		t.Errorf("the cause should be wrapped: %v", err)
	}
}

func TestNewRequiresARepository(t *testing.T) {
	t.Parallel()
	if _, err := revocation.New(t.Context(), revocation.Options{Logger: quiet()}); err == nil {
		t.Error("a certificate repository is required")
	}
}

// An empty fingerprint is not something to wave through.
func TestEmptyFingerprintIsTreatedAsRevoked(t *testing.T) {
	t.Parallel()

	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates: &fakeCerts{}, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !set.IsRevoked(nil) {
		t.Error("a certificate with no fingerprint must not be accepted")
	}
	if !set.IsRevoked([]byte{}) {
		t.Error("an empty fingerprint must not be accepted")
	}
}

// --- refresh ---------------------------------------------------------------

func TestRefreshPicksUpNewRevocations(t *testing.T) {
	t.Parallel()

	certs := &fakeCerts{fingerprints: [][]byte{fp(1)}}
	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates:    certs,
		Logger:          quiet(),
		RefreshInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go set.Run(ctx)

	certs.set([][]byte{fp(1), fp(2)}, nil)

	waitFor(t, 3*time.Second, func() bool { return set.IsRevoked(fp(2)) })
	if !set.IsRevoked(fp(1)) {
		t.Error("the previously known revocation was dropped")
	}
}

// Treating a failed refresh as "revoke everything" would disconnect the fleet on
// a transient blip and then face 10,000 simultaneous reconnections. A briefly
// stale set is the smaller problem.
func TestFailedRefreshKeepsThePreviousSet(t *testing.T) {
	t.Parallel()

	certs := &fakeCerts{fingerprints: [][]byte{fp(1)}}
	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates:    certs,
		Logger:          quiet(),
		RefreshInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go set.Run(ctx)

	before := certs.callCount()
	certs.set(nil, errors.New("database unreachable"))

	// Let several refreshes fail.
	waitFor(t, 3*time.Second, func() bool { return certs.callCount() > before+2 })

	if !set.IsRevoked(fp(1)) {
		t.Error("a failed refresh dropped a known revocation; the certificate would now be honoured")
	}
	if set.Size() != 1 {
		t.Errorf("size = %d, want the previous set retained", set.Size())
	}
}

// A replica quietly serving a set that stopped updating hours ago is exactly the
// condition that turns a revocation into a non-event, so it has to be
// observable.
func TestStalenessIsObservable(t *testing.T) {
	t.Parallel()

	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates: &fakeCerts{}, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if set.Stale(now, time.Minute) {
		t.Error("a freshly loaded set must not report stale")
	}
	if !set.Stale(now.Add(2*time.Hour), time.Minute) {
		t.Error("a set that has not refreshed in two hours must report stale")
	}
	if set.LastRefresh().IsZero() {
		t.Error("the last refresh time should be recorded")
	}
}

// The database read may predate a revocation that arrived by broadcast while it
// was in flight. Dropping it on refresh would un-revoke a certificate.
func TestRefreshDoesNotDropBroadcastRevocations(t *testing.T) {
	t.Parallel()

	certs := &fakeCerts{fingerprints: [][]byte{fp(1)}}
	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates:    certs,
		Logger:          quiet(),
		RefreshInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Arrived out of band, and the repository does not know about it yet.
	set.Revoke(t.Context(), fp(7))
	if !set.IsRevoked(fp(7)) {
		t.Fatal("Revoke should take effect locally and immediately")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go set.Run(ctx)

	before := certs.callCount()
	waitFor(t, 3*time.Second, func() bool { return certs.callCount() > before+1 })

	if !set.IsRevoked(fp(7)) {
		t.Error("a refresh dropped a revocation that arrived while the query was in flight")
	}
}

// --- cross-replica propagation ---------------------------------------------

// The difference between revoking a compromised device and revoking it in a
// minute.
func TestRevocationPropagatesAcrossReplicasImmediately(t *testing.T) {
	t.Parallel()

	r := testsupport.NewRedis(t)
	ctx := t.Context()

	mkBus := func(id string) *bus.Bus {
		b, err := bus.New(ctx, bus.Options{
			Client: r.Client, Prefix: r.Prefix, ReplicaID: id, Logger: quiet(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Two replicas, each with its own in-process set, and a refresh interval
	// long enough that only the broadcast can carry the revocation.
	certsA := &fakeCerts{}
	setA, err := revocation.New(ctx, revocation.Options{
		Certificates: certsA, Bus: mkBus("replica-a"), Logger: quiet(),
		RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	certsB := &fakeCerts{}
	setB, err := revocation.New(ctx, revocation.Options{
		Certificates: certsB, Bus: mkBus("replica-b"), Logger: quiet(),
		RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go setA.Run(runCtx)
	go setB.Run(runCtx)

	// Give both subscriptions time to establish.
	waitFor(t, 3*time.Second, func() bool { return true })
	time.Sleep(200 * time.Millisecond)

	fingerprint := fp(42)
	setA.Revoke(ctx, fingerprint)

	if !setA.IsRevoked(fingerprint) {
		t.Error("the revoking replica should apply it locally at once")
	}
	waitFor(t, 5*time.Second, func() bool { return setB.IsRevoked(fingerprint) })
}

// Without a bus the set still works; revocations simply take up to one refresh
// interval to apply, which is the documented degraded behaviour.
func TestWorksWithoutABus(t *testing.T) {
	t.Parallel()

	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates: &fakeCerts{}, Logger: quiet(), RefreshInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go set.Run(ctx)

	set.Revoke(ctx, fp(3))
	if !set.IsRevoked(fp(3)) {
		t.Error("Revoke should work locally even with no bus configured")
	}
}

// --- hot path --------------------------------------------------------------

// This runs on every mTLS handshake, synchronously, before any application
// code. It has to be a map lookup.
func BenchmarkIsRevoked(b *testing.B) {
	fingerprints := make([][]byte, 10_000)
	for i := range fingerprints {
		f := make([]byte, 32)
		f[0], f[1] = byte(i), byte(i>>8)
		fingerprints[i] = f
	}
	set, err := revocation.New(b.Context(), revocation.Options{
		Certificates: &fakeCerts{fingerprints: fingerprints}, Logger: quiet(),
	})
	if err != nil {
		b.Fatal(err)
	}

	probe := make([]byte, 32)
	probe[0] = 0xff

	b.ReportAllocs()
	for b.Loop() {
		set.IsRevoked(probe)
	}
}

// Concurrent handshakes read the set while a refresh replaces it. The race
// detector is the assertion here.
func TestConcurrentReadsDuringRefresh(t *testing.T) {
	t.Parallel()

	certs := &fakeCerts{fingerprints: [][]byte{fp(1)}}
	set, err := revocation.New(t.Context(), revocation.Options{
		Certificates: certs, Logger: quiet(), RefreshInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go set.Run(ctx)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				set.IsRevoked(fp(1))
				set.Size()
			}
		}()
	}
	// And revocations arriving concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			set.Revoke(ctx, fp(byte(i%200)))
		}
	}()
	wg.Wait()
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}
