-- A source's pictures taken as covers where they are (review #221): the scans follow them as they do
-- the songs (moved, gone, other content), so a cover never points at a place holding nothing or
-- other bytes. sha256 is the picture's, the cover's own.
CREATE TABLE source_covers (
    store_id INTEGER NOT NULL REFERENCES stores (id) ON DELETE CASCADE,
    key      TEXT NOT NULL,
    path     TEXT NOT NULL,
    size     INTEGER NOT NULL,
    version  TEXT NOT NULL,
    sha256   TEXT NOT NULL,
    PRIMARY KEY (store_id, key)
) STRICT;
