-- Play reports carry their order (review #8): seq counts up within a playback, so a late or
-- repeated report never takes the position back; at is the client's clock when it was made, set
-- against the server's by skew (measured on a playback's first report), so a report that arrives
-- late does not make an old playback the latest.
ALTER TABLE plays ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plays ADD COLUMN skew INTEGER NOT NULL DEFAULT 0;
