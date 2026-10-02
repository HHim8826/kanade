-- Drive and resources (P2-6, docs/p2-design.md).

-- 'disk': paused by the disk guard; resumes by itself once space is back.
ALTER TABLE downloads ADD COLUMN paused_by TEXT NOT NULL DEFAULT '';

-- A file that is already in Drive (the inbox, decision D6): read there and moved into the library,
-- not uploaded again. drive_parent is the folder it was in, for its cover and lyrics.
ALTER TABLE import_items ADD COLUMN drive_id TEXT NOT NULL DEFAULT '';
ALTER TABLE import_items ADD COLUMN drive_parent TEXT NOT NULL DEFAULT '';
ALTER TABLE import_items ADD COLUMN drive_size INTEGER NOT NULL DEFAULT 0;
