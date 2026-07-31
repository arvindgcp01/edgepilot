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

	mux.Handle("GET /api/v1/enrollment-tokens", authed(s.listEnrollmentTokens, permEnrollTokenRead))
	mux.Handle("POST /api/v1/enrollment-tokens", authed(s.createEnrollmentToken, permEnrollTokenWrite))
	mux.Handle("DELETE /api/v1/enrollment-tokens/{id}", authed(s.revokeEnrollmentToken, permEnrollTokenWrite))

	// Anything unmatched. Without this, net/http renders its own plain-text 404,
	// which breaks a client that expects Problem Details everywhere.
	mux.Handle("/", Handler(s.notFound))

	return mux
}
