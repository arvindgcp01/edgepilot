package client_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/agent/internal/backoff"
	"github.com/visionify/edgepilot/apps/agent/internal/client"
	"github.com/visionify/edgepilot/apps/agent/internal/collect"
	"github.com/visionify/edgepilot/apps/agent/internal/identity"
	"github.com/visionify/edgepilot/apps/agent/internal/spool"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// The agent is driven against a real gRPC server over bufconn, implementing the
// device-plane contract. What is under test is the agent's behaviour on a live
// stream — the handshake, reconnect, resume, spool draining, disconnect
// handling — and none of that is exercised by calling methods directly.
//
// The server here is the *protocol*, not a stand-in for EdgePilot's server: the
// two meet in the end-to-end test that runs once both binaries exist. mTLS is
// not involved because the server's own tests cover it against real sockets;
// here the transport is insecure so the stream logic is what is being measured.

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeServer is a scriptable implementation of AgentService.
type fakeServer struct {
	v1.UnimplementedAgentServiceServer

	mu sync.Mutex

	// received holds every agent message, in order.
	received []*v1.AgentMessage
	// connections counts streams that got past Hello.
	connections int
	// hellos holds each Hello, so resume behaviour can be asserted across
	// reconnects.
	hellos []*v1.Hello

	// onConnect scripts each connection by index. Returning an error ends the
	// stream with it.
	onConnect func(n int, stream v1.AgentService_ConnectServer) error

	// welcome overrides the default Welcome.
	welcome func(n int, hello *v1.Hello) *v1.Welcome

	notify chan struct{}
}

func newFakeServer() *fakeServer {
	return &fakeServer{notify: make(chan struct{}, 1024)}
}

func (s *fakeServer) Connect(stream v1.AgentService_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errors.New("the first message must be Hello")
	}

	s.mu.Lock()
	s.connections++
	n := s.connections
	s.hellos = append(s.hellos, hello)
	s.mu.Unlock()
	s.ping()

	welcome := &v1.Welcome{
		ServerTime: timestamppb.Now(),
		DeviceId:   uuid.New().String(),
		Approved:   true,
		Config: &v1.ConfigUpdate{
			HeartbeatIntervalS: 1,
			MetricsIntervalS:   1,
			InventoryIntervalS: 3600,
		},
	}
	if s.welcome != nil {
		welcome = s.welcome(n, hello)
	}
	if welcome != nil {
		if err := stream.Send(&v1.ServerMessage{
			Seq: 1, SentAt: timestamppb.Now(),
			Payload: &v1.ServerMessage_Welcome{Welcome: welcome},
		}); err != nil {
			return err
		}
	}

	if s.onConnect != nil {
		return s.onConnect(n, stream)
	}
	return s.readUntilDone(stream)
}

// readUntilDone consumes agent messages until the stream ends.
func (s *fakeServer) readUntilDone(stream v1.AgentService_ConnectServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			// The agent hung up. For a gRPC handler that is an orderly end, not
			// a fault to report.
			return nil //nolint:nilerr // the client closing the stream ends the handler
		}
		s.record(msg)
	}
}

func (s *fakeServer) record(msg *v1.AgentMessage) {
	s.mu.Lock()
	s.received = append(s.received, msg)
	s.mu.Unlock()
	s.ping()
}

func (s *fakeServer) ping() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *fakeServer) messages() []*v1.AgentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*v1.AgentMessage(nil), s.received...)
}

func (s *fakeServer) connectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

func (s *fakeServer) helloAt(i int) *v1.Hello {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.hellos) {
		return nil
	}
	return s.hellos[i]
}

// fixture wires an agent to a fake server.
type fixture struct {
	server *fakeServer
	client *client.Client
	spool  *spool.Spool
}

func newFixture(t *testing.T, srv *fakeServer, tune func(*client.Options)) *fixture {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	v1.RegisterAgentServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	opts := client.Options{
		Identity: testIdentity(t),
		Endpoint: "passthrough:///bufnet",
		Spool:    sp,
		Version:  "1.0.0-test",
		BootID:   uuid.New().String(),
		Logger:   quiet(),
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			// Overrides the mTLS credentials the client sets first.
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NewBackoff: func() *backoff.Backoff {
			// Milliseconds, so a reconnect test finishes in a test's lifetime
			// rather than in five minutes.
			return &backoff.Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}
		},
	}
	if tune != nil {
		tune(&opts)
	}

	c, err := client.New(opts)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return &fixture{server: srv, client: c, spool: sp}
}

// testIdentity builds a complete identity: a real key, a real certificate from a
// throwaway CA, and a real CA bundle.
//
// The certificate is not presented on the wire here — the test transport is
// insecure, so the stream logic rather than the handshake is what is being
// measured — but the client builds its TLS credentials from these fields before
// it dials, and it is right to refuse an identity that cannot produce a usable
// keypair. Stubbing them out would mean testing a code path production never
// takes.
func testIdentity(t *testing.T) *identity.Identity {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "EdgePilot Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := identity.NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: deviceID.String()},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caCert, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}

	return &identity.Identity{
		DeviceID: deviceID, OrgID: uuid.New(), Key: key,
		CertificatePEM:      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		CABundlePEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		NotAfter:            time.Now().Add(90 * 24 * time.Hour),
		Approved:            true,
		DevicePlaneEndpoint: "passthrough:///bufnet",
	}
}

// run starts the client and returns a stop function.
func (f *fixture) run(t *testing.T) (context.Context, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.client.Run(ctx) }()

	return ctx, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			return errors.New("Run did not return after its context was cancelled")
		}
	}
}

func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

// --- the handshake ---------------------------------------------------------

func TestTheAgentIntroducesItselfAndStartsBeating(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	f := newFixture(t, srv, nil)
	_, stop := f.run(t)

	waitFor(t, 10*time.Second, "the first heartbeat", func() bool {
		for _, msg := range srv.messages() {
			if msg.GetHeartbeat() != nil {
				return true
			}
		}
		return false
	})

	hello := srv.helloAt(0)
	if hello == nil {
		t.Fatal("no Hello was received")
	}
	if hello.GetAgentVersion() != "1.0.0-test" {
		t.Errorf("agent version = %q, want 1.0.0-test", hello.GetAgentVersion())
	}
	if hello.GetBootId() == "" {
		t.Error("Hello carried no boot id; the server cannot tell a restart from a reconnect")
	}
	if !f.client.Connected() {
		t.Error("the client does not consider itself connected")
	}
	if !f.client.Approved() {
		t.Error("the Welcome said approved, but the client did not record it")
	}

	if err := stop(); err != nil {
		t.Errorf("stop: %v", err)
	}
	if f.client.Connected() {
		t.Error("the client still reports connected after shutting down")
	}
}

// A heartbeat is what sets the server's presence key. Waiting a full interval
// before the first one would leave a freshly connected device looking offline.
func TestTheFirstHeartbeatDoesNotWaitForTheTicker(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.welcome = func(int, *v1.Hello) *v1.Welcome {
		return &v1.Welcome{
			ServerTime: timestamppb.Now(), Approved: true,
			// A long interval: any heartbeat that arrives promptly came from
			// the immediate send rather than from the ticker.
			Config: &v1.ConfigUpdate{HeartbeatIntervalS: 3600},
		}
	}
	f := newFixture(t, srv, nil)
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 5*time.Second, "an immediate heartbeat", func() bool {
		for _, msg := range srv.messages() {
			if msg.GetHeartbeat() != nil {
				return true
			}
		}
		return false
	})
}

// The server's policy has to take effect, or a fleet-wide interval change would
// do nothing until every agent restarted.
func TestServerPolicyIsApplied(t *testing.T) {
	t.Parallel()

	applied := make(chan client.Policy, 4)
	srv := newFakeServer()
	srv.welcome = func(int, *v1.Hello) *v1.Welcome {
		return &v1.Welcome{
			ServerTime: timestamppb.Now(), Approved: false,
			Config: &v1.ConfigUpdate{
				HeartbeatIntervalS:    7,
				MetricsIntervalS:      11,
				AllowCustomScripts:    true,
				EnabledCommandSlugs:   []string{"restart-service", "clear-cache"},
				MaxConcurrentCommands: 2,
				LogLevel:              "debug",
			},
		}
	}
	f := newFixture(t, srv, func(o *client.Options) {
		o.OnPolicy = func(p client.Policy) {
			select {
			case applied <- p:
			default:
			}
		}
	})
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	select {
	case got := <-applied:
		if got.HeartbeatInterval != 7*time.Second {
			t.Errorf("heartbeat interval = %s, want 7s", got.HeartbeatInterval)
		}
		if got.MetricsInterval != 11*time.Second {
			t.Errorf("metrics interval = %s, want 11s", got.MetricsInterval)
		}
		if !got.AllowCustomScripts {
			t.Error("allow_custom_scripts was not applied")
		}
		if len(got.EnabledCommandSlugs) != 2 {
			t.Errorf("enabled commands = %v, want two", got.EnabledCommandSlugs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the policy was never applied")
	}

	// A device the server has not approved must know it, so it can refuse
	// commands locally rather than relying on the server never sending one.
	if f.client.Approved() {
		t.Error("the Welcome said this device is not approved")
	}
}

// --- liveness --------------------------------------------------------------

func TestPingsAreAnswered(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(_ int, stream v1.AgentService_ConnectServer) error {
		if err := stream.Send(&v1.ServerMessage{
			Seq: 2, SentAt: timestamppb.Now(),
			Payload: &v1.ServerMessage_Ping{Ping: &v1.Ping{Nonce: 4242}},
		}); err != nil {
			return err
		}
		return srv.readUntilDone(stream)
	}

	f := newFixture(t, srv, nil)
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 10*time.Second, "a pong carrying the nonce", func() bool {
		for _, msg := range srv.messages() {
			if pong := msg.GetPong(); pong != nil && pong.GetNonce() == 4242 {
				return true
			}
		}
		return false
	})
}

// --- the spool -------------------------------------------------------------

// The path that makes an outage survivable: work done while disconnected must
// reach the server when the link returns, and must not be dropped until the
// server says it has it.
func TestBufferedWorkIsDeliveredAndThenCommitted(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(_ int, stream v1.AgentService_ConnectServer) error {
		// Acknowledge every durable message, as the real server does.
		for {
			msg, err := stream.Recv()
			if err != nil {
				return nil //nolint:nilerr // the client closing the stream ends the handler
			}
			srv.record(msg)
			if msg.GetEvent() != nil {
				if err := stream.Send(&v1.ServerMessage{
					Seq: 10, SentAt: timestamppb.Now(),
					Payload: &v1.ServerMessage_Ack{Ack: &v1.Ack{AckedSeq: msg.GetSeq()}},
				}); err != nil {
					return nil //nolint:nilerr // a closed stream ends the handler
				}
			}
		}
	}

	f := newFixture(t, srv, nil)

	// Buffered before the client is even started, as it would be during an
	// outage.
	for i := range 3 {
		err := f.client.Report(client.KindEvent, &v1.AgentMessage{
			Payload: &v1.AgentMessage_Event{Event: &v1.DeviceEvent{
				OccurredAt: timestamppb.Now(),
				EventType:  "agent.started",
				Severity:   "info",
				Message:    "buffered while offline",
			}},
		})
		if err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
	}
	if f.spool.Len() != 3 {
		t.Fatalf("spooled %d messages, want 3", f.spool.Len())
	}

	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 15*time.Second, "the buffered events to be delivered", func() bool {
		events := 0
		for _, msg := range srv.messages() {
			if msg.GetEvent() != nil {
				events++
			}
		}
		return events >= 3
	})

	// And then released, because the server acknowledged them.
	waitFor(t, 15*time.Second, "the spool to be committed", func() bool {
		return f.spool.Len() == 0
	})
}

// The inverse property, and the more important one: an unacknowledged message
// stays buffered. A record removed on send and never acknowledged is a lost
// command result.
func TestUnacknowledgedWorkStaysBuffered(t *testing.T) {
	t.Parallel()

	srv := newFakeServer() // never acknowledges anything
	f := newFixture(t, srv, nil)

	if err := f.client.Report(client.KindCommandResult, &v1.AgentMessage{
		Payload: &v1.AgentMessage_Event{Event: &v1.DeviceEvent{
			OccurredAt: timestamppb.Now(), EventType: "command.finished", Severity: "info",
		}},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}

	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 10*time.Second, "the message to be sent", func() bool {
		for _, msg := range srv.messages() {
			if msg.GetEvent() != nil {
				return true
			}
		}
		return false
	})

	// Give the client every chance to commit it wrongly.
	time.Sleep(500 * time.Millisecond)
	if f.spool.Len() != 1 {
		t.Errorf("spool holds %d records; an unacknowledged message must not be "+
			"discarded", f.spool.Len())
	}
}

// The spool sequence has to survive onto the wire, or the server's ack — which
// names a sequence — cannot be mapped back to a buffered record.
func TestSpooledMessagesCarryTheirSequence(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	f := newFixture(t, srv, nil)

	for range 3 {
		if err := f.client.Report(client.KindEvent, &v1.AgentMessage{
			Payload: &v1.AgentMessage_Event{Event: &v1.DeviceEvent{
				OccurredAt: timestamppb.Now(), EventType: "agent.started", Severity: "info",
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 10*time.Second, "three events", func() bool {
		n := 0
		for _, msg := range srv.messages() {
			if msg.GetEvent() != nil {
				n++
			}
		}
		return n >= 3
	})

	var seqs []uint64
	for _, msg := range srv.messages() {
		if msg.GetEvent() != nil {
			seqs = append(seqs, msg.GetSeq())
		}
	}
	for i, seq := range seqs[:3] {
		if seq != uint64(i+1) {
			t.Errorf("event %d arrived with seq %d, want %d", i, seq, i+1)
		}
	}
}

// --- reconnecting ----------------------------------------------------------

func TestADroppedStreamIsReestablished(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(n int, stream v1.AgentService_ConnectServer) error {
		if n <= 2 {
			// Drop the first two connections the moment they are established.
			return errors.New("go away")
		}
		return srv.readUntilDone(stream)
	}

	f := newFixture(t, srv, nil)
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 20*time.Second, "a third connection", func() bool {
		return srv.connectionCount() >= 3
	})
	waitFor(t, 10*time.Second, "the client to settle", func() bool {
		return f.client.Connected()
	})
}

// Resume is what keeps a reconnect from being a restart: the agent tells the
// server the highest sequence it processed, so the server continues rather than
// replaying from the beginning.
func TestTheAgentResumesWhereItLeftOff(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(n int, stream v1.AgentService_ConnectServer) error {
		if n == 1 {
			// Send a config carrying a high sequence, then drop the stream.
			if err := stream.Send(&v1.ServerMessage{
				Seq: 77, SentAt: timestamppb.Now(),
				Payload: &v1.ServerMessage_Config{Config: &v1.ConfigUpdate{
					HeartbeatIntervalS: 1,
				}},
			}); err != nil {
				return err
			}
			time.Sleep(100 * time.Millisecond)
			return errors.New("connection lost")
		}
		return srv.readUntilDone(stream)
	}

	f := newFixture(t, srv, nil)
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 20*time.Second, "a second connection", func() bool {
		return srv.helloAt(1) != nil
	})

	if got := srv.helloAt(1).GetLastSeqSeen(); got != 77 {
		t.Errorf("second Hello reported last_seq_seen = %d, want 77 — "+
			"without it the server replays from the beginning", got)
	}
}

// A backlog must be reported at connect, so the server can size its ingest
// window rather than being surprised by a replay.
func TestHelloReportsTheBacklog(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	f := newFixture(t, srv, nil)

	for range 5 {
		if err := f.client.Report(client.KindEvent, &v1.AgentMessage{
			Payload: &v1.AgentMessage_Event{Event: &v1.DeviceEvent{
				OccurredAt: timestamppb.Now(), EventType: "agent.started", Severity: "info",
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 10*time.Second, "the first Hello", func() bool { return srv.helloAt(0) != nil })
	if got := srv.helloAt(0).GetSpooledMessages(); got != 5 {
		t.Errorf("Hello reported %d spooled messages, want 5", got)
	}
}

// --- being told to stop ----------------------------------------------------

func TestARevokedDeviceStopsRatherThanRetrying(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(_ int, stream v1.AgentService_ConnectServer) error {
		return stream.Send(&v1.ServerMessage{
			Seq: 2, SentAt: timestamppb.Now(),
			Payload: &v1.ServerMessage_Disconnect{Disconnect: &v1.Disconnect{
				Reason:  v1.Disconnect_REASON_DEVICE_REVOKED,
				Message: "this device was revoked by an administrator",
			}},
		})
	}

	f := newFixture(t, srv, nil)
	done := make(chan error, 1)
	go func() { done <- f.client.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, client.ErrRevoked) {
			t.Fatalf("Run returned %v, want ErrRevoked", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a revoked device kept retrying instead of stopping")
	}

	if n := srv.connectionCount(); n != 1 {
		t.Errorf("the agent connected %d times after being revoked, want 1", n)
	}
}

func TestADecommissionedDeviceStops(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(_ int, stream v1.AgentService_ConnectServer) error {
		return stream.Send(&v1.ServerMessage{
			Seq: 2, SentAt: timestamppb.Now(),
			Payload: &v1.ServerMessage_Disconnect{Disconnect: &v1.Disconnect{
				Reason:  v1.Disconnect_REASON_DEVICE_DELETED,
				Message: "decommissioned",
			}},
		})
	}

	f := newFixture(t, srv, nil)
	done := make(chan error, 1)
	go func() { done <- f.client.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, client.ErrDeleted) {
			t.Fatalf("Run returned %v, want ErrDeleted", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a decommissioned device kept retrying instead of stopping")
	}
}

// A draining replica is not an outage. An agent that backs off exponentially
// here turns a rolling deploy into minutes of fleet-wide offline time.
func TestADrainingReplicaIsReconnectedToPromptly(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(n int, stream v1.AgentService_ConnectServer) error {
		if n == 1 {
			return stream.Send(&v1.ServerMessage{
				Seq: 2, SentAt: timestamppb.Now(),
				Payload: &v1.ServerMessage_Disconnect{Disconnect: &v1.Disconnect{
					Reason:  v1.Disconnect_REASON_SHUTTING_DOWN,
					Message: "this replica is draining; reconnect",
				}},
			})
		}
		return srv.readUntilDone(stream)
	}

	f := newFixture(t, srv, func(o *client.Options) {
		// The ordinary backoff is made slow, so a prompt reconnect can only
		// come from the drain-specific path.
		o.NewBackoff = func() *backoff.Backoff {
			return &backoff.Backoff{Initial: time.Minute, Max: time.Minute}
		}
	})
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	start := time.Now()
	waitFor(t, 20*time.Second, "a prompt reconnect", func() bool {
		return srv.connectionCount() >= 2
	})
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("reconnected after %s; a draining replica should be "+
			"reconnected to within a short jittered window", elapsed)
	}
}

// An expired certificate cannot complete the handshake, so dialling is pointless
// and retrying forever hides the one thing an operator needs to see.
func TestAnExpiredCertificateStopsTheAgent(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	f := newFixture(t, srv, func(o *client.Options) {
		o.Identity.NotAfter = time.Now().Add(-time.Hour)
	})

	done := make(chan error, 1)
	go func() { done <- f.client.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, client.ErrCertificateExpired) {
			t.Fatalf("Run returned %v, want ErrCertificateExpired", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an agent with an expired certificate kept trying")
	}
	if srv.connectionCount() != 0 {
		t.Error("an agent with an expired certificate should not have dialled at all")
	}
}

// --- messages this build cannot handle -------------------------------------

// An agent that silently drops a command the server believes it dispatched
// leaves an operator watching a queue that never moves, with nothing anywhere
// to say why.
func TestUnsupportedMessagesAreCountedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	srv.onConnect = func(_ int, stream v1.AgentService_ConnectServer) error {
		if err := stream.Send(&v1.ServerMessage{
			Seq: 5, SentAt: timestamppb.Now(),
			Payload: &v1.ServerMessage_Command{Command: &v1.CommandRequest{
				CommandId: uuid.New().String(), Slug: "restart-service",
			}},
		}); err != nil {
			return err
		}
		return srv.readUntilDone(stream)
	}

	f := newFixture(t, srv, nil)
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 10*time.Second, "the message to be counted", func() bool {
		return f.client.Stats().Unhandled >= 1
	})
	// And the sequence still advances, so a reconnect does not replay it.
	if got := f.client.Stats().LastSeqSeen; got < 5 {
		t.Errorf("last_seq_seen = %d, want at least 5", got)
	}
}

// --- construction ----------------------------------------------------------

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sp.Close() }()

	id := testIdentity(t)
	noEndpoint := *id
	noEndpoint.DevicePlaneEndpoint = ""

	cases := map[string]client.Options{
		"no identity": {Spool: sp, BootID: "b"},
		"no spool":    {Identity: id, BootID: "b"},
		"no boot id":  {Identity: id, Spool: sp},
		"no endpoint": {Identity: &noEndpoint, Spool: sp, BootID: "b"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts.Logger = quiet()
			if _, err := client.New(opts); err == nil {
				t.Error("this configuration should have been refused")
			}
		})
	}
}

// The facts collector supplies the hostname and OS version, which change
// without an inventory refresh — so they travel in every Hello.
func TestHelloCarriesTheCurrentHostAndOSVersion(t *testing.T) {
	t.Parallel()

	srv := newFakeServer()
	f := newFixture(t, srv, func(o *client.Options) {
		o.Facts = collect.New()
	})
	_, stop := f.run(t)
	defer func() { _ = stop() }()

	waitFor(t, 10*time.Second, "the first Hello", func() bool { return srv.helloAt(0) != nil })

	hello := srv.helloAt(0)
	if hello.GetHostname() == "" {
		t.Error("Hello carried no hostname")
	}
	if hello.GetOsVersion() == "" {
		t.Error("Hello carried no OS version")
	}
}
