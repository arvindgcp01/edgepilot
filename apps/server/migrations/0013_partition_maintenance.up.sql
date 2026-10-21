-- Partition maintenance from the application.
--
-- The metrics flusher keeps device_metrics one month ahead of the calendar by
-- calling ensure_monthly_partition, and it does so as edgepilot_admin — the
-- role every unscoped query runs under. That role has grants on tables but no
-- CREATE on the schema, and attaching a partition requires owning the parent.
-- Rather than widen a runtime role to schema-level DDL, the function runs as
-- its owner: the migration user, who owns the parent tables.
--
-- A partition does not inherit its parent's grants, so a child created at
-- runtime would be invisible to the application roles. The function now hands
-- each new child to them explicitly, which is also why this lives in a
-- migration after 0008 — the roles it grants to did not exist when 0003 ran.
--
-- search_path is pinned, as every SECURITY DEFINER function must be: without
-- it a caller could shadow to_regclass or format with their own.
CREATE OR REPLACE FUNCTION ensure_monthly_partition(parent regclass, month date)
    RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    child_name text;
    from_ts    timestamptz := date_trunc('month', month)::timestamptz;
    to_ts      timestamptz := (date_trunc('month', month) + interval '1 month')::timestamptz;
BEGIN
    child_name := format('%s_p%s', parent::text, to_char(from_ts, 'YYYYMM'));

    IF to_regclass(child_name) IS NOT NULL THEN
        RETURN child_name;
    END IF;

    EXECUTE format(
        'CREATE TABLE %I PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
        child_name, parent::text, from_ts, to_ts
    );
    EXECUTE format(
        'GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO edgepilot_app, edgepilot_admin',
        child_name
    );
    RETURN child_name;
END;
$$;

-- Only the application roles may call it: SECURITY DEFINER turns "can execute"
-- into "can create tables", and PUBLIC does not need that.
REVOKE EXECUTE ON FUNCTION ensure_monthly_partition(regclass, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_monthly_partition(regclass, date) TO edgepilot_app, edgepilot_admin;
