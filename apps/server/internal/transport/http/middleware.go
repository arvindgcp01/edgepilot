// Package http is EdgePilot's user plane: the REST API the dashboard, epctl,
// and any CI integration speak.
//
// Agents do not come here. They use the mTLS device plane, with the single
// exception of POST /agent/enroll — which exists because an unenrolled agent has
// no client certificate and so cannot reach the device plane at all.
//
// # Shape
//
// Routing is net/http's ServeMux with method patterns. No third-party router:
// the standard library has done path variables and method matching since 1.22,
// and a router dependency here would buy nothing while adding a supply-chain
// surface to the process that holds the device CA.
//
// Handlers return errors; the middleware renders them. Every error leaves this
// package as RFC 9457 Problem Details with the internal cause stripped, which is
// enforced in one place rather than trusted to every handler.
package http

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
	"github.com/visionify/edgepilot/apps/server/internal/transport/http/problem"
)

// Handler is a handler that can fail.
//
// Returning an error rather than writing one is what makes the Problem Details
// rendering unavoidable: a handler cannot accidentally leak an internal cause,
// because it never writes the error itself.
type Handler func(w http.ResponseWriter, r *http.Request) error

// ServeHTTP renders whatever the handler returns.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		problem.Write(w, r, err)
	}
}

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware in the order given, so the first is outermost.
//
// Order matters and is not arbitrary: recovery must be outside logging (a panic
// in a handler still produces an access log), and logging must be outside
// authentication (a rejected request is still logged).
func Chain(h http.Handler, middleware ...Middleware) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}

// RequestID assigns each request an identifier and carries it in the context and
// the response.
//
// A client-supplied X-Request-ID is honoured so a trace spans the dashboard and
// the server, but it is length-capped and sanitised: it ends up in logs, and a
// header is attacker-controlled input.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
		})
	}
}

// sanitizeRequestID keeps a client-supplied id only if it is plausibly one.
func sanitizeRequestID(raw string) string {
	if len(raw) == 0 || len(raw) > 128 {
		return ""
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			// Anything else — a newline, a control character — could forge a
			// log line.
			return ""
		}
	}
	return raw
}

// AccessLog records one line per request.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			holder := &routeHolder{}
			r = r.WithContext(context.WithValue(r.Context(), routeHolderKey{}, holder))

			next.ServeHTTP(rec, r)

			// The request's own context, which carries the request id every
			// line is correlated by. It may already be cancelled — a client
			// that hung up mid-response — and that is fine: LogAttrs reads
			// values from it and never waits on it.
			ctx := r.Context()

			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}

			log.LogAttrs(ctx, level, "request", //nolint:contextcheck // the request's own context, captured above
				slog.String("method", r.Method),
				// The matched pattern, not the raw path: a path carries device
				// ids and email addresses, and the pattern is what a dashboard
				// groups by anyway.
				slog.String("route", routeOf(r)),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start).Round(time.Microsecond)),
				slog.String("ip", ClientIP(r)))
		})
	}
}

// routeHolder carries the matched pattern back out to the access log.
//
// # Why this is needed at all
//
// ServeMux sets Request.Pattern on the request it dispatches. Every middleware
// between the log and the mux that adds something to the context — the request
// id, the resolved client address, the authenticated principal — does so with
// WithContext, which returns a *new* Request. The mux then stamps the pattern
// onto that one, and the outer request the access log still holds never learns
// it.
//
// The symptom is quiet and total: every line logs `route=unmatched`, including
// the ones that matched perfectly, and grouping requests by route in a dashboard
// silently returns one bucket.
type routeHolder struct{ pattern string }

type routeHolderKey struct{}

// CaptureRoute records the matched pattern for the access log.
//
// Must be installed as the *innermost* middleware, directly around the mux: it
// works precisely because it does not replace the request, so the pointer the
// mux stamps is the one it reads back.
func CaptureRoute() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if holder, ok := r.Context().Value(routeHolderKey{}).(*routeHolder); ok {
				holder.pattern = r.Pattern
			}
		})
	}
}

// routeOf returns the matched route pattern, falling back to a marker for a
// request that matched nothing.
func routeOf(r *http.Request) string {
	if holder, ok := r.Context().Value(routeHolderKey{}).(*routeHolder); ok && holder.pattern != "" {
		return holder.pattern
	}
	if pattern := r.Pattern; pattern != "" {
		return pattern
	}
	return "unmatched"
}

// Recover turns a panic into a 500 rather than a dropped connection.
//
// A panic in one handler must not take the process down: this server holds every
// agent stream in the fleet, and restarting it to recover from one bad request
// disconnects ten thousand Macs.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			//nolint:contextcheck // the deferred func closes over this request
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				// http.ErrAbortHandler is the documented way to abandon a
				// response deliberately. Logging it as a crash would fill the
				// log with noise from clients that hung up.
				if recovered == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(recovered)
				}

				log.ErrorContext(r.Context(), "a handler panicked",
					slog.Any("panic", recovered),
					slog.String("route", routeOf(r)),
					slog.String("stack", string(debug.Stack())))

				problem.Write(w, r, domain.Internal(errPanic))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

var errPanic = &panicError{}

type panicError struct{}

func (*panicError) Error() string { return "a handler panicked" }

// SecurityHeaders sets the headers a browser needs to not undermine the API.
//
// The dashboard is a separate origin in development and the same one in
// production, so the policy here is deliberately API-shaped: nothing is
// embedded, nothing is framed, and no content type is ever sniffed.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			// A JSON response mis-sniffed as HTML is how a reflected value
			// becomes stored XSS.
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			// This API returns no HTML and loads nothing. Saying so costs
			// nothing and closes the case where an error page does.
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			// Nothing here is cacheable: every response is either a token, a
			// tenant-scoped list, or a device record.
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

// CORS handles cross-origin requests from the dashboard.
//
// Origins are an explicit allowlist. There is no wildcard and no reflection of
// arbitrary origins: this API is authenticated with a bearer token and a cookie,
// and an echoing CORS policy is what turns that into a cross-site request
// forgery.
func CORS(allowed []string) Middleware {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, origin := range allowed {
		allowSet[strings.TrimRight(origin, "/")] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Same-origin or a non-browser client. Nothing to negotiate.
				next.ServeHTTP(w, r)
				return
			}

			if _, ok := allowSet[strings.TrimRight(origin, "/")]; !ok {
				// Not allowed. The request still runs — CORS is enforced by the
				// browser, and refusing here would break non-browser clients
				// that send an Origin — but no header invites the browser to
				// expose the response.
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			// The response varies by Origin, so a shared cache must not serve
			// one origin's response to another.
			h.Add("Vary", "Origin")

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers",
					"Authorization, Content-Type, X-Request-ID, Idempotency-Key")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// MaxBody caps the request body.
//
// Applied before any handler reads: a handler that decodes JSON from an
// unbounded reader is a memory exhaustion waiting for one large POST.
func MaxBody(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds how long a handler may run.
//
// Separate from the server's WriteTimeout, which cannot distinguish a slow
// client from a slow handler. This one cancels the context, so a query that
// outlives its request is cancelled at the database rather than left running.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// recorder captures the status and size for the access log.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer, so http.ResponseController can still
// reach Flush and Hijack — which the WebSocket upgrade and any streaming
// response need.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// clientIPKey carries the resolved client address.
type clientIPKey struct{}

// forwardedProtoKey carries the scheme the original client used.
type forwardedProtoKey struct{}

// RealIP resolves the address to attribute a request to, once, at the edge.
//
// # Why a middleware and not a helper
//
// The answer depends on which proxies this deployment trusts, and that is
// per-server configuration. Holding it in a package-level variable — which is
// what this was, and what the race detector found — means two servers in one
// process share one setting, and the last one constructed silently wins.
//
// # Why the last entry of X-Forwarded-For
//
// The header is a list, appended to by each hop. Everything except the final
// entry was supplied by whoever spoke to the trusted proxy, which includes the
// client. Trusting the first entry — the common mistake — lets any caller
// choose the address that appears in the audit log and the address every
// per-IP rate limit counts against.
func RealIP(trusted []net.IPNet) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			peer := remoteHost(r)
			ip := peer
			proto := "http"
			if r.TLS != nil {
				proto = "https"
			}

			if isTrustedProxy(net.ParseIP(peer), trusted) {
				if forwarded := lastForwardedFor(r); forwarded != "" {
					ip = forwarded
				}
				if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
					if strings.EqualFold(scheme, "https") {
						proto = "https"
					} else if strings.EqualFold(scheme, "http") {
						proto = "http"
					}
				}
			}

			ctx = context.WithValue(ctx, clientIPKey{}, ip)
			ctx = context.WithValue(ctx, forwardedProtoKey{}, proto)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIP returns the address to attribute a request to.
//
// Resolved by RealIP. Without that middleware this falls back to the peer
// address, which is correct for a server with no proxy in front of it.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok && ip != "" {
		return ip
	}
	return remoteHost(r)
}

// clientScheme reports whether the original client used TLS.
func clientScheme(r *http.Request) string {
	if proto, ok := r.Context().Value(forwardedProtoKey{}).(string); ok && proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func lastForwardedFor(r *http.Request) string {
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return ""
	}
	parts := strings.Split(forwarded, ",")
	candidate := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(candidate) == nil {
		return ""
	}
	return candidate
}

func isTrustedProxy(ip net.IP, trusted []net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, network := range trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// principalFrom returns the authenticated principal, or an error suitable for
// returning straight from a handler.
func principalFrom(ctx context.Context) (*domain.Principal, error) {
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		// Reachable only if a route is registered without the authentication
		// middleware, which is a wiring bug — but serving it would mean an
		// unauthenticated request reaching a scoped handler.
		return nil, domain.Unauthenticated("authentication required")
	}
	return p, nil
}
