-- Reverse of 0008: remove RLS policies, grants, and seed data.
--
-- Ordering here is dictated by two deliberate ON DELETE RESTRICT constraints:
--
--   commands.definition_id      -> command_definitions   (RESTRICT)
--   command_definitions.required_permission -> permissions.slug
--
-- commands references the catalogue with RESTRICT precisely so a definition with
-- execution history cannot be deleted out from under it. Reversing this migration
-- therefore has to remove that history first: a command row whose definition no
-- longer exists is not a state the schema permits, and leaving it would make the
-- down-migration fail on any database that has ever run a command.
--
-- This is destructive, which is what a down-migration is for.
DELETE FROM commands;
DELETE FROM command_definitions;

DELETE FROM role_permissions WHERE role_id IN (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003');
DELETE FROM roles WHERE is_system;
DELETE FROM permissions;

-- Drop the policies themselves, not merely the RLS switch on each table.
--
-- DISABLE ROW LEVEL SECURITY stops policies being enforced but leaves the policy
-- objects in place, so re-applying 0008 fails with "policy already exists". That
-- is not a theoretical concern: rolling one migration back and redeploying is
-- the normal response to a bad release, and it has to work.
--
-- Driving this from pg_policies rather than a hand-maintained list means a
-- policy added to 0008 later cannot be forgotten here.
DO $$
DECLARE r record;
BEGIN
    FOR r IN
        SELECT schemaname, tablename, policyname
        FROM pg_policies
        WHERE schemaname = 'public'
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I.%I',
                       r.policyname, r.schemaname, r.tablename);
    END LOOP;
END;
$$;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['users','sessions','devices','enrollment_tokens','commands',
                             'software_packages','deployments','files','log_collections',
                             'ssh_keys','ssh_sessions','alert_rules','alerts','device_events',
                             'device_certificates','device_inventory','command_results',
                             'command_approvals','package_versions','deployment_status',
                             'ssh_key_devices','user_roles','audit_logs'] LOOP
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
    END LOOP;
END;
$$;
