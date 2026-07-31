-- Devices. Every query here runs inside a transaction that has already issued
-- SET LOCAL edgepilot.org_id, so row-level security scopes them. The explicit
-- org_id predicates are belt and braces: RLS is the backstop for a forgotten
-- filter, not a substitute for writing one.

-- Enrollment is idempotent on (org_id, serial_number).
--
-- An agent that retries after a network failure must not create a second
-- record, and a re-imaged Mac reporting a serial we already know should reclaim
-- its identity rather than collide. ON CONFLICT targets the partial unique
-- index, which only covers rows where deleted_at IS NULL — so a decommissioned
-- device's serial is free for a genuinely new enrollment.
--
-- name: EnrollDevice :one
INSERT INTO devices (
    id, org_id, serial_number, hardware_uuid, hostname,
    model_identifier, chip, arch, cpu_cores, memory_bytes, disk_total_bytes,
    os_version, os_build, agent_version, local_ip, public_ip,
    labels, enrolled_by_token, enrollment_state, status, first_enrolled_at
) VALUES (
    sqlc.arg('id'), sqlc.arg('org_id'), sqlc.arg('serial_number'),
    sqlc.narg('hardware_uuid'), sqlc.arg('hostname'),
    sqlc.narg('model_identifier'), sqlc.narg('chip'), sqlc.narg('arch'),
    sqlc.narg('cpu_cores'), sqlc.narg('memory_bytes'), sqlc.narg('disk_total_bytes'),
    sqlc.narg('os_version'), sqlc.narg('os_build'), sqlc.narg('agent_version'),
    sqlc.narg('local_ip')::inet, sqlc.narg('public_ip')::inet,
    sqlc.arg('labels')::jsonb, sqlc.narg('enrolled_by_token'),
    sqlc.arg('enrollment_state'), 'pending', now()
)
ON CONFLICT (org_id, serial_number) WHERE deleted_at IS NULL
DO UPDATE SET
    hostname         = EXCLUDED.hostname,
    hardware_uuid    = COALESCE(EXCLUDED.hardware_uuid, devices.hardware_uuid),
    model_identifier = COALESCE(EXCLUDED.model_identifier, devices.model_identifier),
    chip             = COALESCE(EXCLUDED.chip, devices.chip),
    arch             = COALESCE(EXCLUDED.arch, devices.arch),
    cpu_cores        = COALESCE(EXCLUDED.cpu_cores, devices.cpu_cores),
    memory_bytes     = COALESCE(EXCLUDED.memory_bytes, devices.memory_bytes),
    disk_total_bytes = COALESCE(EXCLUDED.disk_total_bytes, devices.disk_total_bytes),
    os_version       = COALESCE(EXCLUDED.os_version, devices.os_version),
    os_build         = COALESCE(EXCLUDED.os_build, devices.os_build),
    agent_version    = COALESCE(EXCLUDED.agent_version, devices.agent_version),
    local_ip         = COALESCE(EXCLUDED.local_ip, devices.local_ip),
    public_ip        = COALESCE(EXCLUDED.public_ip, devices.public_ip),
    labels           = devices.labels || EXCLUDED.labels
    -- enrollment_state is deliberately NOT reset here. A device already
    -- approved stays approved across a re-enrollment; one already rejected does
    -- not silently become pending again by reinstalling the agent.
RETURNING *;

-- name: GetDevice :one
SELECT * FROM devices
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetDeviceBySerial :one
SELECT * FROM devices
WHERE org_id = $1 AND serial_number = $2 AND deleted_at IS NULL;

-- Listing with filters, cursor pagination, and a fixed set of orderings.
--
-- The sort is a bounded enum expanded into CASE arms rather than interpolated
-- text, so an ordering parameter can never become SQL injection. The cursor
-- comparison uses the same key as the ORDER BY, with id as the tiebreaker, so
-- the page boundary is stable when two devices share a timestamp.
--
-- name: ListDevices :many
SELECT sqlc.embed(d),
       (SELECT count(*) FROM alerts a
        WHERE a.device_id = d.id AND a.status <> 'resolved') AS active_alert_count
FROM devices d
WHERE d.org_id = sqlc.arg('org_id')
  AND d.deleted_at IS NULL
  AND (sqlc.narg('statuses')::text[] IS NULL OR d.status = ANY(sqlc.narg('statuses')::text[]))
  AND (sqlc.narg('enrollment_state')::text IS NULL OR d.enrollment_state = sqlc.narg('enrollment_state')::text)
  -- Search across the three fields an operator actually types into the search
  -- box. ILIKE rather than = so a partial serial matches.
  AND (
    sqlc.narg('search')::text IS NULL
    OR d.hostname ILIKE '%' || sqlc.narg('search')::text || '%'
    OR d.serial_number ILIKE '%' || sqlc.narg('search')::text || '%'
    OR d.site ILIKE '%' || sqlc.narg('search')::text || '%'
  )
  -- Containment against the GIN index: a device must carry every requested pair.
  AND (sqlc.narg('labels')::jsonb IS NULL OR d.labels @> sqlc.narg('labels')::jsonb)
  AND (
    sqlc.narg('cursor_id')::uuid IS NULL
    OR CASE sqlc.arg('sort')::text
         WHEN 'last_seen_at'  THEN (d.last_seen_at, d.id) >  (sqlc.narg('cursor_time')::timestamptz, sqlc.narg('cursor_id')::uuid)
         WHEN '-last_seen_at' THEN (d.last_seen_at, d.id) <  (sqlc.narg('cursor_time')::timestamptz, sqlc.narg('cursor_id')::uuid)
         WHEN 'created_at'    THEN (d.created_at, d.id)   >  (sqlc.narg('cursor_time')::timestamptz, sqlc.narg('cursor_id')::uuid)
         WHEN '-created_at'   THEN (d.created_at, d.id)   <  (sqlc.narg('cursor_time')::timestamptz, sqlc.narg('cursor_id')::uuid)
         WHEN 'hostname'      THEN (d.hostname, d.id::text) >  (sqlc.narg('cursor_text')::text, sqlc.narg('cursor_id')::text)
         WHEN '-hostname'     THEN (d.hostname, d.id::text) <  (sqlc.narg('cursor_text')::text, sqlc.narg('cursor_id')::text)
       END
  )
ORDER BY
    CASE WHEN sqlc.arg('sort')::text = 'last_seen_at'  THEN d.last_seen_at END ASC  NULLS LAST,
    CASE WHEN sqlc.arg('sort')::text = '-last_seen_at' THEN d.last_seen_at END DESC NULLS LAST,
    CASE WHEN sqlc.arg('sort')::text = 'created_at'    THEN d.created_at   END ASC,
    CASE WHEN sqlc.arg('sort')::text = '-created_at'   THEN d.created_at   END DESC,
    CASE WHEN sqlc.arg('sort')::text = 'hostname'      THEN d.hostname     END ASC,
    CASE WHEN sqlc.arg('sort')::text = '-hostname'     THEN d.hostname     END DESC,
    -- Total order tiebreaker. Without it, devices sharing a timestamp can be
    -- returned in a different order between pages, which loses or repeats rows.
    CASE WHEN sqlc.arg('sort')::text LIKE '-%' THEN d.id END DESC,
    d.id ASC
LIMIT sqlc.arg('page_size');

-- name: UpdateDevice :one
UPDATE devices
SET site                 = COALESCE(sqlc.narg('site')::text, site),
    notes                = COALESCE(sqlc.narg('notes')::text, notes),
    labels               = COALESCE(sqlc.narg('labels')::jsonb, labels),
    allow_custom_scripts = COALESCE(sqlc.narg('allow_custom_scripts')::boolean, allow_custom_scripts)
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- name: SetDeviceEnrollmentState :one
UPDATE devices
SET enrollment_state = sqlc.arg('enrollment_state'),
    -- A rejected or revoked device is not merely unapproved; it must stop
    -- counting as part of the fleet.
    status = CASE
               WHEN sqlc.arg('enrollment_state')::text IN ('rejected', 'revoked')
                 THEN 'decommissioned'
               ELSE status
             END
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- name: SetDeviceStatus :execrows
UPDATE devices
SET status = sqlc.arg('status')
WHERE id = sqlc.arg('id') AND deleted_at IS NULL AND status <> sqlc.arg('status');

-- Batched liveness update.
--
-- unnest() turns the batch into a single statement: one round trip and one
-- index scan for the whole flush, instead of N statements. At 10,000 devices
-- heartbeating every 15 seconds, the difference is the difference between a
-- working system and a saturated write path.
--
-- name: TouchDevicesLastSeen :execrows
UPDATE devices AS d
SET last_seen_at = v.seen_at,
    -- 'pending' as well as 'offline'. A freshly enrolled device starts
    -- 'pending' — it has never been heard from — and hearing from it is exactly
    -- what this statement records. Transitioning only from 'offline' left every
    -- working device in the fleet showing 'pending' forever, because nothing
    -- else ever moved it.
    --
    -- 'degraded' is deliberately left alone: liveness and health are different
    -- questions, and a heartbeat answers only the first.
    status = CASE WHEN d.status IN ('offline', 'pending') THEN 'online'
                  ELSE d.status END
FROM (
    SELECT unnest(sqlc.arg('device_ids')::uuid[])        AS id,
           unnest(sqlc.arg('seen_ats')::timestamptz[])   AS seen_at
) AS v
WHERE d.id = v.id
  AND d.deleted_at IS NULL
  -- Never move last_seen_at backwards: a late-arriving flush from a replica
  -- that was slow must not make a live device look stale.
  AND (d.last_seen_at IS NULL OR d.last_seen_at < v.seen_at);

-- Mark devices offline whose last_seen_at has aged out.
--
-- Returns the affected ids so the caller can raise alerts for exactly the
-- devices that transitioned, rather than re-querying and racing the next sweep.
--
-- name: MarkStaleDevicesOffline :many
UPDATE devices
SET status = 'offline'
WHERE status = 'online'
  AND deleted_at IS NULL
  AND (last_seen_at IS NULL OR last_seen_at < sqlc.arg('threshold')::timestamptz)
RETURNING id, org_id, hostname;

-- name: DecommissionDevice :execrows
UPDATE devices
SET deleted_at       = now(),
    status           = 'decommissioned',
    enrollment_state = 'revoked'
WHERE id = sqlc.arg('id') AND deleted_at IS NULL;

-- Fleet counters for the dashboard cards, in one pass.
--
-- FILTER rather than six separate counting queries: the table is scanned once,
-- and the numbers are consistent with each other because they come from the
-- same snapshot. Six queries could disagree if a device changed state between
-- them, which shows up as cards that do not add up.
--
-- name: DeviceFleetSummary :one
SELECT
    count(*)                                                          AS total,
    count(*) FILTER (WHERE status = 'online')                         AS online,
    count(*) FILTER (WHERE status = 'offline')                        AS offline,
    count(*) FILTER (WHERE status = 'degraded')                       AS degraded,
    count(*) FILTER (WHERE enrollment_state = 'pending')              AS pending_approval,
    count(*) FILTER (
        WHERE sqlc.narg('current_agent_version')::text IS NOT NULL
          AND agent_version IS DISTINCT FROM sqlc.narg('current_agent_version')::text
    )                                                                 AS updates_available
FROM devices
WHERE org_id = sqlc.arg('org_id') AND deleted_at IS NULL;
