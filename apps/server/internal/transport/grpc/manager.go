package grpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// Errors from the manager.
var (
	// ErrNotConnected means this replica does not hold the device's stream.
	// The caller should consult the presence map to find the replica that does.
	ErrNotConnected = errors.New("device is not connected to this replica")

	// ErrStreamFull means the outbound queue is saturated. See Send.
	ErrStreamFull = errors.New("device stream is not keeping up")

	// ErrDraining means the replica is shutting down and is not accepting work.
	ErrDraining = errors.New("replica is draining")
)

// Manager owns every agent stream terminating on this replica.
//
// It is the routing table the rest of the server consults: "is this device on
// me, and if so, send it this". Devices on other replicas are reached by
// publishing on the bus instead, which is why every send path here returns
// ErrNotConnected rather than trying to be clever about it.
type Manager struct {
	log *slog.Logger

	mu      sync.RWMutex
	streams map[uuid.UUID]*Stream

	draining atomic.Bool

	// connected is exported as a gauge. A step change in it means a replica
	// dropped its streams, which is the first thing to look at when devices
	// start reporting offline in a batch.
	connected atomic.Int64
}

// NewManager builds a Manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, streams: map[uuid.UUID]*Stream{}}
}

// Register adds a stream, displacing any existing one for the same device.
//
// Displacement is not an edge case; it is the normal outcome of a network
// partition. The Mac's side of a TCP connection can vanish without either end
// noticing until a keepalive expires, so a device that reconnects here may
// still have an apparently-live stream from before. The newest connection is
// the one that can actually deliver, so it wins and the old one is closed —
// otherwise commands would be written into a socket nobody is reading.
func (m *Manager) Register(s *Stream) error {
	if m.draining.Load() {
		return ErrDraining
	}

	m.mu.Lock()
	previous, existed := m.streams[s.DeviceID]
	m.streams[s.DeviceID] = s
	m.mu.Unlock()

	if existed {
		m.log.Warn("displacing an existing stream for a reconnecting device",
			slog.String("device_id", s.DeviceID.String()),
			slog.String("previous_connection", previous.ConnectionID.String()),
			slog.String("new_connection", s.ConnectionID.String()))
		previous.CloseWithReason(v1.Disconnect_REASON_REBALANCE,
			"superseded by a newer connection from the same device")
	} else {
		m.connected.Add(1)
	}
	return nil
}

// Unregister removes a stream.
//
// The removal is conditional on this being the stream currently registered. A
// displaced stream's teardown runs after the new one registered, and an
// unconditional delete would remove the live stream and leave the device
// unreachable until it reconnected again.
func (m *Manager) Unregister(s *Stream) {
	m.mu.Lock()
	current, ok := m.streams[s.DeviceID]
	if ok && current.ConnectionID == s.ConnectionID {
		delete(m.streams, s.DeviceID)
		m.connected.Add(-1)
	}
	m.mu.Unlock()
}

// Get returns the stream for a device on this replica.
func (m *Manager) Get(deviceID uuid.UUID) (*Stream, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.streams[deviceID]
	return s, ok
}

// Send delivers a message to a device connected to this replica.
func (m *Manager) Send(deviceID uuid.UUID, msg *v1.ServerMessage) error {
	s, ok := m.Get(deviceID)
	if !ok {
		return ErrNotConnected
	}
	return s.Send(msg)
}

// Connected returns how many streams this replica holds.
func (m *Manager) Connected() int64 { return m.connected.Load() }

// DeviceIDs lists the devices connected to this replica.
func (m *Manager) DeviceIDs() []uuid.UUID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(m.streams))
	for id := range m.streams {
		out = append(out, id)
	}
	return out
}

// Disconnect closes a device's stream with a reason.
//
// Used when a device is revoked or decommissioned: the certificate is no longer
// valid, but the existing connection was authenticated before that and would
// otherwise stay open until it happened to break.
func (m *Manager) Disconnect(deviceID uuid.UUID, reason v1.Disconnect_Reason, message string) bool {
	s, ok := m.Get(deviceID)
	if !ok {
		return false
	}
	s.CloseWithReason(reason, message)
	return true
}

// Drain closes every stream and waits for them to finish.
//
// Agents are told REASON_SHUTTING_DOWN, which means "reconnect immediately"
// rather than the backoff they would apply to an unexplained disconnect. That
// is the difference between a rolling deploy costing a few seconds of
// reconnection and costing several minutes of exponential backoff across the
// whole fleet.
func (m *Manager) Drain(ctx context.Context) error {
	m.draining.Store(true)

	m.mu.RLock()
	streams := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		streams = append(streams, s)
	}
	m.mu.RUnlock()

	m.log.InfoContext(ctx, "draining agent streams", slog.Int("streams", len(streams)))

	for _, s := range streams {
		s.CloseWithReason(v1.Disconnect_REASON_SHUTTING_DOWN,
			"this replica is shutting down; reconnect now")
	}

	// Wait for the handlers to unwind so their presence keys are cleared before
	// the process exits. Skipping this would leave devices marked present on a
	// replica that no longer exists, and commands routed to it would be dropped
	// until the key expired.
	deadline := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	for {
		if m.Connected() == 0 {
			m.log.InfoContext(ctx, "all agent streams drained")
			return nil
		}
		select {
		case <-ctx.Done():
			remaining := m.Connected()
			m.log.WarnContext(ctx, "drain timed out with streams still open",
				slog.Int64("remaining", remaining))
			return ctx.Err()
		case <-deadline.C:
		}
	}
}

// Draining reports whether this replica is shutting down.
func (m *Manager) Draining() bool { return m.draining.Load() }
