-- The leaf certificate's issuer and expiry, recorded per TLS observation so the
-- drawer can answer "who signed this, and when does it lapse?" without a second
-- network call. Pinning is a separate, later feature (SCHEMA.md §8).
ALTER TABLE probe ADD COLUMN cert_issuer TEXT;
ALTER TABLE probe ADD COLUMN cert_not_after INTEGER;
