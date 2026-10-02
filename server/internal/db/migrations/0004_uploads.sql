-- Files sent by clients in 32 MB chunks (decision D4), staged before import.

CREATE TABLE uploads (
    id         INTEGER PRIMARY KEY,
    grp        TEXT NOT NULL,              -- one client selection (album, folder, batch of files)
    path       TEXT NOT NULL,              -- relative path inside the selection
    size       INTEGER NOT NULL,
    sha256     TEXT NOT NULL DEFAULT '',   -- optional, checked on completion when given
    received   INTEGER NOT NULL DEFAULT 0,
    state      TEXT NOT NULL,              -- receiving | complete | imported
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (grp, path)
) STRICT;
