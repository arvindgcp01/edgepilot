package screenshare

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// A recording stand-in for the privileged command, so the reconciler's
// decision-making is tested without touching a real Mac.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil, nil
}

func newTest() (*Reconciler, *recorder) {
	rec := &recorder{}
	r := &Reconciler{log: slog.New(slog.DiscardHandler), run: rec.run}
	return r, rec
}

func TestEnableThenUnchangedPolicyDoesNotReRun(t *testing.T) {
	r, rec := newTest()
	ctx := t.Context()

	if err := r.Apply(ctx, true, "hunter2!"); err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(ctx, true, "hunter2!"); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("enable ran %d times for an unchanged policy, want 1", len(rec.calls))
	}
	// The password must reach kickstart, never a log or a file.
	joined := rec.calls[0]
	if !contains(joined, "hunter2!") {
		t.Errorf("kickstart was not given the password: %v", joined)
	}
}

func TestARotatedPasswordReRunsEvenWhenStillEnabled(t *testing.T) {
	r, rec := newTest()
	ctx := t.Context()
	_ = r.Apply(ctx, true, "first-pw")
	_ = r.Apply(ctx, true, "second-pw")
	if len(rec.calls) != 2 {
		t.Fatalf("a rotated password ran kickstart %d times, want 2", len(rec.calls))
	}
}

func TestDisableRunsAndIsIdempotent(t *testing.T) {
	r, rec := newTest()
	ctx := t.Context()
	_ = r.Apply(ctx, true, "pw")
	_ = r.Apply(ctx, false, "")
	_ = r.Apply(ctx, false, "")
	if len(rec.calls) != 2 {
		t.Fatalf("got %d calls, want enable + one disable", len(rec.calls))
	}
	if !contains(rec.calls[1], "-deactivate") {
		t.Errorf("disable did not deactivate: %v", rec.calls[1])
	}
}

func TestPasswordIsIrrelevantWhenDisabling(t *testing.T) {
	// Off is off regardless of the password field, so two disables with
	// different passwords must not re-run.
	r, rec := newTest()
	ctx := t.Context()
	_ = r.Apply(ctx, false, "a")
	_ = r.Apply(ctx, false, "b")
	if len(rec.calls) != 1 {
		t.Fatalf("disable ran %d times, want 1 — the password should not matter when off", len(rec.calls))
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
