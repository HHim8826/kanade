-- Organizing the library (P2-2, docs/p2-design.md): an edit log that can be undone, the original
-- identity of albums and entries, aliases, and the fields identification fills in.

ALTER TABLE albums ADD COLUMN catalog TEXT NOT NULL DEFAULT '';     -- catalog number, e.g. VICL-35899
ALTER TABLE albums ADD COLUMN edition TEXT NOT NULL DEFAULT '';     -- 初回限定盤, remaster ...
ALTER TABLE albums ADD COLUMN mb_release TEXT NOT NULL DEFAULT '';  -- MusicBrainz release ID once identified
ALTER TABLE albums ADD COLUMN merged_into INTEGER REFERENCES albums (id) ON DELETE SET NULL;
-- The album title and album artist (joined by U+001F) that imports look for: the tags the album was
-- created from, kept when it is renamed. NULL for albums made by hand, which imports never join.
ALTER TABLE albums ADD COLUMN origin TEXT;
UPDATE albums SET origin = title || char(31) || album_artist;
CREATE INDEX albums_origin ON albums (origin);

-- The album origin, disc and track an entry was imported as; a later import of the same file with
-- the same tags is the same entry, wherever it has been moved or renumbered since.
ALTER TABLE album_entries ADD COLUMN origin TEXT;
UPDATE album_entries SET origin = (SELECT origin FROM albums WHERE id = album_id) || char(31) || disc_no || char(31) || track_no;

ALTER TABLE tracks ADD COLUMN mb_recording TEXT NOT NULL DEFAULT '';

-- Other names (romaji, old names, abbreviations) that search also finds (plan §5).
CREATE TABLE aliases (
    target    TEXT NOT NULL,     -- artist | album | track
    target_id INTEGER NOT NULL,
    name      TEXT NOT NULL,
    PRIMARY KEY (target, target_id, name)
) STRICT;

-- Every metadata change, grouped per action (plan §4): undoing a group reverts only the fields it
-- changed, and leaves a field alone when something changed it again later.
CREATE TABLE edit_groups (
    id         INTEGER PRIMARY KEY,
    source     TEXT NOT NULL,             -- user | identify | restore | undo
    summary    TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    undo_of    INTEGER REFERENCES edit_groups (id),
    undone_by  INTEGER REFERENCES edit_groups (id)
) STRICT;

CREATE TABLE edits (
    id        INTEGER PRIMARY KEY,
    group_id  INTEGER NOT NULL REFERENCES edit_groups (id) ON DELETE CASCADE,
    target    TEXT NOT NULL,              -- track | album | entry | artist
    target_id INTEGER NOT NULL,
    field     TEXT NOT NULL,              -- a column, "aliases", or "row" for an entry removed or put back
    old_value TEXT,
    new_value TEXT
) STRICT;
CREATE INDEX edits_group ON edits (group_id);
CREATE INDEX edits_target ON edits (target, target_id);
