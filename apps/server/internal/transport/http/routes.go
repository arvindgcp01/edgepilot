package http

import (
	"net/http"
)

// Permissions this transport enforces. Named constants rather than string
// literals at the call sites, so a typo is a compile error instead of a route
// that silently requires a permission nobody holds — which fails closed, but
// fails invisibly.
const (
	permDeviceRead       = "device:read"
	permDeviceWrite      = "device:write"
	permDeviceApprove    = "device:approve"
	permDeviceDelete     = "device:delete"
	permEnrollTokenRead  = "enrollment_token:read"
	permEnrollTokenWrite = "enrollment_token:write"
	permSSHRead          = "ssh:read"
	// Terminating someone else's session is an access decision, not a read, so
	// it takes the permission that governs SSH access rather than ssh:read.
	permSSHConnect = "ssh:connect"

	permScreenConnect = "screen:connect"

	permUserRead  = "user:read"
	permUserWrite = "user:write"
	permOrgWrite  = "org:write"
)

// routes builds the routing table.
//
// The permission each route requires is stated here rather than inside the
// handler, so the whole authorisation surface can be read in one place and
// checked against the permissions catalogue in migrations 0008 and 0009.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// --- unauthenticated ---------------------------------------------------

	// Liveness and readiness. Deliberately outside /api/v1: they are for the
	// orchestrator, not for API clients, and versioning them would be odd.
	mux.Handle("GET /healthz", Handler(s.healthz))
	mux.Handle("GET /readyz", Handler(s.readyz))

	mux.Handle("POST /api/v1/auth/login", Handler(s.login))
	mux.Handle("POST /api/v1/auth/refresh", Handler(s.refresh))

	// The one user-plane endpoint an agent calls. Authenticated by an
	// enrollment token rather than a session, which is why it sits here rather
	// than behind the bearer middleware.
	mux.Handle("POST /api/v1/agent/enroll", Handler(s.enrollAgent))

	// --- authenticated -----------------------------------------------------

	authed := func(h Handler, permissions ...string) http.Handler {
		middleware := []Middleware{Authenticate(s.authn)}
		for _, permission := range permissions {
			middleware = append(middleware, Require(permission))
		}
		return Chain(h, middleware...)
	}

	// Session management needs no permission beyond being signed in: these act
	// on the caller's own session.
	mux.Handle("POST /api/v1/auth/logout", authed(s.logout))
	mux.Handle("POST /api/v1/auth/logout-all", authed(s.logoutAll))
	mux.Handle("POST /api/v1/auth/change-password", authed(s.changePassword))
	mux.Handle("GET /api/v1/auth/me", authed(s.me))

	mux.Handle("GET /api/v1/devices", authed(s.listDevices, permDeviceRead))
	mux.Handle("GET /api/v1/devices/summary", authed(s.deviceSummary, permDeviceRead))
	mux.Handle("GET /api/v1/devices/{id}", authed(s.getDevice, permDeviceRead))
	mux.Handle("PATCH /api/v1/devices/{id}", authed(s.updateDevice, permDeviceWrite))
	mux.Handle("POST /api/v1/devices/{id}/approve", authed(s.approveDevice, permDeviceApprove))
	mux.Handle("POST /api/v1/devices/{id}/reject", authed(s.rejectDevice, permDeviceApprove))
	mux.Handle("DELETE /api/v1/devices/{id}", authed(s.decommissionDevice, permDeviceDelete))

	// The realtime stream. A browser cannot set headers on a WebSocket upgrade,
	// so this route — and only this route — also accepts the access token as a
	// query parameter. Topics are still authorised individually at subscribe
	// time.
	mux.Handle("GET /api/v1/ws",
		Chain(Handler(s.events), AuthenticateWebSocket(s.authn)))

	// Live SSH sessions across the fleet, and the ability to end one.
	mux.Handle("GET /api/v1/ssh/sessions", authed(s.listSSHSessions, permSSHRead))
	mux.Handle("DELETE /api/v1/ssh/sessions/{id}", authed(s.terminateSSHSession, permSSHConnect))
	mux.Handle("GET /api/v1/devices/{id}/ssh-sessions", authed(s.listDeviceSSHSessions, permSSHRead))

	// Device health. Reading a device's metrics needs no more than reading the
	// device: the readings are what "reading a device" means to an operator.
	mux.Handle("GET /api/v1/devices/{id}/metrics", authed(s.getDeviceMetrics, permDeviceRead))
	mux.Handle("GET /api/v1/devices/{id}/metrics/latest", authed(s.getLatestMetrics, permDeviceRead))

	// Operators, roles, and the organization itself.
	mux.Handle("GET /api/v1/users", authed(s.listUsers, permUserRead))
	mux.Handle("POST /api/v1/users", authed(s.createUser, permUserWrite))
	mux.Handle("GET /api/v1/users/{id}", authed(s.getUser, permUserRead))
	mux.Handle("PATCH /api/v1/users/{id}", authed(s.updateUser, permUserWrite))
	mux.Handle("DELETE /api/v1/users/{id}", authed(s.deleteUser, permUserWrite))
	mux.Handle("GET /api/v1/roles", authed(s.listRoles, permUserRead))
	mux.Handle("GET /api/v1/permissions", authed(s.listPermissions, permUserRead))
	// Any signed-in member may read the organization they belong to.
	mux.Handle("GET /api/v1/organization", authed(s.getOrganization))
	mux.Handle("PATCH /api/v1/organization", authed(s.updateOrganization, permOrgWrite))

	// The browser terminal. A WebSocket, so — like the event stream — it also
	// accepts the access token as a query parameter, because a browser cannot
	// set headers on an upgrade.
	mux.Handle("GET /api/v1/devices/{id}/terminal",
		Chain(Handler(s.deviceTerminal), AuthenticateWebSocket(s.authn), Require(permSSHConnect)))

	// Remote screen (VNC). Turning it on is a device write; opening a session is
	// its own permission, so reaching a desktop is a separate grant from
	// reaching a shell. The relay is a WebSocket, so it too takes the token as a
	// query parameter.
	mux.Handle("POST /api/v1/devices/{id}/screen-sharing", authed(s.setScreenSharing, permScreenConnect))
	mux.Handle("GET /api/v1/devices/{id}/screen",
		Chain(Handler(s.deviceScreen), AuthenticateWebSocket(s.authn), Require(permScreenConnect)))
	mux.Handle("GET /api/v1/ssh/sessions/{id}/recording",
		authed(s.downloadRecording, permSSHRead))

	// File transfer over the SSH tunnel. Gated on ssh:connect — it is the same
	// SSH channel a shell uses, so reaching files is the same grant as reaching
	// a shell, not a broader one.
	mux.Handle("GET /api/v1/devices/{id}/files", authed(s.listFiles, permSSHConnect))
	mux.Handle("GET /api/v1/devices/{id}/files/download", authed(s.downloadFile, permSSHConnect))
	mux.Handle("POST /api/v1/devices/{id}/files/upload", authed(s.uploadFile, permSSHConnect))

	mux.Handle("GET /api/v1/enrollment-tokens", authed(s.listEnrollmentTokens, permEnrollTokenRead))
	mux.Handle("POST /api/v1/enrollment-tokens", authed(s.createEnrollmentToken, permEnrollTokenWrite))
	mux.Handle("DELETE /api/v1/enrollment-tokens/{id}", authed(s.revokeEnrollmentToken, permEnrollTokenWrite))

	// Anything unmatched. Without this, net/http renders its own plain-text 404,
	// which breaks a client that expects Problem Details everywhere.
	mux.Handle("/", Handler(s.notFound))

	return mux
}
