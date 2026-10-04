-- Smart playlists (review #96): a playlist with rules picks its songs itself whenever it is read —
-- from categories, albums, artists, favorites, plays and when they were — instead of holding items.
-- NULL is an ordinary playlist.
ALTER TABLE playlists ADD COLUMN rules TEXT;
