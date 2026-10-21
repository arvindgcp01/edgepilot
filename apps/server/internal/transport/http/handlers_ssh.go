package http

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// SessionTerminator ends a live SSH relay.
//
// Declared here and satisfied by transport/sshd.Server. Nil when the bastion is
// disabled, which is why every use is guarded: a deployment without a host key
// serves the rest of the API perfectly well.
type SessionTerminator interface {
	Terminate(sessionID uuid.UUID) bool
}

type sshSessionResponse struct {
	ID       string `json:"id"`
	DeviceID string `json:"device_id"`
	UserID   string `json:"user_id,omitempty"`
	SSHKeyID string `json:"ssh_key_id,omitempty"`

	ClientIP       string `json:"client_ip,omitempty"`
	TargetUsername string `json:"target_username,omitempty"`
	BastionReplica string `json:"bastion_replica,omitempty"`

	Status      string `json:"status"`
	BytesIn     int64  `json:"bytes_in"`
	BytesOut    int64  `json:"bytes_out"`
	CloseReason string `json:"close_reason,omitempty"`

	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

func toSSHSessionResponse(s *domain.SSHSession) sshSessionResponse {
	out := sshSessionResponse{
		ID:             s.ID.String(),
		DeviceID:       s.DeviceID.String(),
		ClientIP:       s.ClientIP,
		TargetUsername: s.TargetUsername,
		BastionReplica: s.BastionReplica,
		Status:         s.Status,
		BytesIn:        s.BytesIn,
		BytesOut:       s.BytesOut,
		CloseReason:    s.CloseReason,
		StartedAt:      s.StartedAt,
		EndedAt:        s.EndedAt,
	}
	if s.UserID != nil {
		out.UserID = s.UserID.String()
	}
	if s.SSHKeyID != nil {
		out.SSHKeyID = s.SSHKeyID.String()
	}
	return out
}

// listSSHSessions returns the organization's live SSH sessions.
//
// Active only. The full history is per-device and belongs on that device's page;
// this endpoint answers "who is on my fleet right now", which is what the fleet
// grid needs and what an operator checks first.
func (s *Server) listSSHSessions(w http.ResponseWriter, r *http.Request) error {
	sessions, err := s.store.SSHSessions().ListActive(r.Context())
	if err != nil {
		return err
	}

	out := make([]sshSessionResponse, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, toSSHSessionResponse(session))
	}
	return respond(w, http.StatusOK, listResponse[sshSessionResponse]{Data: out})
}

// listDeviceSSHSessions returns one device's session history.
func (s *Server) listDeviceSSHSessions(w http.ResponseWriter, r *http.Request) error {
	deviceID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}

	sessions, err := s.store.SSHSessions().ListForDevice(r.Context(), deviceID, 50)
	if err != nil {
		return err
	}

	out := make([]sshSessionResponse, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, toSSHSessionResponse(session))
	}
	return respond(w, http.StatusOK, listResponse[sshSessionResponse]{Data: out})
}

// terminateSSHSession ends a live session.
//
// The audit row is not written here. Closing the relay makes the bastion's own
// teardown path run, and that is what records the byte counts and the reason —
// writing it here as well would either race that or record numbers this handler
// cannot know.
func (s *Server) terminateSSHSession(w http.ResponseWriter, r *http.Request) error {
	sessionID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}

	// Read it first, scoped to the caller's organization. Without this, a session
	// id from another tenant would be terminated by id alone — the bastion's
	// live map is process-wide and knows nothing about tenancy.
	session, err := s.store.SSHSessions().Get(r.Context(), sessionID)
	if err != nil {
		return err
	}
	if session.Status != "active" {
		return domain.Conflict("session_not_active",
			"that session has already ended")
	}

	if s.sessions == nil {
		return domain.Unavailable("bastion_disabled",
			"the SSH bastion is not enabled on this deployment")
	}

	if !s.sessions.Terminate(sessionID) {
		// The session is active in the database but not on this replica. With
		// one replica that means it ended a moment ago; with several it means
		// another replica holds it.
		return domain.Conflict("session_elsewhere",
			"that session is not running on this server; it may have just ended")
	}

	s.log.InfoContext(r.Context(), "an operator terminated an SSH session",
		"session_id", sessionID.String(),
		"device_id", session.DeviceID.String())

	return noContent(w)
}
