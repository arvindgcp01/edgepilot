// Package sshd is the SSH bastion an operator jumps through to reach a Mac.
//
// It is a jump host and nothing more. An engineer runs
//
//	ssh -J bastion:2222 admin@<device-id>
//
// and their SSH client opens a `direct-tcpip` channel through this server to the
// Mac's own sshd. The bastion authenticates the *operator* by public key against
// ssh_keys, decides whether they may reach that device, and then copies bytes.
//
// # Why the bastion cannot read the session
//
// The inner SSH connection is negotiated end to end between the engineer's
// client and the Mac. This process relays ciphertext; it holds no key to it and
// never sees a password, a keystroke, or a byte of output. That is a deliberate
// trade — see docs/Security.md §5.1 — and it is why ssh_sessions records
// metadata only. A control plane that could replay engineers' sessions would be
// a far more attractive target than one that cannot.
//
// # What it refuses
//
// No shell, no exec, no SFTP, no agent forwarding, no port forwarding other than
// direct-tcpip to a device. `ssh bastion` on its own gets a message explaining
// how to use it and then a closed connection: there is no account on this host
// to land in, and offering one would make the bastion itself a target worth
// having.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/service/tunnel"
)

// sshdPort is the port on the Mac a tunnel targets. Loopback sshd, always: the
// agent enforces the same constraint independently.
const sshdPort = 22

// handshakeTimeout bounds the SSH handshake.
//
// Without it, a connection that opens a socket and sends nothing occupies a
// goroutine and a slot indefinitely — the cheapest denial of service there is.
const handshakeTimeout = 30 * time.Second

// Server is the bastion.
type Server struct {
	store    domain.Store
	tunnels  *tunnel.Registry
	log      *slog.Logger
	now      func() time.Time
	replica  string
	idle     time.Duration
	maxConns int

	config *ssh.ServerConfig

	listener net.Listener
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool
	active int
	// live maps an ssh_sessions row to the means of ending it. Only sessions on
	// this replica appear here — see Terminate.
	live map[uuid.UUID]context.CancelFunc
}

// Options configures the bastion.
type Options struct {
	Store   domain.Store
	Tunnels *tunnel.Registry
	Logger  *slog.Logger
	Now     func() time.Time

	// HostKeyPEM is the bastion's identity. Operators pin its fingerprint, so it
	// must be stable across restarts and across replicas — a regenerated key
	// looks exactly like a man in the middle, and correctly so.
	HostKeyPEM []byte

	ReplicaID   string
	IdleTimeout time.Duration
	MaxSessions int
}

// New builds the bastion.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("sshd: a Store is required")
	case opts.Tunnels == nil:
		return nil, errors.New("sshd: a tunnel Registry is required")
	case len(opts.HostKeyPEM) == 0:
		return nil, errors.New("sshd: a host key is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 200
	}

	hostKey, err := ssh.ParsePrivateKey(opts.HostKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("sshd: parsing the host key: %w", err)
	}

	s := &Server{
		store:    opts.Store,
		tunnels:  opts.Tunnels,
		log:      opts.Logger,
		now:      opts.Now,
		replica:  opts.ReplicaID,
		idle:     opts.IdleTimeout,
		maxConns: opts.MaxSessions,
		live:     make(map[uuid.UUID]context.CancelFunc),
	}

	s.config = &ssh.ServerConfig{
		PublicKeyCallback: s.authenticate,
		// Passwords are not accepted at all. There is no password to accept:
		// identity here is a registered public key, and offering the method
		// would only invite guessing.
		PasswordCallback: nil,
		ServerVersion:    "SSH-2.0-EdgePilot",
		AuthLogCallback: func(conn ssh.ConnMetadata, method string, err error) {
			if err != nil {
				s.log.Info("bastion authentication failed",
					slog.String("method", method),
					slog.String("user", conn.User()),
					slog.String("remote", conn.RemoteAddr().String()))
			}
		},
	}
	s.config.AddHostKey(hostKey)

	return s, nil
}

// permissions carries the authenticated identity from the auth callback to the
// connection handler. ssh.Permissions.Extensions is the only channel the
// library provides for this, and it is strings only.
const (
	extKeyID   = "edgepilot-key-id"
	extOrgID   = "edgepilot-org-id"
	extUserID  = "edgepilot-user-id"
	extKeyName = "edgepilot-key-name"
	// extTarget is the local account on the device this key is registered for.
	// The bastion cannot observe the username the client actually uses — that is
	// inside the encrypted inner session — so the key's own target is the honest
	// thing to record.
	extTarget = "edgepilot-target-user"
)

// authenticate resolves a public key to a registered operator.
func (s *Server) authenticate(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	fingerprint := ssh.FingerprintSHA256(key)

	// A background context: this runs before any request context exists, and
	// the lookup crosses organizations by design.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stored, err := s.store.SSHKeys().FindByFingerprint(ctx, fingerprint)
	if err != nil {
		// Deliberately uniform: "unknown key" and "revoked key" produce the same
		// answer to the client, so the bastion cannot be used to enumerate which
		// keys are registered.
		return nil, errors.New("unknown key")
	}
	if !stored.Active(s.now()) {
		s.log.Info("a revoked or expired key was offered",
			slog.String("fingerprint", fingerprint),
			slog.String("remote", conn.RemoteAddr().String()))
		return nil, errors.New("unknown key")
	}

	ext := map[string]string{
		extKeyID:   stored.ID.String(),
		extOrgID:   stored.OrgID.String(),
		extKeyName: stored.Name,
		extTarget:  stored.TargetUsername,
	}
	if stored.OwnerUserID != nil {
		ext[extUserID] = stored.OwnerUserID.String()
	}

	// Best effort, and after the decision: a failed bookkeeping write must not
	// deny access that is otherwise authorised.
	go func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		if touchErr := s.store.SSHKeys().TouchLastUsed(bg, stored.ID, s.now()); touchErr != nil {
			s.log.Debug("recording ssh key use failed", slog.Any("error", touchErr))
		}
	}()

	return &ssh.Permissions{Extensions: ext}, nil
}

// Listen starts accepting connections and blocks until ctx ends.
func (s *Server) Listen(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sshd: listening on %s: %w", addr, err)
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	s.log.Info("the SSH bastion is listening", slog.String("addr", addr))

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				break
			}
			// A single failed accept is not a reason to stop serving: it is
			// usually one client that vanished between SYN and accept.
			s.log.Warn("accepting a bastion connection failed", slog.Any("error", acceptErr))
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}

	// Wait for sessions to drain rather than cutting them mid-command.
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.log.Warn("bastion sessions did not drain; closing anyway")
	}
	return nil
}

// handle runs one operator connection.
func (s *Server) handle(ctx context.Context, raw net.Conn) {
	defer func() { _ = raw.Close() }()

	remote := raw.RemoteAddr().String()

	// The handshake gets its own deadline; it is cleared once the connection is
	// established, because a long-lived SSH session is legitimate.
	_ = raw.SetDeadline(s.now().Add(handshakeTimeout))

	conn, chans, reqs, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		// Ordinary: port scanners, health checks, and clients with no
		// registered key all land here.
		s.log.Debug("a bastion handshake failed",
			slog.String("remote", remote), slog.Any("error", err))
		return
	}
	defer func() { _ = conn.Close() }()
	_ = raw.SetDeadline(time.Time{})

	orgID, _ := uuid.Parse(conn.Permissions.Extensions[extOrgID])
	keyID, _ := uuid.Parse(conn.Permissions.Extensions[extKeyID])
	var userID *uuid.UUID
	if raw, ok := conn.Permissions.Extensions[extUserID]; ok {
		if parsed, parseErr := uuid.Parse(raw); parseErr == nil {
			userID = &parsed
		}
	}

	log := s.log.With(
		slog.String("remote", remote),
		slog.String("key", conn.Permissions.Extensions[extKeyName]),
		slog.String("org_id", orgID.String()))
	log.Info("an operator connected to the bastion")

	// Global requests are all refused; none of them are part of jumping.
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		switch newChannel.ChannelType() {
		case "direct-tcpip":
			s.wg.Add(1)
			go func(nc ssh.NewChannel) {
				defer s.wg.Done()
				s.forward(ctx, nc, session{
					orgID: orgID, keyID: keyID, userID: userID,
					targetUser: conn.Permissions.Extensions[extTarget],
					clientIP:   hostOnly(remote), log: log,
				})
			}(newChannel)

		case "session":
			// `ssh bastion` with no -J. Answering with a usable shell would make
			// this host worth attacking on its own; answering with silence would
			// leave the operator with no idea what they did wrong.
			s.rejectSession(newChannel, log)

		default:
			_ = newChannel.Reject(ssh.UnknownChannelType,
				"this bastion forwards connections to devices and offers nothing else")
		}
	}

	log.Info("the operator disconnected from the bastion")
}

// session carries the authenticated identity into a forwarded channel.
type session struct {
	orgID      uuid.UUID
	keyID      uuid.UUID
	userID     *uuid.UUID
	targetUser string
	clientIP   string
	log        *slog.Logger
}

// directTCPIP is the payload of a direct-tcpip channel open, per RFC 4254 §7.2.
type directTCPIP struct {
	DestHost string
	DestPort uint32
	SrcHost  string
	SrcPort  uint32
}

// forward relays one channel to a device.
func (s *Server) forward(ctx context.Context, newChannel ssh.NewChannel, sess session) {
	var payload directTCPIP
	if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "malformed forwarding request")
		return
	}

	// The destination host is the device: its UUID, or its hostname. An
	// operator types `ssh -J bastion admin@<device>`, and their client passes
	// whatever they wrote straight through here.
	device, err := s.resolveDevice(ctx, sess.orgID, payload.DestHost)
	if err != nil {
		sess.log.Info("a forwarding request named an unknown device",
			slog.String("target", payload.DestHost))
		_ = newChannel.Reject(ssh.ConnectionFailed,
			fmt.Sprintf("no device %q in this organization", payload.DestHost))
		return
	}

	log := sess.log.With(slog.String("device_id", device.ID.String()),
		slog.String("hostname", device.Hostname))

	if err := s.admit(); err != nil {
		log.Warn("refusing a session; the bastion is at capacity")
		s.record(ctx, sess, device.ID, "denied", "the bastion is at capacity")
		_ = newChannel.Reject(ssh.ResourceShortage, "the bastion is at capacity")
		return
	}
	defer s.release()

	// The audit row is written before the tunnel opens, so an attempt that fails
	// to reach the device is still recorded. A log of successful access only is
	// not an audit log.
	audit, auditErr := s.store.SSHSessions().Open(ctx, domain.OpenSSHSessionInput{
		OrgID:          sess.orgID,
		DeviceID:       device.ID,
		UserID:         sess.userID,
		SSHKeyID:       &sess.keyID,
		BastionReplica: s.replica,
		ClientIP:       sess.clientIP,
		TargetUsername: sess.targetUser,
		Status:         "active",
	})
	if auditErr != nil {
		// Refusing here is deliberate. An unaudited session on a customer's
		// machine is worse than no session: the whole justification for this
		// path is that every use of it is recorded.
		log.Error("could not record the session; refusing it", slog.Any("error", auditErr))
		_ = newChannel.Reject(ssh.ConnectionFailed, "the session could not be recorded")
		return
	}

	tun, tunErr := s.tunnels.Open(ctx, device.ID, audit.ID.String(), "127.0.0.1", sshdPort, s.idle)
	if tunErr != nil {
		reason := tunnelReason(tunErr)
		log.Info("opening a tunnel to the device failed", slog.Any("error", tunErr))
		s.close(audit.ID, "failed", 0, 0, reason)
		_ = newChannel.Reject(ssh.ConnectionFailed, reason)
		return
	}
	defer func() { _ = tun.Close() }()

	channel, requests, acceptErr := newChannel.Accept()
	if acceptErr != nil {
		s.close(audit.ID, "failed", 0, 0, "the operator's client withdrew the request")
		return
	}
	defer func() { _ = channel.Close() }()

	// direct-tcpip channels carry no requests worth honouring.
	go ssh.DiscardRequests(requests)

	log.Info("relaying an SSH session to the device",
		slog.String("session_id", audit.ID.String()))

	// Registered so an operator can end this session from the dashboard.
	// Cancelling is what the relay below is watching.
	relayCtx, endRelay := context.WithCancel(ctx)
	s.mu.Lock()
	s.live[audit.ID] = endRelay
	s.mu.Unlock()
	defer func() {
		endRelay()
		s.mu.Lock()
		delete(s.live, audit.ID)
		s.mu.Unlock()
	}()

	s.relay(relayCtx, channel, tun)

	toDevice, fromDevice, duration := tun.Stats()
	reason := tun.Reason()
	if reason == "" {
		reason = "the operator closed the session"
	}
	// A terminated session reports why. Without this it would be recorded as an
	// ordinary disconnect, and the audit log would not show that someone ended
	// it deliberately.
	if relayCtx.Err() != nil && ctx.Err() == nil {
		reason = "an administrator terminated the session"
	}
	// Named from the server's point of view, matching the column meanings:
	// bytes_in is what came from the device.
	s.close(audit.ID, "closed", int64(fromDevice), int64(toDevice), reason)

	log.Info("the SSH session ended",
		slog.String("session_id", audit.ID.String()),
		slog.Duration("duration", duration),
		slog.Uint64("bytes_from_device", fromDevice),
		slog.Uint64("bytes_to_device", toDevice),
		slog.String("reason", reason))
}

// relay copies in both directions until either end stops.
//
// Either direction ending ends the session. That is not a simplification: a
// tunnel has no half-close, so when the operator's client goes away the
// tunnel→channel copy would otherwise sit in Read forever — holding the
// goroutine, the tunnel, and an ssh_sessions row that never leaves 'active'.
// Whichever copy finishes first therefore tears the other one down.
func (s *Server) relay(ctx context.Context, channel ssh.Channel, tun *tunnel.Tunnel) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Closed by whichever direction finishes first.
	first := make(chan struct{})
	var once sync.Once
	ended := func() { once.Do(func() { close(first) }) }

	go func() {
		defer wg.Done()
		defer ended()
		_, _ = io.Copy(tun, channel)
	}()

	go func() {
		defer wg.Done()
		defer ended()
		_, _ = io.Copy(channel, tun)
		// The device stopped sending, which for SSH means the session ended.
		// Closing the write side turns that into a clean exit at the operator's
		// terminal rather than a hang.
		_ = channel.CloseWrite()
	}()

	select {
	case <-first:
	case <-tun.Done():
	case <-ctx.Done():
	}

	// Closing both ends unblocks whichever copy is still reading. Without this
	// the surviving goroutine outlives the session it belongs to.
	_ = tun.Close()
	_ = channel.Close()

	// Bounded: a copy blocked on a socket that is now closed returns promptly,
	// and waiting forever for one that does not would reintroduce the leak this
	// function exists to avoid.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.log.Warn("a relay did not stop promptly; abandoning it")
	}
}

// rejectSession explains why there is no shell here.
func (s *Server) rejectSession(newChannel ssh.NewChannel, log *slog.Logger) {
	log.Info("an operator opened a session channel; this bastion has no shell")

	channel, requests, err := newChannel.Accept()
	if err != nil {
		return
	}
	defer func() { _ = channel.Close() }()

	go func() {
		// Every request is refused, but they must be *answered*: a client that
		// asked for a pty and got silence waits for its reply rather than
		// showing the message below.
		for req := range requests {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()

	_, _ = io.WriteString(channel,
		"\r\nEdgePilot bastion — there is no shell on this host.\r\n\r\n"+
			"Use it as a jump host to reach a Mac in your fleet:\r\n\r\n"+
			"    ssh -J <bastion>:2222 <user>@<device-id-or-hostname>\r\n\r\n"+
			"The device is named by its EdgePilot id or hostname, and <user> is an\r\n"+
			"account on that Mac. Your key authenticates you to EdgePilot; the Mac's\r\n"+
			"own sshd decides whether you may log in.\r\n\r\n")

	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
}

// resolveDevice finds the device an operator named.
func (s *Server) resolveDevice(ctx context.Context, orgID uuid.UUID, target string) (*domain.Device, error) {
	// Every lookup is scoped to the organization the key belongs to, so naming
	// another tenant's device id returns "unknown device" rather than access.
	ctx = domain.WithOrg(ctx, orgID)

	if id, err := uuid.Parse(target); err == nil {
		return s.store.Devices().Get(ctx, id)
	}

	// A hostname. Macs report theirs as "Name.local"; accept both spellings so
	// an operator does not have to remember which one the fleet listing shows.
	candidates := []string{target}
	if !strings.HasSuffix(target, ".local") {
		candidates = append(candidates, target+".local")
	} else {
		candidates = append(candidates, strings.TrimSuffix(target, ".local"))
	}

	for _, name := range candidates {
		page, err := s.store.Devices().List(ctx,
			domain.DeviceFilter{Search: name},
			domain.PageRequest{Limit: 10})
		if err != nil {
			return nil, err
		}
		// Search matches hostname, serial, and site, so an exact hostname
		// comparison decides it. A prefix match would let "mac" reach a device
		// the operator did not name.
		for _, d := range page.Items {
			if strings.EqualFold(d.Hostname, name) {
				return d, nil
			}
		}
	}
	return nil, domain.NotFound("device", target)
}

// admit reserves a session slot, bounding concurrent relays.
//
// The cap exists because each session holds a goroutine pair and a tunnel on the
// device's stream; without it, one operator scripting connections could exhaust
// the replica for everyone.
func (s *Server) admit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("the bastion is shutting down")
	}
	if s.active >= s.maxConns {
		return errors.New("the bastion is at capacity")
	}
	s.active++
	return nil
}

func (s *Server) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		s.active--
	}
}

// Terminate ends a live session.
//
// Reports false when the session is not on this replica — which is not the same
// as "no such session". With one replica the distinction does not arise; with
// several, terminating has to reach the replica holding the relay, and saying
// so plainly is better than reporting success for something that did not happen.
func (s *Server) Terminate(sessionID uuid.UUID) bool {
	s.mu.Lock()
	cancel, ok := s.live[sessionID]
	s.mu.Unlock()
	if !ok {
		return false
	}

	s.log.Info("terminating an SSH session at an administrator's request",
		slog.String("session_id", sessionID.String()))
	cancel()
	return true
}

// Sessions reports how many relays are running, for the metrics endpoint.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// record writes a terminal audit row for an attempt that never became a session.
func (s *Server) record(ctx context.Context, sess session, deviceID uuid.UUID, status, reason string) {
	audit, err := s.store.SSHSessions().Open(ctx, domain.OpenSSHSessionInput{
		OrgID: sess.orgID, DeviceID: deviceID, UserID: sess.userID,
		SSHKeyID: &sess.keyID, BastionReplica: s.replica,
		ClientIP: sess.clientIP, Status: status,
	})
	if err != nil {
		return
	}
	s.close(audit.ID, status, 0, 0, reason)
}

// close completes an audit row.
func (s *Server) close(id uuid.UUID, status string, bytesIn, bytesOut int64, reason string) {
	// Its own context: the operator's is gone by the time a session ends, and
	// an audit row that is not written because the client hung up is exactly
	// the row worth having.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.store.SSHSessions().Close(ctx, domain.CloseSSHSessionInput{
		ID: id, Status: status, BytesIn: bytesIn, BytesOut: bytesOut,
		CloseReason: reason,
	}); err != nil {
		s.log.Error("closing the session audit record failed",
			slog.String("session_id", id.String()), slog.Any("error", err))
	}
}

// tunnelReason turns a tunnel failure into something worth showing an operator.
func tunnelReason(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrNotConnected):
		return "that device is not currently connected to EdgePilot"
	case errors.Is(err, tunnel.ErrTimeout):
		return "the device did not answer; it may have just gone offline"
	case errors.Is(err, tunnel.ErrRefused):
		// Carries the agent's own explanation, which is usually that Remote
		// Login is off — the one thing the operator can act on.
		return strings.TrimPrefix(err.Error(), "tunnel: ")
	default:
		return "could not open a connection to that device"
	}
}

// hostOnly strips the port from a remote address for the audit record.
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
