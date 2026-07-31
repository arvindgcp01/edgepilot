package grpc_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/visionify/edgepilot/apps/server/internal/bus"
	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
	epgrpc "github.com/visionify/edgepilot/apps/server/internal/transport/grpc"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// The stream tests run a real gRPC server over bufconn rather than calling the
// handler directly. The behaviour worth testing — displacement, drain, queue
// saturation, teardown ordering — is all in how the goroutines interact with a
// live transport, and a direct call would exercise none of it.
//
// mTLS is not involved: it is covered by auth_test.go against real sockets.
// Here the identity is injected, so these tests are about the stream lifecycle
// and nothing else.

type streamFixture struct {
	manager *epgrpc.Manager
	store   *postgres.Store
	bus     *bus.Bus
	conn    *grpc.ClientConn
	client  v1.AgentServiceClient
	org     *domain.Organization
	server  *grpc.Server
}

// newStreamFixture starts a server whose interceptor injects a fixed identity.
func newStreamFixture(t *testing.T, deviceIdentity func() *ca.Identity) *streamFixture {
	t.Helper()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN, MaxConns: 2, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	r := testsupport.NewRedis(t)
	b, err := bus.New(t.Context(), bus.Options{
		Client: r.Client, Prefix: r.Prefix, ReplicaID: "replica-test",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}

	org, err := store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Acme", Slug: "acme-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	manager := epgrpc.NewManager(slog.New(slog.DiscardHandler))
	svc, err := epgrpc.NewAgentService(epgrpc.ServiceOptions{
		Manager: manager, Store: store, Bus: b,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.ChainStreamInterceptor(
		func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, injectedIdentityStream{
				ServerStream: ss,
				ctx:          epgrpc.WithDeviceIdentity(ss.Context(), deviceIdentity()),
			})
		},
	))
	v1.RegisterAgentServiceServer(server, svc)

	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &streamFixture{
		manager: manager, store: store, bus: b,
		conn: conn, client: v1.NewAgentServiceClient(conn), org: org, server: server,
	}
}

type injectedIdentityStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s injectedIdentityStream) Context() context.Context { return s.ctx }

// enrolDevice registers a device so the handler's lookup succeeds.
func (f *streamFixture) enrolDevice(t *testing.T, serial string, approve bool) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	scoped := domain.WithOrg(t.Context(), f.org.ID)
	if _, err := f.store.Devices().Enroll(scoped, domain.EnrollDeviceInput{
		DeviceID: id, OrgID: f.org.ID, SerialNumber: serial,
		Hostname: "mac-" + serial, AutoApprove: approve,
	}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return id
}

// connect opens a stream and sends Hello, returning the Welcome.
func (f *streamFixture) connect(t *testing.T, ctx context.Context) (v1.AgentService_ConnectClient, *v1.Welcome) {
	t.Helper()

	stream, err := f.client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stream.Send(&v1.AgentMessage{
		Seq: 1,
		Payload: &v1.AgentMessage_Hello{Hello: &v1.Hello{
			AgentVersion: "1.0.0", BootId: uuid.New().String(), Hostname: "mac-1",
		}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv welcome: %v", err)
	}
	welcome := msg.GetWelcome()
	if welcome == nil {
		t.Fatalf("expected Welcome, got %T", msg.GetPayload())
	}
	return stream, welcome
}

// --- the happy path --------------------------------------------------------

func TestConnectHandshake(t *testing.T) {
	t.Parallel()

	var deviceID uuid.UUID
	var orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "HANDSHAKE-1", true)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	stream, welcome := f.connect(t, ctx)
	defer func() { _ = stream.CloseSend() }()

	if welcome.GetDeviceId() != deviceID.String() {
		t.Errorf("welcome device = %s, want %s", welcome.GetDeviceId(), deviceID)
	}
	if !welcome.GetApproved() {
		t.Error("an approved device should be told so")
	}
	if welcome.GetConfig() == nil {
		t.Fatal("welcome must carry the agent policy")
	}
	if welcome.GetConfig().GetHeartbeatIntervalS() == 0 {
		t.Error("the heartbeat interval must be set, or the agent has nothing to follow")
	}

	// The manager now holds the stream, which is how the rest of the server
	// finds this device.
	waitFor(t, 5*time.Second, func() bool { return f.manager.Connected() == 1 })
	if _, ok := f.manager.Get(deviceID); !ok {
		t.Error("the device should be registered with the manager")
	}
}

// A pending device connects and reports, but must be told it is not approved so
// it can refuse anything that arrives anyway.
func TestPendingDeviceIsToldItIsNotApproved(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "PENDING-1", false)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	stream, welcome := f.connect(t, ctx)
	defer func() { _ = stream.CloseSend() }()

	if welcome.GetApproved() {
		t.Error("a pending device must not be told it is approved")
	}
}

// Heartbeats are what keep a device present; presence is what routes commands
// to the right replica.
func TestHeartbeatMarksPresence(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "HEARTBEAT-1", true)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	stream, _ := f.connect(t, ctx)
	defer func() { _ = stream.CloseSend() }()

	if err := stream.Send(&v1.AgentMessage{
		Seq:     2,
		Payload: &v1.AgentMessage_Heartbeat{Heartbeat: &v1.Heartbeat{UptimeSeconds: 3600}},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		present, err := f.bus.IsPresent(ctx, deviceID)
		return err == nil && present
	})

	owner, err := f.bus.OwnerOf(ctx, deviceID)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if owner != "replica-test" {
		t.Errorf("owner = %q, want this replica", owner)
	}
}

// --- refusals --------------------------------------------------------------

// The first message must be Hello, so the agent's version and resume point are
// known before anything else is decided.
func TestFirstMessageMustBeHello(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "NOHELLO-1", true)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	stream, err := f.client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stream.Send(&v1.AgentMessage{
		Seq:     1,
		Payload: &v1.AgentMessage_Heartbeat{Heartbeat: &v1.Heartbeat{}},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("a stream that does not begin with Hello must be refused")
	}
}

// A valid certificate for a device that no longer exists means it was
// decommissioned while the agent was offline. Telling it so beats letting it
// reconnect forever.
func TestDecommissionedDeviceIsRefused(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "GONE-1", true)

	if err := f.store.Devices().Decommission(domain.WithOrg(t.Context(), orgID), deviceID); err != nil {
		t.Fatalf("decommission: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	stream, err := f.client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stream.Send(&v1.AgentMessage{
		Seq:     1,
		Payload: &v1.AgentMessage_Hello{Hello: &v1.Hello{AgentVersion: "1.0.0"}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("a decommissioned device must not get a stream")
	}
	if !strings.Contains(err.Error(), "decommissioned") {
		t.Errorf("the agent should be told why: %v", err)
	}
}

func TestRevokedDeviceIsRefused(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "REVOKED-1", true)

	scoped := domain.WithOrg(t.Context(), orgID)
	if _, err := f.store.Devices().SetEnrollmentState(scoped, deviceID, domain.EnrollmentRevoked); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	stream, err := f.client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stream.Send(&v1.AgentMessage{
		Seq:     1,
		Payload: &v1.AgentMessage_Hello{Hello: &v1.Hello{AgentVersion: "1.0.0"}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("a revoked device must not get a stream")
	}
}

// --- displacement ----------------------------------------------------------

// Displacement is the normal outcome of a network partition, not an edge case:
// a Mac's connection can vanish without the server noticing until a keepalive
// expires, so a reconnecting device may still have an apparently-live stream.
// The newest wins, because it is the one that can actually deliver.
func TestReconnectDisplacesTheOlderStream(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "DISPLACE-1", true)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	first, _ := f.connect(t, ctx)
	waitFor(t, 5*time.Second, func() bool { return f.manager.Connected() == 1 })
	firstStream, _ := f.manager.Get(deviceID)
	firstConnID := firstStream.ConnectionID

	// The same device connects again without the first having closed.
	second, _ := f.connect(t, ctx)
	defer func() { _ = second.CloseSend() }()

	// The old stream is told to reconnect rather than being dropped silently,
	// so its agent does not apply backoff.
	msg, err := first.Recv()
	if err == nil {
		if d := msg.GetDisconnect(); d != nil {
			if d.GetReason() != v1.Disconnect_REASON_REBALANCE {
				t.Errorf("displaced stream got reason %v, want REBALANCE", d.GetReason())
			}
		}
	}

	// Exactly one stream remains, and it is the newer one.
	waitFor(t, 10*time.Second, func() bool { return f.manager.Connected() == 1 })
	current, ok := f.manager.Get(deviceID)
	if !ok {
		t.Fatal("the device should still be registered")
	}
	if current.ConnectionID == firstConnID {
		t.Error("the older connection survived displacement; commands would go to a dead socket")
	}
}

// A displaced stream's teardown runs after the new one registered. An
// unconditional delete would remove the live stream and leave the device
// unreachable.
func TestDisplacedTeardownDoesNotUnregisterTheNewStream(t *testing.T) {
	t.Parallel()

	manager := epgrpc.NewManager(slog.New(slog.DiscardHandler))
	deviceID, orgID := uuid.New(), uuid.New()

	old := epgrpc.NewStream(deviceID, orgID, 8, slog.New(slog.DiscardHandler))
	if err := manager.Register(old); err != nil {
		t.Fatal(err)
	}
	fresh := epgrpc.NewStream(deviceID, orgID, 8, slog.New(slog.DiscardHandler))
	if err := manager.Register(fresh); err != nil {
		t.Fatal(err)
	}

	// The displaced stream's deferred Unregister now runs.
	manager.Unregister(old)

	got, ok := manager.Get(deviceID)
	if !ok {
		t.Fatal("the live stream was unregistered by the displaced one's teardown")
	}
	if got.ConnectionID != fresh.ConnectionID {
		t.Error("the wrong stream survived")
	}
	if manager.Connected() != 1 {
		t.Errorf("connected = %d, want 1", manager.Connected())
	}
}

// --- backpressure ----------------------------------------------------------

// The queue is bounded, so something has to give when it fills. A late ping is
// worth nothing, so it is dropped; anything else has a durable record that
// reconciliation re-delivers, so the stream is ended instead of blocking or
// silently losing a command.
func TestSaturatedQueueDropsPingsAndClosesForAnythingElse(t *testing.T) {
	t.Parallel()

	const queueSize = 4
	s := epgrpc.NewStream(uuid.New(), uuid.New(), queueSize, slog.New(slog.DiscardHandler))

	// Fill it with messages nobody is reading.
	for range queueSize {
		if err := s.Send(&v1.ServerMessage{
			Payload: &v1.ServerMessage_Config{Config: &v1.ConfigUpdate{}},
		}); err != nil {
			t.Fatalf("filling the queue should succeed: %v", err)
		}
	}

	// A ping is dropped, and the stream survives.
	if err := s.Send(&v1.ServerMessage{
		Payload: &v1.ServerMessage_Ping{Ping: &v1.Ping{Nonce: 1}},
	}); err != nil {
		t.Errorf("a ping on a full queue should be dropped, not error: %v", err)
	}
	if s.IsClosed() {
		t.Fatal("dropping a ping must not close the stream")
	}
	if s.Stats().DroppedPings != 1 {
		t.Errorf("dropped pings = %d, want 1", s.Stats().DroppedPings)
	}

	// Anything else ends the stream, so the device reconnects into a known state.
	err := s.Send(&v1.ServerMessage{
		Payload: &v1.ServerMessage_Command{Command: &v1.CommandRequest{Slug: "flush_dns"}},
	})
	if err == nil {
		t.Fatal("a non-perishable message on a full queue must not be silently accepted")
	}
	if !s.IsClosed() {
		t.Error("the stream should have been closed so the device reconnects")
	}
}

// Send must never block: its caller is usually an HTTP handler serving an
// operator, and blocking on an unresponsive Mac would hang the request.
func TestSendNeverBlocks(t *testing.T) {
	t.Parallel()

	s := epgrpc.NewStream(uuid.New(), uuid.New(), 2, slog.New(slog.DiscardHandler))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_ = s.Send(&v1.ServerMessage{
				Payload: &v1.ServerMessage_Config{Config: &v1.ConfigUpdate{}},
			})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked; an unresponsive device would hang the caller")
	}
}

func TestSendAfterCloseIsRefused(t *testing.T) {
	t.Parallel()

	s := epgrpc.NewStream(uuid.New(), uuid.New(), 4, slog.New(slog.DiscardHandler))
	s.Close()

	if err := s.Send(&v1.ServerMessage{
		Payload: &v1.ServerMessage_Ping{Ping: &v1.Ping{}},
	}); err == nil {
		t.Error("sending on a closed stream must fail")
	}

	// Teardown paths run more than once.
	s.Close()
	s.CloseWithReason(v1.Disconnect_REASON_SHUTTING_DOWN, "again")
}

// --- deduplication ---------------------------------------------------------

func TestWithinConnectionDeduplication(t *testing.T) {
	t.Parallel()

	s := epgrpc.NewStream(uuid.New(), uuid.New(), 8, slog.New(slog.DiscardHandler))

	if s.Deduplicate(1) {
		t.Error("the first message must not be treated as a duplicate")
	}
	if !s.Deduplicate(1) {
		t.Error("a replayed sequence must be recognised")
	}
	if s.Deduplicate(2) {
		t.Error("a new sequence must be accepted")
	}
	if !s.Deduplicate(1) {
		t.Error("an older sequence must be recognised as a duplicate")
	}
	// Unsequenced messages cannot be deduplicated and are not required to be.
	if s.Deduplicate(0) {
		t.Error("an unsequenced message must not be dropped")
	}
	if got := s.LastAgentSeq(); got != 2 {
		t.Errorf("last seq = %d, want 2", got)
	}
}

func TestDeduplicateIsRaceFree(t *testing.T) {
	t.Parallel()

	s := epgrpc.NewStream(uuid.New(), uuid.New(), 8, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 1; j <= 200; j++ {
				s.Deduplicate(uint64(i*200 + j))
			}
		}()
	}
	wg.Wait()

	if s.LastAgentSeq() == 0 {
		t.Error("no sequence was recorded")
	}
}

// --- drain -----------------------------------------------------------------

// An agent told REASON_SHUTTING_DOWN reconnects at once; an unexplained
// disconnect triggers exponential backoff. Across a fleet that is the
// difference between a rolling deploy costing seconds and costing minutes.
func TestDrainTellsAgentsToReconnectImmediately(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "DRAIN-1", true)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	stream, _ := f.connect(t, ctx)
	waitFor(t, 5*time.Second, func() bool { return f.manager.Connected() == 1 })

	drainCtx, drainCancel := context.WithTimeout(ctx, 10*time.Second)
	defer drainCancel()

	drained := make(chan error, 1)
	go func() { drained <- f.manager.Drain(drainCtx) }()

	// The agent is told why.
	deadline := time.Now().Add(10 * time.Second)
	var sawDisconnect bool
	for time.Now().Before(deadline) {
		msg, err := stream.Recv()
		if err != nil {
			break
		}
		if d := msg.GetDisconnect(); d != nil {
			sawDisconnect = true
			if d.GetReason() != v1.Disconnect_REASON_SHUTTING_DOWN {
				t.Errorf("drain reason = %v, want SHUTTING_DOWN", d.GetReason())
			}
			break
		}
	}
	if !sawDisconnect {
		t.Error("the agent was disconnected without being told the replica was shutting down")
	}

	select {
	case err := <-drained:
		if err != nil {
			t.Errorf("drain: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("drain did not complete")
	}

	if f.manager.Connected() != 0 {
		t.Errorf("connected = %d after drain, want 0", f.manager.Connected())
	}
}

// Drain must wait for handlers to unwind so presence keys are cleared before
// the process exits — otherwise devices stay marked present on a replica that
// no longer exists, and commands routed there are dropped until the key lapses.
func TestDrainClearsPresence(t *testing.T) {
	t.Parallel()

	var deviceID, orgID uuid.UUID
	f := newStreamFixture(t, func() *ca.Identity {
		return &ca.Identity{DeviceID: deviceID, OrgID: orgID}
	})
	orgID = f.org.ID
	deviceID = f.enrolDevice(t, "DRAINPRESENCE-1", true)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	stream, _ := f.connect(t, ctx)
	defer func() { _ = stream.CloseSend() }()

	waitFor(t, 5*time.Second, func() bool {
		present, err := f.bus.IsPresent(ctx, deviceID)
		return err == nil && present
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 10*time.Second)
	defer drainCancel()
	go func() { _, _ = stream.Recv() }()
	if err := f.manager.Drain(drainCtx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		present, err := f.bus.IsPresent(ctx, deviceID)
		return err == nil && !present
	})
}

// A draining replica must refuse new streams rather than accepting one it is
// about to close.
func TestDrainingReplicaRefusesNewStreams(t *testing.T) {
	t.Parallel()

	manager := epgrpc.NewManager(slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := manager.Drain(ctx); err != nil {
		t.Fatalf("drain of an empty manager should succeed: %v", err)
	}
	if !manager.Draining() {
		t.Error("the manager should report draining")
	}

	s := epgrpc.NewStream(uuid.New(), uuid.New(), 8, slog.New(slog.DiscardHandler))
	if err := manager.Register(s); err == nil {
		t.Error("a draining replica must refuse new streams")
	}
}

// --- routing ---------------------------------------------------------------

func TestSendToUnconnectedDevice(t *testing.T) {
	t.Parallel()

	manager := epgrpc.NewManager(slog.New(slog.DiscardHandler))

	// The caller is expected to fall back to the presence map, so this must be
	// a distinguishable error rather than a silent success.
	err := manager.Send(uuid.New(), &v1.ServerMessage{
		Payload: &v1.ServerMessage_Ping{Ping: &v1.Ping{}},
	})
	if err == nil {
		t.Fatal("sending to an unconnected device must fail")
	}
	if manager.Disconnect(uuid.New(), v1.Disconnect_REASON_DEVICE_REVOKED, "gone") {
		t.Error("disconnecting an unconnected device should report false")
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}
