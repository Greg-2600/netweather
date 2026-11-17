-- One captured traceroute per corridor step change (DESIGN.md §9). It is
-- evidence for localising a path change, captured once when the event opens
-- and never scheduled: a walk touches ~30 third-party routers.
ALTER TABLE corridor_event ADD COLUMN trace TEXT NOT NULL DEFAULT '';
