package backoff_test

import (
	"math"
	"testing"
	"time"

	"github.com/visionify/edgepilot/apps/agent/internal/backoff"
)

func TestTheWindowDoublesAndThenStops(t *testing.T) {
	t.Parallel()

	b := &backoff.Backoff{
		Initial: time.Second, Max: 30 * time.Second, Factor: 2,
		// Always draw the top of the window, so the ceiling itself is what is
		// under test rather than the jitter.
		Rand: func() float64 { return 1 },
	}

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, expected := range want {
		if got := b.Next(); got != expected {
			t.Errorf("attempt %d = %s, want %s", i, got, expected)
		}
	}
}

// The property the whole package exists for: ten thousand agents that
// disconnected together must not return together.
func TestDelaysAreSpreadAcrossTheWindow(t *testing.T) {
	t.Parallel()

	const fleet = 10_000
	buckets := make([]int, 10)

	for range fleet {
		// Every agent is at the same attempt, as they would be after one
		// server restart.
		b := backoff.New()
		b.Next() // attempt 0
		b.Next() // attempt 1
		delay := b.Next()

		window := 4 * time.Second // initial 1s, factor 2, third attempt
		if delay < 0 || delay >= window {
			t.Fatalf("delay %s is outside [0, %s)", delay, window)
		}
		idx := int(float64(delay) / float64(window) * float64(len(buckets)))
		if idx >= len(buckets) {
			idx = len(buckets) - 1
		}
		buckets[idx]++
	}

	// A uniform draw puts about a tenth of the fleet in each bucket. Anything
	// far outside that means the herd is still clumped.
	expected := fleet / len(buckets)
	for i, count := range buckets {
		if count < expected/2 || count > expected*2 {
			t.Errorf("bucket %d holds %d agents, want roughly %d — "+
				"the reconnect herd is not being spread", i, count, expected)
		}
	}
}

func TestEveryDelayStaysInsideItsWindow(t *testing.T) {
	t.Parallel()

	for trial := range 1000 {
		b := backoff.New()
		window := backoff.DefaultInitial
		for attempt := range 12 {
			got := b.Next()
			if got < 0 || got >= window {
				t.Fatalf("trial %d attempt %d: delay %s outside [0, %s)",
					trial, attempt, got, window)
			}
			window = min(window*2, backoff.DefaultMax)
		}
	}
}

// A long outage must not overflow the exponent into a negative or absurd
// duration — which is what math.Pow does once it reaches +Inf.
func TestAVeryLongOutageStillProducesASaneDelay(t *testing.T) {
	t.Parallel()

	b := &backoff.Backoff{Rand: func() float64 { return 1 }}
	for range 5000 {
		got := b.Next()
		if got <= 0 || got > backoff.DefaultMax {
			t.Fatalf("delay %s after %d attempts, want at most %s",
				got, b.Attempt(), backoff.DefaultMax)
		}
	}
}

func TestResetReturnsToTheStart(t *testing.T) {
	t.Parallel()

	b := &backoff.Backoff{Rand: func() float64 { return 1 }}
	for range 6 {
		b.Next()
	}
	if b.Attempt() != 6 {
		t.Errorf("attempt = %d, want 6", b.Attempt())
	}
	// Six doublings from one second, still below the five-minute cap.
	if got := b.Peek(); got != 64*time.Second {
		t.Errorf("window after six attempts = %s, want 64s", got)
	}

	b.Reset()
	if b.Attempt() != 0 {
		t.Errorf("attempt = %d after Reset, want 0", b.Attempt())
	}
	if got := b.Next(); got != backoff.DefaultInitial {
		t.Errorf("first delay after Reset = %s, want %s", got, backoff.DefaultInitial)
	}
}

// Peek is for the log line that tells an operator how long the agent intends to
// wait, so it must not consume an attempt.
func TestPeekDoesNotAdvance(t *testing.T) {
	t.Parallel()

	b := backoff.New()
	first := b.Peek()
	for range 5 {
		if got := b.Peek(); got != first {
			t.Fatalf("Peek changed from %s to %s without a Next", first, got)
		}
	}
	if b.Attempt() != 0 {
		t.Errorf("attempt = %d after peeking, want 0", b.Attempt())
	}
}

// Zero values have to behave, because a Backoff embedded in a struct literal
// somewhere will eventually be used without New.
func TestTheZeroValueUsesTheDefaults(t *testing.T) {
	t.Parallel()

	var b backoff.Backoff
	b.Rand = func() float64 { return 1 }

	if got := b.Next(); got != backoff.DefaultInitial {
		t.Errorf("first delay = %s, want %s", got, backoff.DefaultInitial)
	}
	for range 20 {
		b.Next()
	}
	if got := b.Peek(); got != backoff.DefaultMax {
		t.Errorf("window = %s, want %s", got, backoff.DefaultMax)
	}
}

func TestNonsensicalSettingsAreCorrectedRatherThanObeyed(t *testing.T) {
	t.Parallel()

	// A maximum below the initial delay would otherwise produce a window that
	// shrinks with each attempt.
	b := &backoff.Backoff{
		Initial: 10 * time.Second, Max: time.Second, Factor: 0.5,
		Rand: func() float64 { return 1 },
	}
	first := b.Next()
	second := b.Next()
	if first != 10*time.Second || second < first {
		t.Errorf("delays went %s then %s; the window must not shrink", first, second)
	}
}

func TestJitterCoversTheWholeWindow(t *testing.T) {
	t.Parallel()

	// The bottom of the window is reachable — an agent that always waits at
	// least half the window is not using full jitter.
	b := &backoff.Backoff{Initial: time.Second, Max: time.Second, Rand: func() float64 { return 0 }}
	if got := b.Next(); got != 0 {
		t.Errorf("delay with a zero draw = %s, want 0", got)
	}

	b = &backoff.Backoff{
		Initial: time.Second, Max: time.Second,
		Rand: func() float64 { return math.Nextafter(1, 0) },
	}
	if got := b.Next(); got >= time.Second {
		t.Errorf("delay = %s, want strictly less than the window", got)
	}
}
