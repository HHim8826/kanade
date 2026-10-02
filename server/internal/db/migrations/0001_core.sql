-- Accounts, sessions, settings and credentials. Timestamps are Unix milliseconds.

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- OAuth client and tokens. The database file is mode 0600.
CREATE TABLE credentials (
    name       TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    INTEGER NOT NULL
) STRICT;

-- Only the SHA-256 of each login token is stored.
CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   BLOB NOT NULL UNIQUE,
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
) STRICT;

-- Pending Google authorizations; a callback is accepted only with a state listed here.
CREATE TABLE oauth_states (
    state      TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
) STRICT;

-- Drive folder path (relative to the platform root) to folder ID.
CREATE TABLE drive_folders (
    path    TEXT PRIMARY KEY,
    file_id TEXT NOT NULL
) STRICT;
