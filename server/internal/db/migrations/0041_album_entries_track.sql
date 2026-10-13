-- Songs looked up by track (#220): the home page's songs without an album, the songs tab's filter, each
-- listed song's album, and deleting songs all read album_entries by track_id; without this index each
-- read scans the whole table, which grows with the library (30,000 songs: 25 seconds for the home page).
CREATE INDEX album_entries_track ON album_entries (track_id, album_id);
