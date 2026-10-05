-- Discord's status (review #135): an account linked to its owner's Discord account, so the server
-- shows what the account plays as that person's status ("Listening to Kanade"). The OAuth tokens
-- carry only the presence scope (openid, sdk.social_layer_presence, identify); unlinking revokes
-- them. Kanade connects to Discord only while there is something to show.
CREATE TABLE discord_links (
    user_id       INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    discord_id    TEXT NOT NULL,
    discord_name  TEXT NOT NULL DEFAULT '',
    access_token  TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    expires_at    INTEGER NOT NULL,           -- ms
    follow        TEXT NOT NULL DEFAULT '',   -- the browser whose playing it shows; empty: any
    follow_name   TEXT NOT NULL DEFAULT '',
    show          TEXT NOT NULL DEFAULT '{}', -- presence.Show, as JSON
    status        TEXT NOT NULL DEFAULT 'idle', -- online, idle or dnd while it shows something
    linked_at     INTEGER NOT NULL,
    error         TEXT NOT NULL DEFAULT ''    -- why it last failed, until it works again
) STRICT;
