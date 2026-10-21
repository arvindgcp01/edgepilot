-- macOS short names are not Linux usernames.
--
-- The original CHECK was the POSIX useradd pattern, which forbids '.'. macOS
-- derives a short name from the full name at account creation and routinely
-- produces one containing a dot — "City Pallets Inc." becomes
-- "citypalletsinc." — so an entirely ordinary customer account could not be
-- registered at all, and the failure arrived as a constraint violation during
-- a field install. Every device in this fleet is a Mac. The Linux rule was
-- never the right one.
--
-- What the pattern still guarantees matters more than what it now allows. The
-- agent writes /etc/edgepilot/authorized_keys.d/<target_username>, so this
-- value becomes a path element on every Mac in the organization. Excluding '/'
-- and requiring the first character to be [a-z_] means the value can never be
-- '.', '..', or anything else that escapes that directory — which is why this
-- widens the character set rather than dropping the constraint.

ALTER TABLE ssh_keys
    DROP CONSTRAINT ssh_keys_target_username_check;

ALTER TABLE ssh_keys
    ADD CONSTRAINT ssh_keys_target_username_check
    CHECK (target_username ~ '^[a-z_][a-z0-9_.-]{0,31}$');
