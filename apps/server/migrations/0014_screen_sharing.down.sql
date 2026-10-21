-- The role_permissions rows fall with the permission via ON DELETE CASCADE.
DELETE FROM permissions WHERE slug = 'screen:connect';

ALTER TABLE devices
    DROP COLUMN screen_sharing_enabled,
    DROP COLUMN vnc_password_sealed;
