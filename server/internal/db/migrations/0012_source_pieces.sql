-- Which output of a source a library file is (review #19): the CUE track number of a piece cut from
-- a disc image, 0 for a converted file. A source counts as imported only while every output it
-- should give is still in the library.
ALTER TABLE import_sources ADD COLUMN piece INTEGER NOT NULL DEFAULT 0;
ALTER TABLE import_items ADD COLUMN source_piece INTEGER NOT NULL DEFAULT 0;
