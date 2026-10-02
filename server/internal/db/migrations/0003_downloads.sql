-- BitTorrent downloads driven through aria2 (decision D5).

CREATE TABLE downloads (
    id              INTEGER PRIMARY KEY,
    source          TEXT NOT NULL,             -- magnet URI, .torrent URL, or 'upload'
    name            TEXT NOT NULL DEFAULT '',
    info_hash       TEXT NOT NULL DEFAULT '',
    meta_gid        TEXT NOT NULL DEFAULT '',  -- aria2 download that fetches the metadata
    gid             TEXT NOT NULL DEFAULT '',  -- aria2 BitTorrent download
    state           TEXT NOT NULL,             -- metadata | selecting | downloading | paused | seeding | completed | failed | canceled
    dir             TEXT NOT NULL,
    files           TEXT NOT NULL DEFAULT '[]',-- JSON list: index, path, length, selected
    total_bytes     INTEGER NOT NULL DEFAULT 0,-- of the selected files
    done_bytes      INTEGER NOT NULL DEFAULT 0,
    uploaded_bytes  INTEGER NOT NULL DEFAULT 0,
    down_speed      INTEGER NOT NULL DEFAULT 0,
    up_speed        INTEGER NOT NULL DEFAULT 0,
    peers           INTEGER NOT NULL DEFAULT 0,
    error           TEXT NOT NULL DEFAULT '',
    import_batch_id INTEGER REFERENCES import_batches (id),
    files_removed   INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    completed_at    INTEGER
) STRICT;
CREATE INDEX downloads_state ON downloads (state);
