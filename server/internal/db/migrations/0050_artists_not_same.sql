-- Pairs of artists the user said are two, though their names look alike (D13): not proposed as one
-- artist again. The names in order, a < b.
CREATE TABLE artists_not_same (
    a          TEXT NOT NULL,
    b          TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (a, b)
) STRICT;
