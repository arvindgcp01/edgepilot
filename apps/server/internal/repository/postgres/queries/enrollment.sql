-- Enrollment tokens and device certificates.

-- name: CreateEnrollmentToken :one
INSERT INTO enrollment_tokens (
    org_id, token_hash, token_prefix, description,
    max_uses, auto_approve, labels, expires_at, created_by
) VALUES (
    sqlc.arg('org_id'), sqlc.arg('token_hash'), sqlc.arg('token_prefix'),
    sqlc.arg('description'), sqlc.narg('max_uses'), sqlc.arg('auto_approve'),
    sqlc.arg('labels')::jsonb, sqlc.arg('expires_at'), sqlc.narg('created_by')
)
RETURNING id, org_id, token_prefix, description, max_uses, use_count,
          auto_approve, labels, expires_at, revoked_at, created_by, created_at;

-- name: ListEnrollmentTokens :many
SELECT id, org_id, token_prefix, description, max_uses, use_count,
       auto_approve, labels, expires_at, revoked_at, created_by, created_at
FROM enrollment_tokens
WHERE org_id = sqlc.arg('org_id')
  AND (
    sqlc.narg('cursor_id')::uuid IS NULL
    OR (created_at, id) < (sqlc.narg('cursor_time')::timestamptz, sqlc.narg('cursor_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size');

-- name: RevokeEnrollmentToken :execrows
UPDATE enrollment_tokens
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- Validate and consume a token in a single statement.
--
-- Every condition — not revoked, not expired, uses remaining — lives in the
-- WHERE clause of the UPDATE, so the check and the increment are one atomic
-- operation. A read-then-write would let two agents enrolling simultaneously
-- against a single-use token both pass the check and both enrol.
--
-- Returning no row means "not usable"; the caller cannot distinguish unknown
-- from expired from exhausted, which is correct — the agent gets one answer.
--
-- name: RedeemEnrollmentToken :one
UPDATE enrollment_tokens
SET use_count = use_count + 1
WHERE token_hash = sqlc.arg('token_hash')
  AND revoked_at IS NULL
  AND expires_at > sqlc.arg('now')::timestamptz
  AND (max_uses IS NULL OR use_count < max_uses)
RETURNING id, org_id, token_prefix, description, max_uses, use_count,
          auto_approve, labels, expires_at, revoked_at, created_by, created_at;

-- ---------------------------------------------------------------------------
-- Device certificates
-- ---------------------------------------------------------------------------

-- name: IssueDeviceCertificate :one
INSERT INTO device_certificates (
    device_id, serial, fingerprint_sha256, public_key_der,
    certificate_pem, not_before, not_after, issued_via
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetCertificateByFingerprint :one
SELECT * FROM device_certificates WHERE fingerprint_sha256 = $1;

-- name: ListCertificatesForDevice :many
SELECT * FROM device_certificates
WHERE device_id = $1
ORDER BY not_after DESC;

-- The certificate the device should currently be presenting: unrevoked, valid
-- now, and the longest-lived of those. During renewal two certificates are
-- briefly valid at once, and the newer one is the answer.
--
-- name: GetCurrentCertificate :one
SELECT * FROM device_certificates
WHERE device_id = sqlc.arg('device_id')
  AND revoked_at IS NULL
  AND not_before <= sqlc.arg('now')::timestamptz
  AND not_after  >  sqlc.arg('now')::timestamptz
ORDER BY not_after DESC
LIMIT 1;

-- name: RevokeCertificate :execrows
UPDATE device_certificates
SET revoked_at = now(), revoked_reason = sqlc.arg('reason'), revoked_by = sqlc.narg('revoked_by')
WHERE id = sqlc.arg('id') AND revoked_at IS NULL;

-- name: RevokeDeviceCertificates :execrows
UPDATE device_certificates
SET revoked_at = now(), revoked_reason = sqlc.arg('reason')
WHERE device_id = sqlc.arg('device_id') AND revoked_at IS NULL;

-- Feeds the revocation set the mTLS verify callback consults.
--
-- Unscoped across organizations by design: TLS verification happens before any
-- application-level identity exists, so there is no tenant to scope to yet.
-- Only unexpired certificates are returned — an expired one is already rejected
-- by certificate validation, so carrying it in the set would grow it without
-- bound for no benefit.
--
-- name: ListRevokedFingerprints :many
SELECT fingerprint_sha256 FROM device_certificates
WHERE revoked_at IS NOT NULL AND not_after > sqlc.arg('now')::timestamptz;

-- Certificates approaching expiry, so renewal can be nudged and a fleet-wide
-- expiry cliff is visible before it arrives.
--
-- name: ListCertificatesExpiringBefore :many
SELECT c.*, d.org_id, d.hostname
FROM device_certificates c
JOIN devices d ON d.id = c.device_id
WHERE c.revoked_at IS NULL
  AND d.deleted_at IS NULL
  AND c.not_after < sqlc.arg('before')::timestamptz
  AND c.not_after > sqlc.arg('now')::timestamptz
ORDER BY c.not_after;
