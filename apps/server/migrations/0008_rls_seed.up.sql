-- 0008_rls_seed — row-level security and system seed data.

-- ---------------------------------------------------------------------------
-- Row-level security
-- ---------------------------------------------------------------------------
--
-- The repository layer sets `SET LOCAL edgepilot.org_id = '<uuid>'` on every
-- transaction it opens for a user request. RLS then makes a forgotten
-- `WHERE org_id = ?` a query that returns nothing, rather than a query that
-- returns another customer's fleet.
--
-- current_org_id() returns NULL when the GUC is unset, and `org_id = NULL` is
-- never true, so the failure mode is deny-by-default.
--
-- Two database roles:
--   edgepilot_app   — the API server. RLS applies.
--   edgepilot_admin — migrations, workers that legitimately cross orgs (rollups,
--                     sweeper, retention). BYPASSRLS. Never accepts user input.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'edgepilot_app') THEN
        CREATE ROLE edgepilot_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'edgepilot_admin') THEN
        CREATE ROLE edgepilot_admin NOLOGIN BYPASSRLS;
    END IF;
END;
$$;

-- Tables with a direct org_id column get a uniform policy.
DO $$
DECLARE
    t text;
    org_scoped text[] := ARRAY[
        'users', 'sessions', 'devices', 'enrollment_tokens', 'commands',
        'software_packages', 'deployments', 'files', 'log_collections',
        'ssh_keys', 'ssh_sessions', 'alert_rules', 'alerts', 'device_events'
    ];
BEGIN
    FOREACH t IN ARRAY org_scoped LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY %I_org_isolation ON %I USING (org_id = current_org_id())'
            ' WITH CHECK (org_id = current_org_id())', t, t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO edgepilot_app', t);
    END LOOP;
END;
$$;

-- Children reached only through a parent inherit the parent's tenancy via EXISTS.
-- These subqueries are index-supported (all are lookups on the parent's primary key).
ALTER TABLE device_certificates ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_certificates FORCE ROW LEVEL SECURITY;
CREATE POLICY device_certificates_org_isolation ON device_certificates
    USING (EXISTS (SELECT 1 FROM devices d WHERE d.id = device_id AND d.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM devices d WHERE d.id = device_id AND d.org_id = current_org_id()));

ALTER TABLE device_inventory ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_inventory FORCE ROW LEVEL SECURITY;
CREATE POLICY device_inventory_org_isolation ON device_inventory
    USING (EXISTS (SELECT 1 FROM devices d WHERE d.id = device_id AND d.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM devices d WHERE d.id = device_id AND d.org_id = current_org_id()));

ALTER TABLE command_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_results FORCE ROW LEVEL SECURITY;
CREATE POLICY command_results_org_isolation ON command_results
    USING (EXISTS (SELECT 1 FROM commands c WHERE c.id = command_id AND c.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM commands c WHERE c.id = command_id AND c.org_id = current_org_id()));

ALTER TABLE command_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY command_approvals_org_isolation ON command_approvals
    USING (EXISTS (SELECT 1 FROM commands c WHERE c.id = command_id AND c.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM commands c WHERE c.id = command_id AND c.org_id = current_org_id()));

ALTER TABLE package_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE package_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY package_versions_org_isolation ON package_versions
    USING (EXISTS (SELECT 1 FROM software_packages p WHERE p.id = package_id AND p.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM software_packages p WHERE p.id = package_id AND p.org_id = current_org_id()));

ALTER TABLE deployment_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_status FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_status_org_isolation ON deployment_status
    USING (EXISTS (SELECT 1 FROM deployments d WHERE d.id = deployment_id AND d.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM deployments d WHERE d.id = deployment_id AND d.org_id = current_org_id()));

ALTER TABLE ssh_key_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE ssh_key_devices FORCE ROW LEVEL SECURITY;
CREATE POLICY ssh_key_devices_org_isolation ON ssh_key_devices
    USING (EXISTS (SELECT 1 FROM devices d WHERE d.id = device_id AND d.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM devices d WHERE d.id = device_id AND d.org_id = current_org_id()));

ALTER TABLE user_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_roles FORCE ROW LEVEL SECURITY;
CREATE POLICY user_roles_org_isolation ON user_roles
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.org_id = current_org_id()));

-- audit_logs: readable within the org, insert-only (UPDATE/DELETE already blocked
-- by the immutability triggers).
ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_logs_org_read ON audit_logs FOR SELECT USING (org_id = current_org_id());
CREATE POLICY audit_logs_org_write ON audit_logs FOR INSERT WITH CHECK (org_id = current_org_id());

-- Enabling RLS grants nothing; the policies above only constrain access that a
-- GRANT has already allowed. The child tables therefore need explicit privileges
-- of their own, or every query against them fails with "permission denied"
-- regardless of the policy.
GRANT SELECT, INSERT, UPDATE, DELETE ON
    device_certificates, device_inventory, command_results, command_approvals,
    package_versions, deployment_status, ssh_key_devices, user_roles
TO edgepilot_app;

-- device_metrics is keyed by device_id only; the org filter is applied by joining
-- devices, which is itself RLS-protected. Adding a redundant org_id column here
-- would cost 16 bytes on 28.8 M rows/day for no security benefit.
GRANT SELECT, INSERT ON device_metrics, device_metrics_hourly TO edgepilot_app;

-- Reference data: readable by all, writable only by admin.
GRANT SELECT ON organizations, roles, permissions, role_permissions,
                command_definitions, agent_versions TO edgepilot_app;
GRANT INSERT, UPDATE ON organizations, roles, role_permissions TO edgepilot_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO edgepilot_app;

-- edgepilot_admin carries BYPASSRLS, but BYPASSRLS only skips policies — it confers
-- no table privileges. The cross-org workers (rollup, sweeper, retention, audit
-- chain) need real grants to do their job.
GRANT ALL ON ALL TABLES    IN SCHEMA public TO edgepilot_admin;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO edgepilot_admin;
GRANT USAGE ON SCHEMA public TO edgepilot_app, edgepilot_admin;

-- Tables created by later migrations must not silently arrive without privileges.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT ALL ON TABLES TO edgepilot_admin;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT ALL ON SEQUENCES TO edgepilot_admin;

-- ---------------------------------------------------------------------------
-- Seed: permissions
-- ---------------------------------------------------------------------------

INSERT INTO permissions (slug, resource, action, description) VALUES
    ('device:read',            'device',     'read',            'View devices, inventory, and metrics'),
    ('device:write',           'device',     'write',           'Edit device metadata, labels, and notes'),
    ('device:delete',          'device',     'delete',          'Decommission and delete devices'),
    ('device:approve',         'device',     'approve',         'Approve or reject device enrollment'),
    ('command:read',           'command',    'read',            'View command history and results'),
    ('command:execute',        'command',    'execute',         'Run non-destructive allowlisted commands'),
    ('command:destructive',    'command',    'destructive',     'Run destructive commands such as restart and shutdown'),
    ('command:custom_script',  'command',    'custom_script',   'Request execution of a custom script'),
    ('command:approve',        'command',    'approve',         'Approve another user''s command request'),
    ('file:read',              'file',       'read',            'Browse and download files from devices'),
    ('file:write',             'file',       'write',           'Upload, rename, and create folders on devices'),
    ('file:delete',            'file',       'delete',          'Delete files on devices'),
    ('deployment:read',        'deployment', 'read',            'View packages and deployments'),
    ('deployment:write',       'deployment', 'write',           'Upload packages and create deployments'),
    ('deployment:execute',     'deployment', 'execute',         'Start, pause, cancel, and roll back deployments'),
    ('log:read',               'log',        'read',            'View and download logs'),
    ('log:collect',            'log',        'collect',         'Request log collection from a device'),
    ('ssh:read',               'ssh',        'read',            'View SSH keys and session history'),
    ('ssh:connect',            'ssh',        'connect',         'Open an SSH session to a device'),
    ('ssh:manage_keys',        'ssh',        'manage_keys',     'Add, scope, and revoke SSH keys'),
    ('alert:read',             'alert',      'read',            'View alerts'),
    ('alert:write',            'alert',      'write',           'Acknowledge, resolve, and configure alerts'),
    ('user:read',              'user',       'read',            'View users and their roles'),
    ('user:write',             'user',       'write',           'Invite, edit, and disable users'),
    ('role:write',             'role',       'write',           'Create roles and assign permissions'),
    ('audit:read',             'audit',      'read',            'Read the audit log'),
    ('org:write',              'org',        'write',           'Change organization settings'),
    ('agent:release',          'agent',      'release',         'Publish and roll out agent versions');

-- ---------------------------------------------------------------------------
-- Seed: built-in roles
-- ---------------------------------------------------------------------------

INSERT INTO roles (id, org_id, slug, name, description, is_system) VALUES
    ('00000000-0000-0000-0000-000000000001', NULL, 'admin',    'Admin',
     'Full control of the organization, its devices, users, and settings.', true),
    ('00000000-0000-0000-0000-000000000002', NULL, 'operator', 'Operator',
     'Day-to-day fleet operations: commands, files, deployments, SSH. Cannot manage users or run custom scripts.', true),
    ('00000000-0000-0000-0000-000000000003', NULL, 'viewer',   'Viewer',
     'Read-only visibility across the fleet.', true);

-- Admin: everything except agent:release, which is a vendor-level capability held
-- by the internal operations org, not by customer admins.
INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000001', id FROM permissions
WHERE slug <> 'agent:release';

INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000002', id FROM permissions WHERE slug IN (
    'device:read', 'device:write',
    'command:read', 'command:execute', 'command:destructive',
    'file:read', 'file:write', 'file:delete',
    'deployment:read', 'deployment:write', 'deployment:execute',
    'log:read', 'log:collect',
    'ssh:read', 'ssh:connect',
    'alert:read', 'alert:write'
);
-- Deliberately withheld from Operator: command:custom_script, command:approve,
-- ssh:manage_keys, device:approve, user:*, role:write, audit:read. Custom scripts
-- and their approval must be separable, or the two-person rule is theatre.

INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000003', id FROM permissions WHERE slug IN (
    'device:read', 'command:read', 'deployment:read',
    'log:read', 'ssh:read', 'alert:read'
);

-- ---------------------------------------------------------------------------
-- Seed: command catalogue
-- ---------------------------------------------------------------------------
--
-- params_schema is JSON Schema 2020-12. additionalProperties:false everywhere —
-- an unrecognised parameter is a rejected command, not an ignored field.

INSERT INTO command_definitions
    (slug, name, description, category, required_permission, requires_approval,
     is_destructive, default_timeout_seconds, params_schema)
VALUES
    ('restart', 'Restart', 'Gracefully reboot the device.', 'power',
     'command:destructive', false, true, 120,
     '{"type":"object","additionalProperties":false,
       "properties":{"delay_seconds":{"type":"integer","minimum":0,"maximum":3600,"default":0},
                     "reason":{"type":"string","maxLength":500}}}'),

    ('shutdown', 'Shut Down', 'Gracefully power off the device. It will require physical or Wake-on-LAN intervention to return.', 'power',
     'command:destructive', true, true, 120,
     '{"type":"object","additionalProperties":false,
       "properties":{"delay_seconds":{"type":"integer","minimum":0,"maximum":3600,"default":0},
                     "reason":{"type":"string","maxLength":500}}}'),

    ('collect_logs', 'Collect Logs', 'Build and upload a compressed log bundle.', 'diagnostics',
     'log:collect', false, false, 1800,
     '{"type":"object","additionalProperties":false,
       "required":["since"],
       "properties":{"sources":{"type":"array","uniqueItems":true,
                                "items":{"enum":["system","application","crash","agent","install","network"]},
                                "default":["system","agent"]},
                     "since":{"type":"string","format":"date-time"},
                     "until":{"type":"string","format":"date-time"},
                     "max_size_bytes":{"type":"integer","minimum":1048576,"maximum":1073741824}}}'),

    ('flush_dns', 'Flush DNS Cache', 'Clear the DNS cache and restart mDNSResponder.', 'network',
     'command:execute', false, false, 60,
     '{"type":"object","additionalProperties":false}'),

    ('restart_agent', 'Restart Agent', 'Restart the EdgePilot LaunchDaemon.', 'agent',
     'command:execute', false, false, 120,
     '{"type":"object","additionalProperties":false}'),

    ('update_agent', 'Update Agent', 'Download, verify, and install a signed agent release.', 'agent',
     'command:execute', false, false, 900,
     '{"type":"object","additionalProperties":false,
       "required":["version"],
       "properties":{"version":{"type":"string","pattern":"^\\\\d+\\\\.\\\\d+\\\\.\\\\d+(-[0-9A-Za-z.-]+)?$"}}}'),

    ('install_package', 'Install Package', 'Download, verify, and install a signed package version.', 'software',
     'deployment:execute', false, false, 3600,
     '{"type":"object","additionalProperties":false,
       "required":["package_version_id"],
       "properties":{"package_version_id":{"type":"string","format":"uuid"},
                     "deployment_id":{"type":"string","format":"uuid"}}}'),

    ('restart_service', 'Restart Service', 'Restart a launchd service by label.', 'maintenance',
     'command:execute', false, false, 120,
     '{"type":"object","additionalProperties":false,
       "required":["label"],
       "properties":{"label":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"},
                     "domain":{"enum":["system","gui"],"default":"system"}}}'),

    ('refresh_inventory', 'Refresh Inventory', 'Force a full inventory collection.', 'diagnostics',
     'command:execute', false, false, 300,
     '{"type":"object","additionalProperties":false,
       "properties":{"sections":{"type":"array","uniqueItems":true,
                                 "items":{"enum":["system","hardware","network","storage",
                                                  "applications","services","security","peripherals"]}}}}'),

    ('custom_script', 'Custom Script', 'Execute an operator-supplied script. Requires a second approver and a signed payload.', 'maintenance',
     'command:custom_script', true, true, 900,
     '{"type":"object","additionalProperties":false,
       "properties":{"interpreter":{"enum":["/bin/bash","/bin/zsh","/usr/bin/python3"],"default":"/bin/bash"},
                     "arguments":{"type":"array","maxItems":32,"items":{"type":"string","maxLength":1024}},
                     "working_directory":{"type":"string","maxLength":1024},
                     "run_as":{"enum":["root","edgepilot"],"default":"edgepilot"}}}');
