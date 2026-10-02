-- Favorites, playlists and lyrics (P2-1, docs/p2-design.md).

CREATE TABLE favorite_tracks (
    track_id   INTEGER PRIMARY KEY REFERENCES tracks (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE favorite_albums (
    album_id   INTEGER PRIMARY KEY REFERENCES albums (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE playlists (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

-- Items have their own ID, so a song can appear twice (plan §5). album_id is where the song was
-- added from (cover and play context); asset_id pins a file version, NULL means the track's default.
CREATE TABLE playlist_items (
    id          INTEGER PRIMARY KEY,
    playlist_id INTEGER NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    pos         INTEGER NOT NULL,
    track_id    INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    album_id    INTEGER REFERENCES albums (id) ON DELETE SET NULL,
    asset_id    INTEGER REFERENCES assets (id) ON DELETE SET NULL,
    added_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX playlist_items_order ON playlist_items (playlist_id, pos);
CREATE INDEX playlist_items_track ON playlist_items (track_id);

-- One lyrics text per track. source: embedded | lrc | manual; manual text is never replaced by
-- a later import. synced: the text carries [mm:ss.xx] time tags.
CREATE TABLE lyrics (
    track_id   INTEGER PRIMARY KEY REFERENCES tracks (id) ON DELETE CASCADE,
    source     TEXT NOT NULL,
    synced     INTEGER NOT NULL DEFAULT 0,
    text       TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- Lyrics already embedded in imported files: the original tags are kept with each import item.
INSERT OR IGNORE INTO lyrics (track_id, source, synced, text, updated_at)
SELECT track_id, 'embedded', text GLOB '*[[][0-9][0-9]:[0-9][0-9]*', text, CAST(unixepoch('subsec') * 1000 AS INTEGER)
FROM (
    SELECT track_id, trim(coalesce(json_extract(info, '$.raw.LYRICS[0]'), json_extract(info, '$.raw.UNSYNCEDLYRICS[0]'))) AS text
    FROM import_items WHERE track_id IS NOT NULL AND info IS NOT NULL ORDER BY id
) WHERE text IS NOT NULL AND text != '';
