-- Named sections of an album (review #82): a collection's Episode 1, Episode 2… are its discs, and a
-- disc can have a name, shown instead of "Disc N". Changed through the edit log ("sections").
CREATE TABLE album_sections (
    album_id INTEGER NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    disc_no  INTEGER NOT NULL,
    name     TEXT NOT NULL,
    PRIMARY KEY (album_id, disc_no)
) STRICT;

-- How a download's songs are put into albums (review #82): {"mode": "tags" | "folders" |
-- "collection", "title", "artist"}; '' is by their tags.
ALTER TABLE downloads ADD COLUMN grouping TEXT NOT NULL DEFAULT '';
