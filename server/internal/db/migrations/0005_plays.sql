-- Play history (plan §5) and the music / spoken-word distinction (library survey: 44 % of the
-- existing library is drama CDs and radio).

-- One row per playback session; clients repeat reports with the same session, so retries and
-- position updates never double count.
CREATE TABLE plays (
    id          INTEGER PRIMARY KEY,
    session     TEXT NOT NULL UNIQUE,
    asset_id    INTEGER NOT NULL REFERENCES assets (id),
    track_id    INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    album_id    INTEGER REFERENCES albums (id) ON DELETE SET NULL,  -- where it was played from
    started_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    position_ms INTEGER NOT NULL DEFAULT 0,
    listened_ms INTEGER NOT NULL DEFAULT 0,  -- time actually heard, not the position
    duration_ms INTEGER NOT NULL DEFAULT 0,
    counted     INTEGER NOT NULL DEFAULT 0,  -- reached the play threshold (decision D9)
    finished    INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX plays_recent ON plays (updated_at DESC);
CREATE INDEX plays_asset ON plays (asset_id, updated_at DESC);

ALTER TABLE tracks ADD COLUMN kind TEXT NOT NULL DEFAULT 'music';  -- music | spoken

-- Classify what is already imported, with the same rules the importer now applies.
UPDATE tracks SET kind = 'spoken' WHERE id IN (
    SELECT track_id FROM import_items WHERE track_id IS NOT NULL AND (
        lower(rel_path) LIKE '%drama cd%' OR lower(rel_path) LIKE '%dramacd%' OR lower(rel_path) LIKE '%radio%'
        OR lower(rel_path) LIKE '%djcd%' OR rel_path LIKE '%ドラマ%' OR rel_path LIKE '%ラジオ%'
        OR lower(coalesce(json_extract(info, '$.tags.genre'), '')) IN ('spoken', 'spoken word', 'drama', 'radio', 'audio drama', 'audiobook')
        OR coalesce(json_extract(info, '$.tags.genre'), '') LIKE '%朗読%'
        OR coalesce(json_extract(info, '$.tags.genre'), '') LIKE '%ドラマ%'
        OR lower(coalesce(json_extract(info, '$.tags.album'), '')) LIKE '%drama cd%'
        OR coalesce(json_extract(info, '$.tags.album'), '') LIKE '%ドラマCD%'
    )
);
