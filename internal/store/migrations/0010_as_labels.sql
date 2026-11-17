-- Organisation and country labels for the learned ASN. They are enrichment,
-- never configuration: UpsertTarget must not overwrite them.
ALTER TABLE target ADD COLUMN as_country TEXT;
ALTER TABLE target ADD COLUMN as_name TEXT;
