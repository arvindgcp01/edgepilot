package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The interfaces below are declared here, in the package that *consumes* them,
// rather than beside their Postgres implementation. That is what makes the
// dependency point inward: services depend on these, the repository package
// depends on these, and neither depends on the other.
//
// Tenancy is not a parameter on any method. The organization comes from the
// context, set once by the transport layer from the caller's token and read by
// the repository to scope the transaction. Passing it explicitly would mean
// every call site could pass the wrong one, which is precisely the mistake
// row-level security exists to make impossible.

// OrgFromContext and WithOrg carry the tenant scope.
type orgContextKey struct{}

// WithOrg returns a context scoped to an organization. The repository layer
// issues SET LOCAL edgepilot.org_id from this on every transaction it opens.
func WithOrg(ctx context.Context, orgID uuid.UUID) context.Context {
	return context.WithValue(ctx, orgContextKey{}, orgID)
}

// OrgFromContext returns the scoped organization.
//
// The boolean matters: an unscoped context must be a programming error the
// repository refuses loudly, not a query that quietly runs unfiltered. Row-level
// security would deny it anyway, but failing here names the actual bug.
func OrgFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(orgContextKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}

// PrincipalFromContext and WithPrincipal carry the authenticated caller.
type principalContextKey struct{}

// WithPrincipal attaches the authenticated caller.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext returns the authenticated caller, if any.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*Principal)
	return p, ok && p != nil
}

// OrganizationRepository stores organizations.
//
// It is the one repository not scoped by context: a caller must be able to read
// its own organization before a scope exists to read it under. Callers pass the
// id explicitly, and the service layer checks it matches the token.
type OrganizationRepository interface {
	Create(ctx context.Context, in CreateOrganizationInput) (*Organization, error)
	Get(ctx context.Context, id uuid.UUID) (*Organization, error)
	GetBySlug(ctx context.Context, slug string) (*Organization, error)
	List(ctx context.Context, page PageRequest) (Page[*Organization], error)
	Update(ctx context.Context, id uuid.UUID, in UpdateOrganizationInput) (*Organization, error)
	SoftDelete(ctx context.Context, id uuid.UUID) error
	CountDevices(ctx context.Context, id uuid.UUID) (int, error)
}

// UserRepository stores operator accounts.
type UserRepository interface {
	Create(ctx context.Context, in CreateUserInput, passwordHash string) (*User, error)
	Get(ctx context.Context, id uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	List(ctx context.Context, page PageRequest) (Page[*User], error)
	Update(ctx context.Context, id uuid.UUID, in UpdateUserInput) (*User, error)
	SoftDelete(ctx context.Context, id uuid.UUID) error

	// Credentials loads secret material for the authentication path only.
	//
	// It returns ErrNotFound for an unknown email. The caller must still perform
	// a dummy password verification in that case, or the response time discloses
	// whether the account exists.
	Credentials(ctx context.Context, orgID uuid.UUID, email string) (*Credentials, error)

	// RecordLoginSuccess clears the failure counter and stamps last_login_at.
	RecordLoginSuccess(ctx context.Context, userID uuid.UUID, at time.Time) error

	// RecordLoginFailure increments the counter and reports whether the account
	// is now locked, in one statement — a read-then-write would let two
	// simultaneous failures both write count+1.
	//
	// lockUntil is supplied rather than derived from a duration inside the
	// query, so that one clock decides both when a lock starts and when it has
	// passed.
	RecordLoginFailure(ctx context.Context, userID uuid.UUID, lockUntil time.Time, threshold int) (locked bool, err error)

	SetPasswordHash(ctx context.Context, userID uuid.UUID, hash string, at time.Time) error

	// ResolvePermissions expands a user's roles into the permission set the
	// authorization checks run against. Done once per request, not per check.
	ResolvePermissions(ctx context.Context, userID uuid.UUID) (roles []string, permissions []string, err error)

	// RolesForOrg returns every user's role slugs in the caller's organization
	// in one round trip, for listings. A user with no roles is absent.
	RolesForOrg(ctx context.Context) (map[uuid.UUID][]string, error)
}

// RoleRepository reads the role catalogue and assigns roles.
type RoleRepository interface {
	List(ctx context.Context) ([]*Role, error)
	GetBySlug(ctx context.Context, slug string) (*Role, error)
	ListPermissions(ctx context.Context) ([]string, error)
	// ReplaceUserRoles sets a user's roles to exactly the given slugs.
	ReplaceUserRoles(ctx context.Context, userID uuid.UUID, slugs []string, grantedBy *uuid.UUID) error
}

// SessionRepository stores refresh-token families.
type SessionRepository interface {
	Create(ctx context.Context, s *Session, tokenHash []byte) (*Session, error)

	// GetByTokenHash finds a session by the SHA-256 of its refresh token.
	//
	// It returns revoked and expired sessions too. The caller needs to
	// distinguish "unknown token" from "token that was already rotated" —
	// the latter means the token leaked, and the whole family must be revoked.
	GetByTokenHash(ctx context.Context, tokenHash []byte) (*Session, error)

	// Rotate revokes the presented session and creates its successor in the
	// same family, atomically. Doing it in two calls would leave a window where
	// a crash loses the user's session entirely.
	Rotate(ctx context.Context, oldID uuid.UUID, next *Session, nextTokenHash []byte) (*Session, error)

	Revoke(ctx context.Context, id uuid.UUID, reason string) error

	// RevokeFamily revokes every session descended from one login. Called on
	// refresh-token reuse, and when a user is disabled.
	RevokeFamily(ctx context.Context, familyID uuid.UUID, reason string) (int, error)

	RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string) (int, error)
	DeleteExpired(ctx context.Context, before time.Time) (int, error)
}

// DeviceRepository stores the fleet.
type DeviceRepository interface {
	// Enroll registers a device. It is idempotent on (org, serial): an agent
	// that retries enrollment after a network failure must not create a second
	// record, and a re-imaged Mac reporting the same serial should reclaim its
	// identity rather than collide.
	Enroll(ctx context.Context, in EnrollDeviceInput) (*Device, error)

	Get(ctx context.Context, id uuid.UUID) (*Device, error)
	GetBySerial(ctx context.Context, serial string) (*Device, error)
	List(ctx context.Context, filter DeviceFilter, page PageRequest) (Page[*Device], error)
	Update(ctx context.Context, id uuid.UUID, in UpdateDeviceInput) (*Device, error)

	// RefreshReported records the facts an agent sends in Hello.
	//
	// Hostname, OS version and local address change without an inventory
	// refresh — a Mac gets renamed, updated, or moved to another subnet — and
	// until inventory ingest lands this is the only path by which the fleet
	// listing learns about it. Called on connect, and only when something
	// actually differs.
	RefreshReported(ctx context.Context, id uuid.UUID, in ReportedFacts) error

	SetEnrollmentState(ctx context.Context, id uuid.UUID, state EnrollmentState) (*Device, error)
	SetStatus(ctx context.Context, id uuid.UUID, status DeviceStatus) error

	// SetScreenSharing turns remote screen access on or off. A non-nil
	// sealedPassword also stores the sealed VNC password; nil leaves whatever
	// is there, so a plain toggle does not have to re-supply it.
	SetScreenSharing(ctx context.Context, id uuid.UUID, enabled bool, sealedPassword []byte) (*Device, error)

	// TouchLastSeen records liveness for a batch of devices.
	//
	// Batched deliberately: one row update per heartbeat would be 10,000 writes
	// every 30 seconds against the table the dashboard reads most. Redis holds
	// the live answer; this keeps the durable one roughly current.
	TouchLastSeen(ctx context.Context, seen map[uuid.UUID]time.Time) error

	// MarkStaleOffline transitions devices whose last_seen_at has aged out and
	// returns those that changed, so the caller can raise alerts for exactly
	// them rather than re-querying and racing the next sweep.
	//
	// Runs across every organization: the sweeper is a singleton worker, not a
	// tenant-scoped request.
	MarkStaleOffline(ctx context.Context, threshold time.Time) ([]StaleDevice, error)

	// Decommission revokes the device and soft-deletes it, freeing its serial
	// number for re-enrollment.
	Decommission(ctx context.Context, id uuid.UUID) error

	Summary(ctx context.Context, offlineAfter time.Duration) (*FleetSummary, error)
}

// DeviceCertificateRepository stores issued mTLS client certificates.
type DeviceCertificateRepository interface {
	Issue(ctx context.Context, cert *DeviceCertificate) (*DeviceCertificate, error)
	GetByFingerprint(ctx context.Context, fingerprint []byte) (*DeviceCertificate, error)
	ListForDevice(ctx context.Context, deviceID uuid.UUID) ([]*DeviceCertificate, error)
	Current(ctx context.Context, deviceID uuid.UUID) (*DeviceCertificate, error)
	Revoke(ctx context.Context, id uuid.UUID, reason string, by *uuid.UUID) error
	RevokeAllForDevice(ctx context.Context, deviceID uuid.UUID, reason string) (int, error)

	// ListRevokedFingerprints feeds the revocation set the mTLS verify callback
	// consults. Returned across all organizations, so it runs unscoped.
	ListRevokedFingerprints(ctx context.Context) ([][]byte, error)
}

// EnrollmentTokenRepository stores enrollment tokens.
type EnrollmentTokenRepository interface {
	Create(ctx context.Context, in CreateEnrollmentTokenInput, tokenHash []byte, prefix string) (*EnrollmentToken, error)
	List(ctx context.Context, page PageRequest) (Page[*EnrollmentToken], error)
	Revoke(ctx context.Context, id uuid.UUID) error

	// Redeem atomically validates a token and increments its use count.
	//
	// One statement, not a read followed by a write: two agents enrolling at the
	// same instant against a single-use token must not both succeed, and a
	// check-then-update leaves exactly that window open.
	//
	// Runs unscoped — the enrolling agent has no organization yet; the token is
	// what establishes which one it belongs to.
	Redeem(ctx context.Context, tokenHash []byte, now time.Time) (*EnrollmentToken, error)
}

// Store groups the repositories and provides transactions across them.
type Store interface {
	Organizations() OrganizationRepository
	Users() UserRepository
	Roles() RoleRepository
	Sessions() SessionRepository
	Devices() DeviceRepository
	DeviceCertificates() DeviceCertificateRepository
	EnrollmentTokens() EnrollmentTokenRepository
	SSHKeys() SSHKeyRepository
	SSHSessions() SSHSessionRepository
	Metrics() MetricsRepository

	// InTx runs fn against a Store whose repositories all share one
	// transaction, so a multi-repository operation — enrolling a device and
	// issuing its certificate, say — either happens completely or not at all.
	InTx(ctx context.Context, fn func(Store) error) error

	// Ping reports whether the database is reachable, for the readiness probe.
	//
	// A round trip rather than a pool statistic: a pool can hold connections
	// that were valid when they were opened and are now attached to a database
	// that has gone away, and "readiness" that reports on a stale handle is
	// worse than none.
	Ping(ctx context.Context) error

	Close() error
}
