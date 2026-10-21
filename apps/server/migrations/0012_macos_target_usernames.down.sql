-- Back to the POSIX pattern.
--
-- This can fail, and that is correct. A key registered against a macOS short
-- name containing a dot has no representation under the narrower pattern, so
-- restoring it would mean deleting that registration — silently removing
-- somebody's access to a customer's Mac. The reversal stops instead, and the
-- operator decides what to do with the rows:
--
--   SELECT id, name, target_username FROM ssh_keys
--    WHERE target_username !~ '^[a-z_][a-z0-9_-]{0,31}$';

ALTER TABLE ssh_keys
    DROP CONSTRAINT ssh_keys_target_username_check;

ALTER TABLE ssh_keys
    ADD CONSTRAINT ssh_keys_target_username_check
    CHECK (target_username ~ '^[a-z_][a-z0-9_-]{0,31}$');
