-- PeeringDB internet-exchange membership, learned per target from its ASN.
-- It is an enrichment label for the drawer and the ASN lens, never a
-- monitoring signal (DESIGN.md §8.5). Stored as a comma-separated list capped
-- at the few exchanges worth naming.
ALTER TABLE target ADD COLUMN ix_names TEXT;
