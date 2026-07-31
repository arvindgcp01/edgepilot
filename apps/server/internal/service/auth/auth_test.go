package auth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/password"
	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/platform/totp"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/service/auth"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
)

// The auth service is tested against a real store rather than fakes.
//
// The properties worth proving here — that a rotated refresh token cannot be
// reused, that disabling a user ends their sessions, that the failure counter
// increments atomically — are properties of the database as much as of the Go
// code. A fake would let all three pass while the real system was broken.

type fixture struct {
	svc    *auth.Service
	store  *postgres.Store
	hasher *password.Hasher
	issuer *token.Issuer
	sealer *secret.Sealer
	org    *domain.Organization
	now    *time.Time
}

const testPassword = "correct horse battery staple"

func newFixture(t *testing.T) *fixture {
	t.Helper()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN,
		MaxConns:    2,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Deliberately weak parameters: the suite runs hundreds of hashes, and the
	// cost is what production tunes, not what correctness depends on.
	hasher, err := password.New(password.Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1})
	if err != nil {
		t.Fatalf("new hasher: %v", err)
	}

	clock := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	issuer, err := token.NewIssuer(token.Options{
		PrivateKey: mustEd25519(t),
		TTL:        15 * time.Minute,
		Now:        func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	svc, err := auth.New(auth.Options{
		Store:           store,
		Hasher:          hasher,
		Issuer:          issuer,
		Sealer:          sealer,
		Logger:          slog.New(slog.DiscardHandler),
		RefreshTTL:      30 * 24 * time.Hour,
		LockoutDuration: 15 * time.Minute,
		MaxFailedLogins: 3,
		Now:             func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	org, err := store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Acme",
		Slug: "acme-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	return &fixture{svc: svc, store: store, hasher: hasher, issuer: issuer, sealer: sealer, org: org, now: &clock}
}

func mustEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func (f *fixture) ctx(t *testing.T) context.Context {
	t.Helper()
	return domain.WithOrg(t.Context(), f.org.ID)
}

// createUser adds an active user with a known password.
func (f *fixture) createUser(t *testing.T, email string, roles ...string) *domain.User {
	t.Helper()
	if len(roles) == 0 {
		roles = []string{domain.RoleOperator}
	}
	hash, err := f.hasher.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := f.store.Users().Create(f.ctx(t), domain.CreateUserInput{
		OrgID:     f.org.ID,
		Email:     email,
		FullName:  "Test User",
		RoleSlugs: roles,
	}, hash)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func (f *fixture) login(t *testing.T, email, pw string) (*auth.TokenPair, error) {
	t.Helper()
	return f.svc.Login(t.Context(), auth.LoginRequest{
		OrgID: f.org.ID, Email: email, Password: pw, UserAgent: "test", IP: "192.0.2.1",
	})
}

// --- login -----------------------------------------------------------------

func TestLoginSucceedsAndCarriesPermissions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "operator@acme.test", domain.RoleOperator)

	pair, err := f.login(t, "operator@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("both tokens should be issued")
	}
	if pair.ExpiresIn != int((15 * time.Minute).Seconds()) {
		t.Errorf("expires_in = %d", pair.ExpiresIn)
	}
	if pair.Principal.UserID != user.ID || pair.Principal.OrgID != f.org.ID {
		t.Errorf("principal identity is wrong: %+v", pair.Principal)
	}

	// Permissions are resolved once and carried, so an authorization check is a
	// map lookup rather than a join.
	if !pair.Principal.Can(domain.PermDeviceRead) {
		t.Error("operator should hold device:read")
	}
	if pair.Principal.Can(domain.PermCommandCustomScript) {
		t.Error("operator must not hold command:custom_script")
	}

	// The access token must verify and carry the same claims.
	claims, err := f.issuer.Verify(pair.AccessToken)
	if err != nil {
		t.Fatalf("the issued token should verify: %v", err)
	}
	if claims.Subject != user.ID || claims.SessionID != pair.Principal.SessionID {
		t.Error("token claims disagree with the returned principal")
	}
}

// Every rejection must be indistinguishable. Otherwise the endpoint tells an
// attacker which addresses are EdgePilot operators.
func TestLoginFailuresAreUniform(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.createUser(t, "known@acme.test")

	disabled := f.createUser(t, "disabled@acme.test")
	if _, err := f.store.Users().Update(f.ctx(t), disabled.ID, domain.UpdateUserInput{
		Status: ptr(domain.UserDisabled),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	cases := map[string]struct{ email, pw string }{
		"unknown email":    {"nobody@acme.test", testPassword},
		"wrong password":   {"known@acme.test", "wrong password entirely"},
		"disabled account": {"disabled@acme.test", testPassword},
		"empty email":      {"", testPassword},
		"empty password":   {"known@acme.test", ""},
	}

	var codes, kinds []string
	for name, tc := range cases {
		_, err := f.login(t, tc.email, tc.pw)
		if err == nil {
			t.Fatalf("%s: login should have failed", name)
		}
		codes = append(codes, domain.CodeOf(err))
		kinds = append(kinds, string(domain.KindOf(err)))
	}
	for i := range codes {
		if codes[i] != codes[0] {
			t.Errorf("failure codes differ, disclosing which case applied: %v", codes)
			break
		}
		if kinds[i] != kinds[0] {
			t.Errorf("failure kinds differ: %v", kinds)
			break
		}
	}
	if codes[0] != "invalid_credentials" {
		t.Errorf("code = %q, want invalid_credentials", codes[0])
	}
}

// A disabled account must not have its failure counter incremented: it would
// let an attacker lock out an account they cannot use, and the lockout state
// itself would become an enumeration signal.
func TestFailedLoginAgainstDisabledAccountDoesNotCount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	user := f.createUser(t, "disabled@acme.test")
	if _, err := f.store.Users().Update(f.ctx(t), user.ID, domain.UpdateUserInput{
		Status: ptr(domain.UserDisabled),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	for range 5 {
		if _, err := f.login(t, "disabled@acme.test", "wrong"); err == nil {
			t.Fatal("login should fail")
		}
	}

	creds, err := f.store.Users().Credentials(t.Context(), f.org.ID, "disabled@acme.test")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.FailedLoginCount != 0 {
		t.Errorf("failure count = %d, want 0 for a disabled account", creds.FailedLoginCount)
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "lockme@acme.test")

	// MaxFailedLogins is 3 in the fixture.
	for i := range 3 {
		if _, err := f.login(t, "lockme@acme.test", "wrong password here"); err == nil {
			t.Fatalf("attempt %d should fail", i+1)
		}
	}

	// The correct password must now be refused too — that is what lockout means.
	if _, err := f.login(t, "lockme@acme.test", testPassword); err == nil {
		t.Fatal("a locked account must refuse even the correct password")
	}

	// And the refusal is the same generic failure, so lockout is not observable.
	_, err := f.login(t, "lockme@acme.test", testPassword)
	if domain.CodeOf(err) != "invalid_credentials" {
		t.Errorf("lockout should not be distinguishable: %q", domain.CodeOf(err))
	}

	// Once the lock expires, the correct password works again.
	*f.now = f.now.Add(20 * time.Minute)
	if _, err := f.login(t, "lockme@acme.test", testPassword); err != nil {
		t.Errorf("login should succeed after the lock expires: %v", err)
	}
}

func TestSuccessfulLoginClearsTheFailureCounter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	for range 2 {
		if _, err := f.login(t, "user@acme.test", "wrong"); err == nil {
			t.Fatal("should fail")
		}
	}
	if _, err := f.login(t, "user@acme.test", testPassword); err != nil {
		t.Fatalf("login: %v", err)
	}

	creds, err := f.store.Users().Credentials(t.Context(), f.org.ID, "user@acme.test")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.FailedLoginCount != 0 {
		t.Errorf("a successful login should clear the counter, got %d", creds.FailedLoginCount)
	}
}

// A user invited but never activated has no password hash. The login must fail
// without a panic and without being faster than a real account.
func TestLoginAgainstAccountWithNoPassword(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	if _, err := f.store.Users().Create(f.ctx(t), domain.CreateUserInput{
		OrgID:     f.org.ID,
		Email:     "invited@acme.test",
		FullName:  "Invited",
		RoleSlugs: []string{domain.RoleViewer},
	}, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := f.login(t, "invited@acme.test", testPassword)
	if err == nil {
		t.Fatal("an account with no password must not authenticate")
	}
	if domain.CodeOf(err) != "invalid_credentials" {
		t.Errorf("code = %q", domain.CodeOf(err))
	}
}

// Raising the cost parameters only helps if existing hashes are upgraded. Login
// is the one moment the plaintext is available.
func TestLoginRehashesWhenCostIsRaised(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	weak, err := password.New(password.Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	oldHash, err := weak.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, err := f.store.Users().Create(f.ctx(t), domain.CreateUserInput{
		OrgID: f.org.ID, Email: "rehash@acme.test", FullName: "R",
		RoleSlugs: []string{domain.RoleViewer},
	}, oldHash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A service whose hasher is configured stronger than the stored hash.
	strong, err := password.New(password.Params{MemoryKiB: 16 * 1024, Iterations: 2, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.New(auth.Options{
		Store: f.store, Hasher: strong, Issuer: f.issuer,
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return *f.now },
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Login(t.Context(), auth.LoginRequest{
		OrgID: f.org.ID, Email: "rehash@acme.test", Password: testPassword,
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	creds, err := f.store.Users().Credentials(t.Context(), f.org.ID, "rehash@acme.test")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.PasswordHash == oldHash {
		t.Error("the hash should have been upgraded on login")
	}
	if strong.NeedsRehash(creds.PasswordHash) {
		t.Error("the upgraded hash still reports as needing a rehash")
	}
	// And the new hash must still verify the same password.
	if err := strong.Verify(testPassword, creds.PasswordHash); err != nil {
		t.Errorf("the rehashed password no longer verifies: %v", err)
	}
	_ = user
}

// --- refresh ---------------------------------------------------------------

func TestRefreshRotatesTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	first, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	second, err := f.svc.Refresh(t.Context(), first.RefreshToken, "test", "192.0.2.1")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if second.RefreshToken == first.RefreshToken {
		t.Error("the refresh token must rotate")
	}
	if second.AccessToken == first.AccessToken {
		t.Error("a new access token should be issued")
	}
	if second.Principal.UserID != first.Principal.UserID {
		t.Error("identity changed across a refresh")
	}
}

// This is the property the whole session design exists for. Presenting an
// already-rotated token means it leaked; since we cannot tell which holder is
// legitimate, every session from that login ends.
func TestRefreshTokenReuseRevokesTheWholeFamily(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "victim@acme.test")

	first, err := f.login(t, "victim@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	second, err := f.svc.Refresh(t.Context(), first.RefreshToken, "test", "192.0.2.1")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// The attacker replays the token they intercepted.
	if _, err := f.svc.Refresh(t.Context(), first.RefreshToken, "attacker", "198.51.100.1"); err == nil {
		t.Fatal("a rotated refresh token must not be usable again")
	}

	// The legitimate client's current token is now dead too. That is the point:
	// we cannot tell the two apart, so both are ended and the real user signs in
	// again.
	if _, err := f.svc.Refresh(t.Context(), second.RefreshToken, "test", "192.0.2.1"); err == nil {
		t.Error("reuse detection must revoke the whole family, including the live token")
	}

	// Signing in again works, and starts a fresh family.
	third, err := f.login(t, "victim@acme.test", testPassword)
	if err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if _, err := f.svc.Refresh(t.Context(), third.RefreshToken, "test", "192.0.2.1"); err != nil {
		t.Errorf("a fresh login should produce a usable refresh token: %v", err)
	}
}

func TestRefreshRejectsBadTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	for name, tok := range map[string]string{
		"empty":                     "",
		"garbage":                   "hunter2",
		"wrong prefix":              "ep_enroll_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"right shape, never issued": "ep_refresh_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := f.svc.Refresh(t.Context(), tok, "test", "192.0.2.1"); err == nil {
				t.Error("should be rejected")
			}
		})
	}
}

// A user disabled mid-session must stop being able to mint access tokens.
// Without this check they would keep working for the whole refresh lifetime.
func TestRefreshRejectsDisabledUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "leaver@acme.test")

	pair, err := f.login(t, "leaver@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := f.store.Users().Update(f.ctx(t), user.ID, domain.UpdateUserInput{
		Status: ptr(domain.UserDisabled),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken, "test", "192.0.2.1"); err == nil {
		t.Error("a disabled user must not be able to refresh")
	}
}

func TestRefreshRejectsExpiredSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	pair, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	*f.now = f.now.Add(31 * 24 * time.Hour)
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken, "test", "192.0.2.1"); err == nil {
		t.Error("an expired session must not refresh")
	}
}

// --- logout and password change --------------------------------------------

func TestLogoutRevokesTheSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	pair, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := f.svc.Logout(t.Context(), pair.Principal.SessionID); err != nil {
		t.Fatalf("logout: %v", err)
	}

	// The refresh token is dead. Note the access token is NOT — it is
	// self-contained and stays valid until it expires, which is why it lives
	// only 15 minutes and why the revocation checker exists.
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken, "test", "192.0.2.1"); err == nil {
		t.Error("a revoked session must not refresh")
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "user@acme.test")

	first, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	second, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	ctx := domain.WithPrincipal(f.ctx(t), first.Principal)
	const newPassword = "an entirely different passphrase"
	if err := f.svc.ChangePassword(ctx, user.ID, testPassword, newPassword); err != nil {
		t.Fatalf("change password: %v", err)
	}

	// A password change is what someone does after suspecting compromise, so
	// every session must end.
	for name, tok := range map[string]string{"first": first.RefreshToken, "second": second.RefreshToken} {
		if _, err := f.svc.Refresh(t.Context(), tok, "test", "192.0.2.1"); err == nil {
			t.Errorf("the %s session should have been revoked", name)
		}
	}

	if _, err := f.login(t, "user@acme.test", testPassword); err == nil {
		t.Error("the old password must no longer work")
	}
	if _, err := f.login(t, "user@acme.test", newPassword); err != nil {
		t.Errorf("the new password should work: %v", err)
	}
}

func TestChangePasswordValidates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "user@acme.test")

	pair, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	ctx := domain.WithPrincipal(f.ctx(t), pair.Principal)

	if err := f.svc.ChangePassword(ctx, user.ID, "wrong current", "a new long password"); err == nil {
		t.Error("the current password must be verified")
	}
	if err := f.svc.ChangePassword(ctx, user.ID, testPassword, "short"); err == nil {
		t.Error("a short new password must be rejected")
	}
	if err := f.svc.ChangePassword(ctx, user.ID, testPassword, testPassword); err == nil {
		t.Error("reusing the current password must be rejected")
	}

	// A user must not be able to change someone else's password.
	other := f.createUser(t, "other@acme.test")
	if err := f.svc.ChangePassword(ctx, other.ID, testPassword, "a new long password"); err == nil {
		t.Error("a user must not change another user's password")
	}
}

// --- access tokens ---------------------------------------------------------

func TestAuthenticateReturnsThePrincipal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "operator@acme.test", domain.RoleOperator)

	pair, err := f.login(t, "operator@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	principal, err := f.svc.Authenticate(t.Context(), pair.AccessToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if principal.UserID != user.ID {
		t.Errorf("subject = %v, want %v", principal.UserID, user.ID)
	}
	if !principal.Can(domain.PermCommandExecute) {
		t.Error("permissions did not survive the round trip")
	}
}

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	pair, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	for name, tok := range map[string]string{
		"empty":    "",
		"garbage":  "not.a.token",
		"tampered": pair.AccessToken[:len(pair.AccessToken)-4] + "AAAA",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := f.svc.Authenticate(t.Context(), tok); err == nil {
				t.Error("should be rejected")
			}
		})
	}

	// Expired.
	*f.now = f.now.Add(time.Hour)
	if _, err := f.svc.Authenticate(t.Context(), pair.AccessToken); err == nil {
		t.Error("an expired access token must be rejected")
	}
}

// The revocation check must fail closed. Honouring a token while the revocation
// store is unreachable hands an attacker a window precisely when nobody is
// watching.
func TestAuthenticateFailsClosedWhenRevocationCheckErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.createUser(t, "user@acme.test")

	pair, err := f.login(t, "user@acme.test", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// Sanity: valid without a checker.
	if _, err := f.svc.Authenticate(t.Context(), pair.AccessToken); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	f.svc.SetRevocationChecker(brokenChecker{})
	if _, err := f.svc.Authenticate(t.Context(), pair.AccessToken); err == nil {
		t.Error("an unavailable revocation store must cause the token to be refused")
	}

	f.svc.SetRevocationChecker(revokedChecker{})
	if _, err := f.svc.Authenticate(t.Context(), pair.AccessToken); err == nil {
		t.Error("a revoked session must cause the token to be refused")
	}

	f.svc.SetRevocationChecker(liveChecker{})
	if _, err := f.svc.Authenticate(t.Context(), pair.AccessToken); err != nil {
		t.Errorf("a live session should be accepted: %v", err)
	}
}

type brokenChecker struct{}

func (brokenChecker) IsRevoked(context.Context, uuid.UUID) (bool, error) {
	return false, errors.New("redis unreachable")
}

type revokedChecker struct{}

func (revokedChecker) IsRevoked(context.Context, uuid.UUID) (bool, error) { return true, nil }

type liveChecker struct{}

func (liveChecker) IsRevoked(context.Context, uuid.UUID) (bool, error) { return false, nil }

// --- MFA -------------------------------------------------------------------

func TestMFAGatesLogin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "mfa@acme.test")

	// Enrol: generate a secret, seal it bound to the user, and enable MFA.
	secretB32, err := totp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.sealer.Seal([]byte(secretB32), []byte(user.ID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := enableMFA(t, f, user.ID, sealed); err != nil {
		t.Fatalf("enable mfa: %v", err)
	}

	// Correct password, no code: a distinct response, because the caller has
	// already proven the password and needs to be told to supply a code.
	_, err = f.login(t, "mfa@acme.test", testPassword)
	if !errors.Is(err, domain.ErrMFARequired) {
		t.Fatalf("expected an MFA-required error, got: %v", err)
	}

	// Wrong code.
	if _, err := f.svc.Login(t.Context(), auth.LoginRequest{
		OrgID: f.org.ID, Email: "mfa@acme.test", Password: testPassword, MFACode: "000000",
	}); err == nil {
		t.Fatal("a wrong code must be refused")
	}

	// Correct code.
	code, err := totp.Code(secretB32, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := f.svc.Login(t.Context(), auth.LoginRequest{
		OrgID: f.org.ID, Email: "mfa@acme.test", Password: testPassword, MFACode: code,
	})
	if err != nil {
		t.Fatalf("login with a valid code: %v", err)
	}
	if pair.AccessToken == "" {
		t.Error("expected a token pair")
	}
}

// The wrong password must be refused before MFA is even considered — otherwise
// a valid code would tell an attacker the account exists.
func TestMFADoesNotBypassThePasswordCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	user := f.createUser(t, "mfa@acme.test")

	secretB32, err := totp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.sealer.Seal([]byte(secretB32), []byte(user.ID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := enableMFA(t, f, user.ID, sealed); err != nil {
		t.Fatalf("enable mfa: %v", err)
	}

	code, err := totp.Code(secretB32, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Login(t.Context(), auth.LoginRequest{
		OrgID: f.org.ID, Email: "mfa@acme.test", Password: "wrong password", MFACode: code,
	})
	if err == nil {
		t.Fatal("a valid code must not substitute for the password")
	}
	// And the failure must be the generic one, not the MFA-required signal.
	if errors.Is(err, domain.ErrMFARequired) {
		t.Error("a wrong password should not reveal that MFA is enrolled")
	}
	if domain.CodeOf(err) != "invalid_credentials" {
		t.Errorf("code = %q", domain.CodeOf(err))
	}
}

// A sealed secret copied from one user's row to another must not authenticate,
// which is what binding the seal to the user id buys.
func TestMFASecretIsBoundToTheUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	victim := f.createUser(t, "victim@acme.test")
	attacker := f.createUser(t, "attacker@acme.test")

	secretB32, err := totp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	// Sealed for the victim...
	sealed, err := f.sealer.Seal([]byte(secretB32), []byte(victim.ID.String()))
	if err != nil {
		t.Fatal(err)
	}
	// ...but written onto the attacker's row.
	if err := enableMFA(t, f, attacker.ID, sealed); err != nil {
		t.Fatalf("enable mfa: %v", err)
	}

	code, err := totp.Code(secretB32, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Login(t.Context(), auth.LoginRequest{
		OrgID: f.org.ID, Email: "attacker@acme.test", Password: testPassword, MFACode: code,
	})
	if err == nil {
		t.Fatal("a secret sealed for another user must not open")
	}
}

// enableMFA writes a sealed secret directly, standing in for the enrollment
// endpoint that will land with the HTTP layer.
func enableMFA(t *testing.T, f *fixture, userID uuid.UUID, sealed []byte) error {
	t.Helper()
	_, err := f.store.Pool().Exec(t.Context(),
		`UPDATE users SET mfa_secret_enc = $1, mfa_enabled = true WHERE id = $2`,
		sealed, userID)
	return err
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()

	hasher, err := password.New(password.Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := token.NewIssuer(token.Options{PrivateKey: mustEd25519(t)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := auth.New(auth.Options{Hasher: hasher, Issuer: issuer}); err == nil {
		t.Error("a store is required")
	}
	if _, err := auth.New(auth.Options{Store: &postgres.Store{}, Issuer: issuer}); err == nil {
		t.Error("a hasher is required")
	}
	if _, err := auth.New(auth.Options{Store: &postgres.Store{}, Hasher: hasher}); err == nil {
		t.Error("an issuer is required")
	}
}

func ptr[T any](v T) *T { return &v }
