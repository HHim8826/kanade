-- Storage locations besides Google Drive (#95, #99, #100): a folder on the server or a NAS mounted
-- there, an S3-compatible bucket (Cloudflare R2), a OneDrive. A library file's place is a
-- reference: a Drive file ID as before, or "<kind>:<store id>:<key>" for a location in stores.
ALTER TABLE assets RENAME COLUMN drive_file_id TO file_ref;
ALTER TABLE covers RENAME COLUMN drive_file_id TO file_ref;
ALTER TABLE sidecars RENAME COLUMN drive_file_id TO file_ref;
-- Files owed to their location's trash, wherever they are (review #26).
ALTER TABLE drive_trash RENAME TO file_trash;
-- A resumable upload in progress, to whichever location it goes: its session is that location's own.
ALTER TABLE drive_uploads RENAME TO upload_sessions;
ALTER TABLE upload_sessions RENAME COLUMN session_uri TO session;
ALTER TABLE upload_sessions ADD COLUMN store TEXT NOT NULL DEFAULT 'drive';

-- mode: managed (Kanade keeps imported files there, under library/, covers/ and sidecars/) or
-- source (a collection that is already there, imported where it is and never written to).
CREATE TABLE stores (
    -- never reused: references to a removed store's files ("local:3:...", in caches, the trash owed)
    -- must not reach another added later
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,              -- local | s3 | onedrive
    name       TEXT NOT NULL,
    mode       TEXT NOT NULL,              -- managed | source
    config     TEXT NOT NULL,              -- JSON, by kind: where it is; no secrets
    secret     TEXT NOT NULL DEFAULT '',   -- JSON: keys or tokens (the database file is mode 0600)
    state      TEXT NOT NULL DEFAULT '',   -- the last check: ok | offline | error
    error      TEXT NOT NULL DEFAULT '',
    checked_at INTEGER NOT NULL DEFAULT 0,
    usage      TEXT NOT NULL DEFAULT '',   -- JSON: the last check's space and file counts
    auto_scan  INTEGER NOT NULL DEFAULT 1, -- a source: the regular checks import its new files
    scan       TEXT NOT NULL DEFAULT '',   -- JSON: the last scan of a source, or check of a managed one
    created_at INTEGER NOT NULL
) STRICT;

-- The files of a source as its last scan saw them: the next scan reads only what changed, finds a
-- file that moved by its content, and leaves alone what the library let go of.
CREATE TABLE source_files (
    store_id INTEGER NOT NULL REFERENCES stores (id) ON DELETE CASCADE,
    key      TEXT NOT NULL,            -- the path below the root (local), or the item ID (OneDrive)
    path     TEXT NOT NULL,            -- the path below the root, as shown
    size     INTEGER NOT NULL,
    version  TEXT NOT NULL,            -- changes when the content may have: modification time, cTag
    sha256   TEXT NOT NULL DEFAULT '', -- once read
    state    TEXT NOT NULL,            -- queued | imported | skipped | failed | ignored (let go of by the library)
    seen_at  INTEGER NOT NULL,
    gone_at  INTEGER NOT NULL DEFAULT 0, -- not there at the last scan: the library's song is missing
    PRIMARY KEY (store_id, key)
) STRICT;

-- Where the library reads an imported file from when it is imported in place (a source): its
-- reference. local_path is then the file itself (local) or a copy fetched to staging.
ALTER TABLE import_items ADD COLUMN ref TEXT NOT NULL DEFAULT '';
-- A cloud source's file (#95, #100), as the scan saw it: {"version", "hash", "with": the pictures and
-- .lrc beside it}. It is fetched to staging when its turn comes, and checked against this.
ALTER TABLE import_items ADD COLUMN fetch TEXT NOT NULL DEFAULT '';
