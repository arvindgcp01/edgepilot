// Package backoff decides how long the agent waits before reconnecting.
//
// # Why full jitter
//
// Ten thousand Macs lose their connection at the same instant — a server
// restart, a rolling deploy, a network partition that heals. If every agent
// waits the same computed interval, they all return at the same instant too, and
// the server that just came back falls over under a synchronised reconnect
// storm. Backing off in lockstep is not backing off.
//
// Full jitter — a uniform random draw from [0, computed) rather than the
// computed value itself — spreads that herd across the whole window. It is the
// variant AWS measured as best for both server load and client completion time,
// and the reason it wins is that it removes the correlation entirely rather than
// merely reducing it.
//
// # Why the reason matters
//
// A server that says "I am draining, reconnect" should be reconnected to at
// once: the replica is going away by design, another is ready, and backing off
// would make a rolling deploy look like an outage. A connection that dropped
// with no explanation is a different thing, and gets the full exponential
// treatment.
package backoff

import (
	"math"
	"math/rand/v2"
	"time"
)

// Defaults matching Architecture.md §3.2: 1 s to a 5 minute cap.
const (
	DefaultInitial = time.Second
	DefaultMax     = 5 * time.Minute
	DefaultFactor  = 2.0
)

// Backoff computes reconnect delays.
//
// Not safe for concurrent use: one connection loop owns one Backoff, and
// sharing one across connections would make the attempt count meaningless.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64

	// Rand supplies the jitter. Nil means the global source; tests set it to
	// make the sequence reproducible.
	Rand func() float64

	attempt int
}

// New returns a Backoff with the defaults applied.
func New() *Backoff {
	return &Backoff{Initial: DefaultInitial, Max: DefaultMax, Factor: DefaultFactor}
}

// Next returns the next delay and advances the attempt counter.
//
// The returned value is drawn uniformly from [0, window), where the window
// doubles each attempt up to Max. Two agents that lost their connection at the
// same moment therefore return at different moments, which is the entire point.
func (b *Backoff) Next() time.Duration {
	window := b.window()
	b.attempt++

	if window <= 0 {
		return 0
	}
	return time.Duration(b.random() * float64(window))
}

// Peek returns the current window without advancing, for logging.
func (b *Backoff) Peek() time.Duration { return b.window() }

// Attempt returns how many delays have been handed out since the last Reset.
func (b *Backoff) Attempt() int { return b.attempt }

// Reset returns to the initial delay.
//
// Called after a connection has been up long enough to count as healthy — not
// merely on connect. An agent that resets the moment the TCP handshake completes
// will hammer a server that accepts connections and immediately closes them,
// which is precisely how an overloaded server behaves.
func (b *Backoff) Reset() { b.attempt = 0 }

// window is the deterministic ceiling for the current attempt.
func (b *Backoff) window() time.Duration {
	initial := b.Initial
	if initial <= 0 {
		initial = DefaultInitial
	}
	maximum := b.Max
	if maximum <= 0 {
		maximum = DefaultMax
	}
	if maximum < initial {
		maximum = initial
	}
	factor := b.Factor
	if factor < 1 {
		factor = DefaultFactor
	}

	// math.Pow on a large attempt count overflows to +Inf, and converting that
	// to a Duration is undefined. Clamping the exponent keeps it finite.
	const maxExponent = 62
	exponent := float64(b.attempt)
	if exponent > maxExponent {
		exponent = maxExponent
	}

	scaled := float64(initial) * math.Pow(factor, exponent)
	if scaled > float64(maximum) || math.IsInf(scaled, 1) {
		return maximum
	}
	return time.Duration(scaled)
}

func (b *Backoff) random() float64 {
	if b.Rand != nil {
		return b.Rand()
	}
	return rand.Float64()
}
