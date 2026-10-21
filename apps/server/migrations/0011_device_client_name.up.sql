-- Who the device belongs to, as the operator would say it.
--
-- Hostname is reported by the Mac and refreshed on every connect, so it is not
-- something an operator can set: whatever they typed would be overwritten the
-- next time the agent reconnected. That is the right behaviour for a fact the
-- device owns, and the wrong one for the question the fleet table is actually
-- used to answer — "whose machine is this?"
--
-- client_name is the operator's answer. It is deliberately free text rather
-- than a foreign key to a customers table: there is no customer record in this
-- system yet, and inventing one to hold a single string would be a schema for a
-- feature nobody has asked for. If accounts arrive later, this column is what
-- backfills them.
--
-- Empty by default rather than NULL, matching site and notes: "not set" and
-- "set to nothing" are the same state here, and one representation of it means
-- no query ever has to handle both.

ALTER TABLE devices
    ADD COLUMN client_name text NOT NULL DEFAULT '';

-- Searched with the same ILIKE '%…%' as hostname and serial, so it needs the
-- same trigram index. Without one, typing a client name into the search box
-- turns the fleet listing into a sequential scan.
CREATE INDEX devices_client_name_trgm_idx
    ON devices USING gin (client_name gin_trgm_ops);
