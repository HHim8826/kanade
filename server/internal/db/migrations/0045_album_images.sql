-- An album's gallery (#134): the pictures that came with it (scans of the cover, back, booklet,
-- disc, obi), kept as they are, with what each is and their order. A picture is a file of its own,
-- read where it is like a sidecar: in the managed store under images/, or in a source.
CREATE TABLE album_images (
    id         INTEGER PRIMARY KEY,
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
    created_at INTEGER NOT NULL,
    UNIQUE (sha256, size, album_id)
) STRICT;
CREATE INDEX album_images_album ON album_images (album_id);
CREATE INDEX album_images_ref ON album_images (file_ref);
