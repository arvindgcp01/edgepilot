// Package tunnel relays a TCP connection between the server and a local port.
//
// It exists to give an operator a shell on a Mac that has no inbound network
// access. The server cannot dial this machine; this machine already holds one
// outbound stream to the server. A tunnel borrows that stream: the server sends
// TunnelOpen, the agent dials the local target, and from then on the two sides
// exchange TunnelData until either closes.
//
// # What this deliberately cannot do
//
// The target is checked against an allowlist of loopback addresses. Without
// that, an attacker who owned the server could open a tunnel to any host the Mac
// can reach and use the customer's fleet as an egress proxy into their own
// network — which is a far worse outcome than compromising the fleet itself.
//
// The bytes are opaque here. The server relays them between an engineer's SSH
// client and the Mac's sshd, and neither this package nor the server can read
// them: the session is encrypted end to end and this is a pipe. That is why the
// audit record is metadata only.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// readChunk bounds one TunnelData payload.
//
// 32 KiB is the usual pipe buffer and sits far below the server's receive limit,
// so a tunnel cannot be the thing that trips it. Larger frames would cut
// per-message overhead but add latency to an interactive session, which is the
// one thing an SSH user notices immediately.
const readChunk = 32 * 1024

// dialTimeout bounds the connection to the local target.
//
// Loopback either answers immediately or is not listening. A long timeout here
// only delays telling the operator that Remote Login is off.
const dialTimeout = 5 * time.Second

// defaultIdleTimeout applies when the server does not specify one.
const defaultIdleTimeout = 30 * time.Minute

// Sender hands a message to the agent's outbound stream.
//
// It must block rather than drop: tunnel bytes are a stream, and a dropped frame
// does not degrade a session, it corrupts one.
type Sender func(ctx context.Context, msg *v1.AgentMessage) error

// Manager owns every open tunnel on this device.
type Manager struct {
	send Sender
	log  *slog.Logger
	now  func() time.Time

	// allowed is the set of host:port targets a tunnel may reach. See the
	// package comment for why this is not configurable from the server.
	allowed map[string]struct{}

	mu      sync.Mutex
	tunnels map[string]*conn
	closed  bool
}

// Options configures a Manager.
type Options struct {
	Send Sender
	Log  *slog.Logger
	Now  func() time.Time

	// AllowedTargets overrides the loopback-sshd allowlist. Tests set it; the
	// agent does not.
	AllowedTargets []string
}

// New builds a Manager.
func New(opts Options) (*Manager, error) {
	if opts.Send == nil {
		return nil, errors.New("tunnel: a Send function is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	targets := opts.AllowedTargets
	if targets == nil {
		// The local SSH daemon and the local Screen Sharing (VNC) server. Both
		// loopback spellings, because which one the server names depends on how
		// the device reported its addresses. Only loopback and only these two
		// ports: the tunnel cannot be repurposed into a general egress proxy
		// through the Mac, and 5900 is inert until Screen Sharing is enabled by
		// policy — a tunnel to it simply fails to connect until then.
		targets = []string{
			"127.0.0.1:22", "[::1]:22", "localhost:22",
			"127.0.0.1:5900", "[::1]:5900", "localhost:5900",
		}
	}
	allowed := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		allowed[t] = struct{}{}
	}

	return &Manager{
		send:    opts.Send,
		log:     opts.Log,
		now:     opts.Now,
		allowed: allowed,
		tunnels: make(map[string]*conn),
	}, nil
}

// conn is one open tunnel.
type conn struct {
	id     string
	net    net.Conn
	cancel context.CancelFunc

	// seq numbers frames sent to the server. The server checks it for gaps.
	seq atomic.Uint64

	// Byte counts for the audit record. in is device→server, out is
	// server→device, named from the server's point of view so the two ends of
	// the session agree on which is which.
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64

	// lastServerSeq detects a gap in what the server sent.
	lastServerSeq atomic.Uint64

	idleTimeout time.Duration
	lastActive  atomic.Int64 // unix nanos

	closeOnce sync.Once
}

func (c *conn) touch(now time.Time) { c.lastActive.Store(now.UnixNano()) }

// Open honours a TunnelOpen from the server.
//
// It always answers with TunnelOpened — a refusal is as important to report as a
// success, because the operator is sitting at a prompt waiting for one or the
// other.
func (m *Manager) Open(ctx context.Context, req *v1.TunnelOpen) {
	id := req.GetTunnelId()
	if id == "" {
		m.log.Warn("the server opened a tunnel with no identifier; ignoring it")
		return
	}

	target := net.JoinHostPort(req.GetTargetHost(), strconv.Itoa(int(req.GetTargetPort())))
	log := m.log.With(slog.String("tunnel_id", id), slog.String("target", target),
		slog.String("session_id", req.GetSessionId()))

	if _, ok := m.allowed[target]; !ok {
		// Not a fault to fix by retrying: it is the server asking for something
		// this agent will never do. Logged at warn because it is either a server
		// bug or an attempt to misuse the fleet, and both want investigating.
		log.Warn("refusing a tunnel to a target outside the allowlist")
		m.reply(ctx, id, false, "the requested target is not permitted on this device")
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.reply(ctx, id, false, "this agent is shutting down")
		return
	}
	if _, exists := m.tunnels[id]; exists {
		m.mu.Unlock()
		log.Warn("the server reused a tunnel identifier that is already open")
		m.reply(ctx, id, false, "that tunnel identifier is already in use")
		return
	}
	m.mu.Unlock()

	dialer := net.Dialer{Timeout: dialTimeout}
	netConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		// The overwhelmingly common cause is Remote Login being off, and the
		// operator needs to be told that rather than "dial tcp: connection
		// refused" surfacing at their SSH client as a closed channel.
		log.Warn("dialling the local target failed", slog.Any("error", err))
		m.reply(ctx, id, false, sshdHint(err, target))
		return
	}

	idle := time.Duration(req.GetIdleTimeoutS()) * time.Second
	if idle <= 0 {
		idle = defaultIdleTimeout
	}

	tunnelCtx, cancel := context.WithCancel(ctx)
	c := &conn{id: id, net: netConn, cancel: cancel, idleTimeout: idle}
	c.touch(m.now())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		_ = netConn.Close()
		m.reply(ctx, id, false, "this agent is shutting down")
		return
	}
	m.tunnels[id] = c
	m.mu.Unlock()

	log.Info("opened a tunnel to the local target")
	m.reply(ctx, id, true, "")

	go m.pump(tunnelCtx, c, log)
	go m.reapIdle(tunnelCtx, c, log)
}

// pump copies bytes from the local target to the server until either end stops.
func (m *Manager) pump(ctx context.Context, c *conn, log *slog.Logger) {
	buf := make([]byte, readChunk)
	for {
		n, err := c.net.Read(buf)
		if n > 0 {
			c.touch(m.now())
			c.bytesIn.Add(uint64(n))

			frame := &v1.AgentMessage{
				SentAt: timestamppb.New(m.now()),
				Payload: &v1.AgentMessage_TunnelData{TunnelData: &v1.TunnelData{
					TunnelId: c.id,
					// Copied: the buffer is reused on the next iteration, and
					// the message may still be queued when that happens.
					Data:     append([]byte(nil), buf[:n]...),
					Sequence: c.seq.Add(1),
				}},
			}
			if sendErr := m.send(ctx, frame); sendErr != nil {
				// The stream is gone. The server will tear its side down when
				// the stream ends, so there is nothing to report over it.
				log.Debug("sending tunnel data failed; closing",
					slog.Any("error", sendErr))
				m.finish(ctx, c, "the connection to the server was lost", false)
				return
			}
		}
		if err != nil {
			reason := "the local target closed the connection"
			if !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
				log.Debug("reading from the local target ended", slog.Any("error", err))
			}
			if ctx.Err() != nil {
				reason = "the tunnel was cancelled"
			}
			m.finish(ctx, c, reason, true)
			return
		}
	}
}

// reapIdle closes a tunnel nobody is using.
//
// An SSH session that is abandoned rather than exited would otherwise hold a
// connection open on the Mac indefinitely, and the audit record would never
// close.
func (m *Manager) reapIdle(ctx context.Context, c *conn, log *slog.Logger) {
	// Check at a fraction of the timeout so the actual close lands reasonably
	// close to the deadline without a timer per byte.
	tick := c.idleTimeout / 4
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			last := time.Unix(0, c.lastActive.Load())
			if m.now().Sub(last) >= c.idleTimeout {
				log.Info("closing an idle tunnel",
					slog.Duration("idle_timeout", c.idleTimeout))
				m.finish(ctx, c, "the tunnel was idle", true)
				return
			}
		}
	}
}

// Data writes one frame from the server to the local target.
func (m *Manager) Data(ctx context.Context, frame *v1.TunnelData) {
	id := frame.GetTunnelId()

	m.mu.Lock()
	c, ok := m.tunnels[id]
	m.mu.Unlock()
	if !ok {
		// Ordinary during teardown: the server can have frames in flight when
		// this side closes. Not worth a warning.
		m.log.Debug("dropping data for a tunnel that is not open",
			slog.String("tunnel_id", id))
		return
	}

	// A gap means frames were lost or reordered. Writing what did arrive would
	// hand sshd a corrupted stream, which fails in ways far harder to diagnose
	// than a closed session.
	if seq := frame.GetSequence(); seq != 0 {
		if previous := c.lastServerSeq.Swap(seq); seq != previous+1 {
			m.log.Warn("a tunnel frame arrived out of order; tearing the tunnel down",
				slog.String("tunnel_id", id),
				slog.Uint64("expected", previous+1), slog.Uint64("got", seq))
			m.finish(ctx, c, "the tunnel received frames out of order", true)
			return
		}
	}

	data := frame.GetData()
	if len(data) == 0 {
		return
	}
	c.touch(m.now())

	if _, err := c.net.Write(data); err != nil {
		if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
			m.log.Debug("writing to the local target failed",
				slog.String("tunnel_id", id), slog.Any("error", err))
		}
		m.finish(ctx, c, "writing to the local target failed", true)
		return
	}
	c.bytesOut.Add(uint64(len(data)))
}

// Close honours a TunnelClose from the server.
func (m *Manager) Close(ctx context.Context, req *v1.TunnelClose) {
	id := req.GetTunnelId()

	m.mu.Lock()
	c, ok := m.tunnels[id]
	m.mu.Unlock()
	if !ok {
		return
	}

	reason := req.GetReason()
	if reason == "" {
		reason = "the server closed the tunnel"
	}
	m.finish(ctx, c, reason, true)
}

// CloseAll tears down every tunnel, for a stream that is ending.
//
// No TunnelClosed is sent: the stream those messages would travel on is the
// thing that just died. The server closes its side when the stream ends.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	m.closed = true
	open := make([]*conn, 0, len(m.tunnels))
	for _, c := range m.tunnels {
		open = append(open, c)
	}
	m.tunnels = make(map[string]*conn)
	m.mu.Unlock()

	for _, c := range open {
		c.closeOnce.Do(func() {
			c.cancel()
			_ = c.net.Close()
		})
	}
}

// Count reports how many tunnels are currently established.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tunnels)
}

// finish closes one tunnel exactly once and tells the server it is gone.
func (m *Manager) finish(ctx context.Context, c *conn, reason string, report bool) {
	var first bool
	c.closeOnce.Do(func() {
		first = true
		c.cancel()
		_ = c.net.Close()
	})
	if !first {
		return
	}

	m.mu.Lock()
	delete(m.tunnels, c.id)
	m.mu.Unlock()

	if !report {
		return
	}

	// Best effort. If the stream has gone there is nowhere to report to, and
	// the server tears its side down on stream end regardless.
	_ = m.send(ctx, &v1.AgentMessage{
		SentAt: timestamppb.New(m.now()),
		Payload: &v1.AgentMessage_TunnelClosed{TunnelClosed: &v1.TunnelClosed{
			TunnelId: c.id,
			Reason:   reason,
			BytesIn:  c.bytesIn.Load(),
			BytesOut: c.bytesOut.Load(),
		}},
	})
}

// reply answers a TunnelOpen.
func (m *Manager) reply(ctx context.Context, id string, success bool, errMsg string) {
	_ = m.send(ctx, &v1.AgentMessage{
		SentAt: timestamppb.New(m.now()),
		Payload: &v1.AgentMessage_TunnelOpened{TunnelOpened: &v1.TunnelOpened{
			TunnelId: id,
			Success:  success,
			Error:    errMsg,
		}},
	})
}

// sshdHint turns a dial failure into something an operator can act on.
//
// "connection refused" on loopback:22 has one overwhelmingly likely cause on a
// Mac, and naming it here saves a support round trip — the operator sees this
// text at their SSH client instead of a channel that simply closes.
func sshdHint(err error, target string) string {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf(
			"nothing is listening on %s — enable Remote Login on this Mac "+
				"(System Settings › General › Sharing, or "+
				"`sudo systemsetup -setremotelogin on`)", target)
	}
	return fmt.Sprintf("could not reach %s on this device: %v", target, err)
}
