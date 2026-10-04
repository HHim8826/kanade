-- The title an album folder's album was given (review #86): a download grouped by folders names
-- the album after a tag all the folder's songs share, or after the folder when a later round's songs
-- carry another; the title changes only while the album still has the one Kanade gave it.
ALTER TABLE album_scopes ADD COLUMN title TEXT NOT NULL DEFAULT '';
