-- Each file's loudness (review #136), for the player's volume balance: EBU R128 integrated loudness
-- and sample peak as FFmpeg measures them, per file (asset): a file replaced is another asset,
-- measured anew. A file that could not be measured keeps why, and is tried again only when asked.
CREATE TABLE loudness (
    asset_id    INTEGER PRIMARY KEY REFERENCES assets (id) ON DELETE CASCADE,
    lufs        REAL,                      -- NULL: not measured (error says why)
    peak        REAL,                      -- dBFS
    method      TEXT NOT NULL,             -- ebur128: FFmpeg's ebur128 filter, sample peak
    error       TEXT NOT NULL DEFAULT '',
    measured_at INTEGER NOT NULL
) STRICT;
