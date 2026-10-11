-- Devices (review #138): the name an account gave one of its browsers, shown wherever its players
-- are listed (to take over or control their playback, to follow on Discord). A browser not named
-- here goes by the name it gives itself ("Windows · Chrome").
CREATE TABLE devices (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device  TEXT NOT NULL,
    name    TEXT NOT NULL,
    PRIMARY KEY (user_id, device)
) STRICT;
