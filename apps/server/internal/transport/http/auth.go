package http

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
)

// Authenticator verifies an access token.
//
// Declared here and satisfied by auth.Service, so the transport depends on a
// method rather than on the service.
type Authenticator interface {
	Authenticate(ctx context.Context, accessToken string) (*domain.Principal, error)
}

// Authenticate rejects a request without a valid access token.
//
// The principal it produces carries the organization, which every scoped
// repository call reads from the context. That is what makes tenancy a property
// of the token rather than of a parameter a handler might forget.
func Authenticate(a Authenticator) Middleware {
	return func(next http.Handler) http.Handler {
		return Handler(func(w http.ResponseWriter, r *http.Request) error {
			token, err := bearerToken(r)
			if err != nil {
				return err
			}

			principal, err := a.Authenticate(r.Context(), token)
			if err != nil {
				return err
			}

			ctx := domain.WithPrincipal(r.Context(), principal)
			// The org scope every repository call reads. Set from the token and
			// nowhere else: a request cannot name the organization it wants.
			ctx = domain.WithOrg(ctx, principal.OrgID)
			// The actor every log line and audit entry is attributed to.
			ctx = logging.WithActor(ctx, logging.Actor{
				UserID: principal.UserID.String(),
				OrgID:  principal.OrgID.String(),
			})

			next.ServeHTTP(w, r.WithContext(ctx))
			return nil
		})
	}
}

// AuthenticateWebSocket is Authenticate, plus an access_token query parameter.
//
// # Why this exists and why it is one route only
//
// The browser's WebSocket API cannot set request headers. A dashboard therefore
// has no way to send an Authorization header on the upgrade, and the token has
// to travel in the URL.
//
// That is a real cost — query strings appear in proxy and access logs where
// headers do not — so it is confined to this single route rather than accepted
// everywhere. The exposure is bounded by what is being sent: a fifteen-minute
// access token, never the refresh token, which stays an HttpOnly cookie and
// never appears in a URL at all.
//
// The token is also kept out of *this* server's access log: the log records the
// matched route pattern, not the raw query.
func AuthenticateWebSocket(a Authenticator) Middleware {
	return func(next http.Handler) http.Handler {
		return Handler(func(w http.ResponseWriter, r *http.Request) error {
			token, err := bearerToken(r)
			if err != nil {
				token = strings.TrimSpace(r.URL.Query().Get("access_token"))
				if token == "" {
					return domain.Unauthenticated("authentication required")
				}
			}

			principal, err := a.Authenticate(r.Context(), token)
			if err != nil {
				return err
			}

			ctx := domain.WithPrincipal(r.Context(), principal)
			ctx = domain.WithOrg(ctx, principal.OrgID)
			ctx = logging.WithActor(ctx, logging.Actor{
				UserID: principal.UserID.String(),
				OrgID:  principal.OrgID.String(),
			})

			next.ServeHTTP(w, r.WithContext(ctx))
			return nil
		})
	}
}

// bearerToken extracts the credential from the Authorization header.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", domain.Unauthenticated("authentication required")
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		// The scheme is checked rather than assumed: a client sending Basic
		// credentials should be told the request is unauthenticated, not have
		// its password parsed as a JWS.
		return "", domain.Unauthenticated("authentication required")
	}
	return strings.TrimSpace(token), nil
}

// Require rejects a request whose principal lacks a permission.
//
// Applied per-route rather than per-handler, so the permission a route needs is
// visible in the routing table — where it can be reviewed against the
// permissions catalogue — instead of buried in a handler.
func Require(permission string) Middleware {
	return func(next http.Handler) http.Handler {
		return Handler(func(w http.ResponseWriter, r *http.Request) error {
			principal, err := principalFrom(r.Context())
			if err != nil {
				return err
			}
			if err := principal.Authorize(permission); err != nil {
				return err
			}
			next.ServeHTTP(w, r)
			return nil
		})
	}
}

// RateLimiter bounds attempts per key over a window.
//
// # Why this is here and not in front of everything
//
// This limiter exists for the authentication endpoints specifically. A general
// per-request limiter belongs at the edge — a load balancer sees traffic this
// process never does — but credential stuffing has to be stopped by something
// that knows what an account is, and that is only true here.
//
// # Two keys, not one
//
// Per-account, so one account cannot be brute-forced from a botnet. Per-source,
// so one source cannot spray many accounts. Either alone is trivially evaded:
// an attacker with a thousand addresses defeats the per-source limit, and an
// attacker with a thousand accounts defeats the per-account one.
type RateLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	// lastSweep bounds how often expired buckets are collected, so a burst of
	// distinct keys does not turn every request into a full map scan.
	lastSweep time.Time
}

type bucket struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter builds a limiter allowing limit attempts per window per key.
func NewRateLimiter(limit int, window time.Duration, now func() time.Time) *RateLimiter {
	if limit <= 0 {
		limit = 10
	}
	if window <= 0 {
		window = time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		limit: limit, window: window, now: now,
		buckets: map[string]*bucket{}, lastSweep: now(),
	}
}

// Allow records an attempt and reports whether it is within the limit.
//
// Returns the seconds until the window resets, for Retry-After.
func (l *RateLimiter) Allow(key string) (allowed bool, retryAfter int) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		l.buckets[key] = &bucket{count: 1, resetAt: now.Add(l.window)}
		return true, 0
	}

	b.count++
	if b.count > l.limit {
		remaining := int(b.resetAt.Sub(now).Seconds())
		if remaining < 1 {
			remaining = 1
		}
		return false, remaining
	}
	return true, 0
}

// Reset clears a key, called after a successful authentication.
//
// Without it, a user who mistyped their password four times and then succeeded
// would still be one attempt from a lockout for the rest of the window.
func (l *RateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// sweep discards expired buckets.
//
// Without it the map grows with every distinct source address seen, which for a
// public endpoint under a scan is unbounded memory.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if now.After(b.resetAt) {
			delete(l.buckets, key)
		}
	}
}

// Size reports how many keys are being tracked, for metrics and tests.
func (l *RateLimiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
