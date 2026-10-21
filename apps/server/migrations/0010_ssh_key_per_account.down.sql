-- Back to one account per key per organization.
--
-- This can fail, and that is correct. If a key has been registered for two
-- accounts since the up migration ran, restoring the narrower constraint would
-- have to discard one of them — and silently choosing which would remove
-- somebody's access without telling anyone. The reversal stops instead, and the
-- operator decides which registration to drop.

ALTER TABLE ssh_keys
    DROP CONSTRAINT ssh_keys_org_fingerprint_account_key;

ALTER TABLE ssh_keys
    ADD CONSTRAINT ssh_keys_org_id_fingerprint_sha256_key
    UNIQUE (org_id, fingerprint_sha256);
