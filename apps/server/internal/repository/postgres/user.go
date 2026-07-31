package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

type userRepo struct{ s *Store }

func (r *userRepo) Create(ctx context.Context, in domain.CreateUserInput, passwordHash string) (*domain.User, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	// An invited user has no password yet; they set one from the invitation
	// link. Status follows from that rather than being passed in, so a caller
	// cannot create an already-active account with no credentials.
	status := domain.UserInvited
	if passwordHash != "" {
		status = domain.UserActive
	}

	var out *domain.User
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
			OrgID:        toPgUUID(in.OrgID),
			Email:        in.Email,
			PasswordHash: ptrString(passwordHash),
			FullName:     in.FullName,
			Status:       string(status),
			CreatedBy:    toPgUUIDPtr(in.CreatedBy),
		})
		if err != nil {
			return translate(err, "user")
		}
		out = userFromCreateRow(row)

		// Roles are assigned in the same transaction: a user that exists with
		// no roles can do nothing, and would need a second manual step to
		// become useful.
		if err := assignRoles(ctx, q, out.ID, in.OrgID, in.RoleSlugs, in.CreatedBy); err != nil {
			return err
		}
		out.Roles = slices.Clone(in.RoleSlugs)
		return nil
	})
	return out, err
}

func (r *userRepo) Get(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	var out *domain.User
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetUser(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "user")
		}
		out = userFromGetRow(row)
		roles, _, err := resolvePermissions(ctx, q, id)
		if err != nil {
			return err
		}
		out.Roles = roles
		return nil
	})
	return out, err
}

func (r *userRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errNoTenantScope)
	}
	var out *domain.User
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetUserByEmail(ctx, sqlcgen.GetUserByEmailParams{
			OrgID: toPgUUID(orgID),
			Email: email,
		})
		if err != nil {
			return translate(err, "user")
		}
		out = userFromEmailRow(row)
		roles, _, err := resolvePermissions(ctx, q, out.ID)
		if err != nil {
			return err
		}
		out.Roles = roles
		return nil
	})
	return out, err
}

func (r *userRepo) List(ctx context.Context, page domain.PageRequest) (domain.Page[*domain.User], error) {
	page = page.Normalize()
	var result domain.Page[*domain.User]

	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return result, domain.Internal(errNoTenantScope)
	}

	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListUsers(ctx, sqlcgen.ListUsersParams{
			OrgID:      toPgUUID(orgID),
			CursorTime: toPgTime(page.Cursor.Time),
			CursorID:   toPgUUID(page.Cursor.ID),
			PageSize:   int32(page.Limit + 1),
		})
		if err != nil {
			return translate(err, "user")
		}
		users := make([]*domain.User, 0, len(rows))
		for _, row := range rows {
			users = append(users, userFromListRow(row))
		}
		result = domain.NewPage(users, page.Limit, func(u *domain.User) domain.Cursor {
			return domain.Cursor{Time: u.CreatedAt, ID: u.ID}
		})
		return nil
	})
	return result, err
}

func (r *userRepo) Update(ctx context.Context, id uuid.UUID, in domain.UpdateUserInput) (*domain.User, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errNoTenantScope)
	}

	var status *string
	if in.Status != nil {
		s := string(*in.Status)
		status = &s
	}

	var out *domain.User
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.UpdateUser(ctx, sqlcgen.UpdateUserParams{
			ID:       toPgUUID(id),
			FullName: in.FullName,
			Status:   status,
		})
		if err != nil {
			return translate(err, "user")
		}
		out = userFromUpdateRow(row)

		if in.RoleSlugs != nil {
			if _, err := q.DeleteUserRoles(ctx, toPgUUID(id)); err != nil {
				return translate(err, "user")
			}
			if err := assignRoles(ctx, q, id, orgID, in.RoleSlugs, nil); err != nil {
				return err
			}
		}

		// Disabling an account must end its sessions in the same transaction.
		// Leaving them live would mean a disabled user keeps working for up to
		// the refresh-token lifetime.
		if in.Status != nil && *in.Status == domain.UserDisabled {
			if _, err := q.RevokeUserSessions(ctx, sqlcgen.RevokeUserSessionsParams{
				UserID: toPgUUID(id),
				Reason: ptrString("user disabled"),
			}); err != nil {
				return translate(err, "session")
			}
		}

		roles, _, err := resolvePermissions(ctx, q, id)
		if err != nil {
			return err
		}
		out.Roles = roles
		return nil
	})
	return out, err
}

func (r *userRepo) SoftDelete(ctx context.Context, id uuid.UUID) error {
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.SoftDeleteUser(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "user")
		}
		if n == 0 {
			return notFound("user", id.String())
		}
		// Same reasoning as disabling: the account is gone, so its sessions go
		// with it, atomically.
		if _, err := q.RevokeUserSessions(ctx, sqlcgen.RevokeUserSessionsParams{
			UserID: toPgUUID(id),
			Reason: ptrString("user deleted"),
		}); err != nil {
			return translate(err, "session")
		}
		return nil
	})
}

func (r *userRepo) Credentials(ctx context.Context, orgID uuid.UUID, email string) (*domain.Credentials, error) {
	var out *domain.Credentials
	// Unscoped: this runs during authentication, before a session exists to
	// scope by. The organization is supplied explicitly by the caller, which
	// resolved it from the login request.
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetCredentials(ctx, sqlcgen.GetCredentialsParams{
			OrgID: toPgUUID(orgID),
			Email: email,
		})
		if err != nil {
			return translate(err, "user")
		}
		out = &domain.Credentials{
			UserID:             fromPgUUID(row.UserID),
			OrgID:              fromPgUUID(row.OrgID),
			PasswordHash:       derefString(row.PasswordHash),
			MFASecretEncrypted: row.MfaSecretEnc,
			MFAEnabled:         row.MfaEnabled,
			FailedLoginCount:   int(row.FailedLoginCount),
			LockedUntil:        fromPgTimePtr(row.LockedUntil),
			Status:             domain.UserStatus(row.Status),
			DeletedAt:          fromPgTimePtr(row.DeletedAt),
		}
		return nil
	})
	return out, err
}

func (r *userRepo) RecordLoginSuccess(ctx context.Context, userID uuid.UUID, at time.Time) error {
	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.RecordLoginSuccess(ctx, sqlcgen.RecordLoginSuccessParams{
			ID: toPgUUID(userID),
			At: toPgTime(at),
		}); err != nil {
			return translate(err, "user")
		}
		return nil
	})
}

func (r *userRepo) RecordLoginFailure(ctx context.Context, userID uuid.UUID, lockUntil time.Time, threshold int) (bool, error) {
	var locked bool
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.RecordLoginFailure(ctx, sqlcgen.RecordLoginFailureParams{
			ID:        toPgUUID(userID),
			Threshold: int32(threshold),
			LockUntil: toPgTime(lockUntil),
		})
		if err != nil {
			return translate(err, "user")
		}
		locked = row.IsLocked
		return nil
	})
	return locked, err
}

func (r *userRepo) SetPasswordHash(ctx context.Context, userID uuid.UUID, hash string, at time.Time) error {
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.SetPasswordHash(ctx, sqlcgen.SetPasswordHashParams{
			ID:           toPgUUID(userID),
			PasswordHash: ptrString(hash),
			At:           toPgTime(at),
		})
		if err != nil {
			return translate(err, "user")
		}
		if n == 0 {
			return notFound("user", userID.String())
		}
		return nil
	})
}

func (r *userRepo) ResolvePermissions(ctx context.Context, userID uuid.UUID) ([]string, []string, error) {
	var roles, perms []string
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		var err error
		roles, perms, err = resolvePermissions(ctx, q, userID)
		return err
	})
	return roles, perms, err
}

// --- roles -----------------------------------------------------------------

type roleRepo struct{ s *Store }

func (r *roleRepo) List(ctx context.Context) ([]*domain.Role, error) {
	orgID, _ := domain.OrgFromContext(ctx)
	var out []*domain.Role
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListRoles(ctx, toPgUUID(orgID))
		if err != nil {
			return translate(err, "role")
		}
		out = make([]*domain.Role, 0, len(rows))
		for _, row := range rows {
			out = append(out, &domain.Role{
				ID:          fromPgUUID(row.ID),
				OrgID:       fromPgUUIDPtr(row.OrgID),
				Slug:        row.Slug,
				Name:        row.Name,
				Description: row.Description,
				IsSystem:    row.IsSystem,
				Permissions: row.Permissions,
			})
		}
		return nil
	})
	return out, err
}

func (r *roleRepo) GetBySlug(ctx context.Context, slug string) (*domain.Role, error) {
	orgID, _ := domain.OrgFromContext(ctx)
	var out *domain.Role
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetRoleBySlug(ctx, sqlcgen.GetRoleBySlugParams{
			Slug:  slug,
			OrgID: toPgUUID(orgID),
		})
		if err != nil {
			return translate(err, "role")
		}
		out = &domain.Role{
			ID:          fromPgUUID(row.ID),
			OrgID:       fromPgUUIDPtr(row.OrgID),
			Slug:        row.Slug,
			Name:        row.Name,
			Description: row.Description,
			IsSystem:    row.IsSystem,
			Permissions: row.Permissions,
		}
		return nil
	})
	return out, err
}

func (r *roleRepo) ListPermissions(ctx context.Context) ([]string, error) {
	var out []string
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		slugs, err := q.ListPermissionSlugs(ctx)
		if err != nil {
			return translate(err, "permission")
		}
		out = slugs
		return nil
	})
	return out, err
}

func (r *roleRepo) ReplaceUserRoles(ctx context.Context, userID uuid.UUID, slugs []string, grantedBy *uuid.UUID) error {
	if len(slugs) == 0 {
		return domain.Invalid("/role_slugs", "at least one role is required")
	}
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return domain.Internal(errNoTenantScope)
	}
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.DeleteUserRoles(ctx, toPgUUID(userID)); err != nil {
			return translate(err, "user")
		}
		return assignRoles(ctx, q, userID, orgID, slugs, grantedBy)
	})
}

// assignRoles inserts role grants by slug and verifies every slug resolved.
//
// The INSERT ... SELECT silently inserts nothing for an unknown slug, which
// would leave a user with fewer permissions than the caller asked for and no
// indication why. Comparing the affected count against the request turns that
// into an error naming the problem.
func assignRoles(ctx context.Context, q *sqlcgen.Queries, userID, orgID uuid.UUID, slugs []string, grantedBy *uuid.UUID) error {
	if len(slugs) == 0 {
		return nil
	}
	n, err := q.AssignUserRoles(ctx, sqlcgen.AssignUserRolesParams{
		UserID:    toPgUUID(userID),
		Slugs:     slugs,
		OrgID:     toPgUUID(orgID),
		GrantedBy: toPgUUIDPtr(grantedBy),
	})
	if err != nil {
		return translate(err, "role")
	}
	if int(n) != len(slugs) {
		return domain.Invalid("/role_slugs",
			"one or more roles do not exist: requested %d, matched %d", len(slugs), n)
	}
	return nil
}

func resolvePermissions(ctx context.Context, q *sqlcgen.Queries, userID uuid.UUID) ([]string, []string, error) {
	row, err := q.ResolveUserPermissions(ctx, toPgUUID(userID))
	if err != nil {
		return nil, nil, translate(err, "user")
	}
	return row.Roles, row.Permissions, nil
}

// --- row mapping -----------------------------------------------------------
//
// sqlc emits a distinct row type per query, so each needs its own conversion.
// They are mechanical; keeping them together makes it obvious when one drifts.

func userFromCreateRow(row sqlcgen.CreateUserRow) *domain.User {
	return &domain.User{
		ID: fromPgUUID(row.ID), OrgID: fromPgUUID(row.OrgID),
		Email: row.Email, FullName: row.FullName,
		Status: domain.UserStatus(row.Status), MFAEnabled: row.MfaEnabled,
		LastLoginAt:       fromPgTimePtr(row.LastLoginAt),
		PasswordChangedAt: fromPgTimePtr(row.PasswordChangedAt),
		LockedUntil:       fromPgTimePtr(row.LockedUntil),
		CreatedAt:         fromPgTime(row.CreatedAt), UpdatedAt: fromPgTime(row.UpdatedAt),
		DeletedAt: fromPgTimePtr(row.DeletedAt),
	}
}

func userFromGetRow(row sqlcgen.GetUserRow) *domain.User {
	return userFromCreateRow(sqlcgen.CreateUserRow(row))
}

func userFromEmailRow(row sqlcgen.GetUserByEmailRow) *domain.User {
	return userFromCreateRow(sqlcgen.CreateUserRow(row))
}

func userFromListRow(row sqlcgen.ListUsersRow) *domain.User {
	return userFromCreateRow(sqlcgen.CreateUserRow(row))
}

func userFromUpdateRow(row sqlcgen.UpdateUserRow) *domain.User {
	return userFromCreateRow(sqlcgen.CreateUserRow(row))
}
