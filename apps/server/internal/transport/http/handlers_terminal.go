package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/service/terminal"
	"github.com/visionify/edgepilot/apps/server/internal/service/tunnel"
)

// TerminalOpener opens a tunnel for the browser terminal.
//
// Satisfied by service/tunnel.Registry, declared here so the HTTP transport does
// not depend on the concrete type.
type TerminalOpener interface {
	Open(ctx context.Context, deviceID uuid.UUID, sessionID string,
		host string, port uint32, idle time.Duration) (*tunnel.Tunnel, error)
}

// Terminal frames.
//
// Output is sent as binary and input arrives as binary, because terminal traffic
// is bytes and forcing it through UTF-8 text frames would corrupt anything that
// is not valid UTF-8 — which includes half of a multi-byte character split
// across two reads. Control messages are text frames, so the two never collide.
type terminalControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// terminalReadLimit bounds one inbound frame. Keystrokes and paste, nothing more.
const terminalReadLimit = 1 << 20

// deviceTerminal opens an interactive shell on a device and streams it.
//
// Unlike the bastion, this server is the SSH client here: it decrypts the
// session in order to render it in a browser. See service/terminal for what that
// costs and docs/Security.md §5.1 for why it is a separate decision from `ssh
// -J`.
func (s *Server) deviceTerminal(w http.ResponseWriter, r *http.Request) error {
	deviceID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}

	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}

	if s.terminalSigner == nil || s.tunnels == nil {
		return domain.Unavailable("terminal_disabled",
			"the browser terminal is not enabled on this deployment")
	}

	// The device, scoped to the caller's organization. This is also what stops a
	// device id from another tenant reaching a shell.
	device, err := s.store.Devices().Get(r.Context(), deviceID)
	if err != nil {
		return err
	}
	if !device.Approved() {
		return domain.Conflict("device_not_approved",
			"that device has not been approved")
	}

	// Which accounts this server may log in as. Several, because a fleet does
	// not use one username: the same key is registered once per account, and
	// which of them exists is a property of the device, not of the server.
	usernames, err := s.terminalUsernames(r.Context())
	if err != nil {
		return err
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  s.wsOrigins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.DebugContext(r.Context(), "terminal websocket upgrade refused",
			slog.Any("error", err))
		return nil
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(terminalReadLimit)

	// The websocket outlives the request context in coder/websocket's model, so
	// the session gets its own.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	s.runTerminal(ctx, conn, device, usernames, principal, ClientIP(r))
	return nil
}

// runTerminal owns one shell for its lifetime.
func (s *Server) runTerminal(ctx context.Context, conn *websocket.Conn,
	device *domain.Device, usernames []string, principal *domain.Principal, clientIP string,
) {
	log := s.log.With(
		slog.String("device_id", device.ID.String()),
		slog.String("hostname", device.Hostname),
		slog.String("user_id", principal.UserID.String()))

	// The audit row first, as the bastion does: an attempt that never reaches a
	// shell is still an attempt to reach one.
	audit, err := s.store.SSHSessions().Open(ctx, domain.OpenSSHSessionInput{
		OrgID:          device.OrgID,
		DeviceID:       device.ID,
		UserID:         &principal.UserID,
		BastionReplica: s.replicaID,
		ClientIP:       clientIP,
		TargetUsername: usernames[0],
		Status:         "active",
	})
	if err != nil {
		log.ErrorContext(ctx, "could not record the terminal session; refusing it",
			slog.Any("error", err))
		writeTerminalError(ctx, conn, "the session could not be recorded")
		return
	}

	session, username, err := s.dialShell(ctx, device, audit.ID.String(), usernames, log)
	if err != nil {
		reason := shellReason(err, usernames)
		log.InfoContext(ctx, "starting the shell failed", slog.Any("error", err))
		s.closeTerminalAudit(audit.ID, "failed", 0, 0, reason)
		writeTerminalError(ctx, conn, reason)
		return
	}
	defer session.Close()

	// The audit row was opened with the first candidate; correct it to the
	// account that actually answered, so the record says what happened rather
	// than what was attempted first.
	if username != usernames[0] {
		s.retargetAudit(audit.ID, username)
	}

	recorder, err := terminal.NewRecorder(s.recordingDir, audit.ID.String(),
		device.Hostname+" — "+username, 80, 24, s.now())
	if err != nil {
		// Not fatal, but loud: the compensating control for decrypting the
		// session is that it is recorded, and an operator should know when that
		// is not happening.
		log.ErrorContext(ctx, "could not start recording this session",
			slog.Any("error", err))
	}
	defer func() { _ = recorder.Close() }()

	log.InfoContext(ctx, "a browser terminal session started",
		slog.String("session_id", audit.ID.String()),
		slog.String("target_username", username),
		slog.Bool("recorded", recorder != nil))

	var bytesOut, bytesIn int64
	done := make(chan struct{})

	// Device → browser.
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, readErr := session.Read(buf)
			if n > 0 {
				bytesOut += int64(n)
				if recErr := recorder.Output(s.now(), buf[:n]); recErr != nil {
					log.WarnContext(ctx, "writing to the session recording failed",
						slog.Any("error", recErr))
				}
				if writeErr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// Browser → device.
	for {
		select {
		case <-done:
			s.finishTerminal(conn, session, audit.ID, bytesIn, bytesOut,
				"the shell exited", log)
			return
		case <-ctx.Done():
			s.finishTerminal(conn, session, audit.ID, bytesIn, bytesOut,
				"the session was cancelled", log)
			return
		default:
		}

		kind, data, readErr := conn.Read(ctx)
		if readErr != nil {
			// The operator closed the tab or lost their connection. Ordinary.
			s.finishTerminal(conn, session, audit.ID, bytesIn, bytesOut,
				"the operator disconnected", log)
			return
		}

		if kind == websocket.MessageText {
			var control terminalControl
			if json.Unmarshal(data, &control) == nil && control.Type == "resize" {
				if resizeErr := session.Resize(control.Cols, control.Rows); resizeErr != nil {
					log.DebugContext(ctx, "resizing the terminal failed",
						slog.Any("error", resizeErr))
					continue
				}
				_ = recorder.Resize(s.now(), control.Cols, control.Rows)
			}
			continue
		}

		bytesIn += int64(len(data))
		if _, writeErr := session.Write(data); writeErr != nil {
			s.finishTerminal(conn, session, audit.ID, bytesIn, bytesOut,
				"the shell stopped accepting input", log)
			return
		}
	}
}

// dialShell tries each registered account until one lets the server in.
//
// The server cannot know which local account exists on a given Mac — that is
// exactly what the agent decides when it reconciles keys — so it tries the
// accounts its key is registered for. Each attempt needs its own tunnel: an SSH
// handshake fixes the username, and a failed one leaves the connection unusable.
//
// Bounded by the number of registered accounts, which is a handful, and only
// the failures cost anything.
func (s *Server) dialShell(ctx context.Context, device *domain.Device, sessionID string,
	usernames []string, log *slog.Logger,
) (*terminal.Session, string, error) {
	var lastErr error
	for _, username := range usernames {
		tun, err := s.tunnels.Open(ctx, device.ID, sessionID, "127.0.0.1", 22, s.sshIdleTimeout)
		if err != nil {
			// A tunnel failure is about the device, not the account, so there is
			// nothing to gain from trying the next username.
			return nil, "", err
		}

		session, err := terminal.Open(ctx, tun, terminal.Options{
			Signer: s.terminalSigner, Username: username, Log: s.log,
		})
		if err == nil {
			return session, username, nil
		}

		log.DebugContext(ctx, "the device refused a terminal login",
			slog.String("username", username), slog.Any("error", err))
		lastErr = err
		_ = tun.Close()
	}
	return nil, "", lastErr
}

// retargetAudit corrects the account recorded for a session.
func (s *Server) retargetAudit(sessionID uuid.UUID, username string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.SSHSessions().Retarget(ctx, sessionID, username); err != nil {
		s.log.Warn("could not record which account the terminal used",
			slog.String("session_id", sessionID.String()), slog.Any("error", err))
	}
}

// finishTerminal closes the session and its audit row exactly once.
func (s *Server) finishTerminal(conn *websocket.Conn, session *terminal.Session,
	sessionID uuid.UUID, bytesIn, bytesOut int64, reason string, log *slog.Logger,
) {
	session.Close()
	s.closeTerminalAudit(sessionID, "closed", bytesIn, bytesOut, reason)
	_ = conn.Close(websocket.StatusNormalClosure, reason)

	log.Info("a browser terminal session ended",
		slog.String("session_id", sessionID.String()),
		slog.Int64("bytes_from_device", bytesOut),
		slog.Int64("bytes_to_device", bytesIn),
		slog.String("reason", reason))
}

// closeTerminalAudit completes the audit row on its own context.
func (s *Server) closeTerminalAudit(sessionID uuid.UUID, status string,
	bytesIn, bytesOut int64, reason string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// bytes_in is from the device, matching the column's meaning at the bastion.
	if err := s.store.SSHSessions().Close(ctx, domain.CloseSSHSessionInput{
		ID: sessionID, Status: status,
		BytesIn: bytesOut, BytesOut: bytesIn, CloseReason: reason,
	}); err != nil {
		s.log.Error("closing the terminal audit record failed",
			slog.String("session_id", sessionID.String()), slog.Any("error", err))
	}
}

// terminalUsernames lists the accounts the server's key is registered for.
func (s *Server) terminalUsernames(ctx context.Context) ([]string, error) {
	keys, err := s.store.SSHKeys().ListForOrg(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	fingerprint := ssh.FingerprintSHA256(s.terminalSigner.PublicKey())

	var usernames []string
	seen := make(map[string]bool)
	for _, k := range keys {
		if k.FingerprintSHA256 != fingerprint || !k.Active(now) || seen[k.TargetUsername] {
			continue
		}
		seen[k.TargetUsername] = true
		usernames = append(usernames, k.TargetUsername)
	}

	if len(usernames) == 0 {
		// The server holds a key that is not registered, so no device has
		// installed it and every login would fail. Saying so names the fix.
		return nil, domain.Unavailable("terminal_key_unregistered",
			"the server's terminal key is not registered for this organization; "+
				"register it with `epctl ssh-key add` against the account it should log in as")
	}
	return usernames, nil
}

// downloadRecording serves a session recording.
func (s *Server) downloadRecording(w http.ResponseWriter, r *http.Request) error {
	sessionID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if s.recordingDir == "" {
		return domain.Unavailable("recording_disabled",
			"session recording is not enabled on this deployment")
	}

	// Read the row first, so a recording is only served to someone whose
	// organization owns the session. The files are named by session id and
	// nothing else would stop a guessed id being served.
	if _, err := s.store.SSHSessions().Get(r.Context(), sessionID); err != nil {
		return err
	}

	file, err := os.Open(terminal.RecordingPath(s.recordingDir, sessionID.String()))
	if err != nil {
		return domain.NotFound("recording", sessionID.String())
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", "application/x-asciicast")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+sessionID.String()+`.cast"`)
	http.ServeContent(w, r, sessionID.String()+".cast", time.Time{}, file)
	return nil
}

// writeTerminalError tells the browser why there is no shell.
func writeTerminalError(ctx context.Context, conn *websocket.Conn, reason string) {
	// Sent as terminal output so it appears in the terminal the operator is
	// looking at, rather than only in a close frame the UI has to decode.
	_ = conn.Write(ctx, websocket.MessageBinary,
		[]byte("\r\n\x1b[31m"+reason+"\x1b[0m\r\n"))
	_ = conn.Close(websocket.StatusInternalError, "terminal unavailable")
}

func terminalTunnelReason(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrNotConnected):
		return "that device is not currently connected to EdgePilot"
	case errors.Is(err, tunnel.ErrTimeout):
		return "the device did not answer; it may have just gone offline"
	case errors.Is(err, tunnel.ErrRefused):
		return err.Error()
	default:
		return "could not open a connection to that device"
	}
}

// shellReason turns a failed login into something actionable.
func shellReason(err error, usernames []string) string {
	if err == nil {
		return "could not start a shell on that device"
	}
	if reason := terminalTunnelReason(err); !strings.HasPrefix(reason, "could not open") {
		return reason
	}
	// "unable to authenticate" is what x/crypto/ssh reports when the key is not
	// in the account's authorized_keys, which is the overwhelmingly likely cause
	// and the one an operator can fix.
	if containsAny(err.Error(), "unable to authenticate", "no supported methods") {
		return "the device refused the server's key for " +
			strings.Join(usernames, " or ") +
			" — check the agent has installed it and that Remote Login is on"
	}
	return "could not start a shell on that device"
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
