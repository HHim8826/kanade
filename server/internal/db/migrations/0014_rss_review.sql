-- A source's login and cookie go only to the source's own site (review #17); other sites that need
-- them (a CDN serving its .torrent files, say) are listed here, one origin per line.
ALTER TABLE rss_sources ADD COLUMN auth_origins TEXT NOT NULL DEFAULT '';

-- Auto-download is a queue (review #23): items the rules pick wait as pending until a download is
-- started for them, so the per-poll limit and passing failures (a full disk, a site down) delay
-- them instead of dropping them.
ALTER TABLE rss_items ADD COLUMN auto_state TEXT NOT NULL DEFAULT '';  -- pending | done | failed
ALTER TABLE rss_items ADD COLUMN auto_tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rss_items ADD COLUMN auto_next INTEGER NOT NULL DEFAULT 0;   -- not before (ms)
ALTER TABLE rss_items ADD COLUMN auto_error TEXT NOT NULL DEFAULT '';
