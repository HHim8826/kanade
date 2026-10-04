-- album_scopes rows get a number of their own (review #87, #88): a collection made from a download
-- points its scope at the collection in the same edit as the songs move, and undo takes the scope
-- back with them; the edit log names what it changed by number, never given to another row (a
-- scope undone and done again gets its number back).
CREATE TABLE album_scopes_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL UNIQUE,       -- the download folder, the album folder in it and the album tag
    album_id   INTEGER NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    artist     TEXT NOT NULL DEFAULT '',   -- the album artist it was made with
    derived    INTEGER NOT NULL DEFAULT 0, -- worked out from the songs' artists, not named by a tag
    title      TEXT NOT NULL DEFAULT '',   -- the title it was given (grouped by folders)
    created_at INTEGER NOT NULL
) STRICT;
INSERT INTO album_scopes_new (scope, album_id, artist, derived, title, created_at)
SELECT scope, album_id, artist, derived, title, created_at FROM album_scopes ORDER BY rowid;
DROP TABLE album_scopes;
ALTER TABLE album_scopes_new RENAME TO album_scopes;
CREATE INDEX album_scopes_album ON album_scopes (album_id);
