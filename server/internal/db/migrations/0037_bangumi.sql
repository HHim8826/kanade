-- An album's own entry in Bangumi (a music subject), kept as a work of type 3, so its collection
-- can be managed from the album; changed through the edit log (an album's "subject"). And the
-- Bangumi account its owner linked, to read and change their collections; only explicit changes
-- are written to Bangumi.
CREATE TABLE album_subjects (
    album_id INTEGER PRIMARY KEY REFERENCES albums (id) ON DELETE CASCADE,
    work_id  INTEGER NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    added_at INTEGER NOT NULL
) STRICT;
CREATE INDEX album_subjects_work ON album_subjects (work_id);

CREATE TABLE bangumi_links (
    user_id       INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    bgm_id        INTEGER NOT NULL,
    username      TEXT NOT NULL,
    nickname      TEXT NOT NULL DEFAULT '',
    access_token  TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    expires_at    INTEGER NOT NULL, -- ms
    linked_at     INTEGER NOT NULL,
    error         TEXT NOT NULL DEFAULT ''
) STRICT;
