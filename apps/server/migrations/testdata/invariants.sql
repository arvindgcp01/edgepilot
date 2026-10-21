-- Schema invariants.
--
-- Every check below asserts something the SCHEMA is supposed to guarantee on its
-- own — not something the application is trusted to do. Each emits exactly one
-- PASS or FAIL line; scripts/verify-schema.sh fails the build on any FAIL.
--
-- Run against a database with all up-migrations applied.

\set QUIET on
\pset tuples_only on
\pset format unaligned
\set ON_ERROR_STOP off

-- Records the outcome of each check.
CREATE TEMP TABLE _result (seq serial, ok boolean, label text);

-- Runs `stmt` and records whether it failed as expected.
-- want_error = true  → the statement MUST raise (the constraint works)
-- want_error = false → the statement MUST succeed (no false positives)
CREATE OR REPLACE FUNCTION _check(label text, stmt text, want_error boolean)
    RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    raised boolean := false;
    msg    text := '';
BEGIN
    BEGIN
        EXECUTE stmt;
    EXCEPTION WHEN others THEN
        raised := true;
        msg := SQLERRM;
    END;

    IF raised = want_error THEN
        INSERT INTO _result (ok, label) VALUES (true, label);
    ELSIF want_error THEN
        INSERT INTO _result (ok, label) VALUES (false, label || ' -- expected rejection, was accepted');
    ELSE
        INSERT INTO _result (ok, label) VALUES (false, label || ' -- expected success, got: ' || msg);
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION _expect(label text, query text, want anyelement)
    RETURNS void LANGUAGE plpgsql AS $$
DECLARE got text; wanted text := want::text;
BEGIN
    EXECUTE query INTO got;
    IF got IS NOT DISTINCT FROM wanted THEN
        INSERT INTO _result (ok, label) VALUES (true, label);
    ELSE
        INSERT INTO _result (ok, label)
        VALUES (false, label || ' -- expected ' || wanted || ', got ' || coalesce(got, 'NULL'));
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Fixtures: two organizations, so tenancy can be tested in both directions.
-- ---------------------------------------------------------------------------

INSERT INTO organizations (id, name, slug) VALUES
    ('11111111-1111-1111-1111-111111111111', 'Acme',   'acme'),
    ('22222222-2222-2222-2222-222222222222', 'Globex', 'globex');

INSERT INTO devices (id, org_id, serial_number, hostname) VALUES
    ('aaaaaaaa-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 'ACME-1',  'acme-1'),
    ('bbbbbbbb-0000-0000-0000-000000000001', '22222222-2222-2222-2222-222222222222', 'GLOBEX-1','globex-1');

INSERT INTO users (id, org_id, email, full_name, status) VALUES
    ('cccccccc-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 'requester@acme.test', 'Requester', 'active'),
    ('cccccccc-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111', 'approver@acme.test',  'Approver',  'active');

INSERT INTO device_certificates
    (device_id, serial, fingerprint_sha256, public_key_der, certificate_pem, not_before, not_after, issued_via)
VALUES
    ('aaaaaaaa-0000-0000-0000-000000000001', 1, sha256('a'::bytea), '\x00', 'PEM', now(), now() + interval '90 days', 'enrollment'),
    ('bbbbbbbb-0000-0000-0000-000000000001', 2, sha256('b'::bytea), '\x00', 'PEM', now(), now() + interval '90 days', 'enrollment');

INSERT INTO commands (id, org_id, device_id, definition_id, slug, status, requested_by,
                      timeout_seconds, expires_at, script_object_key, script_sha256)
SELECT 'eeeeeeee-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111',
       'aaaaaaaa-0000-0000-0000-000000000001', id, 'custom_script', 'pending_approval',
       'cccccccc-0000-0000-0000-000000000001', 900, now() + interval '1 hour',
       'scripts/abc', sha256('script'::bytea)
FROM command_definitions WHERE slug = 'custom_script';

-- ---------------------------------------------------------------------------
-- Seed data
-- ---------------------------------------------------------------------------

SELECT _expect('seed: 30 permissions',        'SELECT count(*) FROM permissions', 30);
-- Minting an enrollment token is a different capability from approving a device
-- that has already asked to join: the token admits machines nobody has seen yet,
-- and with auto_approve it admits them without a second decision.
SELECT _expect('seed: operator can see enrollment tokens but not mint them',
    $$SELECT count(*) FROM role_permissions rp
      JOIN roles r ON r.id = rp.role_id
      JOIN permissions p ON p.id = rp.permission_id
      WHERE r.slug = 'operator' AND p.slug = 'enrollment_token:write'$$, 0);
SELECT _expect('seed: admin can mint enrollment tokens',
    $$SELECT count(*) FROM role_permissions rp
      JOIN roles r ON r.id = rp.role_id
      JOIN permissions p ON p.id = rp.permission_id
      WHERE r.slug = 'admin' AND p.slug = 'enrollment_token:write'$$, 1);
SELECT _expect('seed: 3 built-in roles',      'SELECT count(*) FROM roles WHERE is_system', 3);
SELECT _expect('seed: 10 command definitions','SELECT count(*) FROM command_definitions', 10);
SELECT _expect('seed: operator lacks command:custom_script',
    $$SELECT count(*) FROM role_permissions rp
      JOIN roles r ON r.id = rp.role_id
      JOIN permissions p ON p.id = rp.permission_id
      WHERE r.slug = 'operator' AND p.slug = 'command:custom_script'$$, 0);
SELECT _expect('seed: operator lacks command:approve',
    $$SELECT count(*) FROM role_permissions rp
      JOIN roles r ON r.id = rp.role_id
      JOIN permissions p ON p.id = rp.permission_id
      WHERE r.slug = 'operator' AND p.slug = 'command:approve'$$, 0);
SELECT _expect('seed: viewer holds only read permissions',
    $$SELECT count(*) FROM role_permissions rp
      JOIN roles r ON r.id = rp.role_id
      JOIN permissions p ON p.id = rp.permission_id
      WHERE r.slug = 'viewer' AND p.action <> 'read'$$, 0);
SELECT _expect('seed: every command definition has an object params_schema',
    $$SELECT count(*) FROM command_definitions WHERE params_schema->>'type' <> 'object'$$, 0);
SELECT _expect('seed: every command definition forbids extra params',
    $$SELECT count(*) FROM command_definitions
      WHERE params_schema->>'additionalProperties' <> 'false'$$, 0);

-- ---------------------------------------------------------------------------
-- Two-person rule
-- ---------------------------------------------------------------------------

SELECT _check('two-person: requester cannot approve their own command',
    $$INSERT INTO command_approvals (command_id, decision, decided_by)
      VALUES ('eeeeeeee-0000-0000-0000-000000000001', 'approved',
              'cccccccc-0000-0000-0000-000000000001')$$, true);

SELECT _check('two-person: a second user can approve',
    $$INSERT INTO command_approvals (command_id, decision, decided_by)
      VALUES ('eeeeeeee-0000-0000-0000-000000000001', 'approved',
              'cccccccc-0000-0000-0000-000000000002')$$, false);

SELECT _check('two-person: the same approver cannot approve twice',
    $$INSERT INTO command_approvals (command_id, decision, decided_by)
      VALUES ('eeeeeeee-0000-0000-0000-000000000001', 'approved',
              'cccccccc-0000-0000-0000-000000000002')$$, true);

-- ---------------------------------------------------------------------------
-- Command integrity
-- ---------------------------------------------------------------------------

SELECT _check('command: custom_script without a script body is rejected',
    $$INSERT INTO commands (id, org_id, device_id, definition_id, slug, status, timeout_seconds, expires_at)
      SELECT gen_random_uuid(), '11111111-1111-1111-1111-111111111111',
             'aaaaaaaa-0000-0000-0000-000000000001', id, 'custom_script', 'queued',
             900, now() + interval '1 hour'
      FROM command_definitions WHERE slug = 'custom_script'$$, true);

SELECT _check('command: a built-in command carrying a script body is rejected',
    $$INSERT INTO commands (id, org_id, device_id, definition_id, slug, status, timeout_seconds,
                            expires_at, script_object_key, script_sha256)
      SELECT gen_random_uuid(), '11111111-1111-1111-1111-111111111111',
             'aaaaaaaa-0000-0000-0000-000000000001', id, 'flush_dns', 'queued',
             60, now() + interval '1 hour', 'scripts/sneaky', sha256('x'::bytea)
      FROM command_definitions WHERE slug = 'flush_dns'$$, true);

SELECT _check('command: an out-of-range timeout is rejected',
    $$INSERT INTO commands (id, org_id, device_id, definition_id, slug, status, timeout_seconds, expires_at)
      SELECT gen_random_uuid(), '11111111-1111-1111-1111-111111111111',
             'aaaaaaaa-0000-0000-0000-000000000001', id, 'flush_dns', 'queued',
             999999, now() + interval '1 hour'
      FROM command_definitions WHERE slug = 'flush_dns'$$, true);

-- ---------------------------------------------------------------------------
-- Alert deduplication
-- ---------------------------------------------------------------------------

SELECT _check('alerts: first firing alert is accepted',
    $$INSERT INTO alerts (org_id, device_id, type, severity, status, summary, fingerprint)
      VALUES ('11111111-1111-1111-1111-111111111111','aaaaaaaa-0000-0000-0000-000000000001',
              'cpu_high','warning','firing','CPU above 90%','fp-cpu-1')$$, false);

SELECT _check('alerts: a duplicate firing alert is rejected',
    $$INSERT INTO alerts (org_id, device_id, type, severity, status, summary, fingerprint)
      VALUES ('11111111-1111-1111-1111-111111111111','aaaaaaaa-0000-0000-0000-000000000001',
              'cpu_high','warning','firing','CPU above 90%','fp-cpu-1')$$, true);

UPDATE alerts SET status = 'resolved', resolved_at = now() WHERE fingerprint = 'fp-cpu-1';

SELECT _check('alerts: resolving frees the fingerprint for a new alert',
    $$INSERT INTO alerts (org_id, device_id, type, severity, status, summary, fingerprint)
      VALUES ('11111111-1111-1111-1111-111111111111','aaaaaaaa-0000-0000-0000-000000000001',
              'cpu_high','warning','firing','CPU above 90%','fp-cpu-1')$$, false);

SELECT _check('alerts: a resolved alert must carry resolved_at',
    $$INSERT INTO alerts (org_id, device_id, type, severity, status, summary, fingerprint)
      VALUES ('11111111-1111-1111-1111-111111111111','aaaaaaaa-0000-0000-0000-000000000001',
              'cpu_high','warning','resolved','x','fp-cpu-2')$$, true);

-- ---------------------------------------------------------------------------
-- Audit log immutability
-- ---------------------------------------------------------------------------

INSERT INTO audit_logs (org_id, actor_type, actor_label, action, resource_type, outcome, hash)
VALUES ('11111111-1111-1111-1111-111111111111','user','Requester','auth.login','session','success',
        sha256('chain-1'::bytea));

SELECT _check('audit: UPDATE is rejected', $$UPDATE audit_logs SET outcome = 'failure'$$, true);
SELECT _check('audit: DELETE is rejected', $$DELETE FROM audit_logs$$, true);
SELECT _expect('audit: the row survived both attempts', 'SELECT count(*) FROM audit_logs', 1);

-- ---------------------------------------------------------------------------
-- Metrics partitioning
-- ---------------------------------------------------------------------------

SELECT _check('metrics: a current sample is accepted',
    $$INSERT INTO device_metrics (device_id, time, cpu_percent)
      VALUES ('aaaaaaaa-0000-0000-0000-000000000001', now(), 12.5)$$, false);

SELECT _expect('metrics: routed to the current monthly partition',
    $$SELECT tableoid::regclass::text FROM device_metrics LIMIT 1$$,
    'device_metrics_p' || to_char(now(), 'YYYYMM'));

-- No DEFAULT partition exists, deliberately: a sample outside the managed range
-- must fail loudly rather than accumulate somewhere unattachable.
SELECT _check('metrics: a sample outside the managed range fails loudly',
    $$INSERT INTO device_metrics (device_id, time, cpu_percent)
      VALUES ('aaaaaaaa-0000-0000-0000-000000000001', now() + interval '5 years', 1)$$, true);

SELECT _expect('metrics: partitions exist at least 3 months ahead',
    $$SELECT count(*) >= 4 FROM pg_class c
      JOIN pg_inherits i ON i.inhrelid = c.oid
      WHERE i.inhparent = 'device_metrics'::regclass$$, true);

-- ---------------------------------------------------------------------------
-- Deployment and file consistency
-- ---------------------------------------------------------------------------

INSERT INTO software_packages (id, org_id, name, slug)
VALUES ('ffffffff-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','Sensor','sensor');
INSERT INTO files (org_id, kind, object_key, filename, size_bytes, status)
VALUES ('11111111-1111-1111-1111-111111111111','package','pkg/sensor-1.0.0.pkg','sensor-1.0.0.pkg',1024,'available');
INSERT INTO package_versions (id, package_id, version, format, object_key, size_bytes, sha256, signature, status)
VALUES ('ffffffff-0000-0000-0000-000000000002','ffffffff-0000-0000-0000-000000000001','1.0.0','pkg',
        'pkg/sensor-1.0.0.pkg',1024, sha256('pkg'::bytea), '\x00','ready');

SELECT _check('deployments: rolling without batch_size is rejected',
    $$INSERT INTO deployments (org_id, package_version_id, name, strategy)
      VALUES ('11111111-1111-1111-1111-111111111111','ffffffff-0000-0000-0000-000000000002','S','rolling')$$, true);

SELECT _check('deployments: scheduled without scheduled_at is rejected',
    $$INSERT INTO deployments (org_id, package_version_id, name, strategy)
      VALUES ('11111111-1111-1111-1111-111111111111','ffffffff-0000-0000-0000-000000000002','S','scheduled')$$, true);

SELECT _check('deployments: rolling with batch_size is accepted',
    $$INSERT INTO deployments (org_id, package_version_id, name, strategy, batch_size)
      VALUES ('11111111-1111-1111-1111-111111111111','ffffffff-0000-0000-0000-000000000002','S','rolling',10)$$, false);

SELECT _check('files: an upload without a remote path is rejected',
    $$INSERT INTO files (org_id, kind, device_id, object_key, filename)
      VALUES ('11111111-1111-1111-1111-111111111111','upload',
              'aaaaaaaa-0000-0000-0000-000000000001','k/1','a.txt')$$, true);

SELECT _check('log collections: an inverted time window is rejected',
    $$INSERT INTO log_collections (org_id, device_id, since, until)
      VALUES ('11111111-1111-1111-1111-111111111111','aaaaaaaa-0000-0000-0000-000000000001',
              now(), now() - interval '1 day')$$, true);

-- ---------------------------------------------------------------------------
-- Row-level security
--
-- These are the checks that matter most: they assert PostgreSQL itself refuses
-- cross-tenant access, so a forgotten WHERE org_id = ? in application code is a
-- query that returns nothing rather than a data leak.
-- ---------------------------------------------------------------------------

-- Role switching happens INSIDE these helpers, which reset the role before
-- returning. Doing it with `SET LOCAL ROLE` around the check instead would run
-- the INSERT into _result as edgepilot_app — which has no privilege on a temp
-- table owned by postgres — so the check would vanish rather than report.
-- Likewise nothing here uses ROLLBACK to undo test writes, because that would
-- discard the recorded results along with them.

CREATE OR REPLACE FUNCTION _rls_count(org uuid, tbl text) RETURNS bigint
    LANGUAGE plpgsql AS $$
DECLARE n bigint;
BEGIN
    PERFORM set_config('role', 'edgepilot_app', true);
    PERFORM set_config('edgepilot.org_id', org::text, true);
    EXECUTE format('SELECT count(*) FROM %I', tbl) INTO n;
    PERFORM set_config('role', 'none', true);
    RETURN n;
END;
$$;

-- No org scope set: current_org_id() returns NULL, `org_id = NULL` is never true,
-- so a correct policy set yields zero rows rather than every row.
CREATE OR REPLACE FUNCTION _rls_count_unscoped(tbl text) RETURNS bigint
    LANGUAGE plpgsql AS $$
DECLARE n bigint;
BEGIN
    PERFORM set_config('edgepilot.org_id', '', true);
    PERFORM set_config('role', 'edgepilot_app', true);
    EXECUTE format('SELECT count(*) FROM %I', tbl) INTO n;
    PERFORM set_config('role', 'none', true);
    RETURN n;
END;
$$;

-- Returns true when the write was refused. set_config runs outside the inner
-- block, so the exception's subtransaction rollback does not undo the role.
CREATE OR REPLACE FUNCTION _rls_insert_device(caller_org uuid, target_org uuid, serial text)
    RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE denied boolean := false;
BEGIN
    PERFORM set_config('role', 'edgepilot_app', true);
    PERFORM set_config('edgepilot.org_id', caller_org::text, true);
    BEGIN
        INSERT INTO devices (id, org_id, serial_number)
        VALUES (gen_random_uuid(), target_org, serial);
    EXCEPTION WHEN others THEN
        denied := true;
    END;
    PERFORM set_config('role', 'none', true);
    RETURN denied;
END;
$$;

CREATE OR REPLACE FUNCTION _admin_count(tbl text) RETURNS bigint
    LANGUAGE plpgsql AS $$
DECLARE n bigint;
BEGIN
    PERFORM set_config('role', 'edgepilot_admin', true);
    EXECUTE format('SELECT count(*) FROM %I', tbl) INTO n;
    PERFORM set_config('role', 'none', true);
    RETURN n;
END;
$$;

BEGIN;
SELECT _expect('RLS: Acme sees only its own device',
    $$SELECT _rls_count('11111111-1111-1111-1111-111111111111','devices')$$, 1);
COMMIT;

BEGIN;
SELECT _expect('RLS: Globex sees only its own device',
    $$SELECT _rls_count('22222222-2222-2222-2222-222222222222','devices')$$, 1);
COMMIT;

BEGIN;
SELECT _expect('RLS: Globex sees no Acme users',
    $$SELECT _rls_count('22222222-2222-2222-2222-222222222222','users')$$, 0);
COMMIT;

-- Child tables reach tenancy through their parent. This also catches the easy
-- mistake of adding a policy without the matching GRANT: enabling RLS grants
-- nothing, so a missing GRANT surfaces here as "permission denied", not as 0.
BEGIN;
SELECT _expect('RLS: child table device_certificates is scoped to the caller org',
    $$SELECT _rls_count('11111111-1111-1111-1111-111111111111','device_certificates')$$, 1);
COMMIT;

BEGIN;
SELECT _expect('RLS: child table command_results is readable (grant present)',
    $$SELECT _rls_count('11111111-1111-1111-1111-111111111111','command_results')$$, 0);
COMMIT;

SELECT _expect('RLS: an unscoped transaction sees nothing (deny by default)',
    $$SELECT _rls_count_unscoped('devices')$$, 0);

SELECT _expect('RLS: writing into another org is rejected by WITH CHECK',
    $$SELECT _rls_insert_device('11111111-1111-1111-1111-111111111111',
                                '22222222-2222-2222-2222-222222222222', 'SMUGGLED')$$, true);

SELECT _expect('RLS: writing into the caller''s own org is accepted',
    $$SELECT _rls_insert_device('11111111-1111-1111-1111-111111111111',
                                '11111111-1111-1111-1111-111111111111', 'ACME-2')$$, false);

SELECT _expect('RLS: the accepted write landed in the caller org, and only there',
    $$SELECT _rls_count('11111111-1111-1111-1111-111111111111','devices')$$, 2);

SELECT _expect('RLS: the rejected write left no trace in the target org',
    $$SELECT _rls_count('22222222-2222-2222-2222-222222222222','devices')$$, 1);

DELETE FROM devices WHERE serial_number = 'ACME-2';

-- The cross-org workers (rollup, sweeper, retention) run as edgepilot_admin.
-- BYPASSRLS alone is not enough: it skips policies but confers no table
-- privileges, so a missing GRANT shows up here as "permission denied".
SELECT _expect('RLS: edgepilot_admin sees every org',
    $$SELECT _admin_count('devices')$$, 2);
SELECT _expect('RLS: edgepilot_admin can read child tables',
    $$SELECT _admin_count('device_certificates')$$, 2);

-- ---------------------------------------------------------------------------
-- Report
-- ---------------------------------------------------------------------------

SELECT CASE WHEN ok THEN 'PASS  ' ELSE 'FAIL  ' END || label
FROM _result ORDER BY seq;
