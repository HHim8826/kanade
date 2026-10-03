-- Which login a play report came from (review #8): a hash of the session, never the token. The
-- clock offset of a device is the smallest gap between making and receiving its recent reports,
-- so the first report of a playback that sat in the network is not taken for a clock offset.
ALTER TABLE plays ADD COLUMN client TEXT NOT NULL DEFAULT '';
CREATE INDEX plays_client ON plays (client, updated_at DESC);
