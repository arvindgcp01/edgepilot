-- 0001_init — extensions, tenancy, identity, RBAC, sessions.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------------------
-- Shared helpers
-- ---------------------------------------------------------------------------

-- Every table with an updated_at column attaches this trigger, so the column is
-- maintained by the database rather than depending on every write path to remember.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- Returns the org the current transaction is scoped to. Set by the repository
-- layer via SET LOCAL edgepilot.org_id. Returns NULL when unset, which makes
-- every RLS policy deny by default rather than fall open.
CREATE OR REPLACE FUNCTION current_org_id() RETURNS uuid
    LANGUAGE plpgsql STABLE AS $$
DECLARE
    v text;
BEGIN
    v := current_setting('edgepilot.org_id', true);
    IF v IS NULL OR v = '' THEN
        RETURN NULL;
    END IF;
    RETURN v::uuid;
EXCEPTION WHEN invalid_text_representation THEN
    RETURN NULL;
END;
$$;

-- ---------------------------------------------------------------------------
-- Tenancy
-- ---------------------------------------------------------------------------

CREATE TABLE organizations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    slug        citext      NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    status      text        NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'suspended', 'archived')),
    -- Per-org policy: command approval requirements, retention overrides,
    -- auto-approve enrollment, allowed command slugs.
    settings    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);
CREATE TRIGGER organizations_updated_at BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- RBAC — permissions are static, roles may be per-org, users hold roles
-- ---------------------------------------------------------------------------

CREATE TABLE permissions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL UNIQUE CHECK (slug ~ '^[a-z_]+:[a-z_]+$'),
    resource    text NOT NULL,
    action      text NOT NULL,
    description text NOT NULL DEFAULT '',
    UNIQUE (resource, action)
);

CREATE TABLE roles (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL org_id marks a built-in role available to every organization.
    org_id      uuid        REFERENCES organizations (id) ON DELETE CASCADE,
    slug        text        NOT NULL CHECK (slug ~ '^[a-z][a-z0-9_]{1,62}$'),
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    is_system   boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
-- Two partial indexes because UNIQUE(org_id, slug) would allow duplicate
-- built-in roles: NULL is never equal to NULL.
CREATE UNIQUE INDEX roles_global_slug_key ON roles (slug) WHERE org_id IS NULL;
CREATE UNIQUE INDEX roles_org_slug_key    ON roles (org_id, slug) WHERE org_id IS NOT NULL;
CREATE TRIGGER roles_updated_at BEFORE UPDATE ON roles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE role_permissions (
    role_id       uuid NOT NULL REFERENCES roles (id)       ON DELETE CASCADE,
    permission_id uuid NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

-- ---------------------------------------------------------------------------
-- Users
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    email               citext      NOT NULL CHECK (position('@' in email) > 1),
    -- argon2id encoded hash. NULL for users provisioned via SSO only.
    password_hash       text,
    full_name           text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT 'invited'
                                    CHECK (status IN ('invited', 'active', 'disabled', 'locked')),
    -- TOTP secret sealed with the application data key; never stored in the clear.
    mfa_secret_enc      bytea,
    mfa_enabled         boolean     NOT NULL DEFAULT false,
    -- Recovery codes stored as an array of SHA-256 hashes, consumed on use.
    mfa_recovery_hashes bytea[]     NOT NULL DEFAULT '{}',
    failed_login_count  integer     NOT NULL DEFAULT 0,
    locked_until        timestamptz,
    last_login_at       timestamptz,
    password_changed_at timestamptz,
    created_by          uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    deleted_at          timestamptz
);
CREATE UNIQUE INDEX users_org_email_key ON users (org_id, email) WHERE deleted_at IS NULL;
CREATE INDEX users_org_status_idx ON users (org_id, status) WHERE deleted_at IS NULL;
CREATE TRIGGER users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE user_roles (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_id    uuid        NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    granted_by uuid        REFERENCES users (id) ON DELETE SET NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_id)
);
CREATE INDEX user_roles_role_idx ON user_roles (role_id);

-- ---------------------------------------------------------------------------
-- Sessions — refresh-token family with reuse detection
-- ---------------------------------------------------------------------------

CREATE TABLE sessions (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id             uuid        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- SHA-256 of the opaque refresh token. The token itself is never stored, so a
    -- database disclosure does not yield usable credentials.
    refresh_token_hash bytea       NOT NULL UNIQUE CHECK (octet_length(refresh_token_hash) = 32),
    -- Rotation chain. Presenting an already-rotated token means the token leaked;
    -- the auth service revokes the entire family on detection.
    parent_id          uuid        REFERENCES sessions (id) ON DELETE SET NULL,
    family_id          uuid        NOT NULL,
    user_agent         text        NOT NULL DEFAULT '',
    ip                 inet,
    expires_at         timestamptz NOT NULL,
    last_used_at       timestamptz,
    revoked_at         timestamptz,
    revoked_reason     text,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx     ON sessions (user_id, created_at DESC);
CREATE INDEX sessions_family_idx   ON sessions (family_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_idx   ON sessions (expires_at) WHERE revoked_at IS NULL;
