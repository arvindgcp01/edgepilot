package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/visionify/edgepilot/apps/agent/internal/tunnel"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// drainBatch is how many spooled records are sent per pass.
//
// Bounded so that a six-hour backlog does not monopolise the stream the moment
// the link returns: the server also wants heartbeats from this device, and an
// agent that spends the first minute of its reconnection replaying looks offline
// for that minute.
const drainBatch = 32

// serve runs the steady state until the stream ends.
//
// One goroutine reads and one writes, because gRPC permits exactly one
// concurrent SendMsg and one concurrent RecvMsg per stream. Everything that
// needs to send therefore goes through a channel rather than touching the
// stream, and the reader is the only caller of Recv.
func (c *Client) serve(ctx context.Context, stream v1.AgentService_ConnectClient) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Two queues, because a pong that waits behind a replay of a six-hour
	// backlog measures the backlog rather than the link — and the server uses
	// exactly that measurement to decide whether this agent is alive.
	priority := make(chan *v1.AgentMessage, 8)
	outbound := make(chan *v1.AgentMessage, 64)

	// Tunnels live and die with the stream: they are relayed over it, so there
	// is nothing to preserve across a reconnect. The manager is published for
	// the duration and withdrawn on the way out, which is also what stops a
	// tunnel outliving the connection that carried it.
	//
	// Tunnel frames block rather than being offered like a heartbeat. A dropped
	// heartbeat costs one interval; a dropped tunnel frame corrupts an SSH
	// session, and sshd's failure mode for that is not a helpful one.
	tunnels, err := tunnel.New(tunnel.Options{
		Log: c.log,
		Now: c.now,
		Send: func(sendCtx context.Context, msg *v1.AgentMessage) error {
			select {
			case outbound <- msg:
				return nil
			case <-sendCtx.Done():
				return sendCtx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		return fmt.Errorf("client: preparing tunnels: %w", err)
	}
	c.tunnels.Store(tunnels)
	defer func() {
		c.tunnels.Store(nil)
		tunnels.CloseAll()
	}()

	// Both loops run in their own goroutines, and this function waits on
	// whichever finishes first.
	//
	// Neither can be the one that blocks here. The receive loop sits in Recv,
	// which nothing on this side can interrupt — only ending the RPC unblocks
	// it. The send loop sits in a select that knows nothing about the stream, so
	// it would happily wait forever after the server has gone. Waiting on either
	// one specifically means a stream that ended the other way is not noticed
	// until the whole agent is shut down, which is a device that looks connected
	// and is not.
	var wg sync.WaitGroup
	recvDone := make(chan error, 1)
	sendDone := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		recvDone <- c.recvLoop(ctx, stream, priority)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		sendDone <- c.sendLoop(ctx, stream, priority, outbound)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.produce(ctx, outbound)
	}()

	var (
		received, sent         error
		haveReceived, haveSent bool
	)
	select {
	case received = <-recvDone:
		haveReceived = true
	case sent = <-sendDone:
		haveSent = true
	case <-ctx.Done():
	}

	// Cancelling ends the RPC, which is what releases a goroutine blocked in
	// Recv, and stops both the send loop and the producer.
	cancel()

	// The send side is drained before anything else, because CloseSend may not
	// run concurrently with Send — gRPC allows one writer, and calling both at
	// once corrupts the stream's internal state rather than failing cleanly.
	if !haveSent {
		select {
		case sent = <-sendDone:
		case <-time.After(5 * time.Second):
		}
	}
	// Now that nothing else can write, tell the server this agent is finished:
	// an orderly half-close rather than a socket that simply stops.
	_ = stream.CloseSend()

	// The receive side usually holds the more informative error — a deliberate
	// Disconnect arrives there, while the send side sees only a closed stream —
	// so it is worth a bounded wait.
	if !haveReceived {
		select {
		case received = <-recvDone:
		case <-time.After(5 * time.Second):
			received = errors.New("client: the receive loop did not stop; abandoning the stream")
		}
	}
	sendErr := sent

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}

	// A deliberate server-side disconnect explains everything that follows it,
	// so it wins over the transport error it causes.
	var stop *serverStop
	switch {
	case errors.As(received, &stop):
		return received
	case errors.As(sendErr, &stop):
		return sendErr
	case received != nil && !errors.Is(received, io.EOF):
		return received
	default:
		return sendErr
	}
}

// sendLoop owns the write side of the stream.
func (c *Client) sendLoop(ctx context.Context, stream v1.AgentService_ConnectClient,
	priority, outbound <-chan *v1.AgentMessage,
) error {
	send := func(msg *v1.AgentMessage) error {
		if err := stream.Send(msg); err != nil {
			if errors.Is(err, io.EOF) {
				// The server closed the stream; Recv carries the reason, and
				// reporting the EOF here would mask it.
				return nil
			}
			return fmt.Errorf("client: send: %w", err)
		}
		c.sends.Add(1)
		return nil
	}

	for {
		// Priority first, and non-blocking, so a pong never queues behind a
		// replay.
		select {
		case msg := <-priority:
			if err := send(msg); err != nil {
				return err
			}
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return nil
		case msg := <-priority:
			if err := send(msg); err != nil {
				return err
			}
		case msg := <-outbound:
			if err := send(msg); err != nil {
				return err
			}
		}
	}
}

// produce generates outbound traffic: heartbeats on a timer, spooled records as
// fast as the stream will take them.
func (c *Client) produce(ctx context.Context, outbound chan<- *v1.AgentMessage) {
	interval := c.Policy().HeartbeatInterval

	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()

	metricsEvery := c.Policy().MetricsInterval
	metrics := time.NewTicker(metricsEvery)
	defer metrics.Stop()

	// The spool is drained on a short timer rather than on a signal: a signal
	// would require every producer to know about the connection, and a backlog
	// is not latency-critical enough to justify that coupling.
	drain := time.NewTicker(time.Second)
	defer drain.Stop()

	// One heartbeat immediately. The server's presence key is set from
	// heartbeats, so waiting a full interval would leave a freshly connected
	// device looking offline for fifteen seconds.
	c.offer(ctx, outbound, c.heartbeat())
	c.drainSpool(ctx, outbound)
	// And one reading straight away, for the same reason as the heartbeat: a
	// device that just connected should not show blank gauges for a minute.
	c.collectMetrics(ctx, outbound)

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeat.C:
			// Re-read the policy each tick: a ConfigUpdate may have changed the
			// interval since this ticker was created.
			if current := c.Policy().HeartbeatInterval; current != interval {
				interval = current
				heartbeat.Reset(current)
				c.log.Info("the server changed the heartbeat interval",
					slog.Duration("interval", current))
			}
			c.offer(ctx, outbound, c.heartbeat())

		case <-metrics.C:
			if current := c.Policy().MetricsInterval; current != metricsEvery {
				metricsEvery = current
				metrics.Reset(current)
				c.log.Info("the server changed the metrics interval",
					slog.Duration("interval", current))
			}
			c.collectMetrics(ctx, outbound)

		case <-drain.C:
			c.drainSpool(ctx, outbound)
		}
	}
}

// offer enqueues a perishable message without blocking.
//
// A full queue means the stream is not draining. Dropping a heartbeat there is
// correct — it is perishable and the next one is one interval away — while
// blocking would wedge the producer behind a connection that is already failing.
func (c *Client) offer(ctx context.Context, outbound chan<- *v1.AgentMessage, msg *v1.AgentMessage) {
	select {
	case outbound <- msg:
	case <-ctx.Done():
	default:
		c.log.Debug("the outbound queue is full; dropped a heartbeat")
	}
}

// drainSpool sends buffered durable records.
//
// Records are *not* committed here. They are committed when the server
// acknowledges them, because a record removed on send and never acknowledged is
// exactly the lost command result the spool exists to prevent.
func (c *Client) drainSpool(ctx context.Context, outbound chan<- *v1.AgentMessage) {
	records, err := c.spool.Peek(drainBatch)
	if err != nil {
		c.log.Error("reading the spool failed", slog.Any("error", err))
		return
	}

	for _, record := range records {
		var msg v1.AgentMessage
		if err := proto.Unmarshal(record.Payload, &msg); err != nil {
			// Undeliverable and unfixable. Committing it is the only way past,
			// and leaving it would block every record behind it forever.
			c.log.Error("a spooled message could not be decoded and was discarded",
				slog.Uint64("seq", record.Seq), slog.Any("error", err))
			if commitErr := c.spool.Commit(record.Seq); commitErr != nil {
				c.log.Error("discarding it failed too", slog.Any("error", commitErr))
			}
			continue
		}

		// The spool sequence becomes the stream sequence, so the server's ack —
		// which names a seq — maps straight back onto a spool record.
		msg.Seq = record.Seq
		if msg.SentAt == nil {
			msg.SentAt = timestamppb.New(c.now())
		}

		select {
		case outbound <- &msg:
		case <-ctx.Done():
			return
		default:
			// The stream is backed up. These records stay spooled and the next
			// pass tries again — this is the one place where not dropping is
			// the right answer.
			return
		}
	}
}

// recvLoop reads until the stream ends.
func (c *Client) recvLoop(ctx context.Context, stream v1.AgentService_ConnectClient,
	priority chan<- *v1.AgentMessage,
) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				// EOF is the server closing its side in an orderly way, and a
				// cancelled context is this agent shutting down. Neither is a
				// fault, and reporting one would put a spurious error in the
				// log on every clean stop.
				return nil //nolint:nilerr // an orderly close is not a failure
			}
			return c.classifyStreamError(err)
		}

		c.receives.Add(1)
		if stop := c.handle(ctx, msg, priority); stop != nil {
			return stop
		}
	}
}

// handle processes one server message, returning non-nil to end the stream.
func (c *Client) handle(ctx context.Context, msg *v1.ServerMessage,
	priority chan<- *v1.AgentMessage,
) error {
	// Record the sequence before acting on it. The server resumes from
	// last_seq_seen, so a message acted on but not recorded would be replayed
	// after a reconnect and acted on twice.
	if seq := msg.GetSeq(); seq > c.lastSeqSeen.Load() {
		c.lastSeqSeen.Store(seq)
	}

	switch payload := msg.GetPayload().(type) {
	case *v1.ServerMessage_Ping:
		pong := &v1.AgentMessage{
			SentAt:  timestamppb.New(c.now()),
			Payload: &v1.AgentMessage_Pong{Pong: &v1.Pong{Nonce: payload.Ping.GetNonce()}},
		}
		select {
		case priority <- pong:
		case <-ctx.Done():
		default:
			// Eight unanswered pings queued means the send side is not moving
			// at all, and the server will conclude this agent is gone — which
			// by then it effectively is.
			c.log.Warn("the priority queue is full; a ping went unanswered")
		}
		return nil

	case *v1.ServerMessage_Ack:
		// The server has durably accepted everything up to this sequence, so
		// the spool can let those records go.
		if err := c.spool.Commit(payload.Ack.GetAckedSeq()); err != nil {
			c.log.Error("committing acknowledged messages failed",
				slog.Uint64("acked_seq", payload.Ack.GetAckedSeq()), slog.Any("error", err))
		}
		return nil

	case *v1.ServerMessage_Config:
		c.applyConfig(payload.Config)
		c.log.Info("the server updated this device's policy",
			slog.Bool("allow_custom_scripts", payload.Config.GetAllowCustomScripts()),
			slog.Int("enabled_commands", len(payload.Config.GetEnabledCommandSlugs())))
		return nil

	case *v1.ServerMessage_Welcome:
		// A second Welcome mid-stream is a server bug, but applying it is
		// harmless and more useful than dropping the connection over it.
		c.applyWelcome(payload.Welcome)
		return nil

	case *v1.ServerMessage_Disconnect:
		return c.disconnect(payload.Disconnect)

	case *v1.ServerMessage_TunnelOpen:
		// Dialling the local target can block for the dial timeout, and this
		// goroutine is the one reading the stream — including the tunnel frames
		// that follow. Opening inline would deadlock the connection against
		// itself.
		if tunnels := c.tunnels.Load(); tunnels != nil {
			go tunnels.Open(ctx, payload.TunnelOpen)
		}
		return nil

	case *v1.ServerMessage_TunnelData:
		// Written inline, deliberately: this is the ordering guarantee. Handing
		// each frame to a goroutine would let two writes race and interleave
		// halves of a packet into sshd.
		if tunnels := c.tunnels.Load(); tunnels != nil {
			tunnels.Data(ctx, payload.TunnelData)
		}
		return nil

	case *v1.ServerMessage_TunnelClose:
		if tunnels := c.tunnels.Load(); tunnels != nil {
			tunnels.Close(ctx, payload.TunnelClose)
		}
		return nil

	default:
		// Commands, files, deployments, log streaming: Phase 2.
		//
		// Counted and logged rather than silently ignored. An agent that
		// quietly drops a command the server believes it dispatched leaves an
		// operator watching a queue that never moves, with nothing anywhere to
		// say why.
		c.unhandled.Add(1)
		c.log.Warn("this agent build cannot handle a message the server sent",
			slog.String("payload", fmt.Sprintf("%T", msg.GetPayload())),
			slog.Uint64("seq", msg.GetSeq()))
		return nil
	}
}

// disconnect turns a deliberate server-side close into the right outcome.
func (c *Client) disconnect(d *v1.Disconnect) error {
	stop := &serverStop{
		reason:     d.GetReason(),
		message:    d.GetMessage(),
		retryAfter: time.Duration(d.GetRetryAfterS()) * time.Second,
	}

	switch d.GetReason() {
	case v1.Disconnect_REASON_DEVICE_REVOKED:
		c.log.Error("the server revoked this device's enrollment; stopping",
			slog.String("message", d.GetMessage()))
		return fmt.Errorf("%w: %s", ErrRevoked, d.GetMessage())

	case v1.Disconnect_REASON_DEVICE_DELETED:
		c.log.Error("this device has been decommissioned; stopping",
			slog.String("message", d.GetMessage()))
		return fmt.Errorf("%w: %s", ErrDeleted, d.GetMessage())

	case v1.Disconnect_REASON_UNAUTHENTICATED:
		// Not terminal: clock skew, or a certificate rotated mid-flight. Both
		// resolve without an operator, and stopping would need one.
		c.log.Error("the server rejected this device's credentials",
			slog.String("message", d.GetMessage()))
		return stop

	default:
		c.log.Info("the server closed the stream",
			slog.String("reason", d.GetReason().String()),
			slog.String("message", d.GetMessage()))
		return stop
	}
}

// heartbeat builds the periodic liveness message.
func (c *Client) heartbeat() *v1.AgentMessage {
	var uptime uint64
	if booted := c.bootedAt.Load(); booted != nil && !booted.IsZero() {
		if d := c.now().Sub(*booted); d > 0 {
			uptime = uint64(d.Seconds())
		}
	}

	return &v1.AgentMessage{
		SentAt: timestamppb.New(c.now()),
		Payload: &v1.AgentMessage_Heartbeat{Heartbeat: &v1.Heartbeat{
			UptimeSeconds:       uptime,
			SpooledMessageCount: uint32(c.spool.Len()),
		}},
	}
}
