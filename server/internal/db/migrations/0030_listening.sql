-- Listening time as it happened (review #93): what each play adds to its heard time, in the
-- 15-minute spans it was heard in, so a day (in any time zone: they are whole quarter hours apart)
-- adds up what was heard during it, a session across midnight on both days. A row keeps the song and
-- its kind after the song is deleted, so the days' totals stay; counted marks the span in which a
-- play reached the D9 threshold. Plays from before are spread back from their last report, marked
-- estimated (BackfillListening).
CREATE TABLE listening (
    id        INTEGER PRIMARY KEY,
    play_id   INTEGER REFERENCES plays (id) ON DELETE SET NULL,
    bucket    INTEGER NOT NULL,             -- the span's start: Unix ms (UTC), a multiple of 15 minutes
    track_id  INTEGER NOT NULL,             -- the song, kept after it is deleted
    album_id  INTEGER,                      -- the album it was played from
    kind      TEXT NOT NULL DEFAULT 'music',
    ms        INTEGER NOT NULL DEFAULT 0,
    counted   INTEGER NOT NULL DEFAULT 0,
    estimated INTEGER NOT NULL DEFAULT 0,
    UNIQUE (play_id, bucket)
) STRICT;
CREATE INDEX listening_bucket ON listening (bucket);
CREATE INDEX listening_track ON listening (track_id, bucket);
