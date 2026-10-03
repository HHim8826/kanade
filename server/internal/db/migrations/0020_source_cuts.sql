-- The cut a piece of a disc image was made by (review #19): its sample range in the image, so a
-- CUE sheet corrected to cut elsewhere is not taken for one imported before. '' for a conversion,
-- and for pieces recorded before this.
ALTER TABLE import_sources ADD COLUMN cut TEXT NOT NULL DEFAULT '';
ALTER TABLE import_items ADD COLUMN source_cut TEXT NOT NULL DEFAULT '';
