-- Dropping the column discards every client name an operator has entered.
--
-- There is nowhere else to put it — it is not derivable from anything the
-- device reports — so this reversal loses data by construction. Reverting past
-- this migration on a fleet that has been labelled means re-labelling it.

DROP INDEX IF EXISTS devices_client_name_trgm_idx;

ALTER TABLE devices
    DROP COLUMN client_name;
