-- Files that left the library and still have to go to the Drive trash (review #26). A row is
-- written in the same transaction that deletes the song, and removed once Drive has the file in
-- its trash (or no longer has it); failures are retried in the background.
CREATE TABLE drive_trash (
    file_id    TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    tries      INTEGER NOT NULL DEFAULT 0,
    next_at    INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
) STRICT;
