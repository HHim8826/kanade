-- Direct downloads: a file fetched over HTTP or HTTPS (an album's zip, an audio file) beside the
-- BitTorrent ones. 'bt' downloads a torrent or magnet; 'http' the one file its source links to, with
-- no file list to choose from and nothing to seed.
ALTER TABLE downloads ADD COLUMN kind TEXT NOT NULL DEFAULT 'bt';
