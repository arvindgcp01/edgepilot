-- Refresh-token families.
--
-- The token itself is never stored, only its SHA-256, so a database disclosure
-- yields nothing usable. family_id and parent_id exist for reuse detection:
-- presenting a token that was already rotated means it leaked, and the response
-- is to revoke the entire family rather than just that one session.

-- name: CreateSession :one
INSERT INTO sessions (
    user_id, org_id, refresh_token_hash, family_id, parent_id,
    user_agent, ip, expires_at
) VALUES (
    sqlc.arg('user_id'), sqlc.arg('org_id'), sqlc.arg('refresh_token_hash'),
    sqlc.arg('family_id'), sqlc.narg('parent_id'),
    sqlc.arg('user_agent'), sqlc.narg('ip')::inet, sqlc.arg('expires_at')
)
RETURNING id, user_id, org_id, family_id, parent_id, user_agent, ip,
          expires_at, last_used_at, revoked_at, revoked_reason, created_at;

-- Look up by token hash, returning revoked and expired rows too.
--
-- Filtering them out here would collapse two very different cases into one:
-- "no such token" is a client with garbage, while "token that was already
-- rotated" is a leaked credential in active use. The caller must be able to
-- tell them apart to revoke the family.
--
-- name: GetSessionByTokenHash :one
SELECT id, user_id, org_id, family_id, parent_id, user_agent, ip,
       expires_at, last_used_at, revoked_at, revoked_reason, created_at
FROM sessions
WHERE refresh_token_hash = $1;

-- name: RevokeSession :execrows
UPDATE sessions
SET revoked_at = now(), revoked_reason = sqlc.arg('reason')
WHERE id = sqlc.arg('id') AND revoked_at IS NULL;

-- name: MarkSessionUsed :execrows
UPDATE sessions
SET last_used_at = now()
WHERE id = $1;

-- Revoke every session descended from one login.
--
-- The blast radius is deliberate. On refresh-token reuse we do not know which
-- holder is legitimate, so the safe move is to end them all and make the real
-- user sign in again.
--
-- name: RevokeSessionFamily :execrows
UPDATE sessions
SET revoked_at = now(), revoked_reason = sqlc.arg('reason')
WHERE family_id = sqlc.arg('family_id') AND revoked_at IS NULL;

-- name: RevokeUserSessions :execrows
UPDATE sessions
SET revoked_at = now(), revoked_reason = sqlc.arg('reason')
WHERE user_id = sqlc.arg('user_id') AND revoked_at IS NULL;

-- name: ListUserSessions :many
SELECT id, user_id, org_id, family_id, parent_id, user_agent, ip,
       expires_at, last_used_at, revoked_at, revoked_reason, created_at
FROM sessions
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- Retention. Expired sessions are kept briefly after expiry so that a reuse
-- attempt against a just-expired token is still recognisable as reuse rather
-- than as an unknown token.
--
-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < sqlc.arg('before')::timestamptz;
