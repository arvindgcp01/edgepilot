// Package auth implements EdgePilot's authentication use cases.
//
// The security-relevant behaviour lives here rather than in the HTTP handlers,
// so it is testable without a server and cannot be bypassed by adding a second
// route that forgets a step.
//
// Three properties this package exists to guarantee:
//
//   - **Login failures are uniform.** An unknown email, a wrong password, a
//     disabled account, and a locked one all produce the same error after the
//     same work. Anything else is a user-enumeration oracle, and knowing which
//     addresses are EdgePilot operators is itself useful to an attacker.
//
//   - **Refresh tokens are single-use, and reuse ends the family.** Presenting
//     an already-rotated token means it leaked. Since we cannot tell which
//     holder is legitimate, every session descended from that login is revoked.
//
//   - **Permissions are resolved once**, at login and at refresh, into the
//     access token. An authorization check is then a map lookup rather than a
//     join, and the 15-minute token lifetime bounds how long a revoked
//     permission keeps working.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/password"
	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/platform/totp"
)

// Service performs authentication.
type Service struct {
	store  domain.Store
	hasher *password.Hasher
	issuer *token.Issuer
	sealer *secret.Sealer
	log    *slog.Logger
	// revoked is optional; see SetRevocationChecker.
	revoked RevocationChecker

	refreshTTL      time.Duration
	lockoutDuration time.Duration
	maxFailedLogins int

	now func() time.Time
}

// Options configures the Service.
type Options struct {
	Store  domain.Store
	Hasher *password.Hasher
	Issuer *token.Issuer
	Sealer *secret.Sealer
	Logger *slog.Logger

	RefreshTTL      time.Duration
	LockoutDuration time.Duration
	MaxFailedLogins int

	// Now is injectable so expiry and lockout can be tested exactly rather than
	// with sleeps.
	Now func() time.Time
}

// New builds the Service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("auth: Store is required")
	case opts.Hasher == nil:
		return nil, errors.New("auth: Hasher is required")
	case opts.Issuer == nil:
		return nil, errors.New("auth: Issuer is required")
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = 30 * 24 * time.Hour
	}
	if opts.LockoutDuration <= 0 {
		opts.LockoutDuration = 15 * time.Minute
	}
	if opts.MaxFailedLogins <= 0 {
		opts.MaxFailedLogins = 5
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Service{
		store:           opts.Store,
		hasher:          opts.Hasher,
		issuer:          opts.Issuer,
		sealer:          opts.Sealer,
		log:             opts.Logger,
		refreshTTL:      opts.RefreshTTL,
		lockoutDuration: opts.LockoutDuration,
		maxFailedLogins: opts.MaxFailedLogins,
		now:             opts.Now,
	}, nil
}

// LoginRequest is what a sign-in attempt supplies.
type LoginRequest struct {
	OrgID     uuid.UUID
	Email     string
	Password  string
	MFACode   string
	UserAgent string
	IP        string
}

// TokenPair is what a successful authentication returns.
//
// RefreshToken is the plaintext, returned exactly once. Only its SHA-256 is
// stored, so it cannot be recovered afterwards — which is what makes a database
// disclosure useless for impersonation.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Principal    *domain.Principal
}

// Login authenticates a user.
//
// The shape of this function is dictated by timing: every path that can fail
// performs the same password-hash work before returning, so an attacker cannot
// distinguish "no such account" from "wrong password" by measuring.
func (s *Service) Login(ctx context.Context, req LoginRequest) (*TokenPair, error) {
	// The generic failure. Every rejection below returns exactly this, so the
	// client learns only that authentication failed.
	fail := domain.Unauthenticated("invalid_credentials")

	if req.OrgID == uuid.Nil || req.Email == "" || req.Password == "" {
		// Still pay the hash cost: a request missing a field would otherwise
		// return measurably faster than a complete one.
		s.hasher.VerifyDummy(req.Password)
		return nil, fail
	}

	creds, err := s.store.Users().Credentials(ctx, req.OrgID, req.Email)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			// A real failure — the database is down. Surface it rather than
			// reporting bad credentials, or an outage looks like every user's
			// password suddenly being wrong.
			return nil, err
		}
		// No such account. Do the work anyway.
		s.hasher.VerifyDummy(req.Password)
		return nil, fail
	}

	// Verify the password BEFORE checking account state.
	//
	// Reversing this — rejecting a disabled account before hashing — makes the
	// disabled path faster, and that difference tells an attacker the account
	// exists. Correctness costs one wasted hash on an account that cannot sign
	// in anyway.
	passwordOK := s.verifyPassword(req.Password, creds.PasswordHash)

	usable := s.credentialsUsable(creds)

	if !passwordOK {
		if usable {
			// Only count failures against accounts that could otherwise sign
			// in. Counting them for a disabled account would let an attacker
			// lock out an account they cannot use, and would make the lockout
			// state itself an enumeration signal.
			s.recordFailure(ctx, creds.UserID)
		}
		return nil, fail
	}
	if !usable {
		return nil, fail
	}

	// Password is correct and the account is usable. MFA is the remaining gate.
	if creds.MFAEnabled {
		if req.MFACode == "" {
			// A distinct response, deliberately. At this point the caller has
			// already proven possession of the password, so telling them a
			// second factor is required discloses nothing they did not know —
			// and not telling them makes the UI impossible to build.
			return nil, domain.MFARequired()
		}
		ok, err := s.verifyMFA(creds, req.MFACode)
		if err != nil {
			return nil, err
		}
		if !ok {
			s.recordFailure(ctx, creds.UserID)
			return nil, fail
		}
	}

	pair, err := s.issue(ctx, creds.UserID, creds.OrgID, req.Email, req.UserAgent, req.IP, uuid.Nil)
	if err != nil {
		return nil, err
	}

	if err := s.store.Users().RecordLoginSuccess(ctx, creds.UserID, s.now()); err != nil {
		// The user is authenticated; failing the login because the bookkeeping
		// write failed would be worse than a stale last_login_at.
		s.log.WarnContext(ctx, "recording successful login failed",
			slog.String("user_id", creds.UserID.String()), slog.Any("error", err))
	}

	// Upgrade the stored hash if the cost parameters have been raised since it
	// was written. This is the only moment the plaintext is available.
	//
	// The write is tenant-scoped, and the login context is not — authentication
	// is what establishes which organization the caller belongs to. The scope
	// comes from the credentials just verified.
	if s.hasher.NeedsRehash(creds.PasswordHash) {
		if newHash, err := s.hasher.Hash(req.Password); err == nil {
			scoped := domain.WithOrg(ctx, creds.OrgID)
			if err := s.store.Users().SetPasswordHash(scoped, creds.UserID, newHash, s.now()); err != nil {
				s.log.WarnContext(ctx, "rehashing password failed",
					slog.String("user_id", creds.UserID.String()), slog.Any("error", err))
			}
		}
	}

	return pair, nil
}

// Refresh exchanges a refresh token for a new pair.
func (s *Service) Refresh(ctx context.Context, refreshToken, userAgent, ip string) (*TokenPair, error) {
	fail := domain.Unauthenticated("invalid_refresh_token")

	if !token.LooksLikeRefreshToken(refreshToken) {
		return nil, fail
	}

	session, err := s.store.Sessions().GetByTokenHash(ctx, token.HashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fail
		}
		return nil, err
	}

	now := s.now()

	// A token that was already rotated is a leaked token in active use.
	//
	// Two parties now hold it: the legitimate client and whoever took it. There
	// is no way to tell which one is presenting it, so the only safe response is
	// to end every session descended from that login and make the real user
	// sign in again.
	if session.RevokedAt != nil {
		n, revokeErr := s.store.Sessions().RevokeFamily(ctx, session.FamilyID, "refresh token reuse detected")
		if revokeErr != nil {
			s.log.ErrorContext(ctx, "revoking session family after token reuse failed",
				slog.String("family_id", session.FamilyID.String()), slog.Any("error", revokeErr))
		}
		s.log.WarnContext(ctx, "refresh token reuse detected; session family revoked",
			slog.String("family_id", session.FamilyID.String()),
			slog.String("user_id", session.UserID.String()),
			slog.Int("sessions_revoked", n))
		return nil, fail
	}

	if !session.Active(now) {
		return nil, fail
	}

	// Re-check the account on every refresh. Without this a user disabled
	// mid-session would keep minting access tokens for the whole refresh
	// lifetime.
	user, err := s.store.Users().Get(domain.WithOrg(ctx, session.OrgID), session.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fail
		}
		return nil, err
	}
	if !user.CanAuthenticate(now) {
		if _, err := s.store.Sessions().RevokeFamily(ctx, session.FamilyID, "account no longer active"); err != nil {
			s.log.WarnContext(ctx, "revoking sessions for an inactive account failed",
				slog.Any("error", err))
		}
		return nil, fail
	}

	return s.rotate(ctx, session, user.Email, userAgent, ip)
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.store.Sessions().Revoke(ctx, sessionID, "signed out")
}

// LogoutAll revokes every session belonging to a user.
func (s *Service) LogoutAll(ctx context.Context, userID uuid.UUID) (int, error) {
	return s.store.Sessions().RevokeAllForUser(ctx, userID, "signed out everywhere")
}

// ChangePassword replaces a user's password.
//
// Every other session is revoked. A password change is what someone does after
// suspecting compromise, so leaving other sessions live would defeat the point.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	principal, ok := domain.PrincipalFromContext(ctx)
	if !ok || principal.UserID != userID {
		return domain.Forbidden(domain.PermUserWrite)
	}

	creds, err := s.store.Users().Credentials(ctx, principal.OrgID, principal.Email)
	if err != nil {
		return err
	}
	if !s.verifyPassword(current, creds.PasswordHash) {
		return domain.Unauthenticated("invalid_credentials")
	}
	if err := password.ValidateLength(next); err != nil {
		return domain.Invalid("/new_password", "%s", err.Error())
	}
	if current == next {
		return domain.Invalid("/new_password", "must differ from the current password")
	}

	hash, err := s.hasher.Hash(next)
	if err != nil {
		return domain.Internal(err)
	}
	if err := s.store.Users().SetPasswordHash(ctx, userID, hash, s.now()); err != nil {
		return err
	}
	if _, err := s.store.Sessions().RevokeAllForUser(ctx, userID, "password changed"); err != nil {
		s.log.WarnContext(ctx, "revoking sessions after a password change failed",
			slog.String("user_id", userID.String()), slog.Any("error", err))
	}
	return nil
}

// RevocationChecker reports whether a session has been revoked.
//
// Access tokens are self-contained, so nothing in the token itself changes when
// a user signs out — without this, a stolen token keeps working for its full
// 15-minute lifetime after revocation. The implementation is a Redis set
// (Phase 1.4); the interface is declared here, where it is consumed.
type RevocationChecker interface {
	// IsRevoked reports whether a session has been revoked. It must fail
	// closed: an error is treated as revoked, because the alternative is
	// honouring a token during exactly the outage an attacker would exploit.
	IsRevoked(ctx context.Context, sessionID uuid.UUID) (bool, error)
}

// SetRevocationChecker installs the session revocation check.
//
// Optional at construction so the service is usable before the Redis presence
// layer exists. Without it, revocation takes effect at token expiry rather than
// immediately — a documented 15-minute window, not a silent gap.
func (s *Service) SetRevocationChecker(rc RevocationChecker) { s.revoked = rc }

// Authenticate verifies an access token and returns the principal.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (*domain.Principal, error) {
	claims, err := s.issuer.Verify(accessToken)
	if err != nil {
		// The specific reason is logged, never returned: telling a caller their
		// token is expired rather than forged is a small hint, and free to
		// withhold.
		s.log.DebugContext(ctx, "access token rejected", slog.Any("error", err))
		return nil, domain.Unauthenticated("invalid_token")
	}

	if s.revoked != nil {
		revoked, err := s.revoked.IsRevoked(ctx, claims.SessionID)
		if err != nil {
			// Fail closed. Honouring the token while the revocation store is
			// unreachable would hand an attacker a window precisely when
			// nobody can see it.
			s.log.ErrorContext(ctx, "session revocation check failed; refusing the token",
				slog.String("session_id", claims.SessionID.String()), slog.Any("error", err))
			return nil, domain.Unauthenticated("invalid_token")
		}
		if revoked {
			return nil, domain.Unauthenticated("invalid_token")
		}
	}

	perms := make(map[string]struct{}, len(claims.Permissions))
	for _, p := range claims.Permissions {
		perms[p] = struct{}{}
	}

	return &domain.Principal{
		UserID:      claims.Subject,
		OrgID:       claims.OrgID,
		Email:       claims.Email,
		SessionID:   claims.SessionID,
		Roles:       claims.Roles,
		Permissions: perms,
	}, nil
}

// --- MFA enrollment --------------------------------------------------------

// BeginMFAEnrollment generates a secret and the URI an authenticator scans.
//
// The secret is not persisted until the user proves they can generate a code
// from it. Storing it first would leave accounts in a state where MFA is
// recorded but the user never completed setup and cannot sign in.
func (s *Service) BeginMFAEnrollment(ctx context.Context, userID uuid.UUID, issuerName string) (secretB32, uri string, err error) {
	if s.sealer == nil {
		return "", "", domain.Internal(errors.New("auth: MFA requires a configured sealer"))
	}
	principal, ok := domain.PrincipalFromContext(ctx)
	if !ok || principal.UserID != userID {
		return "", "", domain.Forbidden(domain.PermUserWrite)
	}

	secretB32, err = totp.GenerateSecret()
	if err != nil {
		return "", "", domain.Internal(err)
	}
	return secretB32, totp.ProvisioningURI(secretB32, principal.Email, issuerName), nil
}

// --- internals -------------------------------------------------------------

// verifyPassword runs the comparison, spending the hash cost even when the
// stored hash is absent.
//
// An SSO-only account has no password hash. Returning early would make it
// respond faster than a password account, so the dummy verification runs
// instead.
func (s *Service) verifyPassword(plaintext, storedHash string) bool {
	if storedHash == "" {
		s.hasher.VerifyDummy(plaintext)
		return false
	}
	return s.hasher.Verify(plaintext, storedHash) == nil
}

// credentialsUsable reports whether the account may sign in.
//
// Deliberately returns only a boolean: the caller must not be able to branch
// differently on *why*, because that difference would reach the client as a
// timing or response variation.
func (s *Service) credentialsUsable(c *domain.Credentials) bool {
	now := s.now()
	switch {
	case c.DeletedAt != nil:
		return false

	case c.LockedUntil != nil && c.LockedUntil.After(now):
		// The lock is still in force, whatever the status says.
		return false

	case c.Status == domain.UserLocked:
		// Reached only once the lock above has expired, so the status is stale:
		// it was set when the threshold was hit and nothing clears it until the
		// next successful login. Treating a stale 'locked' as permanently
		// unusable would turn a 15-minute lockout into a support ticket, which
		// is how a rate limit becomes a denial of service against your own
		// users.
		//
		// A lock with no expiry is administrative and stays in force.
		return c.LockedUntil != nil

	case c.Status != domain.UserActive:
		return false
	}
	return true
}

func (s *Service) recordFailure(ctx context.Context, userID uuid.UUID) {
	locked, err := s.store.Users().RecordLoginFailure(ctx, userID, s.now().Add(s.lockoutDuration), s.maxFailedLogins)
	if err != nil {
		s.log.WarnContext(ctx, "recording a failed login failed",
			slog.String("user_id", userID.String()), slog.Any("error", err))
		return
	}
	if locked {
		// A security event worth seeing in aggregate: many accounts locking at
		// once is a credential-stuffing campaign.
		s.log.WarnContext(ctx, "account locked after repeated failed sign-ins",
			slog.String("user_id", userID.String()),
			slog.Int("threshold", s.maxFailedLogins))
	}
}

func (s *Service) verifyMFA(creds *domain.Credentials, code string) (bool, error) {
	if s.sealer == nil {
		return false, domain.Internal(errors.New("auth: MFA is enabled but no sealer is configured"))
	}
	// The secret is bound to the user id, so one copied between rows fails to
	// open rather than authenticating the wrong person.
	plaintext, err := s.sealer.Open(creds.MFASecretEncrypted, []byte(creds.UserID.String()))
	if err != nil {
		return false, domain.Internal(fmt.Errorf("open MFA secret: %w", err))
	}
	_, ok := totp.Verify(string(plaintext), code, s.now(), totp.DefaultSkew)
	return ok, nil
}

// issue mints a new session and token pair.
func (s *Service) issue(ctx context.Context, userID, orgID uuid.UUID, email, userAgent, ip string, familyID uuid.UUID) (*TokenPair, error) {
	roles, permissions, err := s.store.Users().ResolvePermissions(ctx, userID)
	if err != nil {
		return nil, err
	}

	if familyID == uuid.Nil {
		familyID, err = uuid.NewV7()
		if err != nil {
			return nil, domain.Internal(err)
		}
	}

	refreshPlain, refreshHash, err := token.NewRefreshToken()
	if err != nil {
		return nil, domain.Internal(err)
	}

	now := s.now()
	session, err := s.store.Sessions().Create(ctx, &domain.Session{
		UserID:    userID,
		OrgID:     orgID,
		FamilyID:  familyID,
		UserAgent: truncate(userAgent, 512),
		IP:        ip,
		ExpiresAt: now.Add(s.refreshTTL),
	}, refreshHash)
	if err != nil {
		return nil, err
	}

	return s.pairFor(session, email, roles, permissions, refreshPlain)
}

// rotate issues the successor to an existing session, atomically revoking it.
func (s *Service) rotate(ctx context.Context, prev *domain.Session, email, userAgent, ip string) (*TokenPair, error) {
	roles, permissions, err := s.store.Users().ResolvePermissions(ctx, prev.UserID)
	if err != nil {
		return nil, err
	}

	refreshPlain, refreshHash, err := token.NewRefreshToken()
	if err != nil {
		return nil, domain.Internal(err)
	}

	now := s.now()
	next, err := s.store.Sessions().Rotate(ctx, prev.ID, &domain.Session{
		UserID: prev.UserID,
		OrgID:  prev.OrgID,
		// The family is inherited, which is what makes reuse detection able to
		// revoke every descendant of one login.
		FamilyID:  prev.FamilyID,
		UserAgent: truncate(userAgent, 512),
		IP:        ip,
		ExpiresAt: now.Add(s.refreshTTL),
	}, refreshHash)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// Lost a race with a concurrent refresh of the same token. Treated
			// as reuse, because from here the two are indistinguishable.
			return nil, domain.Unauthenticated("invalid_refresh_token")
		}
		return nil, err
	}

	return s.pairFor(next, email, roles, permissions, refreshPlain)
}

func (s *Service) pairFor(session *domain.Session, email string, roles, permissions []string, refreshPlain string) (*TokenPair, error) {
	access, err := s.issuer.Sign(token.Claims{
		Subject:     session.UserID,
		OrgID:       session.OrgID,
		SessionID:   session.ID,
		Email:       email,
		Roles:       roles,
		Permissions: permissions,
	})
	if err != nil {
		return nil, domain.Internal(err)
	}

	perms := make(map[string]struct{}, len(permissions))
	for _, p := range permissions {
		perms[p] = struct{}{}
	}

	return &TokenPair{
		AccessToken:  access,
		RefreshToken: refreshPlain,
		ExpiresIn:    int(s.issuer.TTL().Seconds()),
		Principal: &domain.Principal{
			UserID:      session.UserID,
			OrgID:       session.OrgID,
			Email:       email,
			SessionID:   session.ID,
			Roles:       roles,
			Permissions: perms,
		},
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
