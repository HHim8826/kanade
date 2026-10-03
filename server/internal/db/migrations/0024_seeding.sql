-- What a download uploaded before its current aria2 task (an earlier round, or seeding resumed
-- after a restart), so its share ratio counts all of it (review #75).
ALTER TABLE downloads ADD COLUMN uploaded_before INTEGER NOT NULL DEFAULT 0;
