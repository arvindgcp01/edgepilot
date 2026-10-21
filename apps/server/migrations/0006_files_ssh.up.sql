-- 0006_files_ssh — object registry, log collection jobs, SSH keys and sessions.

-- ---------------------------------------------------------------------------
-- Files — the registry for every object in blob storage
-- ---------------------------------------------------------------------------
--
-- One table for uploads to a device, downloads from a device, collected log
-- bundles, and overflowed command output. They share an identical lifecycle
-- (reserve key → transfer → verify checksum → expire) and identical access
-- control, so splitting them would duplicate that machinery four times.

CREATE TABLE files (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    kind             text        NOT NULL CHECK (kind IN ('upload', 'download', 'log_bundle',
                                                          'command_output', 'package',
                                                          'agent_release')),
    device_id        uuid        REFERENCES devices (id) ON DELETE SET NULL,
    object_key       text        NOT NULL UNIQUE,
    filename         text        NOT NULL,
    -- Absolute path on the Mac for upload/download; NULL otherwise.
    remote_path      text,
    size_bytes       bigint      NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    content_type     text        NOT NULL DEFAULT 'application/octet-stream',
    sha256           bytea       CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    status           text        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'transferring', 'available',
                                                   'failed', 'expired', 'deleted')),
    progress_percent smallint    NOT NULL DEFAULT 0 CHECK (progress_percent BETWEEN 0 AND 100),
    bytes_transferred bigint     NOT NULL DEFAULT 0,
    error_message    text,
    -- Retention horizon. The reaper deletes the blob and marks the row 'expired'.
    expires_at       timestamptz,
    created_by       uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- Transfers to and from a device are meaningless without both a device and a path.
    CHECK (kind NOT IN ('upload', 'download')
           OR (device_id IS NOT NULL AND remote_path IS NOT NULL))
);
CREATE INDEX files_org_kind_idx  ON files (org_id, kind, created_at DESC);
CREATE INDEX files_device_idx    ON files (device_id, created_at DESC) WHERE device_id IS NOT NULL;
CREATE INDEX files_active_idx    ON files (status) WHERE status IN ('pending', 'transferring');
CREATE INDEX files_expiry_idx    ON files (expires_at) WHERE status = 'available';
CREATE TRIGGER files_updated_at BEFORE UPDATE ON files
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE package_versions
    ADD CONSTRAINT package_versions_object_fk FOREIGN KEY (object_key)
    REFERENCES files (object_key) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- Log collection jobs
-- ---------------------------------------------------------------------------

CREATE TABLE log_collections (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    device_id     uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    command_id    uuid        REFERENCES commands (id) ON DELETE SET NULL,
    file_id       uuid        REFERENCES files (id) ON DELETE SET NULL,
    -- Which sources to include: system, application, crash, agent, install, network.
    sources       text[]      NOT NULL DEFAULT '{system,agent}',
    -- Bounded window; the agent refuses unbounded `log show` invocations because
    -- they can produce tens of gigabytes on a long-running machine.
    since         timestamptz NOT NULL,
    until         timestamptz,
    max_size_bytes bigint     NOT NULL DEFAULT 268435456,   -- 256 MiB
    status        text        NOT NULL DEFAULT 'queued'
                              CHECK (status IN ('queued', 'collecting', 'uploading',
                                                'available', 'failed', 'cancelled')),
    error_message text,
    requested_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (until IS NULL OR until > since)
);
CREATE INDEX log_collections_device_idx ON log_collections (device_id, created_at DESC);
CREATE INDEX log_collections_active_idx ON log_collections (status)
    WHERE status IN ('queued', 'collecting', 'uploading');
CREATE TRIGGER log_collections_updated_at BEFORE UPDATE ON log_collections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- SSH keys
-- ---------------------------------------------------------------------------
--
-- These are PUBLIC keys only. EdgePilot never holds an operator's private key:
-- the bastion authenticates the outer hop by signature, and the inner hop is
-- end-to-end between the operator's client and the Mac's sshd.

CREATE TABLE ssh_keys (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id             uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- NULL for an org-wide service key (break-glass, owned by the org not a person).
    owner_user_id      uuid        REFERENCES users (id) ON DELETE CASCADE,
    name               text        NOT NULL,
    public_key         text        NOT NULL,                     -- authorized_keys line
    key_type           text        NOT NULL CHECK (key_type IN ('ssh-ed25519',
                                                                'ecdsa-sha2-nistp256',
                                                                'ecdsa-sha2-nistp384',
                                                                'sk-ssh-ed25519@openssh.com',
                                                                'sk-ecdsa-sha2-nistp256@openssh.com')),
    fingerprint_sha256 text        NOT NULL,
    -- 'org' → reconciled onto every approved device in the org.
    -- 'device' → only onto devices listed in ssh_key_devices.
    scope              text        NOT NULL DEFAULT 'device' CHECK (scope IN ('org', 'device')),
    -- Local account on the Mac this key is installed for.
    target_username    text        NOT NULL DEFAULT 'edgepilot'
                                   CHECK (target_username ~ '^[a-z_][a-z0-9_-]{0,31}$'),
    -- Written into the authorized_keys line as expiry-time=, so sshd enforces it
    -- even if the device never receives the revocation.
    expires_at         timestamptz,
    last_used_at       timestamptz,
    revoked_at         timestamptz,
    revoked_reason     text,
    created_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, fingerprint_sha256)
);
CREATE INDEX ssh_keys_owner_idx  ON ssh_keys (owner_user_id) WHERE revoked_at IS NULL;
CREATE INDEX ssh_keys_active_idx ON ssh_keys (org_id, scope) WHERE revoked_at IS NULL;
-- RSA is intentionally absent from key_type: no new RSA keys are accepted.

CREATE TABLE ssh_key_devices (
    ssh_key_id uuid        NOT NULL REFERENCES ssh_keys (id) ON DELETE CASCADE,
    device_id  uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    granted_by uuid        REFERENCES users (id) ON DELETE SET NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ssh_key_id, device_id)
);
CREATE INDEX ssh_key_devices_device_idx ON ssh_key_devices (device_id);

-- ---------------------------------------------------------------------------
-- SSH sessions — the audit record for every tunnelled connection
-- ---------------------------------------------------------------------------
--
-- Metadata only. The bastion relays an encrypted SSH stream and cannot read
-- session content, which is the intended property: a compromised control plane
-- must not yield the contents of engineers' sessions.

CREATE TABLE ssh_sessions (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    device_id       uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    user_id         uuid        REFERENCES users (id) ON DELETE SET NULL,
    ssh_key_id      uuid        REFERENCES ssh_keys (id) ON DELETE SET NULL,
    bastion_replica text        NOT NULL DEFAULT '',
    client_ip       inet,
    target_username text        NOT NULL DEFAULT '',
    status          text        NOT NULL DEFAULT 'active'
                                CHECK (status IN ('active', 'closed', 'denied', 'failed')),
    bytes_in        bigint      NOT NULL DEFAULT 0,
    bytes_out       bigint      NOT NULL DEFAULT 0,
    close_reason    text,
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz
);
CREATE INDEX ssh_sessions_device_idx ON ssh_sessions (device_id, started_at DESC);
CREATE INDEX ssh_sessions_user_idx   ON ssh_sessions (user_id, started_at DESC);
CREATE INDEX ssh_sessions_active_idx ON ssh_sessions (status) WHERE status = 'active';
