package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	ephttp "github.com/visionify/edgepilot/apps/server/internal/transport/http"
)

// The hub is exercised over a real WebSocket against a real listener. What is
// worth testing is the authorisation at subscribe time and the behaviour of a
// client that stops reading — neither of which a direct call to Publish would
// exercise.

// wsFixture starts a listener so a real upgrade can happen; httptest's
// ResponseRecorder cannot be hijacked.
type wsFixture struct {
	*fixture
	http *httptest.Server
}

func newWSFixture(t *testing.T) *wsFixture {
	t.Helper()
	f := newFixture(t)
	srv := httptest.NewServer(f.server.Handler())
	t.Cleanup(srv.Close)
	return &wsFixture{fixture: f, http: srv}
}

// connect opens an authenticated WebSocket.
func (f *wsFixture) connect(t *testing.T, accessToken string) *websocket.Conn {
	t.Helper()

	url := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/v1/ws"
	conn, _, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + accessToken}},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func sendJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readFrame reads one message, failing the test if none arrives.
func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode frame %q: %v", data, err)
	}
	return out
}

// readUntil reads frames until one has the wanted type.
func readUntil(t *testing.T, conn *websocket.Conn, wantType string) map[string]any {
	t.Helper()
	for range 10 {
		frame := readFrame(t, conn)
		if frame["type"] == wantType {
			return frame
		}
	}
	t.Fatalf("no %q frame arrived", wantType)
	return nil
}

func TestADashboardReceivesTheEventsItSubscribedTo(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	accessToken := f.signIn(t)
	conn := f.connect(t, accessToken)

	sendJSON(t, conn, map[string]any{
		"type": "subscribe", "topics": []string{ephttp.TopicFleet},
	})
	confirmed := readUntil(t, conn, "subscribed")
	topics, _ := confirmed["topics"].([]any)
	if len(topics) != 1 || topics[0] != ephttp.TopicFleet {
		t.Fatalf("subscribed to %v, want the fleet topic", confirmed["topics"])
	}

	deviceID := uuid.New()
	f.server.Hub().Publish(f.org.ID, ephttp.TopicFleet, ephttp.Event{
		Type: "device.status", DeviceID: deviceID.String(),
	})

	event := readUntil(t, conn, "device.status")
	if event["device_id"] != deviceID.String() {
		t.Errorf("device_id = %v, want %s", event["device_id"], deviceID)
	}
	if event["at"] == nil {
		t.Error("the event carries no timestamp, so a client cannot order it")
	}
}

// A device id is not a secret: it appears in URLs, in logs, and in the
// dashboard's own state. Treating it as one would make "unguessable" the access
// control.
func TestEventsDoNotCrossOrganizations(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	accessToken := f.signIn(t)
	conn := f.connect(t, accessToken)

	sendJSON(t, conn, map[string]any{
		"type": "subscribe", "topics": []string{ephttp.TopicFleet},
	})
	readUntil(t, conn, "subscribed")

	// An event for a different tenant, on the same topic name.
	f.server.Hub().Publish(uuid.New(), ephttp.TopicFleet, ephttp.Event{
		Type: "device.status", DeviceID: uuid.New().String(),
	})
	// And one for this tenant, so the test has something to wait for rather
	// than a sleep.
	mine := uuid.New()
	f.server.Hub().Publish(f.org.ID, ephttp.TopicFleet, ephttp.Event{
		Type: "device.status", DeviceID: mine.String(),
	})

	event := readUntil(t, conn, "device.status")
	if event["device_id"] != mine.String() {
		t.Errorf("received another organization's event: %v", event)
	}
}

// A subscription the caller may not have is refused and said so. Silently
// dropping it would leave a dashboard that believes it is subscribed and
// receives nothing — indistinguishable from a quiet fleet.
func TestUnauthorisedTopicsAreRefusedAudibly(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)

	// A viewer holds device:read but not file:read.
	viewer := f.createUser(t, "viewer@acme.test", domain.RoleViewer)
	accessToken := f.signInAs(t, viewer)
	conn := f.connect(t, accessToken)

	sendJSON(t, conn, map[string]any{"type": "subscribe", "topics": []string{
		ephttp.TopicFleet,
		"file:" + uuid.New().String(),
		"nonsense:topic",
	}})

	refused := readUntil(t, conn, "subscription.refused")
	topics, _ := refused["topics"].([]any)
	if len(topics) != 2 {
		t.Errorf("refused %v, want the file topic and the unknown one", refused["topics"])
	}

	confirmed := readUntil(t, conn, "subscribed")
	allowed, _ := confirmed["topics"].([]any)
	if len(allowed) != 1 || allowed[0] != ephttp.TopicFleet {
		t.Errorf("subscribed to %v, want only the fleet topic", confirmed["topics"])
	}
}

// A browser cannot set headers on a WebSocket upgrade, so the token travels in
// the query for this route. Shipping a server without that support left every
// dashboard's live updates permanently failing while the rest of the app
// worked — the kind of break that is invisible until someone opens a console.
func TestTheStreamAcceptsTheTokenInTheQuery(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	accessToken := f.signIn(t)

	url := "ws" + strings.TrimPrefix(f.http.URL, "http") +
		"/api/v1/ws?access_token=" + accessToken

	// No Authorization header: exactly what a browser sends.
	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("upgrade with a query-parameter token: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	sendJSON(t, conn, map[string]any{"type": "subscribe", "topics": []string{ephttp.TopicFleet}})
	readUntil(t, conn, "subscribed")
}

// The query parameter is a concession to the browser, not a second front door:
// a bad token must be refused there exactly as it is in the header.
func TestAQueryTokenIsStillVerified(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	url := "ws" + strings.TrimPrefix(f.http.URL, "http") +
		"/api/v1/ws?access_token=not-a-real-token"

	//nolint:bodyclose // Dial returns a nil response body on failure
	if _, _, err := websocket.Dial(t.Context(), url, nil); err == nil {
		t.Fatal("an upgrade with a forged query token was accepted")
	}
}

func TestTheStreamRequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	url := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/v1/ws"

	//nolint:bodyclose // Dial returns a nil response body on failure
	if _, _, err := websocket.Dial(t.Context(), url, nil); err == nil {
		t.Fatal("an unauthenticated upgrade was accepted")
	}
}

// A browser tab that stops reading must be disconnected rather than buffered:
// holding frames for it is how one wedged tab grows the server's memory.
func TestAClientThatStopsReadingIsDisconnected(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	accessToken := f.signIn(t)
	conn := f.connect(t, accessToken)

	sendJSON(t, conn, map[string]any{
		"type": "subscribe", "topics": []string{ephttp.TopicFleet},
	})
	readUntil(t, conn, "subscribed")

	// Far more than the per-client queue holds, with nothing reading them.
	for range 500 {
		f.server.Hub().Publish(f.org.ID, ephttp.TopicFleet, ephttp.Event{
			Type: "device.status", DeviceID: uuid.New().String(),
		})
	}

	// The connection is closed by the server; reading eventually fails.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, _, err := conn.Read(ctx)
		cancel()
		if err != nil {
			return // disconnected, as intended
		}
	}
	t.Error("a client that fell behind was never disconnected")
}

func TestTheHubTracksConnections(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	accessToken := f.signIn(t)

	if got := f.server.Hub().Connected(); got != 0 {
		t.Fatalf("connected = %d before any dashboard attached", got)
	}

	conn := f.connect(t, accessToken)
	sendJSON(t, conn, map[string]any{"type": "subscribe", "topics": []string{ephttp.TopicFleet}})
	readUntil(t, conn, "subscribed")

	if got := f.server.Hub().Connected(); got != 1 {
		t.Errorf("connected = %d with one dashboard attached, want 1", got)
	}

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.server.Hub().Connected() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a closed connection was never unregistered; the hub leaks clients")
}

func TestAPingIsAnswered(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	conn := f.connect(t, f.signIn(t))

	sendJSON(t, conn, map[string]any{"type": "ping"})
	readUntil(t, conn, "pong")
}

func TestAnUnknownMessageTypeIsReported(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t)
	conn := f.connect(t, f.signIn(t))

	sendJSON(t, conn, map[string]any{"type": "teleport"})
	frame := readUntil(t, conn, "error")
	if msg, _ := frame["message"].(string); !strings.Contains(msg, "teleport") {
		t.Errorf("the error does not name the message type: %v", frame)
	}
}
