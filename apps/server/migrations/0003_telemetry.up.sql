-- 0003_telemetry — metrics (partitioned), hourly rollups, structured device events.
--
-- Volume model: 1 sample / device / 30 s = 2,880 rows/device/day.
-- At 10,000 devices that is 28.8 M rows/day, ~2.6 GB/day raw.
--
-- Consequences baked into this schema:
--   * Range partitioning by time, so 90-day retention is DROP PARTITION (instant)
--     rather than DELETE (a multi-hour vacuum-generating scan).
--   * Fixed-width columns (real / bigint), no jsonb, so rows stay narrow.
--   * Hourly rollups retained for 400 days serve every chart wider than 48 hours,
--     so the dashboard never scans raw partitions for a "last 30 days" view.

-- ---------------------------------------------------------------------------
-- Raw samples
-- ---------------------------------------------------------------------------

CREATE TABLE device_metrics (
    device_id           uuid        NOT NULL,
    time                timestamptz NOT NULL,

    cpu_percent         real        NOT NULL CHECK (cpu_percent >= 0),
    cpu_user_percent    real,
    cpu_system_percent  real,
    load_1              real,
    load_5              real,
    load_15             real,

    memory_used_bytes   bigint,
    memory_total_bytes  bigint,
    -- macOS memory pressure (0-100), from the same source as Activity Monitor.
    memory_pressure     real,
    swap_used_bytes     bigint,

    disk_used_bytes     bigint,
    disk_total_bytes    bigint,
    -- Monotonic counters since boot; the server derives rates between samples.
    disk_read_bytes     bigint,
    disk_write_bytes    bigint,

    net_rx_bytes        bigint,
    net_tx_bytes        bigint,

    -- NULL on Apple silicon without an accessible SMC sensor; charts must tolerate it.
    temperature_c       real,
    uptime_seconds      bigint,
    process_count       integer,

    PRIMARY KEY (device_id, time)
) PARTITION BY RANGE (time);

-- Monthly partitions. The maintenance worker keeps three months ahead and drops
-- partitions past the retention horizon. A default partition is deliberately NOT
-- created: rows outside the managed range must fail loudly, not silently pile into
-- an unpartitionable catch-all.
CREATE INDEX device_metrics_time_idx ON device_metrics (time DESC);

-- ---------------------------------------------------------------------------
-- Hourly rollups — written by the rollup worker, one row per device per hour
-- ---------------------------------------------------------------------------

CREATE TABLE device_metrics_hourly (
    device_id            uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    bucket               timestamptz NOT NULL,     -- truncated to the hour, UTC
    sample_count         integer     NOT NULL,

    cpu_avg              real, cpu_max              real, cpu_p95              real,
    memory_used_avg      bigint, memory_used_max    bigint,
    memory_pressure_avg  real, memory_pressure_max  real,
    disk_used_avg        bigint, disk_used_max      bigint,
    load_1_avg           real, load_1_max           real,
    net_rx_delta         bigint, net_tx_delta       bigint,
    disk_read_delta      bigint, disk_write_delta   bigint,
    temperature_avg      real, temperature_max      real,
    uptime_max           bigint,

    PRIMARY KEY (device_id, bucket)
);
CREATE INDEX device_metrics_hourly_bucket_idx ON device_metrics_hourly (bucket DESC);

-- ---------------------------------------------------------------------------
-- Structured device events — the searchable slice of "logs"
-- ---------------------------------------------------------------------------
--
-- Raw macOS log lines are NOT stored here (see docs/Architecture.md §8). This table
-- holds only agent-emitted structured events, which are low-volume, high-signal, and
-- worth indexing across the whole fleet.

CREATE TABLE device_events (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
    org_id      uuid        NOT NULL,
    device_id   uuid        NOT NULL,
    occurred_at timestamptz NOT NULL,
    -- agent.started, agent.stopped, agent.updated, command.executed,
    -- deployment.completed, tunnel.opened, service.failed, disk.pressure, ...
    event_type  text        NOT NULL,
    severity    text        NOT NULL DEFAULT 'info'
                            CHECK (severity IN ('debug', 'info', 'warning', 'error', 'critical')),
    message     text        NOT NULL DEFAULT '',
    attributes  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (occurred_at, id)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX device_events_device_idx  ON device_events (device_id, occurred_at DESC);
CREATE INDEX device_events_org_type_idx ON device_events (org_id, event_type, occurred_at DESC);
CREATE INDEX device_events_message_trgm_idx ON device_events USING gin (message gin_trgm_ops);

-- ---------------------------------------------------------------------------
-- Partition management
-- ---------------------------------------------------------------------------

-- Creates a monthly range partition if absent. Called by the maintenance worker
-- (which holds a Redis leader lock) and by the test harness. Idempotent.
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

-- Bootstrap: current month plus three ahead, so a fresh install accepts writes
-- immediately and survives three months even if the maintenance worker never runs.
DO $$
DECLARE
    i int;
BEGIN
    FOR i IN 0..3 LOOP
        PERFORM ensure_monthly_partition('device_metrics', (CURRENT_DATE + (i || ' month')::interval)::date);
        PERFORM ensure_monthly_partition('device_events',  (CURRENT_DATE + (i || ' month')::interval)::date);
    END LOOP;
END;
$$;
