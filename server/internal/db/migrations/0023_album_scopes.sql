-- The album each album folder of a download went to (review #81). A download's rounds are imported
-- apart, and the album artist worked out from one round's songs alone can differ from another's:
-- a later round's songs join the album an earlier round made for the same folder and album tag of
-- the same download instead.
ALTER TABLE import_batches ADD COLUMN root TEXT NOT NULL DEFAULT ''; -- the folder its paths are relative to
UPDATE import_batches SET root = coalesce(
    (SELECT d.dir FROM downloads d, json_each(d.files) f WHERE json_extract(f.value, '$.batch') = import_batches.id LIMIT 1),
    (SELECT d.dir FROM downloads d WHERE d.import_batch_id = import_batches.id LIMIT 1), '')
WHERE kind = 'download';

CREATE TABLE album_scopes (
    scope      TEXT PRIMARY KEY,           -- the download folder, the album folder in it and the album tag
    album_id   INTEGER NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    artist     TEXT NOT NULL DEFAULT '',   -- the album artist it was made with
    derived    INTEGER NOT NULL DEFAULT 0, -- worked out from the songs' artists, not named by a tag
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX album_scopes_album ON album_scopes (album_id);

-- The albums earlier rounds made, the first round's where they differ.
INSERT OR IGNORE INTO album_scopes (scope, album_id, artist, derived, created_at)
SELECT b.root || char(31) || json_extract(i.plan, '$.folder') || char(31)
        || coalesce(nullif(json_extract(i.plan, '$.anchor.Album'), ''), json_extract(i.plan, '$.album')),
    e.album_id, a.album_artist, 0, b.created_at
FROM import_items i JOIN import_batches b ON b.id = i.batch_id JOIN album_entries e ON e.id = i.entry_id
    JOIN albums a ON a.id = e.album_id
WHERE b.kind = 'download' AND b.root != '' AND i.role = 'audio' AND i.plan IS NOT NULL
    AND coalesce(json_extract(i.plan, '$.album'), '') != '' AND coalesce(json_extract(i.plan, '$.group'), '') != ''
ORDER BY i.id;
