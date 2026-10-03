-- A round also reserves the work space its import needs: FFmpeg output for files that are
-- converted or cut by a CUE sheet, and unpacked archives (review #46). It is kept apart from
-- round_bytes, which stays the round's own download size.
ALTER TABLE downloads ADD COLUMN round_work INTEGER NOT NULL DEFAULT 0;
