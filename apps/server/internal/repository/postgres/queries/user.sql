-- Users, credentials, and role resolution.
--
-- Note what is NOT selected by the ordinary read queries: password_hash and
-- mfa_secret_enc appear only in GetCredentials, which the authentication path
-- calls and nothing else does. A list endpoint physically cannot leak a hash it
-- never selected.

-- name: CreateUser :one
INSERT INTO users (org_id, email, password_hash, full_name, status, created_by)
VALUES (
    sqlc.arg('org_id'), sqlc.arg('email'), sqlc.narg('password_hash'),
    sqlc.arg('full_name'), sqlc.arg('status'), sqlc.narg('created_by')
)
RETURNING id, org_id, email, full_name, status, mfa_enabled,
          last_login_at, password_changed_at, locked_until,
          created_at, updated_at, deleted_at;

-- name: GetUser :one
SELECT id, org_id, email, full_name, status, mfa_enabled,
       last_login_at, password_changed_at, locked_until,
       created_at, updated_at, deleted_at
FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByEmail :one
SELECT id, org_id, email, full_name, status, mfa_enabled,
       last_login_at, password_changed_at, locked_until,
       created_at, updated_at, deleted_at
FROM users
WHERE org_id = $1 AND email = $2 AND deleted_at IS NULL;

-- name: ListUsers :many
SELECT id, org_id, email, full_name, status, mfa_enabled,
       last_login_at, password_changed_at, locked_until,
       created_at, updated_at, deleted_at
FROM users
WHERE org_id = sqlc.arg('org_id')
  AND deleted_at IS NULL
  AND (
    sqlc.narg('cursor_id')::uuid IS NULL
    OR (created_at, id) < (sqlc.narg('cursor_time')::timestamptz, sqlc.narg('cursor_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size');

-- name: UpdateUser :one
UPDATE users
SET full_name = COALESCE(sqlc.narg('full_name')::text, full_name),
    status    = COALESCE(sqlc.narg('status')::text, status)
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, org_id, email, full_name, status, mfa_enabled,
          last_login_at, password_changed_at, locked_until,
          created_at, updated_at, deleted_at;

-- name: SoftDeleteUser :execrows
UPDATE users
SET deleted_at = now(), status = 'disabled'
WHERE id = $1 AND deleted_at IS NULL;

-- The only query that reads secret material.
--
-- It deliberately does NOT filter on status or deleted_at. The caller must load
-- the credentials, run the password comparison, and only then decide the account
-- cannot sign in — otherwise a disabled account short-circuits before the hash
-- comparison and its faster response discloses that the account exists.
--
-- name: GetCredentials :one
SELECT id AS user_id, org_id, password_hash, mfa_secret_enc, mfa_enabled,
       failed_login_count, locked_until, status, deleted_at
FROM users
WHERE org_id = $1 AND email = $2;

-- name: RecordLoginSuccess :execrows
UPDATE users
SET last_login_at      = sqlc.arg('at'),
    failed_login_count = 0,
    locked_until       = NULL,
    status             = CASE WHEN status = 'locked' THEN 'active' ELSE status END
WHERE id = sqlc.arg('id') AND deleted_at IS NULL;

-- Increment and lock in one statement.
--
-- A read-then-write would let two simultaneous failed attempts each read the
-- same count and both write count+1, so the threshold is reached a try later
-- than intended. Deciding the lock inside the same UPDATE closes that window
-- and returns the outcome, saving a second round trip.
--
-- The expiry is supplied by the caller rather than computed here with now().
-- The service evaluates whether a lock has passed, so if the database picked
-- the expiry there would be two clocks deciding one fact — which works in
-- production only because both happen to be wall time, and cannot be tested
-- deterministically at all.
--
-- name: RecordLoginFailure :one
UPDATE users
SET failed_login_count = failed_login_count + 1,
    locked_until = CASE
                     WHEN failed_login_count + 1 >= sqlc.arg('threshold')::int
                       THEN sqlc.arg('lock_until')::timestamptz
                     ELSE locked_until
                   END,
    status = CASE
               WHEN failed_login_count + 1 >= sqlc.arg('threshold')::int
                 THEN 'locked'
               ELSE status
             END
WHERE id = sqlc.arg('id')
RETURNING failed_login_count, locked_until, (status = 'locked') AS is_locked;

-- name: SetPasswordHash :execrows
UPDATE users
SET password_hash       = sqlc.arg('password_hash'),
    password_changed_at = sqlc.arg('at'),
    failed_login_count  = 0,
    locked_until        = NULL,
    status              = CASE WHEN status IN ('invited', 'locked') THEN 'active' ELSE status END
WHERE id = sqlc.arg('id') AND deleted_at IS NULL;

-- Resolve a user's roles and the permissions those roles grant.
--
-- Returned as two arrays from one query rather than two round trips: this runs
-- on every authenticated request, and the permission set is what every
-- authorization check consults.
--
-- name: ResolveUserPermissions :one
SELECT
    COALESCE(array_agg(DISTINCT r.slug) FILTER (WHERE r.slug IS NOT NULL), '{}')::text[] AS roles,
    COALESCE(array_agg(DISTINCT p.slug) FILTER (WHERE p.slug IS NOT NULL), '{}')::text[] AS permissions
FROM user_roles ur
JOIN roles r ON r.id = ur.role_id
LEFT JOIN role_permissions rp ON rp.role_id = r.id
LEFT JOIN permissions p ON p.id = rp.permission_id
WHERE ur.user_id = $1;

-- name: ListRoles :many
SELECT r.id, r.org_id, r.slug, r.name, r.description, r.is_system,
       COALESCE(array_agg(p.slug) FILTER (WHERE p.slug IS NOT NULL), '{}')::text[] AS permissions
FROM roles r
LEFT JOIN role_permissions rp ON rp.role_id = r.id
LEFT JOIN permissions p ON p.id = rp.permission_id
WHERE r.org_id IS NULL OR r.org_id = sqlc.narg('org_id')::uuid
GROUP BY r.id
ORDER BY r.is_system DESC, r.slug;

-- name: GetRoleBySlug :one
SELECT r.id, r.org_id, r.slug, r.name, r.description, r.is_system,
       COALESCE(array_agg(p.slug) FILTER (WHERE p.slug IS NOT NULL), '{}')::text[] AS permissions
FROM roles r
LEFT JOIN role_permissions rp ON rp.role_id = r.id
LEFT JOIN permissions p ON p.id = rp.permission_id
WHERE r.slug = sqlc.arg('slug')
  AND (r.org_id IS NULL OR r.org_id = sqlc.narg('org_id')::uuid)
GROUP BY r.id;

-- name: ListPermissionSlugs :many
SELECT slug FROM permissions ORDER BY slug;

-- name: DeleteUserRoles :execrows
DELETE FROM user_roles WHERE user_id = $1;

-- Assign roles by slug in one statement.
--
-- Resolving slugs to ids inside the INSERT means an unknown slug inserts
-- nothing rather than silently granting a subset; the caller compares the
-- returned count against what it asked for.
--
-- name: AssignUserRoles :execrows
INSERT INTO user_roles (user_id, role_id, granted_by)
SELECT sqlc.arg('user_id'), r.id, sqlc.narg('granted_by')
FROM roles r
WHERE r.slug = ANY(sqlc.arg('slugs')::text[])
  AND (r.org_id IS NULL OR r.org_id = sqlc.narg('org_id')::uuid)
ON CONFLICT (user_id, role_id) DO NOTHING;
