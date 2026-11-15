-- A song that is another's instrumental or a shortened one (TV size), told by its title or version
-- note, so that shuffling can leave it out: '' | instrumental | short. Filled in by the library at
-- start (library.EnsureVariants) and kept up as titles and notes change.
ALTER TABLE tracks ADD COLUMN variant TEXT NOT NULL DEFAULT '';
