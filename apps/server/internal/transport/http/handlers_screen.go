package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/service/screen"
	"github.com/visionify/edgepilot/apps/server/internal/service/tunnel"
	"github.com/visionify/edgepilot/apps/server/internal/service/vnc"
)

// screenReadLimit bounds one inbound WebSocket frame from the browser. noVNC
// sends small control messages — key events, pointer events, framebuffer
// requests — never anything large; a megabyte is generous.
const screenReadLimit = 1 << 20

// deviceScreen relays a remote screen (VNC) session to the browser.
//
// The shape mirrors the browser terminal, and the trust model is the same: the
// server sits between an authenticated operator and a device it manages, and
// terminates the device's VNC authentication so the password never reaches the
// browser (see service/vnc). What differs is that after the handshake this is a
// pure byte pipe — there is nothing to record frame by frame the way a terminal
// is, so the audit is who/when/how-much, not what was on screen.
func (s *Server) deviceScreen(w http.ResponseWriter, r *http.Request) error {
	deviceID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	if s.tunnels == nil || s.sealer == nil {
		return domain.Unavailable("screen_disabled",
			"remote screen is not enabled on this deployment")
	}

	device, err := s.store.Devices().Get(r.Context(), deviceID)
	if err != nil {
		return err
	}
	if !device.Approved() {
		return domain.Conflict("device_not_approved", "that device has not been approved")
	}
	if !device.ScreenSharingEnabled {
		return domain.Conflict("screen_not_enabled",
			"remote screen is not enabled for this device; turn it on from the device page")
	}

	password, err := screen.Open(s.sealer, device.ID, device.VNCPasswordSealed)
	if err != nil {
		return err
	}
	if password == "" {
		return domain.Conflict("screen_not_provisioned",
			"remote screen is enabled but no password is set yet; wait for the device to reconnect")
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  s.wsOrigins,
		CompressionMode: websocket.CompressionDisabled,
		// noVNC negotiates the "binary" subprotocol.
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		s.log.DebugContext(r.Context(), "screen websocket upgrade refused", slog.Any("error", err))
		return nil
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(screenReadLimit)

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	s.runScreen(ctx, conn, device, password, principal, ClientIP(r))
	return nil
}

// runScreen owns one screen session for its lifetime.
func (s *Server) runScreen(ctx context.Context, conn *websocket.Conn,
	device *domain.Device, password string, principal *domain.Principal, clientIP string,
) {
	log := s.log.With(
		slog.String("device_id", device.ID.String()),
		slog.String("hostname", device.Hostname),
		slog.String("user_id", principal.UserID.String()))

	// The audit row first, as the terminal and bastion do: an attempt that never
	// reaches a desktop is still an attempt to reach one. "screen" is the target,
	// distinguishing it in the session history from an SSH login.
	audit, err := s.store.SSHSessions().Open(ctx, domain.OpenSSHSessionInput{
		OrgID:          device.OrgID,
		DeviceID:       device.ID,
		UserID:         &principal.UserID,
		BastionReplica: s.replicaID,
		ClientIP:       clientIP,
		TargetUsername: "screen",
		Status:         "active",
	})
	if err != nil {
		log.ErrorContext(ctx, "could not record the screen session; refusing it", slog.Any("error", err))
		_ = conn.Close(websocket.StatusInternalError, "the session could not be recorded")
		return
	}

	// A tunnel to the Mac's loopback VNC server. Enabled Screen Sharing is
	// listening there; if it is not — the agent has not applied policy yet — the
	// tunnel fails to open and the operator gets that reason.
	tun, err := s.tunnels.Open(ctx, device.ID, audit.ID.String(), "127.0.0.1", 5900, s.sshIdleTimeout)
	if err != nil {
		reason := screenReason(err)
		log.InfoContext(ctx, "opening the screen tunnel failed", slog.Any("error", err))
		s.closeScreenAudit(audit.ID, "failed", 0, 0, reason)
		_ = conn.Close(websocket.StatusTryAgainLater, reason)
		return
	}
	defer func() { _ = tun.Close() }()

	browser := &wsStream{conn: conn, ctx: ctx}

	// Terminate the device's VNC auth and present the browser a no-auth
	// handshake. After this both sides are at the framebuffer stream.
	if err := vnc.Handshake(tun, browser, password); err != nil {
		reason := "the remote screen handshake failed"
		log.WarnContext(ctx, reason, slog.Any("error", err))
		s.closeScreenAudit(audit.ID, "failed", 0, 0, reason)
		_ = conn.Close(websocket.StatusProtocolError, reason)
		return
	}

	log.InfoContext(ctx, "a remote screen session started", slog.String("session_id", audit.ID.String()))

	var bytesFromDevice, bytesToDevice int64
	done := make(chan struct{})

	// Device → browser: the framebuffer.
	go func() {
		defer close(done)
		n, _ := io.Copy(browser, tun)
		bytesFromDevice = n
	}()

	// Browser → device: input events. Runs on this goroutine.
	n, _ := io.Copy(tun, browser)
	bytesToDevice = n

	// Whichever direction ended, tear the other down and settle the audit.
	_ = tun.Close()
	_ = conn.Close(websocket.StatusNormalClosure, "the session ended")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}

	s.closeScreenAudit(audit.ID, "closed", bytesFromDevice, bytesToDevice, "the session ended")
	log.InfoContext(ctx, "a remote screen session ended",
		slog.String("session_id", audit.ID.String()),
		slog.Int64("bytes_from_device", bytesFromDevice),
		slog.Int64("bytes_to_device", bytesToDevice))
}

// closeScreenAudit completes the audit row on its own context.
func (s *Server) closeScreenAudit(sessionID uuid.UUID, status string, fromDevice, toDevice int64, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.SSHSessions().Close(ctx, domain.CloseSSHSessionInput{
		ID: sessionID, Status: status,
		BytesIn: fromDevice, BytesOut: toDevice, CloseReason: reason,
	}); err != nil {
		s.log.Error("closing the screen audit record failed",
			slog.String("session_id", sessionID.String()), slog.Any("error", err))
	}
}

// screenReason turns a tunnel error into an operator-facing sentence.
func screenReason(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrNotConnected):
		return "that device is not currently connected"
	default:
		return "could not reach the device's screen; it may not have applied the setting yet"
	}
}

// wsStream adapts a binary WebSocket to an io.ReadWriter of raw bytes.
//
// Reads pull one binary message at a time and hand out its bytes, buffering
// whatever a caller did not consume — RFB reads are small and specific (a
// 16-byte challenge, a 24-byte header) and will routinely take less than a
// whole frame. Writes send one binary frame each; VNC framebuffer updates are
// large, and coalescing them is the WebSocket layer's job, not ours.
type wsStream struct {
	conn *websocket.Conn
	ctx  context.Context
	buf  []byte
}

func (s *wsStream) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		typ, data, err := s.conn.Read(s.ctx)
		if err != nil {
			return 0, err
		}
		if typ != websocket.MessageBinary {
			// noVNC speaks only binary; a text frame is noise, not data.
			continue
		}
		s.buf = data
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *wsStream) Write(p []byte) (int, error) {
	if err := s.conn.Write(s.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
