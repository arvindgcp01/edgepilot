package grpc

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// DefaultSendQueueSize bounds the per-stream outbound queue.
//
// Sized for a burst of dispatch, not for buffering an unresponsive agent. A
// device that falls this far behind is not going to catch up, and holding more
// for it would trade one stuck stream for memory across the whole fleet: at
// 10,000 devices, every 100 queued messages is a million messages resident.
const DefaultSendQueueSize = 64

// Stream is one agent's connection to this replica.
//
// # The backpressure policy
//
// The outbound queue is bounded, so something has to give when it fills. The
// choice made here is: drop what is perishable, and terminate the stream for
// anything else.
//
// Perishable messages — pings — have no value once late, so dropping one is
// free. Everything else (command dispatch, config, deployment tasks) has a
// durable record in PostgreSQL that a reconciliation loop re-delivers within
// seconds. So terminating a saturated stream loses nothing and returns the
// device to a known state, whereas blocking would stall the writer and
// silently dropping would lose a command with no record that it happened.
//
// The one thing this must never become is an unbounded queue. That converts a
// single unresponsive Mac into server-wide memory growth.
type Stream struct {
	DeviceID     uuid.UUID
	OrgID        uuid.UUID
	ConnectionID uuid.UUID

	// AgentVersion and BootID come from Hello. BootID distinguishes a
	// reconnect from a restart, which is what makes the agent_stopped alert
	// accurate rather than firing on every network blip.
	AgentVersion string
	BootID       string

	ConnectedAt time.Time

	log *slog.Logger

	// outbound is the bounded send queue. The send loop is its only reader.
	outbound chan *v1.ServerMessage

	// closeOnce guards teardown, which several paths can trigger: the recv loop
	// on a transport error, the manager on displacement or drain, and the
	// device itself by disconnecting.
	closeOnce sync.Once
	closed    chan struct{}

	// disconnect carries a final message to send before closing. Buffered so
	// CloseWithReason never blocks — it is called from the manager while it may
	// hold a lock, and from teardown paths that must not stall.
	disconnect chan *v1.Disconnect

	// seq is this replica's outbound sequence for the connection.
	seq atomic.Uint64

	// lastAgentSeq is the highest agent sequence processed, for within-
	// connection deduplication. See Deduplicate.
	lastAgentSeq atomic.Uint64

	// Counters, exported as metrics.
	sent          atomic.Int64
	received      atomic.Int64
	droppedPings  atomic.Int64
	lastHeartbeat atomic.Int64 // unix nanos
}

// NewStream builds a Stream.
func NewStream(deviceID, orgID uuid.UUID, queueSize int, log *slog.Logger) *Stream {
	if queueSize <= 0 {
		queueSize = DefaultSendQueueSize
	}
	if log == nil {
		log = slog.Default()
	}
	connectionID, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 fails only if the entropy source does, which is not a
		// condition to keep serving through — but neither is it worth failing
		// a connection over, so fall back to v4.
		connectionID = uuid.New()
	}

	return &Stream{
		DeviceID:     deviceID,
		OrgID:        orgID,
		ConnectionID: connectionID,
		ConnectedAt:  time.Now().UTC(),
		log: log.With(
			slog.String("device_id", deviceID.String()),
			slog.String("connection_id", connectionID.String())),
		outbound:   make(chan *v1.ServerMessage, queueSize),
		closed:     make(chan struct{}),
		disconnect: make(chan *v1.Disconnect, 1),
	}
}

// Send enqueues a message for the agent.
//
// Never blocks. See the type comment for why a full queue terminates the stream
// rather than waiting: the caller here is usually an HTTP handler serving an
// operator, and blocking it on a Mac that stopped reading would turn one
// unresponsive device into a hung request.
func (s *Stream) Send(msg *v1.ServerMessage) error {
	select {
	case <-s.closed:
		return ErrNotConnected
	default:
	}

	msg.Seq = s.seq.Add(1)
	if msg.SentAt == nil {
		msg.SentAt = timestampNow()
	}

	select {
	case s.outbound <- msg:
		s.sent.Add(1)
		return nil
	case <-s.closed:
		return ErrNotConnected
	default:
	}

	// The queue is full.
	if isPerishable(msg) {
		// A late ping is worth nothing. Drop it and carry on.
		s.droppedPings.Add(1)
		return nil
	}

	// Anything else has a durable record that reconciliation will re-deliver,
	// so ending the stream costs a reconnect rather than a lost command.
	s.log.Warn("outbound queue saturated; closing the stream so the device reconnects",
		slog.Int("queue_size", cap(s.outbound)),
		slog.Int64("dropped_pings", s.droppedPings.Load()))
	s.CloseWithReason(v1.Disconnect_REASON_REBALANCE,
		"the server could not keep up with this stream; reconnect")
	return ErrStreamFull
}

// Outbound is the queue the send loop reads.
func (s *Stream) Outbound() <-chan *v1.ServerMessage { return s.outbound }

// Disconnects carries the final message to send before closing.
func (s *Stream) Disconnects() <-chan *v1.Disconnect { return s.disconnect }

// Closed is closed when the stream is finished.
func (s *Stream) Closed() <-chan struct{} { return s.closed }

// IsClosed reports whether the stream has been closed.
func (s *Stream) IsClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// Close ends the stream without an explanation.
func (s *Stream) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// CloseWithReason ends the stream, telling the agent why.
//
// The reason matters: an agent told REASON_SHUTTING_DOWN reconnects at once,
// while an unexplained disconnect triggers exponential backoff. Across a fleet
// during a rolling deploy that is the difference between seconds and minutes of
// devices appearing offline.
func (s *Stream) CloseWithReason(reason v1.Disconnect_Reason, message string) {
	// Offered, not guaranteed. If the send loop has already gone the buffered
	// channel simply keeps it, and the close below is what actually matters.
	select {
	case s.disconnect <- &v1.Disconnect{Reason: reason, Message: message}:
	default:
	}
	s.Close()
}

// RecordHeartbeat stamps the last heartbeat time.
func (s *Stream) RecordHeartbeat() { s.lastHeartbeat.Store(time.Now().UnixNano()) }

// LastHeartbeat returns when the agent last checked in.
func (s *Stream) LastHeartbeat() time.Time {
	n := s.lastHeartbeat.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// RecordReceived counts an inbound message.
func (s *Stream) RecordReceived() { s.received.Add(1) }

// Stats reports counters for metrics and debugging.
func (s *Stream) Stats() StreamStats {
	return StreamStats{
		DeviceID:      s.DeviceID,
		OrgID:         s.OrgID,
		ConnectionID:  s.ConnectionID,
		AgentVersion:  s.AgentVersion,
		ConnectedAt:   s.ConnectedAt,
		Sent:          s.sent.Load(),
		Received:      s.received.Load(),
		DroppedPings:  s.droppedPings.Load(),
		QueueDepth:    len(s.outbound),
		QueueCapacity: cap(s.outbound),
		LastHeartbeat: s.LastHeartbeat(),
	}
}

// StreamStats is a point-in-time view of a stream.
type StreamStats struct {
	DeviceID      uuid.UUID
	OrgID         uuid.UUID
	ConnectionID  uuid.UUID
	AgentVersion  string
	ConnectedAt   time.Time
	Sent          int64
	Received      int64
	DroppedPings  int64
	QueueDepth    int
	QueueCapacity int
	LastHeartbeat time.Time
}

// Deduplicate reports whether an agent message has already been processed on
// this connection, and records it otherwise.
//
// Scope is deliberately one connection. An agent replaying its spool after a
// reconnect starts its sequence again, so cross-connection duplicates are not
// caught here — they are made harmless by construction instead: a command
// result upserts on (command_id, attempt), and a metric sample upserts on
// (device_id, time). Applying either twice changes nothing.
//
// Tracking cross-connection sequences durably would mean a database write per
// message on the hottest path in the system, to prevent duplicates that are
// already idempotent. That trade is not worth making.
func (s *Stream) Deduplicate(seq uint64) bool {
	if seq == 0 {
		// Unsequenced messages cannot be deduplicated and are not required to
		// be — only durable ones carry a sequence.
		return false
	}
	for {
		last := s.lastAgentSeq.Load()
		if seq <= last {
			return true
		}
		if s.lastAgentSeq.CompareAndSwap(last, seq) {
			return false
		}
	}
}

// LastAgentSeq returns the highest agent sequence processed.
func (s *Stream) LastAgentSeq() uint64 { return s.lastAgentSeq.Load() }

// isPerishable reports whether a message loses all value when late.
//
// Only pings qualify today. Metrics would if the server ever sent them, but
// everything else the server sends is either an instruction with a durable
// record or a tunnel byte that cannot be reordered.
func isPerishable(msg *v1.ServerMessage) bool {
	_, ping := msg.GetPayload().(*v1.ServerMessage_Ping)
	return ping
}
