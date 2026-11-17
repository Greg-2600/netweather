-- The operator's status page for a target (DESIGN.md §10.5). It is config-owned
-- like the target's name; the host is the match key against stored statuspage
-- events, so the drawer can answer "what does the provider say?" without
-- guessing which company owns a hostname.
ALTER TABLE target ADD COLUMN status_url TEXT;
