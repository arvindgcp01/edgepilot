-- 0004_commands — allowlisted command catalogue, queue, approvals, results.

-- ---------------------------------------------------------------------------
-- Command catalogue
-- ---------------------------------------------------------------------------
--
-- The catalogue is the contract between server and agent. The server dispatches a
-- SLUG plus validated PARAMS — never an argv. The agent maps the slug to a handler
-- it was compiled with. A compromised server therefore cannot execute arbitrary
-- code on the fleet; it can only invoke operations the agent already implements.

CREATE TABLE command_definitions (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                    text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z][a-z0-9_]{1,62}$'),
    name                    text        NOT NULL,
    description             text        NOT NULL DEFAULT '',
    category                text        NOT NULL DEFAULT 'general'
                                        CHECK (category IN ('general', 'power', 'network',
                                                            'diagnostics', 'maintenance',
                                                            'software', 'agent')),
    -- JSON Schema (draft 2020-12) for params. Enforced server-side at enqueue and
    -- again agent-side at execution: neither side trusts the other's validation.
    params_schema           jsonb       NOT NULL DEFAULT '{"type":"object","additionalProperties":false}'::jsonb,
    required_permission     text        NOT NULL REFERENCES permissions (slug) ON UPDATE CASCADE,
    requires_approval       boolean     NOT NULL DEFAULT false,
    is_destructive          boolean     NOT NULL DEFAULT false,
    default_timeout_seconds integer     NOT NULL DEFAULT 120
                                        CHECK (default_timeout_seconds BETWEEN 5 AND 86400),
    -- Semver floor. Dispatch to an older agent fails at enqueue with a clear error
    -- rather than producing an unknown-slug failure on the device.
    min_agent_version       text,
    enabled                 boolean     NOT NULL DEFAULT true,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER command_definitions_updated_at BEFORE UPDATE ON command_definitions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Command queue
-- ---------------------------------------------------------------------------
--
-- This table, not Redis, is the durable queue. Redis pub/sub only notifies the
-- replica that owns the device's stream; a lost notification costs latency because
-- the owning replica also reconciles this table every 5 seconds.

CREATE TABLE commands (
    id                 uuid        PRIMARY KEY,              -- UUIDv7
    org_id             uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    device_id          uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    definition_id      uuid        NOT NULL REFERENCES command_definitions (id) ON DELETE RESTRICT,
    -- Denormalised so history survives a definition rename and the agent stream
    -- needs no join to dispatch.
    slug               text        NOT NULL,
    params             jsonb       NOT NULL DEFAULT '{}'::jsonb,

    -- custom_script only: content lives in object storage, never in the database.
    script_object_key  text,
    script_sha256      bytea       CHECK (script_sha256 IS NULL OR octet_length(script_sha256) = 32),
    -- Ed25519 signature over (device_id, script_sha256, command_id, expires_at),
    -- produced at approval and verified by the agent against a pinned public key.
    script_signature   bytea,

    status             text        NOT NULL DEFAULT 'queued'
                                   CHECK (status IN ('pending_approval', 'queued', 'dispatched',
                                                     'running', 'succeeded', 'failed',
                                                     'timed_out', 'cancelled', 'rejected',
                                                     'expired')),
    priority           smallint    NOT NULL DEFAULT 5 CHECK (priority BETWEEN 0 AND 9),

    requested_by       uuid        REFERENCES users (id) ON DELETE SET NULL,
    -- Groups commands fanned out to many devices from one operator action.
    correlation_id     uuid,
    -- Set when the command was created by the deployment scheduler.
    deployment_id      uuid,

    timeout_seconds    integer     NOT NULL CHECK (timeout_seconds BETWEEN 5 AND 86400),
    attempt            smallint    NOT NULL DEFAULT 0,
    max_attempts       smallint    NOT NULL DEFAULT 1 CHECK (max_attempts BETWEEN 1 AND 5),

    -- A command for a device that never comes back must not sit queued forever.
    expires_at         timestamptz NOT NULL,
    approved_at        timestamptz,
    queued_at          timestamptz,
    dispatched_at      timestamptz,
    started_at         timestamptz,
    finished_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    -- A custom script is meaningless without its content; anything else must not carry one.
    CHECK ((slug = 'custom_script') = (script_object_key IS NOT NULL))
);
-- The dispatcher's hot path: pending work for one device, highest priority first.
CREATE INDEX commands_dispatch_idx ON commands (device_id, priority, created_at)
    WHERE status IN ('queued', 'dispatched');
CREATE INDEX commands_org_created_idx  ON commands (org_id, created_at DESC);
CREATE INDEX commands_device_created_idx ON commands (device_id, created_at DESC);
CREATE INDEX commands_correlation_idx  ON commands (correlation_id) WHERE correlation_id IS NOT NULL;
CREATE INDEX commands_deployment_idx   ON commands (deployment_id) WHERE deployment_id IS NOT NULL;
-- Drives the expiry sweeper.
CREATE INDEX commands_expiry_idx ON commands (expires_at)
    WHERE status IN ('pending_approval', 'queued', 'dispatched', 'running');
CREATE TRIGGER commands_updated_at BEFORE UPDATE ON commands
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Approvals — two-person rule for destructive and custom commands
-- ---------------------------------------------------------------------------

CREATE TABLE command_approvals (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    command_id uuid        NOT NULL REFERENCES commands (id) ON DELETE CASCADE,
    decision   text        NOT NULL CHECK (decision IN ('approved', 'rejected')),
    decided_by uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    reason     text        NOT NULL DEFAULT '',
    decided_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (command_id, decided_by)
);
CREATE INDEX command_approvals_command_idx ON command_approvals (command_id);

-- The requester may not approve their own command. Enforced in the database because
-- this is the single control standing between "operator account compromised" and
-- "arbitrary code on every Mac" — it must not depend on application correctness.
CREATE OR REPLACE FUNCTION reject_self_approval() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    requester uuid;
BEGIN
    SELECT requested_by INTO requester FROM commands WHERE id = NEW.command_id;
    IF requester IS NOT NULL AND requester = NEW.decided_by THEN
        RAISE EXCEPTION 'command %: requester cannot approve their own command', NEW.command_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER command_approvals_two_person BEFORE INSERT ON command_approvals
    FOR EACH ROW EXECUTE FUNCTION reject_self_approval();

-- ---------------------------------------------------------------------------
-- Results
-- ---------------------------------------------------------------------------

CREATE TABLE command_results (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    command_id         uuid        NOT NULL REFERENCES commands (id) ON DELETE CASCADE,
    attempt            smallint    NOT NULL DEFAULT 0,
    exit_code          integer,
    -- Capped at 256 KiB each by the ingest path; the full stream goes to object
    -- storage and truncated is set. Keeps a runaway command from bloating the table.
    stdout             text,
    stderr             text,
    stdout_object_key  text,
    stderr_object_key  text,
    truncated          boolean     NOT NULL DEFAULT false,
    duration_ms        bigint,
    -- Agent-side failure that produced no exit code (timeout, unknown slug,
    -- signature mismatch, policy refusal).
    error_code         text,
    error_message      text,
    received_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (command_id, attempt)
);
CREATE INDEX command_results_command_idx ON command_results (command_id);
