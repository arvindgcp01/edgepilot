-- Device health: the raw sample stream and the denormalised "last known"
-- utilisation the fleet table reads.

-- name: InsertDeviceMetric :batchexec
-- A sample for a device that has since been decommissioned is dropped by the
-- WHERE, not rejected: one stale agent must not fail the batch for the others.
INSERT INTO device_metrics (
    device_id, time,
    cpu_percent, cpu_user_percent, cpu_system_percent,
    load_1, load_5, load_15,
    memory_used_bytes, memory_total_bytes, memory_pressure, swap_used_bytes,
    disk_used_bytes, disk_total_bytes, disk_read_bytes, disk_write_bytes,
    net_rx_bytes, net_tx_bytes,
    temperature_c, uptime_seconds, process_count
)
SELECT
    sqlc.arg('device_id')::uuid, sqlc.arg('time')::timestamptz,
    sqlc.arg('cpu_percent')::real, sqlc.narg('cpu_user_percent')::real, sqlc.narg('cpu_system_percent')::real,
    sqlc.narg('load_1')::real, sqlc.narg('load_5')::real, sqlc.narg('load_15')::real,
    sqlc.narg('memory_used_bytes')::bigint, sqlc.narg('memory_total_bytes')::bigint,
    sqlc.narg('memory_pressure')::real, sqlc.narg('swap_used_bytes')::bigint,
    sqlc.narg('disk_used_bytes')::bigint, sqlc.narg('disk_total_bytes')::bigint,
    sqlc.narg('disk_read_bytes')::bigint, sqlc.narg('disk_write_bytes')::bigint,
    sqlc.narg('net_rx_bytes')::bigint, sqlc.narg('net_tx_bytes')::bigint,
    sqlc.narg('temperature_c')::real, sqlc.narg('uptime_seconds')::bigint, sqlc.narg('process_count')::integer
WHERE EXISTS (
    SELECT 1 FROM devices d WHERE d.id = sqlc.arg('device_id')::uuid AND d.deleted_at IS NULL
);

-- name: UpdateDevicesLastMetrics :execrows
-- One statement for the whole batch, like TouchDevicesLastSeen. The percent
-- columns are what the fleet table sorts and colours by; boot time is derived
-- from uptime because a metrics sample is the one message that carries it.
UPDATE devices AS d
SET last_cpu_percent    = v.cpu,
    last_memory_percent = v.mem,
    last_disk_percent   = v.disk,
    last_boot_at        = COALESCE(v.booted_at, d.last_boot_at)
FROM (
    SELECT unnest(sqlc.arg('device_ids')::uuid[])         AS id,
           unnest(sqlc.arg('cpu_percents')::real[])       AS cpu,
           unnest(sqlc.arg('memory_percents')::real[])    AS mem,
           unnest(sqlc.arg('disk_percents')::real[])      AS disk,
           unnest(sqlc.arg('booted_ats')::timestamptz[])  AS booted_at
) AS v
WHERE d.id = v.id
  AND d.deleted_at IS NULL;

-- name: LatestDeviceMetric :one
SELECT * FROM device_metrics
WHERE device_id = sqlc.arg('device_id')
ORDER BY time DESC
LIMIT 1;

-- name: DeviceMetricSeries :many
-- Server-side bucketing. Gauges are averaged; monotonic counters keep their
-- maximum so the reader can difference consecutive buckets into a rate.
--
-- Sentinels instead of NULLs: sqlc types a cast expression as NOT NULL, so a
-- genuinely absent reading — a Mac with no temperature sensor, an agent too
-- old to report network counters — is carried as NaN for reals and -1 for
-- byte counts (which are never negative) and turned back into nil by the
-- repository. The API never sees the sentinel.
SELECT
    to_timestamp(floor(extract(epoch FROM time) / sqlc.arg('bucket_seconds')::bigint)
                 * sqlc.arg('bucket_seconds')::bigint)::timestamptz AS bucket,
    COALESCE(avg(cpu_percent), 'NaN')::real         AS cpu_percent,
    COALESCE(avg(load_1), 'NaN')::real              AS load_1,
    COALESCE(avg(load_5), 'NaN')::real              AS load_5,
    COALESCE(avg(load_15), 'NaN')::real             AS load_15,
    COALESCE(avg(memory_used_bytes), -1)::bigint    AS memory_used_bytes,
    COALESCE(max(memory_total_bytes), -1)::bigint   AS memory_total_bytes,
    COALESCE(avg(memory_pressure), 'NaN')::real     AS memory_pressure,
    COALESCE(avg(disk_used_bytes), -1)::bigint      AS disk_used_bytes,
    COALESCE(max(disk_total_bytes), -1)::bigint     AS disk_total_bytes,
    COALESCE(max(disk_read_bytes), -1)::bigint      AS disk_read_bytes,
    COALESCE(max(disk_write_bytes), -1)::bigint     AS disk_write_bytes,
    COALESCE(max(net_rx_bytes), -1)::bigint         AS net_rx_bytes,
    COALESCE(max(net_tx_bytes), -1)::bigint         AS net_tx_bytes,
    COALESCE(avg(temperature_c), 'NaN')::real       AS temperature_c,
    COALESCE(max(uptime_seconds), -1)::bigint       AS uptime_seconds,
    COALESCE(max(process_count), -1)::integer       AS process_count
FROM device_metrics
WHERE device_id = sqlc.arg('device_id')
  AND time >= sqlc.arg('from_time')
  AND time <  sqlc.arg('to_time')
GROUP BY 1
ORDER BY 1;

-- name: EnsureMetricsPartition :exec
-- Idempotent: the function is a no-op for a partition that already exists.
SELECT ensure_monthly_partition('device_metrics', sqlc.arg('month')::date);
