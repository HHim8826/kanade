-- Deleting songs (#229): a song or file deleted is looked for in each table that points at it (the
-- foreign keys' checks and cascades, and the clearing of import records); without an index on the
-- pointing column each look scans the whole table, once a song. Removing a source of 30,000 songs
-- read the listening history and the import records 30,000 times each.
CREATE INDEX plays_track ON plays (track_id);
CREATE INDEX import_items_track ON import_items (track_id);
CREATE INDEX import_items_asset ON import_items (asset_id);
CREATE INDEX import_items_entry ON import_items (entry_id);
CREATE INDEX import_sources_asset ON import_sources (asset_id);
CREATE INDEX playlist_items_asset ON playlist_items (asset_id);
