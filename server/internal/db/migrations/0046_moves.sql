-- Moving the library's files to another storage location: Google Drive or a managed one. Every
-- file of a location (to empty it before it is removed, or to change where the library lives), or
-- those of chosen albums. Each file is read and checked against its SHA-256, kept at the
-- destination and checked there; then the library reads it from there, and the old copy goes to
-- its location's trash a while later, once streams reading it are over. A source's files are not
-- Kanade's to move.
CREATE TABLE moves (
    id          INTEGER PRIMARY KEY,
    dest        TEXT NOT NULL,              -- 'drive' or a managed store's ID
    origin      TEXT NOT NULL DEFAULT '',   -- every file there: 'drive' or a store's ID; '' for albums
    albums      TEXT NOT NULL DEFAULT '',   -- JSON: the albums whose files move (their songs, cover, CUE and log, gallery)
    title       TEXT NOT NULL,              -- what moves: the location's name, or the first album's title
    state       TEXT NOT NULL,              -- running | waiting (tried again a while later) | done | canceled
    reason      TEXT NOT NULL DEFAULT '',   -- why it waits or stopped, for people: dest_offline, dest_full, origin_offline, staging...
    error       TEXT NOT NULL DEFAULT '',   -- and what was said
    next_at     INTEGER NOT NULL DEFAULT 0, -- when a waiting move is tried again
    created_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE move_items (
    id         INTEGER PRIMARY KEY,
    move_id    INTEGER NOT NULL REFERENCES moves (id) ON DELETE CASCADE,
    ref        TEXT NOT NULL,              -- the file where it was
    size       INTEGER NOT NULL,
    sha256     TEXT NOT NULL,
    mime       TEXT NOT NULL,
    kind       TEXT NOT NULL,              -- song | cover | sidecar | image
    name       TEXT NOT NULL,              -- the song's title, the file's name; the album's for a cover
    state      TEXT NOT NULL,              -- pending | done | skipped (no longer the library's there) | failed
    new_ref    TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',   -- why it failed: gone (no longer there), changed (other bytes there), error
    error      TEXT NOT NULL DEFAULT '',
    tries      INTEGER NOT NULL DEFAULT 0, -- failures that may pass (a network); the third fails it
    updated_at INTEGER NOT NULL,
    UNIQUE (move_id, ref)
) STRICT;
CREATE INDEX move_items_move ON move_items (move_id, state);
CREATE INDEX move_items_ref ON move_items (ref); -- the old copies a move owes the trash
