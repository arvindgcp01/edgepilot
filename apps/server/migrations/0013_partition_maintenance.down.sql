-- Back to the 0003 definition: SECURITY INVOKER, no grants on the child.
--
-- Partitions already created by the runtime version keep the grants they were
-- given; nothing here removes them, because removing them would break inserts
-- into the current month.
CREATE OR REPLACE FUNCTION ensure_monthly_partition(parent regclass, month date)
    RETURNS text LANGUAGE plpgsql AS $$
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
    RETURN child_name;
END;
$$;

GRANT EXECUTE ON FUNCTION ensure_monthly_partition(regclass, date) TO PUBLIC;
