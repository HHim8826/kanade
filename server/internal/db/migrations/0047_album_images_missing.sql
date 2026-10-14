-- A gallery's pictures follow a source's files (review #238). A picture whose file is gone, or has
-- other bytes now, is missing: not listed nor read (its ID would stand for bytes no longer there)
-- until a scan sees it again as it was; other bytes at its place are another picture, with an ID of
-- its own, which takes its place in the gallery. A picture's ID is never given to another (a
-- browser keeps a picture by its ID for good): the table is made again with AUTOINCREMENT, each
-- picture keeping its ID.
CREATE TABLE album_images_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    album_id   INTEGER REFERENCES albums (id) ON DELETE SET NULL,
    path       TEXT NOT NULL,              -- where it was in the album's folder (Scans/01.jpg): its name, and the order at first
    kind       TEXT NOT NULL DEFAULT '',   -- front | back | booklet | disc | obi | other; '' not said
    disc       INTEGER NOT NULL DEFAULT 0, -- the disc it goes with (a section of the album); 0 the album as a whole
    position   INTEGER NOT NULL DEFAULT 0, -- the order chosen; 0 for none (by path)
    sha256     TEXT NOT NULL,
    size       INTEGER NOT NULL,
    mime       TEXT NOT NULL,
    width      INTEGER NOT NULL DEFAULT 0,
    height     INTEGER NOT NULL DEFAULT 0,
    file_ref   TEXT NOT NULL,
    -- taken out of the gallery; kept so that a source's scan or an import of the same folder does
    -- not bring it back (a managed file goes to the trash once no other album or cover uses it)
    hidden     INTEGER NOT NULL DEFAULT 0,
    -- a source's file gone, or with other bytes until they are read again
    missing    INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (sha256, size, album_id)
) STRICT;
INSERT INTO album_images_new (id, album_id, path, kind, disc, position, sha256, size, mime, width, height, file_ref, hidden, created_at)
SELECT id, album_id, path, kind, disc, position, sha256, size, mime, width, height, file_ref, hidden, created_at FROM album_images;
DROP TABLE album_images;
ALTER TABLE album_images_new RENAME TO album_images;
CREATE INDEX album_images_album ON album_images (album_id);
CREATE INDEX album_images_ref ON album_images (file_ref);
