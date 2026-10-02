-- Library: assets (files), tracks (recordings), albums and album entries (plan §4),
-- plus import bookkeeping and resumable Drive uploads.

CREATE TABLE assets (
    id            INTEGER PRIMARY KEY,
    sha256        TEXT NOT NULL,
    size          INTEGER NOT NULL,
    format        TEXT NOT NULL,
    codec         TEXT NOT NULL,
    sample_rate   INTEGER NOT NULL DEFAULT 0,
    bit_depth     INTEGER NOT NULL DEFAULT 0,
    channels      INTEGER NOT NULL DEFAULT 0,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    bitrate       INTEGER NOT NULL DEFAULT 0,
    audio_md5     TEXT NOT NULL DEFAULT '',
    drive_file_id TEXT UNIQUE,
    state         TEXT NOT NULL,           -- uploading | verified | missing
    created_at    INTEGER NOT NULL,
    verified_at   INTEGER,
    UNIQUE (sha256, size)
) STRICT;

CREATE TABLE tracks (
    id         INTEGER PRIMARY KEY,
    title      TEXT NOT NULL,
    artist     TEXT NOT NULL DEFAULT '',   -- display string as tagged
    version    TEXT NOT NULL DEFAULT '',   -- live / remix / re-recording, when known
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE track_assets (
    track_id INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    asset_id INTEGER NOT NULL REFERENCES assets (id),
    PRIMARY KEY (track_id, asset_id)
) STRICT;
CREATE INDEX track_assets_asset ON track_assets (asset_id);

CREATE TABLE artists (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE track_artists (
    track_id  INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    artist_id INTEGER NOT NULL REFERENCES artists (id),
    PRIMARY KEY (track_id, artist_id)
) STRICT;
CREATE INDEX track_artists_artist ON track_artists (artist_id);

CREATE TABLE covers (
    id            INTEGER PRIMARY KEY,
    sha256        TEXT NOT NULL UNIQUE,
    mime          TEXT NOT NULL,
    drive_file_id TEXT,
    created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE albums (
    id           INTEGER PRIMARY KEY,
    title        TEXT NOT NULL,
    album_artist TEXT NOT NULL DEFAULT '',
    date         TEXT NOT NULL DEFAULT '',
    cover_id     INTEGER REFERENCES covers (id),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX albums_title ON albums (title, album_artist);

-- Entry-specific data lives here; the shared asset file is never rewritten for an album.
CREATE TABLE album_entries (
    id         INTEGER PRIMARY KEY,
    album_id   INTEGER NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    track_id   INTEGER NOT NULL REFERENCES tracks (id),
    asset_id   INTEGER NOT NULL REFERENCES assets (id),
    disc_no    INTEGER NOT NULL DEFAULT 1,
    track_no   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (album_id, disc_no, track_no, asset_id)
) STRICT;
CREATE INDEX album_entries_asset ON album_entries (asset_id);

CREATE TABLE import_batches (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,             -- local | download | upload
    source      TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL,             -- running | done
    created_at  INTEGER NOT NULL,
    finished_at INTEGER
) STRICT;

CREATE TABLE import_items (
    id         INTEGER PRIMARY KEY,
    batch_id   INTEGER NOT NULL REFERENCES import_batches (id) ON DELETE CASCADE,
    local_path TEXT NOT NULL,
    rel_path   TEXT NOT NULL,
    state      TEXT NOT NULL,              -- pending | uploading | published | duplicate | skipped | failed
    error      TEXT NOT NULL DEFAULT '',
    info       TEXT,                       -- media.Info as JSON, including original tag bytes
    sha256     TEXT NOT NULL DEFAULT '',
    asset_id   INTEGER REFERENCES assets (id),
    entry_id   INTEGER REFERENCES album_entries (id),
    track_id   INTEGER REFERENCES tracks (id),
    updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX import_items_batch ON import_items (batch_id, state);

-- A resumable upload in progress; the session URI survives restarts (P0 §1).
CREATE TABLE drive_uploads (
    asset_id    INTEGER PRIMARY KEY REFERENCES assets (id) ON DELETE CASCADE,
    session_uri TEXT NOT NULL,
    created_at  INTEGER NOT NULL
) STRICT;

CREATE VIRTUAL TABLE search_index USING fts5 (kind UNINDEXED, ref_id UNINDEXED, text, tokenize = 'trigram');
