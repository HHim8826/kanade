-- RSS sources (P2-5, plan §3, docs/p2-design.md).

CREATE TABLE rss_sources (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    url           TEXT NOT NULL,
    interval_min  INTEGER NOT NULL DEFAULT 30,
    enabled       INTEGER NOT NULL DEFAULT 1,
    auto_download INTEGER NOT NULL DEFAULT 0,
    include_rules TEXT NOT NULL DEFAULT '',     -- one rule per line: every word must appear
    exclude_rules TEXT NOT NULL DEFAULT '',
    auth_user     TEXT NOT NULL DEFAULT '',     -- optional HTTP basic auth
    auth_pass     TEXT NOT NULL DEFAULT '',
    cookie        TEXT NOT NULL DEFAULT '',     -- optional Cookie header (private trackers)
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    -- 1: the next fetch only records what the feed holds now; auto-download then acts on what
    -- appears after it (set when a source is added and whenever auto-download is turned on).
    baseline      INTEGER NOT NULL DEFAULT 1,
    next_poll_at  INTEGER NOT NULL DEFAULT 0,
    last_poll_at  INTEGER NOT NULL DEFAULT 0,
    last_ok_at    INTEGER NOT NULL DEFAULT 0,
    failures      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE rss_items (
    id            INTEGER PRIMARY KEY,
    source_id     INTEGER NOT NULL REFERENCES rss_sources (id) ON DELETE CASCADE,
    guid          TEXT NOT NULL,
    title         TEXT NOT NULL,
    page          TEXT NOT NULL DEFAULT '',    -- the item's web page
    download      TEXT NOT NULL DEFAULT '',    -- a .torrent URL or magnet link; '' when there is only a page
    info_hash     TEXT NOT NULL DEFAULT '',    -- lowercase hex
    size          INTEGER NOT NULL DEFAULT 0,  -- bytes, 0 when unknown
    seeders       INTEGER NOT NULL DEFAULT -1, -- -1 when unknown
    published_at  INTEGER NOT NULL DEFAULT 0,
    first_seen_at INTEGER NOT NULL,
    baseline      INTEGER NOT NULL DEFAULT 0,  -- seen by a baseline fetch: never auto-downloaded
    download_id   INTEGER REFERENCES downloads (id) ON DELETE SET NULL,
    UNIQUE (source_id, guid)
) STRICT;
CREATE INDEX rss_items_hash ON rss_items (info_hash);
CREATE INDEX rss_items_recent ON rss_items (source_id, first_seen_at DESC);

-- Downloads started by an RSS rule take the suggested file selection by themselves.
ALTER TABLE downloads ADD COLUMN auto_select INTEGER NOT NULL DEFAULT 0;
