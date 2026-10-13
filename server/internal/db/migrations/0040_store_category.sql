-- The category a source's albums are put in when they are imported (#95, #92); none when NULL.
ALTER TABLE stores ADD COLUMN category INTEGER REFERENCES categories (id) ON DELETE SET NULL;
