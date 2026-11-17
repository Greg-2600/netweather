-- The chain shape observed alongside a learned issuer (CLASSIFIER.md §3.5).
-- It is recorded for the pin baseline and reported as informational; a chain
-- that changes shape is never proof of tampering on its own.
ALTER TABLE cert_pin ADD COLUMN chain_len INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cert_pin ADD COLUMN intermediate_spki TEXT;
