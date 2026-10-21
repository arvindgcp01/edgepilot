-- 0007_alerts_audit — alert rules, alert instances, tamper-evident audit log.

-- ---------------------------------------------------------------------------
-- Alert rules
-- ---------------------------------------------------------------------------

CREATE TABLE alert_rules (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name             text        NOT NULL,
    type             text        NOT NULL CHECK (type IN ('device_offline', 'cpu_high',
                                                          'memory_high', 'disk_full',
                                                          'agent_stopped', 'service_failed',
                                                          'temperature_high', 'deployment_failed',
                                                          'certificate_expiring')),
    enabled          boolean     NOT NULL DEFAULT true,
    severity         text        NOT NULL DEFAULT 'warning'
                                 CHECK (severity IN ('info', 'warning', 'critical')),
    -- Percentage for cpu/memory/disk, seconds for offline, degrees for temperature.
    -- NULL for rules whose condition is boolean (agent_stopped, service_failed).
    threshold        numeric,
    -- The condition must hold for this long before firing. Prevents a 30-second CPU
    -- spike from paging anyone.
    duration_seconds integer     NOT NULL DEFAULT 300 CHECK (duration_seconds >= 0),
    -- Label selector scoping the rule to a device subset; {} means all devices.
    selector         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- Notification channels: [{"type":"webhook","url":"..."},{"type":"email",...}]
    notify           jsonb       NOT NULL DEFAULT '[]'::jsonb,
    -- Suppress re-notification for this long while an alert stays firing.
    renotify_seconds integer     NOT NULL DEFAULT 3600,
    created_by       uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX alert_rules_org_idx ON alert_rules (org_id) WHERE enabled = true;
CREATE TRIGGER alert_rules_updated_at BEFORE UPDATE ON alert_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Alerts
-- ---------------------------------------------------------------------------

CREATE TABLE alerts (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    rule_id         uuid        REFERENCES alert_rules (id) ON DELETE SET NULL,
    device_id       uuid        REFERENCES devices (id) ON DELETE CASCADE,
    type            text        NOT NULL,
    severity        text        NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    status          text        NOT NULL DEFAULT 'firing'
                                CHECK (status IN ('firing', 'acknowledged', 'resolved')),
    summary         text        NOT NULL,
    details         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    value           numeric,
    threshold       numeric,
    -- Stable identity of the condition: hash of (rule_id, device_id, type[, subject]).
    -- Combined with the partial unique index below this is the whole deduplication
    -- mechanism: a re-firing condition updates one row instead of creating a
    -- thousandth copy.
    fingerprint     text        NOT NULL,
    started_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    notified_at     timestamptz,
    acknowledged_at timestamptz,
    acknowledged_by uuid        REFERENCES users (id) ON DELETE SET NULL,
    resolved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'resolved') = (resolved_at IS NOT NULL)),
    CHECK ((acknowledged_at IS NULL) = (acknowledged_by IS NULL))
);
-- At most one unresolved alert per condition. Resolved rows accumulate as history.
CREATE UNIQUE INDEX alerts_fingerprint_active_key ON alerts (fingerprint)
    WHERE status <> 'resolved';
CREATE INDEX alerts_org_status_idx ON alerts (org_id, status, severity, started_at DESC);
CREATE INDEX alerts_device_idx     ON alerts (device_id, started_at DESC);
CREATE TRIGGER alerts_updated_at BEFORE UPDATE ON alerts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Audit log — append-only, partitioned, hash-chained
-- ---------------------------------------------------------------------------
--
-- Every state-changing action lands here: who did what to which resource, from
-- where, and whether it succeeded. Denied attempts are recorded too — a rejected
-- privilege escalation is exactly the event worth keeping.
--
-- Each row carries hash = SHA-256(prev_hash || canonical_row). Removing or editing
-- a row breaks the chain from that point forward, so tampering is detectable by
-- re-walking the chain. The audit writer serialises appends per org with a
-- transaction-scoped advisory lock.

CREATE TABLE audit_logs (
    id            bigint      GENERATED ALWAYS AS IDENTITY,
    org_id        uuid,
    occurred_at   timestamptz NOT NULL DEFAULT now(),

    actor_type    text        NOT NULL CHECK (actor_type IN ('user', 'agent', 'system', 'api_key')),
    actor_id      uuid,
    -- Human-readable actor at the time of the event, so the log stays legible after
    -- the user is deleted.
    actor_label   text        NOT NULL DEFAULT '',

    -- Dotted verb: device.command.execute, ssh.session.open, user.role.grant,
    -- deployment.rollback, enrollment.approve, auth.login.
    action        text        NOT NULL,
    resource_type text        NOT NULL,
    resource_id   text,
    device_id     uuid,

    outcome       text        NOT NULL CHECK (outcome IN ('success', 'failure', 'denied')),
    ip            inet,
    user_agent    text        NOT NULL DEFAULT '',
    request_id    uuid,
    -- Redacted before write: no tokens, passwords, keys, or script bodies.
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,

    prev_hash     bytea,
    hash          bytea       NOT NULL CHECK (octet_length(hash) = 32),

    PRIMARY KEY (occurred_at, id)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_logs_org_idx      ON audit_logs (org_id, occurred_at DESC);
CREATE INDEX audit_logs_actor_idx    ON audit_logs (actor_id, occurred_at DESC);
CREATE INDEX audit_logs_action_idx   ON audit_logs (action, occurred_at DESC);
CREATE INDEX audit_logs_device_idx   ON audit_logs (device_id, occurred_at DESC)
    WHERE device_id IS NOT NULL;
CREATE INDEX audit_logs_resource_idx ON audit_logs (resource_type, resource_id, occurred_at DESC);
CREATE INDEX audit_logs_denied_idx   ON audit_logs (org_id, occurred_at DESC)
    WHERE outcome = 'denied';

-- The log is append-only at the database level, not merely by convention. Even a
-- compromised application role cannot rewrite history without also having DDL
-- rights to drop these triggers.
CREATE OR REPLACE FUNCTION audit_logs_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only (attempted %)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
CREATE TRIGGER audit_logs_no_update BEFORE UPDATE ON audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION audit_logs_immutable();
CREATE TRIGGER audit_logs_no_delete BEFORE DELETE ON audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION audit_logs_immutable();

DO $$
DECLARE
    i int;
BEGIN
    FOR i IN 0..3 LOOP
        PERFORM ensure_monthly_partition('audit_logs', (CURRENT_DATE + (i || ' month')::interval)::date);
    END LOOP;
END;
$$;
