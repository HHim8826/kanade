-- Folders of the user's own for albums (review #92): 海貓, ARIA, P3R… An album can be in several,
-- and stays as it is (its tags, identity and files); a category may later hold others (parent_id),
-- shown one level for now. What is in a category changes through the edit log (an album's
-- "categories", a category's "row"), so undo takes it back; numbers are never given again, so a
-- category undone back into being keeps its own.
CREATE TABLE categories (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    parent_id  INTEGER REFERENCES categories (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX categories_name ON categories (coalesce(parent_id, 0), name COLLATE NOCASE);

CREATE TABLE album_categories (
    category_id INTEGER NOT NULL REFERENCES categories (id) ON DELETE CASCADE,
    album_id    INTEGER NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    added_at    INTEGER NOT NULL,
    PRIMARY KEY (category_id, album_id)
) STRICT;
CREATE INDEX album_categories_album ON album_categories (album_id);
