-- One operator key, several local accounts.
--
-- The original constraint was UNIQUE (org_id, fingerprint_sha256): a key could
-- be registered once per organization, and therefore for exactly one local
-- account name. That assumed a fleet whose Macs all use the same username.
--
-- Real fleets do not. The same engineer's key needs to reach `pallet` on one
-- Mac and `palletvision` on another, and with the old constraint the second
-- registration failed as a duplicate — leaving no way to express it short of
-- issuing that engineer a second keypair, which defeats the point of revoking
-- one key to cut off the whole fleet.
--
-- Including target_username makes the row mean what it always should have:
-- "this key may log in as this account". A device installs only the rows whose
-- account exists on it, so a Mac with no `palletvision` simply ignores that one.

ALTER TABLE ssh_keys
    DROP CONSTRAINT ssh_keys_org_id_fingerprint_sha256_key;

ALTER TABLE ssh_keys
    ADD CONSTRAINT ssh_keys_org_fingerprint_account_key
    UNIQUE (org_id, fingerprint_sha256, target_username);
