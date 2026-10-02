-- A selection larger than the staging budget downloads in rounds that each fit (review #28): a
-- round is fetched, imported and cleared before the next starts. Rounds take staging space only
-- while they run.
ALTER TABLE downloads ADD COLUMN round INTEGER NOT NULL DEFAULT 0;        -- the round running or last finished
ALTER TABLE downloads ADD COLUMN round_bytes INTEGER NOT NULL DEFAULT 0;  -- what the running round fetches (reserved)
ALTER TABLE downloads ADD COLUMN done_before INTEGER NOT NULL DEFAULT 0;  -- bytes of the rounds finished before it
ALTER TABLE downloads ADD COLUMN note TEXT NOT NULL DEFAULT '';           -- what it waits for, in plain words
