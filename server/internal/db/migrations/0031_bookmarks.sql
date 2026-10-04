-- Named places in songs (review #98): "下次從這裡", a part worth hearing again in a long drama. A
-- bookmark keeps the file it was made on and that file's length: where the song's file has changed
-- since (removed, cut again), the place may not match, and the bookmark says so instead of jumping
-- to the same second of other audio.
CREATE TABLE bookmarks (
    id          INTEGER PRIMARY KEY,
    track_id    INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    asset_id    INTEGER NOT NULL,           -- the file it was made on (kept after the file goes)
    duration_ms INTEGER NOT NULL DEFAULT 0, -- that file's length then
    position_ms INTEGER NOT NULL,
    name        TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX bookmarks_track ON bookmarks (track_id, position_ms);
