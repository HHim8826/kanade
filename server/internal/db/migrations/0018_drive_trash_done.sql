-- A file moved to the Drive trash is remembered for an hour (review #44): a re-import that found
-- that same file in Drive just before must not use it. done_at is when it went to the trash (or
-- turned out to be gone); 0 while it is still owed.
ALTER TABLE drive_trash ADD COLUMN done_at INTEGER NOT NULL DEFAULT 0;
