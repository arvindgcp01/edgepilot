package http

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/service/auth"
)

// refreshCookie is where the refresh token lives.
//
// A cookie rather than the response body, and HttpOnly, because a refresh token
// in JavaScript's reach turns any XSS into permanent account takeover: the
// access token expires in fifteen minutes, the refresh token does not.
//
// Path is scoped to the auth endpoints, so it is not attached to every API call
// that has no use for it.
const (
	refreshCookie     = "edgepilot_refresh"
	refreshCookiePath = "/api/v1/auth"
)

// AuthService is what the auth handlers need. Declared here so this package
// depends on behaviour rather than on the service type.
type AuthService interface {
	Login(ctx context.Context, req auth.LoginRequest) (*auth.TokenPair, error)
	Refresh(ctx context.Context, refreshToken, userAgent, ip string) (*auth.TokenPair, error)
	Logout(ctx context.Context, sessionID uuid.UUID) error
	LogoutAll(ctx context.Context, userID uuid.UUID) (int, error)
	ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error
	// CreateUser adds an account and returns its one-time password.
	CreateUser(ctx context.Context, in domain.CreateUserInput) (*domain.User, string, error)
}

// TenantResolver decides which organization a sign-in belongs to.
//
// # Why this exists at all
//
// Credentials are scoped to an organization: the same email can belong to two
// customers, and a password that works for one must not authenticate against
// the other. But the login request cannot simply be trusted to name its
// organization — that would let a caller aim an attempt at any tenant.
//
// So the organization is resolved, in this order:
//
//  1. An explicit `organization` slug in the request. It selects a tenant; it
//     does not grant anything, because the password still has to match that
//     tenant's record.
//  2. The request's host, for deployments that give each customer a subdomain.
//  3. The only organization, when the deployment has exactly one — which is the
//     common self-hosted case, and the one where asking an operator to type
//     their own tenant slug would be absurd.
//
// If none resolves, the attempt fails as invalid credentials rather than as
// "unknown organization". The difference matters: the second answer tells an
// unauthenticated caller which tenants exist.
type TenantResolver struct {
	orgs domain.OrganizationRepository
	// baseDomain enables subdomain resolution: with "edgepilot.example.com", a
	// request to acme.edgepilot.example.com resolves the slug "acme". Empty
	// disables it.
	baseDomain string
}

// NewTenantResolver builds a resolver.
func NewTenantResolver(orgs domain.OrganizationRepository, baseDomain string) *TenantResolver {
	return &TenantResolver{orgs: orgs, baseDomain: strings.ToLower(strings.TrimSpace(baseDomain))}
}

// Resolve returns the organization a sign-in should be checked against.
func (t *TenantResolver) Resolve(ctx context.Context, r *http.Request, slug string) (uuid.UUID, error) {
	if slug != "" {
		org, err := t.orgs.GetBySlug(ctx, strings.ToLower(strings.TrimSpace(slug)))
		if err != nil {
			return uuid.Nil, err
		}
		return org.ID, nil
	}

	if fromHost := t.slugFromHost(r.Host); fromHost != "" {
		org, err := t.orgs.GetBySlug(ctx, fromHost)
		if err == nil {
			return org.ID, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return uuid.Nil, err
		}
		// An unrecognised subdomain falls through rather than failing: a
		// deployment may serve both a bare hostname and per-tenant ones.
	}

	// Two is enough to know whether there is exactly one.
	page, err := t.orgs.List(ctx, domain.PageRequest{Limit: 2})
	if err != nil {
		return uuid.Nil, err
	}
	if len(page.Items) == 1 {
		return page.Items[0].ID, nil
	}
	return uuid.Nil, domain.ErrNotFound
}

// slugFromHost extracts a tenant slug from a subdomain.
func (t *TenantResolver) slugFromHost(host string) string {
	if t.baseDomain == "" || host == "" {
		return ""
	}
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	suffix := "." + t.baseDomain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	slug := strings.TrimSuffix(host, suffix)
	// Only a single label. A deeper name is not a tenant slug, and treating
	// "a.b" as one would make lookups depend on how DNS happens to be arranged.
	if slug == "" || strings.Contains(slug, ".") {
		return ""
	}
	return slug
}

// loginRequest mirrors the contract in openapi.yaml.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	MFACode  string `json:"mfa_code"`
	// Organization is the tenant slug, needed only where a deployment serves
	// more than one and does not use per-tenant hostnames.
	Organization string `json:"organization"`
}

type tokenPairResponse struct {
	AccessToken string            `json:"access_token"`
	TokenType   string            `json:"token_type"`
	ExpiresIn   int               `json:"expires_in"`
	Principal   principalResponse `json:"principal"`
}

type principalResponse struct {
	UserID      string   `json:"user_id"`
	OrgID       string   `json:"org_id"`
	Email       string   `json:"email"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
}

// login exchanges credentials for a token pair.
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var req loginRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	// The generic failure, used for every rejection that is not a lockout or a
	// missing MFA code. A caller learns that authentication failed and nothing
	// else — not whether the account exists, not whether the tenant does.
	fail := domain.Unauthenticated("invalid_credentials")

	ip := ClientIP(r)
	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Rate limiting happens before any database work, so a flood costs a map
	// lookup rather than a password hash. Both keys are checked: per-source
	// stops one host spraying many accounts, per-account stops a botnet
	// brute-forcing one.
	for _, key := range []string{"ip:" + ip, "account:" + email} {
		if allowed, retryAfter := s.loginLimiter.Allow(key); !allowed {
			return domain.RateLimited(retryAfter)
		}
	}

	orgID, err := s.tenants.Resolve(r.Context(), r, req.Organization)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The tenant does not exist. Run the login anyway with no
			// organization: the service pays its usual dummy-hash cost for a
			// request it cannot resolve, so an unknown tenant does not return
			// measurably faster than a known one. Doing this through the real
			// path rather than a sleep means the two timings cannot drift
			// apart as the hasher's parameters change.
			_, loginErr := s.auth.Login(r.Context(), auth.LoginRequest{
				Email: email, Password: req.Password, UserAgent: userAgent(r), IP: ip,
			})
			if loginErr != nil {
				return loginErr
			}
			return fail
		}
		return err
	}

	pair, err := s.auth.Login(r.Context(), auth.LoginRequest{
		OrgID:     orgID,
		Email:     email,
		Password:  req.Password,
		MFACode:   strings.TrimSpace(req.MFACode),
		UserAgent: userAgent(r),
		IP:        ip,
	})
	if err != nil {
		return err
	}

	// A successful sign-in clears the account's budget. Without this a user who
	// mistyped four times and then succeeded would stay one attempt from a
	// lockout for the rest of the window.
	s.loginLimiter.Reset("account:" + email)
	s.loginLimiter.Reset("ip:" + ip)

	s.setRefreshCookie(w, r, pair.RefreshToken)
	return respond(w, http.StatusOK, tokenPairFrom(pair))
}

// refresh rotates the refresh token and mints a new access token.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) error {
	token := s.refreshTokenFrom(r)
	if token == "" {
		return domain.Unauthenticated("invalid_credentials")
	}

	// Rate limited by source only: there is no account to key on until the
	// token is validated, and validating it is the expensive part.
	if allowed, retryAfter := s.loginLimiter.Allow("refresh:" + ClientIP(r)); !allowed {
		return domain.RateLimited(retryAfter)
	}

	pair, err := s.auth.Refresh(r.Context(), token, userAgent(r), ClientIP(r))
	if err != nil {
		// A rejected refresh clears the cookie. Leaving a dead token in the
		// browser means every subsequent page load retries it and fails the
		// same way, which looks like the server being broken.
		s.clearRefreshCookie(w, r)
		return err
	}

	s.setRefreshCookie(w, r, pair.RefreshToken)
	return respond(w, http.StatusOK, tokenPairFrom(pair))
}

// logout ends the current session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}

	if err := s.auth.Logout(r.Context(), principal.SessionID); err != nil {
		return err
	}
	s.clearRefreshCookie(w, r)
	return noContent(w)
}

// logoutAll ends every session for the caller.
func (s *Server) logoutAll(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}

	count, err := s.auth.LogoutAll(r.Context(), principal.UserID)
	if err != nil {
		return err
	}
	s.clearRefreshCookie(w, r)
	return respond(w, http.StatusOK, map[string]int{"sessions_revoked": count})
}

// me returns the authenticated principal.
func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, renderPrincipal(principal))
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword updates the caller's own password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}

	var req changePasswordRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	if err := s.auth.ChangePassword(r.Context(),
		principal.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		return err
	}

	// Every other session is revoked by the service. Clearing the cookie here
	// keeps the browser consistent with that.
	s.clearRefreshCookie(w, r)
	return noContent(w)
}

// --- cookie handling -------------------------------------------------------

func (s *Server) setRefreshCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:  refreshCookie,
		Value: token,
		Path:  refreshCookiePath,
		// The refresh token outlives the access token by design; the cookie
		// matches the session's own lifetime so a browser does not keep sending
		// one the server has already forgotten.
		Expires:  s.now().Add(s.refreshTTL),
		MaxAge:   int(s.refreshTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies(r),
		// Strict, not Lax: nothing here is reached by following a link from
		// another site, and Lax would attach this cookie to a top-level GET
		// from anywhere.
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookie,
		Value:    "",
		Path:     refreshCookiePath,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// refreshTokenFrom reads the refresh token from the cookie, falling back to the
// body for non-browser clients.
func (s *Server) refreshTokenFrom(r *http.Request) string {
	if cookie, err := r.Cookie(refreshCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}

	// epctl and CI integrations have no cookie jar. They send the token in the
	// body, which is safe for them because the XSS that makes a body-carried
	// token dangerous requires a browser.
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(r, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.RefreshToken)
}

// secureCookies decides whether to set the Secure attribute.
//
// Always true in production. A plain-HTTP request to localhost is the exception,
// because a Secure cookie is silently dropped there and would make local
// development impossible in a way that looks like a broken login.
func (s *Server) secureCookies(r *http.Request) bool {
	if s.insecureCookies {
		return false
	}
	// Behind a TLS-terminating proxy the request arrives as plain HTTP, so the
	// original scheme is what matters. RealIP resolves it, honouring
	// X-Forwarded-Proto only from a trusted proxy — for the same reason
	// X-Forwarded-For is only honoured there.
	if clientScheme(r) == "https" {
		return true
	}
	return !isLocalhost(r.Host)
}

func isLocalhost(host string) bool {
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func userAgent(r *http.Request) string {
	ua := r.UserAgent()
	// Stored on the session and shown to the user in a session list, so it is
	// bounded rather than trusted.
	if len(ua) > 256 {
		return ua[:256]
	}
	return ua
}

func tokenPairFrom(pair *auth.TokenPair) tokenPairResponse {
	return tokenPairResponse{
		AccessToken: pair.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   pair.ExpiresIn,
		Principal:   renderPrincipal(pair.Principal),
	}
}

func renderPrincipal(p *domain.Principal) principalResponse {
	permissions := make([]string, 0, len(p.Permissions))
	for name := range p.Permissions {
		permissions = append(permissions, name)
	}
	slices.Sort(permissions)

	return principalResponse{
		UserID:      p.UserID.String(),
		OrgID:       p.OrgID.String(),
		Email:       p.Email,
		Roles:       p.Roles,
		Permissions: permissions,
	}
}
