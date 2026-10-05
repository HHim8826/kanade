-- Works (review #94): an anime, a game, a book… as Bangumi (bgm.tv) describes it, that albums and
-- songs belong to. A work is linked by hand only; what the source says is kept as it said it (empty
-- or NULL where it says nothing) with when it was read, so the library shows it when the source
-- cannot be reached. A work is never deleted: unlinked, it is only not listed, and undo can link it
-- again. Links change through the edit log (an album's "works", a track's "works").
CREATE TABLE works (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    source     TEXT NOT NULL,              -- bangumi
    source_id  TEXT NOT NULL,              -- the subject's number there
    type       INTEGER NOT NULL DEFAULT 0, -- Bangumi's: 1 book, 2 anime, 3 music, 4 game, 6 real
    name       TEXT NOT NULL,              -- as first published
    name_cn    TEXT NOT NULL DEFAULT '',   -- the Chinese name, if the source has one
    platform   TEXT NOT NULL DEFAULT '',   -- TV, OVA, 剧场版, 游戏…
    date       TEXT NOT NULL DEFAULT '',   -- first aired or released
    summary    TEXT NOT NULL DEFAULT '',
    score      REAL,                       -- the source's rating, NULL when it has none
    rank       INTEGER,
    votes      INTEGER,
    image      TEXT NOT NULL DEFAULT '',   -- the source's picture
    fetched_at INTEGER NOT NULL,           -- when the source was last read
    created_at INTEGER NOT NULL,
    UNIQUE (source, source_id)
) STRICT;

CREATE TABLE album_works (
    album_id INTEGER NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    work_id  INTEGER NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    added_at INTEGER NOT NULL,
    PRIMARY KEY (album_id, work_id)
) STRICT;
CREATE INDEX album_works_work ON album_works (work_id);

-- What a song is to a work: its opening, ending, an insert song… (use), and a note ("episodes 1-12").
CREATE TABLE track_works (
    track_id INTEGER NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    work_id  INTEGER NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    use      TEXT NOT NULL DEFAULT '', -- op, ed, insert, theme, character, bgm, other; '' unsaid
    note     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (track_id, work_id)
) STRICT;
CREATE INDEX track_works_work ON track_works (work_id);
