-- 0005_deployments — software packages, versions, deployments, per-device status,
--                    agent release channel.

CREATE TABLE software_packages (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name        text        NOT NULL,
    slug        text        NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9._-]{0,126}$'),
    description text        NOT NULL DEFAULT '',
    created_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);
CREATE UNIQUE INDEX software_packages_org_slug_key ON software_packages (org_id, slug)
    WHERE deleted_at IS NULL;
CREATE TRIGGER software_packages_updated_at BEFORE UPDATE ON software_packages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Package versions — immutable once 'ready'
-- ---------------------------------------------------------------------------

CREATE TABLE package_versions (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    package_id    uuid        NOT NULL REFERENCES software_packages (id) ON DELETE CASCADE,
    version       text        NOT NULL,
    format        text        NOT NULL CHECK (format IN ('pkg', 'zip', 'tar.gz', 'dmg')),

    -- Artifacts are uploaded straight to object storage via a SAS URL and never
    -- transit the API server.
    object_key    text        NOT NULL UNIQUE,
    size_bytes    bigint      NOT NULL CHECK (size_bytes > 0),
    sha256        bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    -- Ed25519 signature over sha256 by the release key. The agent verifies this
    -- against a compiled-in public key before installing anything.
    signature     bytea       NOT NULL,

    -- Install recipe: pre/post hooks (allowlisted slugs only), target path for
    -- archives, verification command, whether a reboot is required.
    install_spec  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    min_os_version text,
    arch          text        NOT NULL DEFAULT 'universal'
                              CHECK (arch IN ('arm64', 'x86_64', 'universal')),

    status        text        NOT NULL DEFAULT 'uploading'
                              CHECK (status IN ('uploading', 'ready', 'quarantined', 'deprecated')),
    uploaded_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (package_id, version)
);
CREATE INDEX package_versions_package_idx ON package_versions (package_id, created_at DESC);
CREATE TRIGGER package_versions_updated_at BEFORE UPDATE ON package_versions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Deployments
-- ---------------------------------------------------------------------------

CREATE TABLE deployments (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                  uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    package_version_id      uuid        NOT NULL REFERENCES package_versions (id) ON DELETE RESTRICT,
    name                    text        NOT NULL,

    strategy                text        NOT NULL DEFAULT 'rolling'
                                        CHECK (strategy IN ('immediate', 'rolling', 'scheduled')),
    batch_size              integer     CHECK (batch_size IS NULL OR batch_size > 0),
    batch_pause_seconds     integer     NOT NULL DEFAULT 60,

    -- Label selector resolved at start; the resulting target set is frozen into
    -- deployment_status so a device enrolled mid-rollout is not silently included.
    target_selector         jsonb       NOT NULL DEFAULT '{}'::jsonb,

    rollback_to_version_id  uuid        REFERENCES package_versions (id) ON DELETE SET NULL,
    -- Fraction of a batch that may fail before the scheduler halts and rolls back.
    auto_rollback_threshold real        CHECK (auto_rollback_threshold IS NULL
                                               OR auto_rollback_threshold BETWEEN 0 AND 1),

    status                  text        NOT NULL DEFAULT 'draft'
                                        CHECK (status IN ('draft', 'scheduled', 'running',
                                                          'paused', 'completed', 'failed',
                                                          'rolled_back', 'cancelled')),
    total_targets           integer     NOT NULL DEFAULT 0,
    succeeded_count         integer     NOT NULL DEFAULT 0,
    failed_count            integer     NOT NULL DEFAULT 0,

    scheduled_at            timestamptz,
    started_at              timestamptz,
    finished_at             timestamptz,
    created_by              uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (strategy <> 'scheduled' OR scheduled_at IS NOT NULL),
    CHECK (strategy <> 'rolling'   OR batch_size IS NOT NULL)
);
CREATE INDEX deployments_org_idx    ON deployments (org_id, created_at DESC);
CREATE INDEX deployments_active_idx ON deployments (status)
    WHERE status IN ('scheduled', 'running', 'paused');
CREATE TRIGGER deployments_updated_at BEFORE UPDATE ON deployments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE commands
    ADD CONSTRAINT commands_deployment_fk
    FOREIGN KEY (deployment_id) REFERENCES deployments (id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- Per-device deployment status
-- ---------------------------------------------------------------------------

CREATE TABLE deployment_status (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id       uuid        NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    device_id           uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,

    status              text        NOT NULL DEFAULT 'queued'
                                    CHECK (status IN ('queued', 'downloading', 'verifying',
                                                      'installing', 'completed', 'failed',
                                                      'rolled_back', 'skipped', 'cancelled')),
    progress_percent    smallint    NOT NULL DEFAULT 0 CHECK (progress_percent BETWEEN 0 AND 100),
    bytes_downloaded    bigint      NOT NULL DEFAULT 0,

    -- Which rolling batch this device belongs to; the scheduler advances by batch.
    batch_index         integer     NOT NULL DEFAULT 0,
    command_id          uuid        REFERENCES commands (id) ON DELETE SET NULL,
    -- What was installed before, so rollback has a concrete target per device.
    previous_version_id uuid        REFERENCES package_versions (id) ON DELETE SET NULL,

    error_code          text,
    error_message       text,
    attempt             smallint    NOT NULL DEFAULT 0,

    started_at          timestamptz,
    finished_at         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (deployment_id, device_id)
);
CREATE INDEX deployment_status_deployment_idx ON deployment_status (deployment_id, status);
CREATE INDEX deployment_status_device_idx     ON deployment_status (device_id, created_at DESC);
CREATE INDEX deployment_status_batch_idx      ON deployment_status (deployment_id, batch_index)
    WHERE status = 'queued';
CREATE TRIGGER deployment_status_updated_at BEFORE UPDATE ON deployment_status
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Agent releases
-- ---------------------------------------------------------------------------
--
-- Global, not org-scoped: the agent binary is our product, not a customer artifact.

CREATE TABLE agent_versions (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    version           text        NOT NULL,          -- semver
    channel           text        NOT NULL DEFAULT 'stable'
                                  CHECK (channel IN ('stable', 'beta', 'canary')),
    arch              text        NOT NULL DEFAULT 'universal'
                                  CHECK (arch IN ('arm64', 'x86_64', 'universal')),
    object_key        text        NOT NULL UNIQUE,
    size_bytes        bigint      NOT NULL CHECK (size_bytes > 0),
    sha256            bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    -- Ed25519 signature over sha256. The running agent verifies this before
    -- replacing its own binary, so a compromised object store is not sufficient
    -- to push a backdoored agent.
    signature         bytea       NOT NULL,
    release_notes     text        NOT NULL DEFAULT '',
    -- Refuse to upgrade agents older than this directly; they must step through.
    min_upgrade_from  text,
    -- Staged rollout: agents self-select by hashing their device UUID into 0-99.
    rollout_percent   smallint    NOT NULL DEFAULT 0 CHECK (rollout_percent BETWEEN 0 AND 100),
    is_yanked         boolean     NOT NULL DEFAULT false,
    published_at      timestamptz,
    created_by        uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (version, arch)
);
CREATE INDEX agent_versions_channel_idx ON agent_versions (channel, published_at DESC)
    WHERE is_yanked = false;
