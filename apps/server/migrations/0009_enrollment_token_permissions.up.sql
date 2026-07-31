-- Permissions for enrollment-token management.
--
-- The catalogue in 0008 was written before the REST API existed, and it covers
-- every resource an operator touches *except* the one that admits new hardware
-- to the fleet. Enrollment tokens were managed only by `epctl` at that point,
-- which authenticates as a database superuser and consults no permission at all.
--
-- These are deliberately not folded into `device:approve`. Approving a device is
-- a decision about one machine that has already asked to join; minting an
-- enrollment token creates a credential that admits machines nobody has seen
-- yet — and with auto_approve, admits them without a second decision. An
-- operator who should be able to approve the Mac in front of them should not
-- necessarily be able to issue a credential that enrolls a hundred more.

INSERT INTO permissions (slug, resource, action, description) VALUES
    ('enrollment_token:read',  'enrollment_token', 'read',
     'View enrollment tokens and their usage'),
    ('enrollment_token:write', 'enrollment_token', 'write',
     'Create and revoke enrollment tokens')
ON CONFLICT (slug) DO NOTHING;

-- Admin holds everything except agent:release, which is the rule 0008 states.
INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000001', id FROM permissions
WHERE slug IN ('enrollment_token:read', 'enrollment_token:write')
ON CONFLICT DO NOTHING;

-- Operator can see which tokens are outstanding — that is fleet visibility, and
-- an operator investigating an unexpected device needs it — but cannot mint one.
INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000002', id FROM permissions
WHERE slug = 'enrollment_token:read'
ON CONFLICT DO NOTHING;

-- Viewer sees the fleet, not its credentials.
