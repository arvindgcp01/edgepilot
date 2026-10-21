-- Remote screen access: per-device state, and the permission that governs it.
--
-- The feature reuses two things that already exist — the agent's TCP tunnel and
-- the operator relay — to reach macOS's own Screen Sharing (VNC on 5900). What
-- is new is a per-device on/off, a place to keep the VNC password, and a
-- permission distinct from ssh:connect so that reaching a customer's desktop is
-- a separate grant from reaching their shell.

ALTER TABLE devices
    ADD COLUMN screen_sharing_enabled boolean NOT NULL DEFAULT false,
    -- The device's VNC password, sealed with the application data key (AEAD,
    -- device id as the associated data). Never stored or logged in the clear.
    -- The agent receives the plaintext in ConfigUpdate to configure Screen
    -- Sharing; the relay unseals the same value to answer the VNC challenge, so
    -- an operator authenticates without ever seeing it. NULL until first
    -- enabled.
    ADD COLUMN vnc_password_sealed bytea;

-- ---------------------------------------------------------------------------
-- Permission: screen:connect
-- ---------------------------------------------------------------------------
--
-- Granted to Admin and Operator (the two roles that already hold ssh:connect's
-- neighbourhood of day-to-day access). Viewer does not get it: watching a
-- read-only fleet table is a different thing from driving someone's desktop.

INSERT INTO permissions (slug, resource, action, description) VALUES
    ('screen:connect', 'screen', 'connect', 'Open a remote screen (VNC) session to a device')
ON CONFLICT (slug) DO NOTHING;

-- Admin: all permissions except agent:release, matching how 0008 seeds it — so
-- the new permission joins admin the same way any future one would.
INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000001', id FROM permissions
WHERE slug = 'screen:connect'
ON CONFLICT DO NOTHING;

-- Operator: explicitly, because 0008 gives Operator a named subset, not
-- "everything but". Remote control is squarely day-to-day fleet operations.
INSERT INTO role_permissions (role_id, permission_id)
SELECT '00000000-0000-0000-0000-000000000002', id FROM permissions
WHERE slug = 'screen:connect'
ON CONFLICT DO NOTHING;
