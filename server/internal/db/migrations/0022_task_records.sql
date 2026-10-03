-- A finished task the user removed from the task center. Its record stays (the library, the files'
-- original tags and the download's bookkeeping use it); only the task center leaves it out.
ALTER TABLE downloads ADD COLUMN cleared_at INTEGER;
ALTER TABLE import_batches ADD COLUMN cleared_at INTEGER;
