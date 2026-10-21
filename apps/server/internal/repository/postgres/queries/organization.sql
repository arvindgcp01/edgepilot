-- Organizations are the tenancy root. Every other table hangs off one, and the
-- org id set here is what the repository layer feeds to `SET LOCAL
-- edgepilot.org_id` for every subsequent query in a request.
--
-- This table is deliberately NOT row-level-secured: a user must be able to read
-- their own organization before a scope exists to read it under. Access is
-- constrained by the caller's token instead, which carries exactly one org.

-- name: CreateOrganization :one
INSERT INTO organizations (name, slug, settings)
VALUES ($1, $2, COALESCE(sqlc.narg('settings')::jsonb, '{}'::jsonb))
RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations
WHERE slug = $1 AND deleted_at IS NULL;

-- Cursor pagination on (created_at, id): offset pagination against a table
-- taking concurrent inserts silently skips and duplicates rows between pages.
-- The id tiebreaker keeps the order total when two rows share a timestamp.
--
-- name: ListOrganizations :many
SELECT * FROM organizations
WHERE deleted_at IS NULL
  AND (
    sqlc.narg('cursor_created_at')::timestamptz IS NULL
    OR (created_at, id) < (sqlc.narg('cursor_created_at')::timestamptz, sqlc.narg('cursor_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size');

-- COALESCE against a nullable argument gives a partial update without building
-- SQL by hand: an argument left NULL leaves its column untouched.
--
-- name: UpdateOrganization :one
UPDATE organizations
SET name     = COALESCE(sqlc.narg('name')::text, name),
    status   = COALESCE(sqlc.narg('status')::text, status),
    settings = COALESCE(sqlc.narg('settings')::jsonb, settings)
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- Soft delete, so audit history keeps referring to something legible. The
-- partial unique index on slug frees the name for reuse once deleted_at is set.
--
-- name: SoftDeleteOrganization :execrows
UPDATE organizations
SET deleted_at = now(), status = 'archived'
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountDevicesByOrganization :one
SELECT count(*) FROM devices
WHERE org_id = $1 AND deleted_at IS NULL;
