-- Credits (decision D13): each name a song is credited to is an artist of its own, in a role. The
-- names as written stay on the track (artist, and now composer, lyricist and arranger): credits are
-- worked out from them and from the user's word on how a name divides (artist_splits), so editing
-- and undoing a name goes on as for any field.
ALTER TABLE tracks ADD COLUMN composer TEXT NOT NULL DEFAULT '';
ALTER TABLE tracks ADD COLUMN lyricist TEXT NOT NULL DEFAULT '';
ALTER TABLE tracks ADD COLUMN arranger TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN kind TEXT NOT NULL DEFAULT '';  -- '' (not known) | person | character

CREATE TABLE credits (
    track_id    INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    role        TEXT NOT NULL,     -- artist | voice | composer | lyricist | arranger
    position    INTEGER NOT NULL,  -- its place among the role's names, from 0; a voice is at its character's
    artist_id   INTEGER NOT NULL REFERENCES artists (id),
    credited_as TEXT NOT NULL DEFAULT '',  -- a voice: the character voiced
    PRIMARY KEY (track_id, role, position)
) STRICT;
CREATE INDEX credits_artist ON credits (artist_id, role);

-- The links so far: each song to the artist its whole artist string is.
INSERT OR IGNORE INTO credits (track_id, role, position, artist_id) SELECT track_id, 'artist', 0, artist_id FROM track_artists;
DROP TABLE track_artists;

-- How a name divides, as the user said: parts is JSON, [{"name": …, "cv": …}], or [] for a name
-- that is one artist; NULL when nothing was said (or it was undone).
CREATE TABLE artist_splits (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    parts      TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
