package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
	"github.com/visionify/edgepilot/apps/server/internal/service/screen"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// PresenceRecorder receives liveness signals from streams.
//
// Declared here and satisfied by worker.PresenceFlusher, so the transport does
// not depend on the worker package. Heartbeats land in memory and are flushed
// to PostgreSQL in batches — writing last_seen_at per heartbeat would be 10,000
// row updates every 15 seconds against the table the dashboard reads most.
type PresenceRecorder interface {
	Record(deviceID uuid.UUID, at time.Time)
}

// AgentService implements the device plane.
type AgentService struct {
	v1.UnimplementedAgentServiceServer

	manager  *Manager
	store    domain.Store
	bus      *bus.Bus
	presence PresenceRecorder
	tunnels  TunnelRouter
	metrics  MetricsRecorder
	events   EventPublisher
	sealer   *secret.Sealer
	log      *slog.Logger

	policy AgentPolicy
	now    func() time.Time
}

// TunnelRouter receives the device's half of an SSH relay.
//
// Declared here and satisfied by service/tunnel.Registry, for the same reason as
// PresenceRecorder: the dependency runs from the transport to the service, and
// stating the interface where it is used keeps it that way.
type TunnelRouter interface {
	OnOpened(*v1.TunnelOpened)
	OnData(*v1.TunnelData)
	OnClosed(*v1.TunnelClosed)
	CloseDevice(deviceID uuid.UUID)
}

// AgentPolicy is what the server tells an agent to do, sent in Welcome and
// whenever it changes.
type AgentPolicy struct {
	HeartbeatInterval     time.Duration
	MetricsInterval       time.Duration
	InventoryInterval     time.Duration
	MaxConcurrentCommands int
	SpoolMaxBytes         int64
	SigningPublicKeys     [][]byte
	LogLevel              string
}

// ServiceOptions configures the AgentService.
type ServiceOptions struct {
	Manager  *Manager
	Store    domain.Store
	Bus      *bus.Bus
	Presence PresenceRecorder
	Tunnels  TunnelRouter
	// Metrics receives health samples. Optional: without it samples are
	// acknowledged and dropped, which is what this server did before it had
	// a metrics pipeline at all.
	Metrics MetricsRecorder
	// Events fans connect, disconnect and metrics out to dashboards. Optional.
	Events EventPublisher
	// Sealer unseals a device's VNC password so it can be handed to the agent
	// in ConfigUpdate. Optional: without it screen sharing is never configured.
	Sealer *secret.Sealer
	Logger *slog.Logger
	Policy AgentPolicy
	Now    func() time.Time
}

// NewAgentService builds the service.
func NewAgentService(opts ServiceOptions) (*AgentService, error) {
	switch {
	case opts.Manager == nil:
		return nil, errors.New("grpc: Manager is required")
	case opts.Store == nil:
		return nil, errors.New("grpc: Store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Policy.HeartbeatInterval <= 0 {
		opts.Policy.HeartbeatInterval = 15 * time.Second
	}
	if opts.Policy.MetricsInterval <= 0 {
		opts.Policy.MetricsInterval = 30 * time.Second
	}
	if opts.Policy.InventoryInterval <= 0 {
		opts.Policy.InventoryInterval = 6 * time.Hour
	}
	if opts.Policy.MaxConcurrentCommands <= 0 {
		opts.Policy.MaxConcurrentCommands = 4
	}
	if opts.Policy.SpoolMaxBytes <= 0 {
		opts.Policy.SpoolMaxBytes = 64 << 20
	}

	return &AgentService{
		manager:  opts.Manager,
		store:    opts.Store,
		bus:      opts.Bus,
		presence: opts.Presence,
		tunnels:  opts.Tunnels,
		metrics:  opts.Metrics,
		events:   opts.Events,
		sealer:   opts.Sealer,
		log:      opts.Logger,
		policy:   opts.Policy,
		now:      opts.Now,
	}, nil
}

// Connect is the single bidirectional stream every agent holds open.
//
// The device is already authenticated: mTLS verified its certificate and the
// interceptor put the identity in the context. Nothing here re-checks a token,
// because there is no token.
func (s *AgentService) Connect(stream v1.AgentService_ConnectServer) error {
	ctx := stream.Context()

	identity, ok := DeviceIdentityFromContext(ctx)
	if !ok {
		// Only reachable if the interceptor is not installed, which is a wiring
		// bug rather than an attack — but serving it would mean an
		// unauthenticated stream.
		return status.Error(codes.Unauthenticated, "no authenticated device identity")
	}
	if s.manager.Draining() {
		return status.Error(codes.Unavailable, "this replica is draining; reconnect")
	}

	// The first message must be Hello, so the agent's version and resume point
	// are known before anything else is decided.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "the first message must be Hello")
	}

	ep := NewStream(identity.DeviceID, identity.OrgID, DefaultSendQueueSize, s.log)
	ep.AgentVersion = hello.GetAgentVersion()
	ep.BootID = hello.GetBootId()
	ep.RecordHeartbeat()

	if err := s.manager.Register(ep); err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	defer s.manager.Unregister(ep)

	// The device row is the authority on whether this device may receive
	// commands. A pending or revoked device connects and reports, but is
	// dispatched nothing.
	scoped := domain.WithOrg(ctx, identity.OrgID)
	device, err := s.store.Devices().Get(scoped, identity.DeviceID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// A valid certificate for a device that no longer exists: it was
			// decommissioned while this agent was offline. Tell it so, rather
			// than letting it reconnect forever.
			s.log.WarnContext(ctx, "rejecting a stream for a deleted device",
				slog.String("device_id", identity.DeviceID.String()))
			return status.Error(codes.NotFound, "this device has been decommissioned")
		}
		return status.Error(codes.Internal, "could not load the device record")
	}
	if device.EnrollmentState == domain.EnrollmentRevoked ||
		device.EnrollmentState == domain.EnrollmentRejected {
		return status.Error(codes.PermissionDenied, "this device's enrollment has been revoked")
	}

	// A Mac that has been renamed, updated, or moved to another subnet reports
	// the new values here. Until inventory ingest lands this is the only path by
	// which the fleet listing learns about any of it, so a device renamed without
	// re-enrolling would otherwise show its old name indefinitely.
	//
	// Written only when something differs — this runs on every reconnect for
	// every device, and an UPDATE that changes nothing is still a write.
	facts := domain.ReportedFacts{
		Hostname:     hello.GetHostname(),
		OSName:       hello.GetOsName(),
		OSVersion:    hello.GetOsVersion(),
		OSBuild:      hello.GetOsBuild(),
		AgentVersion: hello.GetAgentVersion(),
		LocalIP:      hello.GetLocalIp(),
	}
	if facts.DiffersFrom(device) {
		if refreshErr := s.store.Devices().RefreshReported(scoped, device.ID, facts); refreshErr != nil {
			// Not fatal. The device is connected and manageable; the listing is
			// merely stale, and dropping the stream over cosmetics would be a
			// worse outcome than the stale row.
			s.log.WarnContext(ctx, "could not refresh what this device reports about itself",
				slog.String("device_id", device.ID.String()), slog.Any("error", refreshErr))
		} else {
			s.log.InfoContext(ctx, "a device reported changed facts",
				slog.String("device_id", device.ID.String()),
				slog.String("hostname", facts.Hostname),
				slog.String("previous_hostname", device.Hostname),
				slog.String("os_version", facts.OSVersion))

			// Keep the in-memory copy consistent: Welcome and the policy built
			// below are derived from it, and shipping the pre-update values
			// would contradict what was just written.
			applyReported(device, facts)
		}
	}

	s.log.InfoContext(ctx, "agent connected",
		slog.String("device_id", identity.DeviceID.String()),
		slog.String("org_id", identity.OrgID.String()),
		slog.String("agent_version", ep.AgentVersion),
		slog.String("boot_id", ep.BootID),
		slog.Bool("approved", device.Approved()),
		slog.Uint64("agent_last_seq_seen", hello.GetLastSeqSeen()),
		slog.Uint64("spooled_messages", uint64(hello.GetSpooledMessages())))

	if s.events != nil {
		s.events.PublishDeviceEvent(identity.OrgID, identity.DeviceID, EventDeviceConnected, map[string]any{
			"agent_version": ep.AgentVersion,
			"approved":      device.Approved(),
		})
	}

	// The connection's own context, cancelled when the stream ends by any path.
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	// Bus notifications addressed to this device. Without this the replica
	// holding the stream would never learn that an API request on another
	// replica queued a command — until its reconciliation loop noticed.
	if s.bus != nil {
		cancelSub, subErr := s.bus.SubscribeDevice(connCtx, identity.DeviceID, func(n bus.DeviceNotification) {
			s.handleNotification(connCtx, ep, n)
		})
		if subErr != nil {
			// Not fatal: reconciliation still delivers, just later. Losing the
			// stream over it would be worse than the added latency.
			s.log.WarnContext(ctx, "subscribing to device notifications failed; "+
				"this device's commands will arrive via reconciliation instead",
				slog.String("device_id", identity.DeviceID.String()), slog.Any("error", subErr))
		} else {
			defer cancelSub()
		}
	}

	if err := s.sendWelcome(ctx, ep, device, hello); err != nil {
		return err
	}

	// The receive loop runs in its own goroutine rather than in this one.
	//
	// stream.Recv() blocks until the agent sends something or the RPC ends, and
	// nothing on the server side can interrupt it — not closing our own stream
	// object, not cancelling a context we derived. If this goroutine sat in
	// Recv, a server-initiated close (drain, displacement, revocation) would
	// leave the handler blocked until the agent happened to notice, and a
	// stalled agent would hold it forever. Returning from the handler is what
	// actually ends the RPC, so this goroutine has to stay free to do that.
	sendDone := make(chan struct{})
	presenceDone := make(chan struct{})
	recvDone := make(chan error, 1)

	go func() { defer close(sendDone); s.sendLoop(connCtx, stream, ep) }()
	go func() { defer close(presenceDone); s.presenceLoop(connCtx, ep) }()
	go func() { recvDone <- s.recvLoop(connCtx, stream, ep) }()

	var recvErr error
	select {
	case recvErr = <-recvDone:
		// The agent went away, or the transport failed.

	case <-ep.Closed():
		// We closed it. Give the send loop a moment to put the Disconnect on
		// the wire before returning — an agent told REASON_SHUTTING_DOWN
		// reconnects at once, while one that just sees a dropped connection
		// applies exponential backoff.
		select {
		case <-sendDone:
		case <-time.After(2 * time.Second):
		}

	case <-ctx.Done():
		recvErr = ctx.Err()
	}

	cancelConn()
	ep.Close()

	// Bounded waits. The receive goroutine unblocks only once this handler
	// returns and gRPC tears the stream down, so it is deliberately not waited
	// on here — waiting would deadlock on exactly the case this restructuring
	// exists to fix.
	select {
	case <-sendDone:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-presenceDone:
	case <-time.After(2 * time.Second):
	}

	s.clearPresence(ep)

	// Any tunnel relayed over this stream is now a pipe to nowhere. Closing it
	// here is what turns "the Mac went away" into a disconnect at the operator's
	// SSH client rather than a session that hangs until they notice.
	if s.tunnels != nil {
		s.tunnels.CloseDevice(identity.DeviceID)
	}

	stats := ep.Stats()
	s.log.InfoContext(ctx, "agent disconnected",
		slog.String("device_id", identity.DeviceID.String()),
		slog.Duration("connected_for", s.now().Sub(stats.ConnectedAt)),
		slog.Int64("messages_received", stats.Received),
		slog.Int64("messages_sent", stats.Sent),
		slog.Int64("dropped_pings", stats.DroppedPings),
		slog.Any("error", recvErr))

	if s.events != nil {
		s.events.PublishDeviceEvent(identity.OrgID, identity.DeviceID, EventDeviceDisconnected, nil)
	}

	return recvErr
}

// applyReported mirrors a successful refresh onto the loaded device.
func applyReported(d *domain.Device, f domain.ReportedFacts) {
	if f.Hostname != "" {
		d.Hostname = f.Hostname
	}
	if f.OSVersion != "" {
		d.OSVersion = f.OSVersion
	}
	if f.OSBuild != "" {
		d.OSBuild = f.OSBuild
	}
	if f.AgentVersion != "" {
		d.AgentVersion = f.AgentVersion
	}
	if f.LocalIP != "" {
		d.LocalIP = f.LocalIP
	}
}

// sendWelcome answers Hello.
func (s *AgentService) sendWelcome(ctx context.Context, ep *Stream, device *domain.Device, hello *v1.Hello) error {
	// Ask only for the inventory sections whose checksums disagree with what
	// the agent says it holds. A device that has not changed since its last
	// report sends nothing, which is what keeps a stable fleet quiet.
	requested := s.inventoryToRequest(hello)

	welcome := &v1.ServerMessage{
		SentAt: timestampNow(),
		Payload: &v1.ServerMessage_Welcome{
			Welcome: &v1.Welcome{
				ServerTime: timestampNow(),
				DeviceId:   device.ID.String(),
				OrgId:      device.OrgID.String(),
				// A pending device connects and reports inventory but is
				// dispatched no commands. It is told so explicitly, so it can
				// refuse anything that arrives anyway.
				Approved:         device.Approved(),
				Config:           s.configFor(ctx, device),
				RequestInventory: requested,
				ResumeFromSeq:    hello.GetLastSeqSeen(),
			},
		},
	}
	if err := ep.Send(welcome); err != nil {
		return status.Error(codes.Unavailable, "could not send Welcome")
	}
	return nil
}

// configFor builds the policy pushed to an agent.
func (s *AgentService) configFor(ctx context.Context, device *domain.Device) *v1.ConfigUpdate {
	cfg := &v1.ConfigUpdate{
		AuthorizedKeys:     s.authorizedKeysFor(ctx, device),
		HeartbeatIntervalS: uint32(s.policy.HeartbeatInterval.Seconds()),
		MetricsIntervalS:   uint32(s.policy.MetricsInterval.Seconds()),
		InventoryIntervalS: uint32(s.policy.InventoryInterval.Seconds()),
		// Per-device policy the agent enforces locally. A device with this
		// false refuses a custom script even when the payload carries a valid
		// server signature.
		AllowCustomScripts:    device.AllowCustomScripts,
		MaxConcurrentCommands: uint32(s.policy.MaxConcurrentCommands),
		SpoolMaxBytes:         uint64(s.policy.SpoolMaxBytes),
		SigningPublicKeys:     s.policy.SigningPublicKeys,
		LogLevel:              s.policy.LogLevel,
	}

	// Remote screen. The plaintext VNC password is sent only here, only when
	// the device has screen sharing on and a sealer is configured to unseal it.
	// A failure to unseal is logged, not fatal: the device stays managed and
	// simply un-screenshareable until the next config, which is far better than
	// dropping its stream.
	if device.ScreenSharingEnabled && s.sealer != nil {
		password, err := screen.Open(s.sealer, device.ID, device.VNCPasswordSealed)
		if err != nil {
			s.log.WarnContext(ctx, "could not unseal the VNC password; not configuring screen sharing",
				slog.String("device_id", device.ID.String()), slog.Any("error", err))
		} else if password != "" {
			cfg.ScreenSharing = true
			cfg.VncPassword = password
		}
	}
	return cfg
}

// authorizedKeysFor is the set of operator keys this device should install.
//
// Sent with the policy rather than as its own message, because it *is* policy:
// the agent reconciles whatever arrives here against what is on disk, so adding
// a key to an organization propagates to every Mac in it on the next connect,
// and revoking one removes it the same way. That is the whole point — an
// operator registers a key once, not once per machine.
//
// A failure here returns nil, and nil means "install nothing". That is
// deliberate and it is the safe direction: a device that cannot be told the
// current key set should not keep honouring a stale one it happens to remember.
func (s *AgentService) authorizedKeysFor(ctx context.Context, device *domain.Device) []*v1.AuthorizedKey {
	keys, err := s.store.SSHKeys().ListForOrg(domain.WithOrg(ctx, device.OrgID))
	if err != nil {
		s.log.ErrorContext(ctx, "could not read the organization's SSH keys",
			slog.String("device_id", device.ID.String()), slog.Any("error", err))
		return nil
	}

	now := s.now()
	out := make([]*v1.AuthorizedKey, 0, len(keys))
	for _, k := range keys {
		// Scope 'device' means the key is granted through ssh_key_devices
		// rather than to the whole organization. Until that grant table is
		// wired up, honouring it here would install keys nobody granted.
		if k.Scope != "org" || !k.Active(now) {
			continue
		}

		entry := &v1.AuthorizedKey{
			KeyId:          k.ID.String(),
			PublicKey:      strings.TrimSpace(k.PublicKey),
			TargetUsername: k.TargetUsername,
			// The bastion is the only intended path to these accounts, and it
			// needs none of these. Forwarding through a fleet machine is how a
			// foothold on one Mac becomes a route into the customer's network.
			Options: []string{
				"no-agent-forwarding",
				"no-port-forwarding",
				"no-X11-forwarding",
			},
		}
		if k.ExpiresAt != nil {
			// sshd enforces this itself, so the key stops working even on a Mac
			// that never hears about the revocation.
			entry.ExpiresAt = timestamppb.New(*k.ExpiresAt)
		}
		out = append(out, entry)
	}
	return out
}

// inventoryToRequest returns the sections the agent should re-send.
func (s *AgentService) inventoryToRequest(hello *v1.Hello) []string {
	// Until inventory ingest lands (Phase 2.2) there is nothing stored to
	// compare against, so an agent reporting checksums is asked for nothing and
	// one reporting none is asked for everything it has. Returning a guess
	// would make a stable fleet re-send its whole inventory on every reconnect.
	if len(hello.GetInventoryChecksums()) > 0 {
		return nil
	}
	return nil
}

// recvLoop reads from the agent until the stream ends.
func (s *AgentService) recvLoop(ctx context.Context, stream v1.AgentService_ConnectServer, ep *Stream) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ep.Closed():
			return nil
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The agent closed cleanly.
				return nil
			}
			if ep.IsClosed() {
				// We closed it — displacement, drain, or revocation. Not an
				// error worth surfacing.
				return nil
			}
			return err
		}

		ep.RecordReceived()

		// Within-connection replay, usually from an agent retry loop. Dropping
		// it here saves the work; cross-connection replay is made harmless by
		// idempotent writes instead. See Stream.Deduplicate.
		if ep.Deduplicate(msg.GetSeq()) {
			s.log.DebugContext(ctx, "ignoring a duplicate agent message",
				slog.Uint64("seq", msg.GetSeq()))
			continue
		}

		if err := s.handle(ctx, ep, msg); err != nil {
			// One bad message must not end a stream that is otherwise healthy:
			// a device that reconnects on every malformed payload would spin.
			s.log.WarnContext(ctx, "handling an agent message failed",
				slog.Uint64("seq", msg.GetSeq()), slog.Any("error", err))
		}
	}
}

// handle dispatches one agent message.
func (s *AgentService) handle(ctx context.Context, ep *Stream, msg *v1.AgentMessage) error {
	switch payload := msg.GetPayload().(type) {
	case *v1.AgentMessage_Heartbeat:
		return s.handleHeartbeat(ctx, ep, payload.Heartbeat)

	case *v1.AgentMessage_Metrics:
		return s.handleMetrics(ctx, ep, payload.Metrics)

	case *v1.AgentMessage_Pong:
		ep.RecordHeartbeat()
		return nil

	case *v1.AgentMessage_Hello:
		// Hello is only valid as the first message. A second one means the
		// agent is confused; say so rather than silently re-initialising.
		return errors.New("a second Hello arrived mid-stream")

	case *v1.AgentMessage_Ack:
		// Acknowledgement of a server message. Nothing is retried per-message
		// today — reconciliation is the retry — so this is informational.
		return nil

	case *v1.AgentMessage_Event:
		return s.handleEvent(ctx, ep, payload.Event)

	case *v1.AgentMessage_TunnelOpened:
		if s.tunnels != nil {
			s.tunnels.OnOpened(payload.TunnelOpened)
		}
		return nil

	case *v1.AgentMessage_TunnelData:
		if s.tunnels != nil {
			s.tunnels.OnData(payload.TunnelData)
		}
		return nil

	case *v1.AgentMessage_TunnelClosed:
		if s.tunnels != nil {
			s.tunnels.OnClosed(payload.TunnelClosed)
		}
		return nil

	default:
		// Metrics, inventory, command results, file and tunnel traffic land
		// here until their handlers arrive in Phase 2. Acknowledged and
		// counted, not silently discarded: an agent that sends something this
		// server cannot yet process should not conclude the connection is
		// broken.
		s.log.DebugContext(ctx, "agent message type not yet handled",
			slog.String("type", messageTypeName(msg)),
			slog.String("device_id", ep.DeviceID.String()))
		return nil
	}
}

// handleHeartbeat records liveness.
//
// This is the hot path: 10,000 devices every 15 seconds. It touches Redis once
// and an in-memory map, and never PostgreSQL.
func (s *AgentService) handleHeartbeat(ctx context.Context, ep *Stream, hb *v1.Heartbeat) error {
	ep.RecordHeartbeat()
	now := s.now()

	if s.presence != nil {
		s.presence.Record(ep.DeviceID, now)
	}
	if s.bus != nil {
		if err := s.bus.MarkPresent(ctx, ep.DeviceID); err != nil {
			// Redis being unavailable degrades routing — commands fall back to
			// reconciliation — but the device is still connected and still
			// working. Ending the stream over it would turn a Redis outage into
			// a fleet outage.
			s.log.WarnContext(ctx, "marking presence failed; "+
				"routing for this device will fall back to reconciliation",
				slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
		}
	}

	s.log.DebugContext(ctx, "heartbeat",
		slog.String("device_id", ep.DeviceID.String()),
		slog.Uint64("uptime_seconds", hb.GetUptimeSeconds()),
		slog.Uint64("active_commands", uint64(hb.GetActiveCommandCount())),
		slog.Uint64("spooled", uint64(hb.GetSpooledMessageCount())))
	return nil
}

// handleEvent records a structured agent event.
func (s *AgentService) handleEvent(ctx context.Context, ep *Stream, event *v1.DeviceEvent) error {
	// Persisted in Phase 2.5 with the rest of the log pipeline. Logged now so
	// an agent event is at least visible during bring-up.
	s.log.InfoContext(ctx, "device event",
		slog.String("device_id", ep.DeviceID.String()),
		slog.String("event_type", event.GetEventType()),
		slog.String("severity", event.GetSeverity()),
		slog.String("message", event.GetMessage()))
	return nil
}

// sendLoop writes queued messages to the agent.
//
// One writer, always. gRPC streams are not safe for concurrent Send, so every
// outbound message funnels through this goroutine — which is also what makes
// the bounded queue the single place backpressure is decided.
func (s *AgentService) sendLoop(ctx context.Context, stream v1.AgentService_ConnectServer, ep *Stream) {
	for {
		select {
		case <-ctx.Done():
			return

		case d := <-ep.Disconnects():
			// Best effort. The stream may already be broken, which is exactly
			// when this is most likely to fail and least likely to matter.
			_ = stream.Send(&v1.ServerMessage{
				Seq:     0,
				SentAt:  timestampNow(),
				Payload: &v1.ServerMessage_Disconnect{Disconnect: d},
			})
			return

		case msg, ok := <-ep.Outbound():
			if !ok {
				return
			}
			if err := stream.Send(msg); err != nil {
				s.log.DebugContext(ctx, "sending to the agent failed; closing the stream",
					slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
				ep.Close()
				return
			}

		case <-ep.Closed():
			// Drain any final disconnect that was queued as we closed.
			select {
			case d := <-ep.Disconnects():
				_ = stream.Send(&v1.ServerMessage{
					SentAt:  timestampNow(),
					Payload: &v1.ServerMessage_Disconnect{Disconnect: d},
				})
			default:
			}
			return
		}
	}
}

// presenceLoop refreshes the Redis presence key between heartbeats.
//
// The key's TTL is three heartbeat intervals, so a heartbeat alone would keep
// it alive — but a device that goes quiet without disconnecting (a stalled
// agent, a suspended Mac) would then be marked offline while its socket is
// still open. Refreshing on a timer keeps presence tied to the connection
// rather than to the agent's own liveness, and the heartbeat gap is what the
// sweeper judges instead.
func (s *AgentService) presenceLoop(ctx context.Context, ep *Stream) {
	if s.bus == nil {
		return
	}
	// Comfortably inside the TTL so an ordinary scheduling delay does not let
	// the key lapse.
	interval := bus.PresenceTTL / 3
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Mark present immediately: a device that connects and then goes quiet
	// should still show as connected until the TTL lapses.
	if err := s.bus.MarkPresent(ctx, ep.DeviceID); err != nil {
		s.log.WarnContext(ctx, "initial presence write failed",
			slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ep.Closed():
			return
		case <-ticker.C:
			if err := s.bus.MarkPresent(ctx, ep.DeviceID); err != nil {
				s.log.WarnContext(ctx, "refreshing presence failed",
					slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
			}
		}
	}
}

// clearPresence removes the presence key when a stream ends.
//
// Uses a fresh context: the connection's is already cancelled by the time this
// runs, and a cancelled context would make the cleanup a no-op — leaving the
// device marked present on a replica that no longer holds it.
func (s *AgentService) clearPresence(ep *Stream) {
	if s.bus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.bus.ClearPresence(ctx, ep.DeviceID); err != nil {
		s.log.WarnContext(ctx, "clearing presence failed; "+
			"the key will lapse on its own",
			slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
	}
}

// handleNotification acts on a bus message addressed to this device.
func (s *AgentService) handleNotification(ctx context.Context, ep *Stream, n bus.DeviceNotification) {
	switch n.Kind {
	case bus.KindDisconnect:
		s.log.InfoContext(ctx, "disconnecting a device at the bus's request",
			slog.String("device_id", ep.DeviceID.String()), slog.String("reason", n.Reason))
		ep.CloseWithReason(v1.Disconnect_REASON_DEVICE_REVOKED, n.Reason)

	case bus.KindConfigChanged:
		scoped := domain.WithOrg(ctx, ep.OrgID)
		device, err := s.store.Devices().Get(scoped, ep.DeviceID)
		if err != nil {
			s.log.WarnContext(ctx, "reloading device policy failed",
				slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
			return
		}
		if err := ep.Send(&v1.ServerMessage{
			Payload: &v1.ServerMessage_Config{Config: s.configFor(ctx, device)},
		}); err != nil {
			s.log.WarnContext(ctx, "pushing config failed",
				slog.String("device_id", ep.DeviceID.String()), slog.Any("error", err))
		}

	case bus.KindCommandQueued, bus.KindCommandCancelled:
		// Command dispatch lands in Phase 2.3. The notification is logged now
		// so the routing path is observable during bring-up; the durable
		// commands row means nothing is lost until the handler exists.
		s.log.DebugContext(ctx, "command notification received",
			slog.String("kind", n.Kind),
			slog.String("device_id", ep.DeviceID.String()))
	}
}

func timestampNow() *timestamppb.Timestamp { return timestamppb.Now() }

// messageTypeName names a payload for logging.
func messageTypeName(msg *v1.AgentMessage) string {
	switch msg.GetPayload().(type) {
	case *v1.AgentMessage_Metrics:
		return "metrics"
	case *v1.AgentMessage_Inventory:
		return "inventory"
	case *v1.AgentMessage_CommandProgress:
		return "command_progress"
	case *v1.AgentMessage_CommandResult:
		return "command_result"
	case *v1.AgentMessage_FileChunk:
		return "file_chunk"
	case *v1.AgentMessage_FileListing:
		return "file_listing"
	case *v1.AgentMessage_FileOperationResult:
		return "file_operation_result"
	case *v1.AgentMessage_TunnelOpened:
		return "tunnel_opened"
	case *v1.AgentMessage_TunnelData:
		return "tunnel_data"
	case *v1.AgentMessage_TunnelClosed:
		return "tunnel_closed"
	case *v1.AgentMessage_DeploymentProgress:
		return "deployment_progress"
	case *v1.AgentMessage_LogStreamChunk:
		return "log_stream_chunk"
	case *v1.AgentMessage_CertificateRequest:
		return "certificate_request"
	default:
		return "unknown"
	}
}
