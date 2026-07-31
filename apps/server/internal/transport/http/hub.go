package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// Hub pushes realtime events to dashboards.
//
// # What it is for
//
// The fleet table has to show a device going offline without the operator
// reloading. Polling would mean every open dashboard querying the device list
// every few seconds — at ten thousand devices and a handful of operators, that
// is a continuous scan of the hottest table in the system for information that
// changes rarely.
//
// # What it deliberately does not do
//
// It carries no data a caller could not already fetch over REST. An event says
// *that* a device's status changed; the dashboard refetches what it needs. That
// is what makes a stale or replayed frame harmless, and it is why the
// authorisation check at subscribe time is sufficient — there is no payload to
// leak between the check and the frame.
//
// # Scale
//
// One goroutine per connection, a bounded queue per connection, and a fan-out
// keyed by topic. A dashboard that stops reading is disconnected rather than
// buffered: it will reconnect and refetch, and holding frames for it would let
// one wedged browser tab grow the server's memory.
type Hub struct {
	log *slog.Logger

	mu     sync.RWMutex
	byID   map[uuid.UUID]*wsClient
	topics map[string]map[uuid.UUID]*wsClient

	// closed stops new registrations during shutdown.
	closed bool
}

// Event is what the hub delivers.
//
// The shape matches the contract in openapi.yaml: a type, the entity it
// concerns, and an optional data object carrying only what the dashboard needs
// to decide whether to refetch.
type Event struct {
	Type     string          `json:"type"`
	DeviceID string          `json:"device_id,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	// At is when the event happened, so a client can discard one that arrived
	// out of order after a reconnect.
	At time.Time `json:"at"`
}

// Topics a client may subscribe to.
const (
	TopicFleet  = "fleet"
	TopicAlerts = "alerts"

	topicDevicePrefix     = "device:"
	topicCommandPrefix    = "command:"
	topicDeploymentPrefix = "deployment:"
	topicFilePrefix       = "file:"
)

// wsSendQueue bounds one client's backlog.
//
// Sized for a burst — a hundred devices going offline at once when a site's
// uplink drops — not for buffering a browser that stopped reading.
const wsSendQueue = 64

// wsWriteTimeout bounds one frame write, so a client on a stalled TCP
// connection cannot hold a goroutine indefinitely.
const wsWriteTimeout = 10 * time.Second

// wsClient is one dashboard connection.
type wsClient struct {
	id        uuid.UUID
	orgID     uuid.UUID
	principal *domain.Principal

	send   chan Event
	closed chan struct{}
	once   sync.Once

	mu     sync.Mutex
	topics map[string]struct{}
}

// NewHub builds a Hub.
func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		log:    log,
		byID:   map[uuid.UUID]*wsClient{},
		topics: map[string]map[uuid.UUID]*wsClient{},
	}
}

// Publish delivers an event to every client subscribed to a topic.
//
// Never blocks: a client whose queue is full is disconnected, because the
// alternative is one wedged browser tab slowing every other dashboard down.
func (h *Hub) Publish(orgID uuid.UUID, topic string, event Event) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	h.mu.RLock()
	subscribers := make([]*wsClient, 0, len(h.topics[topic]))
	for _, client := range h.topics[topic] {
		// The topic namespace is global, so the tenant check happens here.
		// Without it a device topic could be subscribed to by name from another
		// organization — the id is a UUID, but "unguessable" is not an access
		// control.
		if client.orgID == orgID {
			subscribers = append(subscribers, client)
		}
	}
	h.mu.RUnlock()

	for _, client := range subscribers {
		select {
		case client.send <- event:
		default:
			h.log.Warn("a dashboard connection is not keeping up; disconnecting it",
				slog.String("client_id", client.id.String()),
				slog.String("topic", topic))
			client.close()
		}
	}
}

// Connected reports how many dashboards are attached, for metrics.
func (h *Hub) Connected() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byID)
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	clients := make([]*wsClient, 0, len(h.byID))
	for _, client := range h.byID {
		clients = append(clients, client)
	}
	h.mu.Unlock()

	for _, client := range clients {
		client.close()
	}
}

func (h *Hub) register(client *wsClient) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("http: the event hub is shutting down")
	}
	h.byID[client.id] = client
	return nil
}

func (h *Hub) unregister(client *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.byID, client.id)
	for topic := range client.topics {
		if subscribers, ok := h.topics[topic]; ok {
			delete(subscribers, client.id)
			if len(subscribers) == 0 {
				// Topics are per-device and per-command, so leaving empty maps
				// behind would grow with every device ever viewed.
				delete(h.topics, topic)
			}
		}
	}
}

func (h *Hub) subscribe(client *wsClient, topics []string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, topic := range topics {
		if _, ok := h.topics[topic]; !ok {
			h.topics[topic] = map[uuid.UUID]*wsClient{}
		}
		h.topics[topic][client.id] = client
		client.topics[topic] = struct{}{}
	}
}

func (c *wsClient) close() {
	c.once.Do(func() { close(c.closed) })
}

// clientMessage is what a dashboard sends.
type clientMessage struct {
	Type   string   `json:"type"`
	Topics []string `json:"topics"`
}

// events upgrades the connection and serves the realtime stream.
func (s *Server) events(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The browser's Origin is checked against the same allowlist as CORS.
		// Without this, any site could open a WebSocket to this API with the
		// user's cookies attached — the one cross-origin hole a same-origin
		// policy does not close by itself.
		OriginPatterns: s.wsOrigins,
		// Compression is off: the frames are small, and CRIME-style attacks on
		// compressed channels carrying authenticated data are not worth the
		// bandwidth saving.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Accept has already written a response.
		s.log.DebugContext(r.Context(), "websocket upgrade refused", slog.Any("error", err))
		return nil
	}

	client := &wsClient{
		id:        uuid.New(),
		orgID:     principal.OrgID,
		principal: principal,
		send:      make(chan Event, wsSendQueue),
		closed:    make(chan struct{}),
		topics:    map[string]struct{}{},
	}

	if err := s.hub.register(client); err != nil {
		_ = conn.Close(websocket.StatusGoingAway, "the server is shutting down")
		return nil
	}
	defer s.hub.unregister(client)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The read loop runs in its own goroutine for the same reason the agent's
	// does: a blocked Read cannot be interrupted from this side, and the write
	// loop has to be free to notice a shutdown.
	go func() {
		defer cancel()
		s.readSubscriptions(ctx, conn, client)
	}()

	s.writeEvents(ctx, conn, client)
	return nil
}

// readSubscriptions handles messages from the dashboard.
func (s *Server) readSubscriptions(ctx context.Context, conn *websocket.Conn, client *wsClient) {
	// A dashboard sends subscribe frames and nothing else. 4 KiB is generous
	// for a list of topics and small enough that a hostile client cannot make
	// the server allocate.
	conn.SetReadLimit(4 << 10)

	for {
		var msg clientMessage
		if err := readJSON(ctx, conn, &msg); err != nil {
			return
		}

		switch msg.Type {
		case "subscribe":
			allowed, refused := s.authorizeTopics(client.principal, msg.Topics)
			if len(allowed) > 0 {
				s.hub.subscribe(client, allowed)
			}
			if len(refused) > 0 {
				// Told, not silently dropped: a dashboard that believes it is
				// subscribed and receives nothing is indistinguishable from a
				// fleet where nothing is happening.
				s.sendControl(ctx, conn, map[string]any{
					"type": "subscription.refused", "topics": refused,
				})
			}
			s.sendControl(ctx, conn, map[string]any{
				"type": "subscribed", "topics": allowed,
			})

		case "ping":
			s.sendControl(ctx, conn, map[string]any{"type": "pong"})

		default:
			s.sendControl(ctx, conn, map[string]any{
				"type": "error", "message": "unknown message type: " + msg.Type,
			})
		}
	}
}

// writeEvents pushes events until the connection ends.
func (s *Server) writeEvents(ctx context.Context, conn *websocket.Conn, client *wsClient) {
	// A keepalive, so a connection through a NAT or a load balancer that drops
	// idle flows is detected rather than silently dead.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return

		case <-client.closed:
			_ = conn.Close(websocket.StatusPolicyViolation, "this connection fell too far behind")
			return

		case event := <-client.send:
			writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := writeJSON(writeCtx, conn, event)
			cancel()
			if err != nil {
				return
			}

		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// authorizeTopics splits requested topics into those the caller may have and
// those it may not.
//
// Every topic is checked. A UUID in a topic name is not an authorisation: the
// device id appears in URLs, in logs, and in the dashboard's own state, and
// treating it as a secret would make "unguessable" the access control.
func (s *Server) authorizeTopics(principal *domain.Principal, requested []string) (allowed, refused []string) {
	for _, topic := range requested {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}

		var permission string
		switch {
		case topic == TopicFleet, strings.HasPrefix(topic, topicDevicePrefix):
			permission = permDeviceRead
		case topic == TopicAlerts:
			permission = "alert:read"
		case strings.HasPrefix(topic, topicCommandPrefix):
			permission = "command:read"
		case strings.HasPrefix(topic, topicDeploymentPrefix):
			permission = "deployment:read"
		case strings.HasPrefix(topic, topicFilePrefix):
			permission = "file:read"
		default:
			refused = append(refused, topic)
			continue
		}

		if !principal.Can(permission) {
			refused = append(refused, topic)
			continue
		}
		allowed = append(allowed, topic)
	}
	return allowed, refused
}

func (s *Server) sendControl(ctx context.Context, conn *websocket.Conn, payload any) {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	if err := writeJSON(writeCtx, conn, payload); err != nil {
		s.log.DebugContext(ctx, "writing a control frame failed", slog.Any("error", err))
	}
}

func readJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return fmt.Errorf("http: expected a text frame, got %v", typ)
	}
	return json.Unmarshal(data, v)
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// DeviceTopic names the topic for one device's events.
func DeviceTopic(deviceID uuid.UUID) string { return topicDevicePrefix + deviceID.String() }
