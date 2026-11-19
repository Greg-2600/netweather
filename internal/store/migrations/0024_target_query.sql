-- A DNS target is identified by the question it asks as well as the server it
-- asks. The ccTLD fleet has ~250 anchors sharing a handful of authoritative
-- nameservers; keying the natural key on (host, port, probe) alone made the
-- documented `seed --country-anchors` fleet impossible to store.
--
-- The question is stored so the natural key can distinguish anchors and so a
-- config reload can reconcile them independently. Existing rows have no
-- question and default to the empty string, which keeps their identity.
ALTER TABLE target ADD COLUMN query TEXT NOT NULL DEFAULT '';

DROP INDEX target_natural_key;
CREATE UNIQUE INDEX target_natural_key ON target(host, port, probe, query) WHERE enabled = 1;
