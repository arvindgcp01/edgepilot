package http_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
	"github.com/visionify/edgepilot/apps/server/internal/platform/password"
	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/service/auth"
	"github.com/visionify/edgepilot/apps/server/internal/service/enrollment"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
	ephttp "github.com/visionify/edgepilot/apps/server/internal/transport/http"
)

// The API is exercised against the real services and a real database: what is
// worth testing here is that a request arrives at the right service with the
// right tenant scope and that the response says what the contract says it says.
// A handler tested against a stubbed store would prove none of that — and the
// one bug this layer can have that matters most, serving another tenant's
// device, only exists when a real query runs under a real policy.

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

type fixture struct {
	server *ephttp.Server
	store  *postgres.Store
	auth   *auth.Service

	org   *domain.Organization
	admin adminUser
}

type adminUser struct {
	id       uuid.UUID
	email    string
	password string
}

const testPassword = "correct-horse-battery-staple"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithLogger(t, quiet())
}

// newFixtureWithLogger builds a fixture whose server logs somewhere a test can
// read, for the assertions about what the access log records.
func newFixtureWithLogger(t *testing.T, log *slog.Logger) *fixture {
	t.Helper()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN, MaxConns: 2, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Argon2id at production parameters would make every login in this file
	// take a quarter of a second. The minimum RFC 9106 permits keeps the
	// algorithm and the code path identical while the suite stays usable.
	hasher, err := password.New(password.Params{
		MemoryKiB: 8 << 10, Iterations: 1, Parallelism: 1,
	})
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}

	_, priv, err := ed25519GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := token.NewIssuer(token.Options{
		PrivateKey: priv, Issuer: "edgepilot-test", Audience: "edgepilot-api",
		TTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}

	sealer, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}

	authService, err := auth.New(auth.Options{
		Store: store, Hasher: hasher, Issuer: issuer, Sealer: sealer, Logger: quiet(),
		RefreshTTL: time.Hour, LockoutDuration: time.Minute, MaxFailedLogins: 5,
	})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	authority := testAuthority(t)
	enrollService, err := enrollment.New(enrollment.Options{
		Store: store, CA: authority, Logger: quiet(),
		DevicePlaneEndpoint: "edgepilot.test:9443",
	})
	if err != nil {
		t.Fatalf("enrollment: %v", err)
	}

	org, admin := seedOrganization(t, store, hasher)

	server, err := ephttp.New(ephttp.Options{
		Auth: authService, Authenticator: authService, Enrollment: enrollService,
		Store: store, Logger: log,
		// Loopback in tests, so the Secure attribute would make the refresh
		// cookie unusable.
		InsecureCookies: true,
		LoginAttempts:   50,
		LoginWindow:     time.Minute,
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	return &fixture{server: server, store: store, auth: authService, org: org, admin: admin}
}

// do sends a request to the API and returns the response.
func (f *fixture) do(t *testing.T, method, path string, body any, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}

	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, req)
	return rec
}

func withToken(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// signIn authenticates the seeded administrator and returns the access token.
func (f *fixture) signIn(t *testing.T) string {
	t.Helper()

	rec := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": f.admin.email, "password": f.admin.password,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	decodeBody(t, rec, &body)
	if body.AccessToken == "" {
		t.Fatal("login returned no access token")
	}
	return body.AccessToken
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body.String())
	}
}

// --- health ----------------------------------------------------------------

// Liveness must not depend on the database. Restarting a healthy replica
// because PostgreSQL blipped would drop every agent stream it holds.
func TestLivenessDoesNotDependOnTheDatabase(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rec := f.do(t, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d with the database down, want 200", rec.Code)
	}
}

// Readiness must, because a replica that cannot reach PostgreSQL should leave
// the load balancer rather than serve 500s.
func TestReadinessDependsOnTheDatabase(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if rec := f.do(t, http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d while healthy, want 200", rec.Code)
	}

	if err := f.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if rec := f.do(t, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d with the database down, want 503", rec.Code)
	}
}

// --- authentication --------------------------------------------------------

func TestLoginReturnsATokenAndSetsTheRefreshCookie(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": f.admin.email, "password": f.admin.password,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Principal   struct {
			Email       string   `json:"email"`
			OrgID       string   `json:"org_id"`
			Permissions []string `json:"permissions"`
		} `json:"principal"`
	}
	decodeBody(t, rec, &body)

	if body.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", body.TokenType)
	}
	if body.ExpiresIn <= 0 {
		t.Errorf("expires_in = %d, want positive", body.ExpiresIn)
	}
	if body.Principal.Email != f.admin.email {
		t.Errorf("principal email = %q, want %q", body.Principal.Email, f.admin.email)
	}
	if body.Principal.OrgID != f.org.ID.String() {
		t.Errorf("principal org = %q, want %q", body.Principal.OrgID, f.org.ID)
	}
	if len(body.Principal.Permissions) == 0 {
		t.Error("the principal carries no permissions; the dashboard cannot render a menu")
	}

	// The refresh token must not be in the body. A refresh token JavaScript can
	// read turns any XSS into permanent account takeover — the access token
	// expires, this one does not.
	if strings.Contains(rec.Body.String(), "refresh_token") {
		t.Error("the response body carries a refresh token")
	}

	var refresh *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "edgepilot_refresh" {
			refresh = cookie
		}
	}
	if refresh == nil {
		t.Fatal("no refresh cookie was set")
	}
	if !refresh.HttpOnly {
		t.Error("the refresh cookie is not HttpOnly")
	}
	if refresh.SameSite != http.SameSiteStrictMode {
		t.Errorf("the refresh cookie is SameSite=%v, want Strict", refresh.SameSite)
	}
	if refresh.Path != "/api/v1/auth" {
		t.Errorf("the refresh cookie path is %q; it should not be sent to every endpoint", refresh.Path)
	}
}

// An unknown email, a wrong password, and an unknown tenant must be
// indistinguishable — otherwise the login endpoint enumerates accounts.
func TestEveryAuthenticationFailureLooksTheSame(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	cases := map[string]map[string]string{
		"unknown email":    {"email": "nobody@example.com", "password": testPassword},
		"wrong password":   {"email": f.admin.email, "password": "not-the-password"},
		"unknown tenant":   {"email": f.admin.email, "password": testPassword, "organization": "no-such-org"},
		"empty password":   {"email": f.admin.email, "password": ""},
		"empty everything": {"email": "", "password": ""},
	}

	var bodies []string
	for name, payload := range cases {
		rec := f.do(t, http.MethodPost, "/api/v1/auth/login", payload)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}

	for i := 1; i < len(bodies); i++ {
		if normalizeProblem(bodies[i]) != normalizeProblem(bodies[0]) {
			t.Errorf("authentication failures are distinguishable:\n%s\nvs\n%s",
				bodies[0], bodies[i])
		}
	}
}

// normalizeProblem strips the per-request fields so two problems can be compared
// for the parts a client actually sees.
func normalizeProblem(body string) string {
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return body
	}
	delete(p, "instance")
	delete(p, "request_id")
	out, _ := json.Marshal(p)
	return string(out)
}

func TestRefreshRotatesTheToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	login := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": f.admin.email, "password": f.admin.password,
	})
	if login.Code != http.StatusOK {
		t.Fatalf("login: %s", login.Body.String())
	}

	cookie := cookieNamed(login, "edgepilot_refresh")
	if cookie == nil {
		t.Fatal("no refresh cookie")
	}

	rec := f.do(t, http.MethodPost, "/api/v1/auth/refresh", nil, func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d: %s", rec.Code, rec.Body.String())
	}

	rotated := cookieNamed(rec, "edgepilot_refresh")
	if rotated == nil {
		t.Fatal("refresh did not set a new cookie")
	}
	if rotated.Value == cookie.Value {
		t.Error("the refresh token was not rotated; single use is what makes reuse detectable")
	}

	// Presenting the old one again means it leaked. The family is revoked.
	replay := f.do(t, http.MethodPost, "/api/v1/auth/refresh", nil, func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if replay.Code != http.StatusUnauthorized {
		t.Errorf("replaying a rotated refresh token = %d, want 401", replay.Code)
	}
}

func TestMeReturnsTheCaller(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	rec := f.do(t, http.MethodGet, "/api/v1/auth/me", nil, withToken(accessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	decodeBody(t, rec, &body)
	if body.Email != f.admin.email {
		t.Errorf("email = %q, want %q", body.Email, f.admin.email)
	}
	if body.UserID != f.admin.id.String() {
		t.Errorf("user_id = %q, want %q", body.UserID, f.admin.id)
	}
}

func TestProtectedEndpointsRefuseAnythingButAValidBearerToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	cases := map[string]func(*http.Request){
		"no header":        func(*http.Request) {},
		"empty bearer":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"the wrong scheme": func(r *http.Request) { r.Header.Set("Authorization", "Basic abc123") },
		"not a token":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer nonsense") },
		"a forged token": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer eyJhbGciOiJub25lIn0.e30.")
		},
	}

	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, "/api/v1/devices", nil, opt)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	login := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": f.admin.email, "password": f.admin.password,
	})
	var body struct {
		AccessToken string `json:"access_token"`
	}
	decodeBody(t, login, &body)
	cookie := cookieNamed(login, "edgepilot_refresh")

	rec := f.do(t, http.MethodPost, "/api/v1/auth/logout", nil, withToken(body.AccessToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d: %s", rec.Code, rec.Body.String())
	}

	// The refresh token is dead, which is what actually ends the session: the
	// access token remains valid until it expires, by design and documented.
	after := f.do(t, http.MethodPost, "/api/v1/auth/refresh", nil, func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if after.Code != http.StatusUnauthorized {
		t.Errorf("refreshing after logout = %d, want 401", after.Code)
	}
}

// The limiter has to be in front of the password hash, or a flood costs a
// quarter-second of Argon2id per attempt.
func TestLoginIsRateLimited(t *testing.T) {
	t.Parallel()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN, MaxConns: 2, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hasher, _ := password.New(password.Params{MemoryKiB: 8 << 10, Iterations: 1, Parallelism: 1})
	_, priv, _ := ed25519GenerateKey()
	issuer, _ := token.NewIssuer(token.Options{PrivateKey: priv, Issuer: "t", Audience: "t", TTL: time.Minute})
	sealer, _ := secret.New(bytes.Repeat([]byte{3}, 32))
	authService, err := auth.New(auth.Options{
		Store: store, Hasher: hasher, Issuer: issuer, Sealer: sealer, Logger: quiet(),
		RefreshTTL: time.Hour, LockoutDuration: time.Minute, MaxFailedLogins: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedOrganization(t, store, hasher)

	server, err := ephttp.New(ephttp.Options{
		Auth: authService, Authenticator: authService, Store: store, Logger: quiet(),
		InsecureCookies: true, LoginAttempts: 3, LoginWindow: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	limited := false
	for i := range 6 {
		body, _ := json.Marshal(map[string]string{
			"email": "someone@example.com", "password": "wrong-password-entirely",
		})
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a rate-limited response carries no Retry-After")
			}
			break
		}
		if i >= 5 {
			t.Fatalf("six attempts with a limit of three were all accepted")
		}
	}
	if !limited {
		t.Error("the login endpoint is not rate limited")
	}
}

// --- devices ---------------------------------------------------------------

func TestDeviceListingIsScopedToTheCallersOrganization(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	mine := f.enrolDevice(t, f.org.ID, "MINE-1")

	// Another tenant, with a device of its own.
	other, err := f.store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Globex", Slug: "globex-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}
	theirs := f.enrolDevice(t, other.ID, "THEIRS-1")

	rec := f.do(t, http.MethodGet, "/api/v1/devices", nil, withToken(accessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data []struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
		} `json:"data"`
	}
	decodeBody(t, rec, &body)

	if len(body.Data) != 1 {
		t.Fatalf("the list holds %d devices, want exactly this tenant's one", len(body.Data))
	}
	if body.Data[0].ID != mine.String() {
		t.Errorf("listed %s, want %s", body.Data[0].ID, mine)
	}

	// And the other tenant's device is not merely hidden from the list — it is
	// unreachable by id, and answers 404 rather than 403, because a 403 would
	// confirm it exists.
	direct := f.do(t, http.MethodGet, "/api/v1/devices/"+theirs.String(), nil, withToken(accessToken))
	if direct.Code != http.StatusNotFound {
		t.Errorf("fetching another tenant's device = %d, want 404", direct.Code)
	}
}

func TestDeviceFiltersAreValidated(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	cases := map[string]string{
		"an unknown status":     "/api/v1/devices?status=exploded",
		"an unknown state":      "/api/v1/devices?enrollment_state=maybe",
		"an unsupported sort":   "/api/v1/devices?sort=hostname%3BDROP+TABLE+devices",
		"a malformed label":     "/api/v1/devices?label=nocolon",
		"a malformed cursor":    "/api/v1/devices?cursor=not-a-cursor",
		"a non-numeric limit":   "/api/v1/devices?limit=lots",
		"a malformed device id": "/api/v1/devices/not-a-uuid",
	}

	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, path, nil, withToken(accessToken))
			if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want a validation failure: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Errorf("content-type = %q, want application/problem+json", ct)
			}
		})
	}
}

func TestDeviceSummaryCountsTheFleet(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)
	f.enrolDevice(t, f.org.ID, "SUMMARY-1")
	f.enrolDevice(t, f.org.ID, "SUMMARY-2")

	rec := f.do(t, http.MethodGet, "/api/v1/devices/summary", nil, withToken(accessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Total           int `json:"total"`
		PendingApproval int `json:"pending_approval"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 2 {
		t.Errorf("total = %d, want 2", body.Total)
	}
	if body.PendingApproval != 2 {
		t.Errorf("pending = %d, want 2 — neither device was auto-approved", body.PendingApproval)
	}
}

func TestApprovingADeviceAdmitsItToTheFleet(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)
	deviceID := f.enrolDevice(t, f.org.ID, "APPROVE-1")

	rec := f.do(t, http.MethodPost,
		"/api/v1/devices/"+deviceID.String()+"/approve", nil, withToken(accessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Approved        bool   `json:"approved"`
		EnrollmentState string `json:"enrollment_state"`
	}
	decodeBody(t, rec, &body)
	if !body.Approved || body.EnrollmentState != "approved" {
		t.Errorf("approved = %v, state = %q", body.Approved, body.EnrollmentState)
	}
}

func TestUpdatingADeviceChangesOnlyOperatorOwnedFields(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)
	deviceID := f.enrolDevice(t, f.org.ID, "UPDATE-1")

	site := "London"
	rec := f.do(t, http.MethodPatch, "/api/v1/devices/"+deviceID.String(),
		map[string]any{"site": site, "labels": map[string]string{"ring": "canary"}},
		withToken(accessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Site   string            `json:"site"`
		Labels map[string]string `json:"labels"`
	}
	decodeBody(t, rec, &body)
	if body.Site != site {
		t.Errorf("site = %q, want %q", body.Site, site)
	}
	if body.Labels["ring"] != "canary" {
		t.Errorf("labels = %v", body.Labels)
	}

	// Hardware facts come from inventory. An operator editing the chip name
	// would make the record disagree with the machine, so the field is not
	// merely ignored — the request is refused.
	rejected := f.do(t, http.MethodPatch, "/api/v1/devices/"+deviceID.String(),
		map[string]any{"chip": "Apple M9 Ultra"}, withToken(accessToken))
	if rejected.Code != http.StatusUnprocessableEntity && rejected.Code != http.StatusBadRequest {
		t.Errorf("setting a hardware fact = %d, want a validation failure", rejected.Code)
	}
}

func TestDecommissioningRevokesAndHides(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)
	deviceID := f.enrolDevice(t, f.org.ID, "GONE-1")

	rec := f.do(t, http.MethodDelete, "/api/v1/devices/"+deviceID.String(), nil, withToken(accessToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("decommission = %d: %s", rec.Code, rec.Body.String())
	}

	after := f.do(t, http.MethodGet, "/api/v1/devices/"+deviceID.String(), nil, withToken(accessToken))
	if after.Code != http.StatusNotFound {
		t.Errorf("a decommissioned device answers %d, want 404", after.Code)
	}
}

// --- enrollment tokens -----------------------------------------------------

// The plaintext is returned once and never again: only its SHA-256 is stored,
// so a database disclosure yields nothing an attacker can enroll with.
func TestAnEnrollmentTokenIsShownExactlyOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	rec := f.do(t, http.MethodPost, "/api/v1/enrollment-tokens", map[string]any{
		"description": "Site rollout", "auto_approve": true,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, withToken(accessToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	var created struct {
		ID          string `json:"id"`
		Token       string `json:"token"`
		TokenPrefix string `json:"token_prefix"`
		AutoApprove bool   `json:"auto_approve"`
		Usable      bool   `json:"usable"`
	}
	decodeBody(t, rec, &created)

	if created.Token == "" {
		t.Fatal("the creation response carries no token")
	}
	if !strings.HasPrefix(created.Token, "ep_enroll_") {
		t.Errorf("token = %q, want an ep_enroll_ prefix", created.Token)
	}
	if !created.AutoApprove || !created.Usable {
		t.Errorf("auto_approve = %v, usable = %v", created.AutoApprove, created.Usable)
	}

	list := f.do(t, http.MethodGet, "/api/v1/enrollment-tokens", nil, withToken(accessToken))
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), created.Token) {
		t.Error("the listing returned the plaintext token; it is stored hashed and must not be recoverable")
	}
	if !strings.Contains(list.Body.String(), created.TokenPrefix) {
		t.Error("the listing shows no prefix, so two tokens cannot be told apart")
	}
}

// A token that outlives the machines it installs is a standing credential, so
// the ceiling is enforced against the server's clock rather than the client's.
func TestTokenLifetimeIsBounded(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	cases := map[string]map[string]any{
		"a year away": {
			"description": "too long",
			"expires_at":  time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
		"already expired": {
			"description": "in the past",
			"expires_at":  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
		"no expiry": {"description": "forever"},
		"no description": {
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/v1/enrollment-tokens",
				payload, withToken(accessToken))
			if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want a validation failure: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- agent enrollment ------------------------------------------------------

// The path an agent takes on first boot, end to end through the real
// enrollment service and the real CA.
func TestAnAgentCanEnrollWithAToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	create := f.do(t, http.MethodPost, "/api/v1/enrollment-tokens", map[string]any{
		"description": "test", "auto_approve": true,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, withToken(accessToken))
	if create.Code != http.StatusCreated {
		t.Fatalf("create token: %s", create.Body.String())
	}
	var token struct {
		Token string `json:"token"`
	}
	decodeBody(t, create, &token)

	deviceID, csr := testCSR(t)

	rec := f.do(t, http.MethodPost, "/api/v1/agent/enroll", map[string]any{
		"device_id":     deviceID.String(),
		"csr":           base64.StdEncoding.EncodeToString(csr),
		"serial_number": "H2WK9J4YQ1",
		"hostname":      "mac-mini-01",
		"arch":          "arm64",
		"cpu_cores":     12,
	}, withToken(token.Token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		DeviceID            string `json:"device_id"`
		CertificatePEM      string `json:"certificate_pem"`
		CABundlePEM         string `json:"ca_bundle_pem"`
		Approved            bool   `json:"approved"`
		DevicePlaneEndpoint string `json:"device_plane_endpoint"`
	}
	decodeBody(t, rec, &body)

	if body.DeviceID != deviceID.String() {
		t.Errorf("device_id = %q, want %q", body.DeviceID, deviceID)
	}
	if !strings.Contains(body.CertificatePEM, "BEGIN CERTIFICATE") {
		t.Error("no certificate was issued")
	}
	if !strings.Contains(body.CABundlePEM, "BEGIN CERTIFICATE") {
		t.Error("no CA bundle was returned; the agent cannot verify the server")
	}
	if !body.Approved {
		t.Error("the token carried auto-approve but the device is not approved")
	}
	if body.DevicePlaneEndpoint == "" {
		t.Error("the agent was not told where to connect")
	}

	// And the device is now in this tenant's fleet.
	list := f.do(t, http.MethodGet, "/api/v1/devices", nil, withToken(accessToken))
	if !strings.Contains(list.Body.String(), "mac-mini-01") {
		t.Errorf("the enrolled device is not in the fleet listing: %s", list.Body.String())
	}
}

func TestEnrollmentRefusesABadToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	deviceID, csr := testCSR(t)

	body := map[string]any{
		"device_id":     deviceID.String(),
		"csr":           base64.StdEncoding.EncodeToString(csr),
		"serial_number": "NOPE-1",
		"hostname":      "mac-nope",
	}

	cases := map[string]func(*http.Request){
		"no token":      func(*http.Request) {},
		"unknown token": withToken("ep_enroll_completelymadeup"),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/v1/agent/enroll", body, opt)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestEnrollmentRefusesAMalformedCSR(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	create := f.do(t, http.MethodPost, "/api/v1/enrollment-tokens", map[string]any{
		"description": "malformed-csr test",
		"expires_at":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, withToken(accessToken))
	var token struct {
		Token string `json:"token"`
	}
	decodeBody(t, create, &token)

	for name, csr := range map[string]string{
		"not base64":   "!!!not base64!!!",
		"empty":        "",
		"not a CSR":    base64.StdEncoding.EncodeToString([]byte("hello")),
		"a truncation": base64.StdEncoding.EncodeToString([]byte{0x30, 0x82, 0x01}),
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/v1/agent/enroll", map[string]any{
				"device_id":     uuid.New().String(),
				"csr":           csr,
				"serial_number": "BADCSR-1",
				"hostname":      "mac-bad",
			}, withToken(token.Token))
			if rec.Code < 400 {
				t.Errorf("status = %d, want a failure: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- the response envelope -------------------------------------------------

// Every access-log line said route=unmatched before this was fixed, including
// the ones that matched — which makes grouping requests by route in a dashboard
// return exactly one bucket.
func TestTheAccessLogRecordsTheMatchedRoute(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	f := newFixtureWithLogger(t, slog.New(slog.NewTextHandler(&logged, nil)))
	accessToken := f.signIn(t)

	deviceID := f.enrolDevice(t, f.org.ID, "ROUTE-1")
	f.do(t, http.MethodGet, "/api/v1/devices/"+deviceID.String(), nil, withToken(accessToken))

	out := logged.String()
	if !strings.Contains(out, `route="GET /api/v1/devices/{id}"`) {
		t.Errorf("the access log does not carry the matched pattern:\n%s", out)
	}
	// The device id must not be in the route field: it would make every request
	// its own bucket, and it puts an identifier in a field meant for grouping.
	if strings.Contains(out, "route="+deviceID.String()) {
		t.Error("the route field carries a device id rather than the pattern")
	}
}

func TestUnknownRoutesAreProblemDetails(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/there-is-no-such-thing", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/healthz", nil)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("no request id was returned; a client cannot correlate a failure with a log line")
	}
}

// A body larger than the limit must be refused rather than read into memory.
func TestOversizedRequestsAreRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)

	huge := strings.Repeat("a", 2<<20)
	rec := f.do(t, http.MethodPatch, "/api/v1/devices/"+uuid.New().String(),
		map[string]any{"notes": huge}, withToken(accessToken))

	if rec.Code < 400 {
		t.Errorf("status = %d, want a failure for an oversized body", rec.Code)
	}
}

// A typo in a field name must be reported, not silently ignored — the
// difference between catching it in development and believing a permission was
// granted that was not.
func TestUnknownFieldsAreRejected(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	accessToken := f.signIn(t)
	deviceID := f.enrolDevice(t, f.org.ID, "TYPO-1")

	rec := f.do(t, http.MethodPatch, "/api/v1/devices/"+deviceID.String(),
		map[string]any{"allow_custom_script": true}, withToken(accessToken))

	if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want a validation failure", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "allow_custom_script") {
		t.Errorf("the error does not name the offending field: %s", rec.Body.String())
	}
}

func TestCORSOnlyAnswersAllowedOrigins(t *testing.T) {
	t.Parallel()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN, MaxConns: 2, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hasher, _ := password.New(password.Params{MemoryKiB: 8 << 10, Iterations: 1, Parallelism: 1})
	_, priv, _ := ed25519GenerateKey()
	issuer, _ := token.NewIssuer(token.Options{PrivateKey: priv, Issuer: "t", Audience: "t", TTL: time.Minute})
	sealer, _ := secret.New(bytes.Repeat([]byte{9}, 32))
	authService, _ := auth.New(auth.Options{
		Store: store, Hasher: hasher, Issuer: issuer, Sealer: sealer, Logger: quiet(),
		RefreshTTL: time.Hour, LockoutDuration: time.Minute, MaxFailedLogins: 5,
	})

	server, err := ephttp.New(ephttp.Options{
		Auth: authService, Authenticator: authService, Store: store, Logger: quiet(),
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	send := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	allowed := send("https://dashboard.example.com")
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://dashboard.example.com" {
		t.Errorf("allowed origin got %q", got)
	}

	// An origin that is not on the list gets no header, so the browser refuses
	// to expose the response. Echoing arbitrary origins is what turns a bearer
	// API into a cross-site forgery.
	denied := send("https://evil.example.com")
	if got := denied.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was allowed: %q", got)
	}
}

// --- helpers ---------------------------------------------------------------

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// createUser adds a user with the given role, so a test can exercise a
// permission the seeded administrator holds.
func (f *fixture) createUser(t *testing.T, email, role string) adminUser {
	t.Helper()

	hasher, err := password.New(password.Params{
		MemoryKiB: 8 << 10, Iterations: 1, Parallelism: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}

	user, err := f.store.Users().Create(domain.WithOrg(t.Context(), f.org.ID),
		domain.CreateUserInput{
			OrgID: f.org.ID, Email: email, FullName: "Test User",
			RoleSlugs: []string{role},
		}, hash)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return adminUser{id: user.ID, email: email, password: testPassword}
}

// signInAs authenticates a specific user.
func (f *fixture) signInAs(t *testing.T, user adminUser) string {
	t.Helper()

	rec := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.email, "password": user.password,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login as %s: %d %s", user.email, rec.Code, rec.Body.String())
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	decodeBody(t, rec, &body)
	return body.AccessToken
}

// enrolDevice registers a device directly, bypassing the API, so a test can set
// up fleet state without going through enrollment every time.
func (f *fixture) enrolDevice(t *testing.T, orgID uuid.UUID, serial string) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Devices().Enroll(domain.WithOrg(t.Context(), orgID),
		domain.EnrollDeviceInput{
			DeviceID: id, OrgID: orgID, SerialNumber: serial, Hostname: "mac-" + serial,
		}); err != nil {
		t.Fatalf("enroll device: %v", err)
	}
	return id
}

func testAuthority(t *testing.T) *ca.Authority {
	t.Helper()
	generated, err := ca.Generate(ca.GenerateOptions{Organization: "EdgePilot Test"})
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	authority, err := ca.New(ca.Options{
		SigningCertPEM: generated.ChainPEM,
		SigningKeyPEM:  generated.IntermediateKeyPEM,
		DeviceTTL:      90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open CA: %v", err)
	}
	return authority
}

// seedOrganization creates a tenant with an administrator who can do everything
// the API exposes.
func seedOrganization(t *testing.T, store *postgres.Store, hasher *password.Hasher) (*domain.Organization, adminUser) {
	t.Helper()

	org, err := store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Acme", Slug: "acme-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	hash, err := hasher.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	email := "admin@acme.test"
	user, err := store.Users().Create(domain.WithOrg(t.Context(), org.ID),
		domain.CreateUserInput{
			OrgID: org.ID, Email: email, FullName: "Admin",
			RoleSlugs: []string{domain.RoleAdmin},
		}, hash)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	return org, adminUser{id: user.ID, email: email, password: testPassword}
}

// testCSR builds a device id and a CSR signed by a fresh key, as an agent would.
func testCSR(t *testing.T) (uuid.UUID, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: deviceID.String()},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return deviceID, der
}

// ed25519GenerateKey is a thin alias so the import stays obvious at the call
// sites, which build several issuers.
func ed25519GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
