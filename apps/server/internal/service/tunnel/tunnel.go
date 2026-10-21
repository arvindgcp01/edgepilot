// Package tunnel relays TCP bytes to a device over its existing stream.
//
// A Mac at a customer site accepts no inbound connections. The only path to it
// is the mTLS stream it already holds open to this server, so a tunnel is a
// virtual connection multiplexed onto that stream: the server sends TunnelOpen,
// the agent dials its own loopback sshd, and the two sides exchange TunnelData
// until one closes.
//
// # Why a Tunnel is an io.ReadWriteCloser
//
// Because the caller is a byte pipe and should be written as one. The bastion
// does io.Copy in each direction and nothing else; every detail of framing,
// sequencing, and teardown lives here rather than being repeated by callers who
// would each get it slightly wrong.
//
// # What this cannot see
//
// The bytes are an SSH session encrypted between the engineer's client and the
// Mac's sshd. This server relays them and cannot read them, which is the point:
// a compromised control plane must not yield the contents of engineers'
// sessions. The audit record in ssh_sessions is metadata only, and that is a
// deliberate trade rather than a gap — see docs/Security.md §5.1.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// Errors callers distinguish.
var (
	// ErrNotConnected means the device holds no stream on this replica.
	ErrNotConnected = errors.New("tunnel: the device is not connected")

	// ErrRefused means the agent declined to open the tunnel. Its message
	// explains why — most often that Remote Login is off.
	ErrRefused = errors.New("tunnel: the device refused the tunnel")

	// ErrTimeout means the agent never answered the open request.
	ErrTimeout = errors.New("tunnel: the device did not answer")

	// ErrClosed means the tunnel is finished.
	ErrClosed = errors.New("tunnel: the tunnel is closed")
)

// openTimeout bounds the wait for TunnelOpened.
//
// The agent's own dial timeout is five seconds, so this has to exceed it or the
// server gives up while the device is still answering — and the operator is told
// "no answer" for what is actually a refusal with a useful message in it.
const openTimeout = 15 * time.Second

// dataBacklog is how many frames may queue for a reader that has stalled.
//
// SSH is interactive and its flow control lives above this layer, so a deep
// buffer here only adds latency. When it fills, the tunnel is torn down rather
// than allowing the device's stream to block behind one slow operator.
const dataBacklog = 64

// Dispatcher sends a message to a connected device.
//
// Declared here and satisfied by the gRPC manager so this package does not
// import the transport — the dependency runs the other way, and a cycle is the
// least of the reasons that would be wrong.
type Dispatcher interface {
	Send(deviceID uuid.UUID, msg *v1.ServerMessage) error
}

// Registry owns every tunnel this replica has open.
type Registry struct {
	dispatch Dispatcher
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

// Options configures a Registry.
type Options struct {
	Dispatch Dispatcher
	Logger   *slog.Logger
	Now      func() time.Time
}

// New builds a Registry.
func New(opts Options) (*Registry, error) {
	if opts.Dispatch == nil {
		return nil, errors.New("tunnel: a Dispatcher is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Registry{
		dispatch: opts.Dispatch,
		log:      opts.Logger,
		now:      opts.Now,
		tunnels:  make(map[string]*Tunnel),
	}, nil
}

// Tunnel is one relayed connection, usable as a byte pipe.
type Tunnel struct {
	ID        string
	DeviceID  uuid.UUID
	SessionID string

	registry *Registry
	log      *slog.Logger

	// opened carries the agent's answer to TunnelOpen exactly once.
	opened chan error

	// data carries frames from the device to Read.
	data chan []byte
	// leftover is the remainder of a frame a short Read did not consume.
	leftover []byte

	closed      chan struct{}
	closeOnce   sync.Once
	closeReason atomic.Pointer[string]

	// seq numbers frames sent to the device; the agent checks it for gaps.
	seq atomic.Uint64
	// lastAgentSeq detects a gap in what the device sent.
	lastAgentSeq atomic.Uint64

	bytesToDevice   atomic.Uint64
	bytesFromDevice atomic.Uint64

	startedAt time.Time
}

// Open establishes a tunnel to a device and waits for the agent to confirm it.
//
// Returns ErrRefused with the agent's own explanation when the device declines,
// because that message — "Remote Login is off" — is the one the operator needs.
func (r *Registry) Open(ctx context.Context, deviceID uuid.UUID, sessionID string,
	host string, port uint32, idle time.Duration,
) (*Tunnel, error) {
	id := uuid.NewString()

	t := &Tunnel{
		ID:        id,
		DeviceID:  deviceID,
		SessionID: sessionID,
		registry:  r,
		log: r.log.With(slog.String("tunnel_id", id),
			slog.String("device_id", deviceID.String()),
			slog.String("session_id", sessionID)),
		opened:    make(chan error, 1),
		data:      make(chan []byte, dataBacklog),
		closed:    make(chan struct{}),
		startedAt: r.now(),
	}

	r.mu.Lock()
	r.tunnels[id] = t
	r.mu.Unlock()

	// Registered before the send, so an implausibly fast TunnelOpened cannot
	// arrive for a tunnel the registry has not heard of yet.
	err := r.dispatch.Send(deviceID, &v1.ServerMessage{
		SentAt: timestamppb.New(r.now()),
		Payload: &v1.ServerMessage_TunnelOpen{TunnelOpen: &v1.TunnelOpen{
			TunnelId:     id,
			TargetHost:   host,
			TargetPort:   port,
			SessionId:    sessionID,
			IdleTimeoutS: uint32(idle.Seconds()),
		}},
	})
	if err != nil {
		r.forget(id)
		return nil, fmt.Errorf("%w: %w", ErrNotConnected, err)
	}

	select {
	case openErr := <-t.opened:
		if openErr != nil {
			r.forget(id)
			return nil, openErr
		}
		t.log.Info("opened a tunnel to the device")
		return t, nil

	case <-time.After(openTimeout):
		r.forget(id)
		// Tell the device to drop it: without this the agent holds a connection
		// to its own sshd that nothing will ever read.
		r.sendClose(deviceID, id, "the server timed out waiting for the tunnel to open")
		return nil, ErrTimeout

	case <-ctx.Done():
		r.forget(id)
		r.sendClose(deviceID, id, "the operator disconnected")
		return nil, ctx.Err()
	}
}

// OnOpened routes the agent's answer to Open.
func (r *Registry) OnOpened(msg *v1.TunnelOpened) {
	t, ok := r.get(msg.GetTunnelId())
	if !ok {
		return
	}
	if msg.GetSuccess() {
		select {
		case t.opened <- nil:
		default:
		}
		return
	}

	reason := msg.GetError()
	if reason == "" {
		reason = "the device gave no reason"
	}
	select {
	case t.opened <- fmt.Errorf("%w: %s", ErrRefused, reason):
	default:
	}
}

// OnData routes one frame from the device to whoever is reading the tunnel.
func (r *Registry) OnData(msg *v1.TunnelData) {
	t, ok := r.get(msg.GetTunnelId())
	if !ok {
		return
	}

	// A gap means frames were lost or reordered. Delivering what arrived would
	// hand the operator's SSH client a corrupt stream, which fails far less
	// legibly than a closed session.
	if seq := msg.GetSequence(); seq != 0 {
		if previous := t.lastAgentSeq.Swap(seq); seq != previous+1 {
			t.log.Warn("a tunnel frame arrived out of order; tearing the tunnel down",
				slog.Uint64("expected", previous+1), slog.Uint64("got", seq))
			t.closeWith("the tunnel received frames out of order")
			return
		}
	}

	data := msg.GetData()
	if len(data) == 0 {
		return
	}
	t.bytesFromDevice.Add(uint64(len(data)))

	select {
	case t.data <- data:
	case <-t.closed:
	default:
		// The reader has stalled. Blocking here would stall the device's whole
		// stream — heartbeats included — behind one operator, so this tunnel
		// dies instead.
		t.log.Warn("the tunnel backlog is full; tearing the tunnel down")
		t.closeWith("the operator was not reading fast enough")
	}
}

// OnClosed records that the device ended the tunnel.
func (r *Registry) OnClosed(msg *v1.TunnelClosed) {
	t, ok := r.get(msg.GetTunnelId())
	if !ok {
		return
	}
	reason := msg.GetReason()
	if reason == "" {
		reason = "the device closed the tunnel"
	}
	t.log.Info("the device closed the tunnel",
		slog.String("reason", reason),
		slog.Uint64("bytes_in", msg.GetBytesIn()),
		slog.Uint64("bytes_out", msg.GetBytesOut()))
	t.closeWith(reason)
}

// CloseDevice tears down every tunnel for a device whose stream has ended.
//
// Without this a tunnel would wait out its read on a stream that no longer
// exists, and the operator's SSH client would hang rather than disconnect.
func (r *Registry) CloseDevice(deviceID uuid.UUID) {
	r.mu.Lock()
	var doomed []*Tunnel
	for _, t := range r.tunnels {
		if t.DeviceID == deviceID {
			doomed = append(doomed, t)
		}
	}
	r.mu.Unlock()

	for _, t := range doomed {
		t.closeWith("the device disconnected")
	}
}

// Count reports how many tunnels are open on this replica.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tunnels)
}

func (r *Registry) get(id string) (*Tunnel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tunnels[id]
	return t, ok
}

func (r *Registry) forget(id string) {
	r.mu.Lock()
	delete(r.tunnels, id)
	r.mu.Unlock()
}

func (r *Registry) sendClose(deviceID uuid.UUID, id, reason string) {
	// Best effort: the usual cause of needing this is a stream that has already
	// gone, and there is nothing useful to do about a failure to reach it.
	_ = r.dispatch.Send(deviceID, &v1.ServerMessage{
		SentAt: timestamppb.New(r.now()),
		Payload: &v1.ServerMessage_TunnelClose{TunnelClose: &v1.TunnelClose{
			TunnelId: id,
			Reason:   reason,
		}},
	})
}

// Read delivers bytes the device sent.
func (t *Tunnel) Read(p []byte) (int, error) {
	if len(t.leftover) > 0 {
		n := copy(p, t.leftover)
		t.leftover = t.leftover[n:]
		return n, nil
	}

	select {
	case frame := <-t.data:
		n := copy(p, frame)
		if n < len(frame) {
			// Held rather than dropped: a short read is normal, and discarding
			// the remainder would silently truncate the session.
			t.leftover = frame[n:]
		}
		return n, nil

	case <-t.closed:
		// Drain whatever arrived before the close. An SSH server's goodbye can
		// be in flight when the connection ends, and dropping it turns a clean
		// logout into a broken pipe at the client.
		select {
		case frame := <-t.data:
			n := copy(p, frame)
			if n < len(frame) {
				t.leftover = frame[n:]
			}
			return n, nil
		default:
		}
		return 0, io.EOF
	}
}

// Write sends bytes to the device.
func (t *Tunnel) Write(p []byte) (int, error) {
	select {
	case <-t.closed:
		return 0, ErrClosed
	default:
	}

	err := t.registry.dispatch.Send(t.DeviceID, &v1.ServerMessage{
		SentAt: timestamppb.New(t.registry.now()),
		Payload: &v1.ServerMessage_TunnelData{TunnelData: &v1.TunnelData{
			TunnelId: t.ID,
			// Copied: io.Copy reuses its buffer, and the frame may still be
			// queued on the device's stream when it does.
			Data:     append([]byte(nil), p...),
			Sequence: t.seq.Add(1),
		}},
	})
	if err != nil {
		t.closeWith("the connection to the device was lost")
		return 0, fmt.Errorf("%w: %w", ErrClosed, err)
	}

	t.bytesToDevice.Add(uint64(len(p)))
	return len(p), nil
}

// Close ends the tunnel and tells the device to drop its end.
func (t *Tunnel) Close() error {
	t.closeWith("the operator closed the session")
	return nil
}

// Stats reports the audit counters, valid once the tunnel is closed.
func (t *Tunnel) Stats() (bytesToDevice, bytesFromDevice uint64, duration time.Duration) {
	return t.bytesToDevice.Load(), t.bytesFromDevice.Load(),
		t.registry.now().Sub(t.startedAt)
}

// Reason reports why the tunnel closed, if it has.
func (t *Tunnel) Reason() string {
	if r := t.closeReason.Load(); r != nil {
		return *r
	}
	return ""
}

// Done is closed when the tunnel ends, whichever side ended it.
func (t *Tunnel) Done() <-chan struct{} { return t.closed }

// closeWith ends the tunnel exactly once.
func (t *Tunnel) closeWith(reason string) {
	t.closeOnce.Do(func() {
		t.closeReason.Store(&reason)
		close(t.closed)
		t.registry.forget(t.ID)
		t.registry.sendClose(t.DeviceID, t.ID, reason)
	})
}
