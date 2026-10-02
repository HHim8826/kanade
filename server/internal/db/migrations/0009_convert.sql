-- Conversion and CUE splitting (P2-4, decision D2 §2–3): files FFmpeg made remember what they were
-- made from, and a source imported once is skipped the next time.

ALTER TABLE import_items ADD COLUMN source_path TEXT NOT NULL DEFAULT '';   -- the file it was converted or cut from
ALTER TABLE import_items ADD COLUMN source_kind TEXT NOT NULL DEFAULT '';   -- converted | split
ALTER TABLE import_items ADD COLUMN source_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE import_items ADD COLUMN source_size INTEGER NOT NULL DEFAULT 0;

-- Source files (an APE file, a whole-disc image) and the library files made from them.
CREATE TABLE import_sources (
    sha256     TEXT NOT NULL,
    size       INTEGER NOT NULL,
    asset_id   INTEGER NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,          -- converted | split
    created_at INTEGER NOT NULL,
    PRIMARY KEY (sha256, size, asset_id)
) STRICT;
