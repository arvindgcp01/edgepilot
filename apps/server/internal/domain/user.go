package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// UserStatus is an account's lifecycle state.
type UserStatus string

const (
	UserInvited  UserStatus = "invited"
	UserActive   UserStatus = "active"
	UserDisabled UserStatus = "disabled"
	UserLocked   UserStatus = "locked"
)

func (s UserStatus) Valid() bool {
	switch s {
	case UserInvited, UserActive, UserDisabled, UserLocked:
		return true
	}
	return false
}

// User is an operator account.
//
// It carries no password hash and no MFA secret. Those live in Credentials,
// which only the authentication service asks for. Keeping them off the entity
// that gets returned from list endpoints, embedded in responses, and written to
// logs means they cannot leak through a forgotten field — the type simply does
// not have them.
type User struct {
	ID    uuid.UUID
	OrgID uuid.UUID

	Email    string
	FullName string
	Status   UserStatus

	MFAEnabled bool

	// Roles are role slugs. Permissions resolves them to the permission set the
	// caller is actually checked against.
	Roles []string

	LastLoginAt       *time.Time
	PasswordChangedAt *time.Time
	LockedUntil       *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// CanAuthenticate reports whether the account is in a state that may sign in.
//
// It deliberately does not distinguish *why* not. The caller turns any false
// into the same generic failure, because an unknown email, a wrong password,
// and a disabled account must be indistinguishable or the endpoint becomes a
// user-enumeration oracle.
func (u *User) CanAuthenticate(now time.Time) bool {
	switch {
	case u == nil, u.DeletedAt != nil:
		return false
	case u.Status != UserActive:
		return false
	case u.LockedUntil != nil && u.LockedUntil.After(now):
		return false
	}
	return true
}

// Credentials is the secret material for one account, loaded only by the
// authentication path.
type Credentials struct {
	UserID uuid.UUID
	OrgID  uuid.UUID

	// PasswordHash is an argon2id encoded hash. Empty for an SSO-only account,
	// which must still consume a constant-time comparison so that the absence
	// of a password is not observable through response timing.
	PasswordHash string

	// MFASecretEncrypted is sealed with the application data key; the plaintext
	// TOTP secret never reaches this struct.
	MFASecretEncrypted []byte
	MFAEnabled         bool

	FailedLoginCount int
	LockedUntil      *time.Time
	Status           UserStatus
	DeletedAt        *time.Time
}

// Role is a named bundle of permissions.
type Role struct {
	ID uuid.UUID
	// OrgID is nil for the built-in roles, which every organization shares.
	OrgID       *uuid.UUID
	Slug        string
	Name        string
	Description string
	IsSystem    bool
	Permissions []string
}

// Built-in role slugs.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Permission slugs. These are the strings the service layer checks against, so
// they are constants rather than literals scattered through handlers — a typo
// in a permission check is a silent authorization hole.
const (
	PermDeviceRead    = "device:read"
	PermDeviceWrite   = "device:write"
	PermDeviceDelete  = "device:delete"
	PermDeviceApprove = "device:approve"

	PermCommandRead         = "command:read"
	PermCommandExecute      = "command:execute"
	PermCommandDestructive  = "command:destructive"
	PermCommandCustomScript = "command:custom_script"
	PermCommandApprove      = "command:approve"

	PermFileRead   = "file:read"
	PermFileWrite  = "file:write"
	PermFileDelete = "file:delete"

	PermDeploymentRead    = "deployment:read"
	PermDeploymentWrite   = "deployment:write"
	PermDeploymentExecute = "deployment:execute"

	PermLogRead    = "log:read"
	PermLogCollect = "log:collect"

	PermSSHRead       = "ssh:read"
	PermSSHConnect    = "ssh:connect"
	PermSSHManageKeys = "ssh:manage_keys"

	PermScreenConnect = "screen:connect"

	PermAlertRead  = "alert:read"
	PermAlertWrite = "alert:write"

	PermUserRead   = "user:read"
	PermUserWrite  = "user:write"
	PermRoleWrite  = "role:write"
	PermAuditRead  = "audit:read"
	PermOrgWrite   = "org:write"
	PermAgentRelse = "agent:release"
)

// Principal is an authenticated caller with its permissions already resolved.
//
// Resolution happens once per request rather than per check: re-deriving
// permissions from role names at each call site is how one path ends up
// checking a different set from another.
type Principal struct {
	UserID      uuid.UUID
	OrgID       uuid.UUID
	Email       string
	SessionID   uuid.UUID
	Roles       []string
	Permissions map[string]struct{}
}

// Can reports whether the principal holds a permission.
func (p *Principal) Can(permission string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Permissions[permission]
	return ok
}

// Authorize returns a Forbidden error when the permission is absent, so call
// sites read as `if err := p.Authorize(...); err != nil { return err }`.
func (p *Principal) Authorize(permission string) error {
	if p.Can(permission) {
		return nil
	}
	return Forbidden(permission)
}

// Session is a refresh-token family member.
//
// The token itself is never stored — only its SHA-256 — so a database
// disclosure yields no usable credential. ParentID and FamilyID exist to detect
// reuse: presenting an already-rotated token means it leaked, and the whole
// family is revoked.
type Session struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	OrgID    uuid.UUID
	FamilyID uuid.UUID
	ParentID *uuid.UUID

	UserAgent string
	IP        string

	ExpiresAt     time.Time
	LastUsedAt    *time.Time
	RevokedAt     *time.Time
	RevokedReason string
	CreatedAt     time.Time
}

// Active reports whether the session may still be used to refresh.
func (s *Session) Active(now time.Time) bool {
	return s != nil && s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// CreateUserInput is what the service layer supplies to invite a user.
type CreateUserInput struct {
	OrgID     uuid.UUID
	Email     string
	FullName  string
	RoleSlugs []string
	CreatedBy *uuid.UUID
}

func (in CreateUserInput) Validate() error {
	var fields []FieldError
	if !validEmail(in.Email) {
		fields = append(fields, FieldError{Pointer: "/email", Message: "must be a valid email address"})
	}
	if l := len(in.FullName); l > 200 {
		fields = append(fields, FieldError{Pointer: "/full_name", Message: "must be at most 200 characters"})
	}
	if len(in.RoleSlugs) == 0 {
		fields = append(fields, FieldError{
			Pointer: "/role_slugs",
			Message: "at least one role is required; a user with no role can do nothing and is a support ticket waiting to happen",
		})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// UpdateUserInput carries a partial update.
type UpdateUserInput struct {
	FullName  *string
	Status    *UserStatus
	RoleSlugs []string // nil leaves roles unchanged; empty is rejected
}

func (in UpdateUserInput) Validate() error {
	var fields []FieldError
	if in.FullName != nil && len(*in.FullName) > 200 {
		fields = append(fields, FieldError{Pointer: "/full_name", Message: "must be at most 200 characters"})
	}
	if in.Status != nil {
		switch *in.Status {
		case UserActive, UserDisabled:
		default:
			fields = append(fields, FieldError{
				Pointer: "/status",
				Message: "must be active or disabled; invited and locked are set by the system",
			})
		}
	}
	if in.RoleSlugs != nil && len(in.RoleSlugs) == 0 {
		fields = append(fields, FieldError{Pointer: "/role_slugs", Message: "at least one role is required"})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// validEmail is deliberately permissive. Anything stricter rejects addresses
// that are legal and deliverable; the authoritative check is whether the
// invitation arrives.
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 320 {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
