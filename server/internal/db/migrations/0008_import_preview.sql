-- Import preview (P2-3, docs/p2-design.md): a batch is analyzed first and, when asked, waits for the
-- user to check the grouping; ZIP archives; CUE and LOG files kept with their album.

-- state: analyzing | review | running | done | canceled
ALTER TABLE import_batches ADD COLUMN preview INTEGER NOT NULL DEFAULT 0;    -- wait in review after analysis
ALTER TABLE import_batches ADD COLUMN options TEXT NOT NULL DEFAULT '{}';    -- {"encoding": "gbk"}

-- role: audio | sidecar (a .cue or .log kept with the album) | zip (expanded at analysis)
ALTER TABLE import_items ADD COLUMN role TEXT NOT NULL DEFAULT 'audio';
-- What the import makes of the file: decided at analysis, adjusted in the preview (JSON).
ALTER TABLE import_items ADD COLUMN plan TEXT;
-- Extracted from a ZIP into staging/work: deleted once handled.
ALTER TABLE import_items ADD COLUMN temp INTEGER NOT NULL DEFAULT 0;

CREATE TABLE sidecars (
    id            INTEGER PRIMARY KEY,
    album_id      INTEGER REFERENCES albums (id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL,           -- cue | log
    sha256        TEXT NOT NULL,
    size          INTEGER NOT NULL,
    drive_file_id TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE (sha256, size, album_id)
) STRICT;
CREATE INDEX sidecars_album ON sidecars (album_id);
