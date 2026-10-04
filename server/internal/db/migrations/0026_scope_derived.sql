-- 0023 recorded every album artist it found as named by a tag (review #85). One worked out from the
-- songs' artists is told by the files themselves: no song of the album folder, in any round, has an
-- album artist tag, and the preview did not change it (the plan's album artist is still the one the
-- tags gave). Such an album becomes Various Artists when another artist's songs come, and takes a
-- later round's album artist tag, as one import of the whole folder would have decided.
UPDATE album_scopes SET derived = 1
WHERE derived = 0
    AND EXISTS (
        SELECT 1 FROM import_items i JOIN import_batches b ON b.id = i.batch_id
        WHERE b.kind = 'download' AND b.root != '' AND i.role = 'audio' AND i.plan IS NOT NULL
            AND b.root || char(31) || json_extract(i.plan, '$.folder') || char(31)
                || coalesce(nullif(json_extract(i.plan, '$.anchor.Album'), ''), json_extract(i.plan, '$.album')) = album_scopes.scope)
    AND NOT EXISTS (
        SELECT 1 FROM import_items i JOIN import_batches b ON b.id = i.batch_id
        WHERE b.kind = 'download' AND b.root != '' AND i.role = 'audio' AND i.plan IS NOT NULL
            AND b.root || char(31) || json_extract(i.plan, '$.folder') || char(31)
                || coalesce(nullif(json_extract(i.plan, '$.anchor.Album'), ''), json_extract(i.plan, '$.album')) = album_scopes.scope
            AND (coalesce(json_extract(i.info, '$.tags.album_artist'), '') != ''
                OR coalesce(json_extract(i.plan, '$.album_artist'), '') != coalesce(json_extract(i.plan, '$.tagged.AlbumArtist'), '')));
