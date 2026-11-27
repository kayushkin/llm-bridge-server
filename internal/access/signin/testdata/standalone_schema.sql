-- Create-only, so it is safe on every boot.

-- A sign-in link: a one-time secret that signs one principal in. Only the
-- secret's SHA-256 is kept.
CREATE TABLE IF NOT EXISTS sign_in_links (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    secret_sha256    TEXT NOT NULL UNIQUE,
    principal_id     TEXT NOT NULL,
    sent_by          TEXT NOT NULL,          -- operator, discord or phone
    made_by          TEXT NOT NULL DEFAULT '', -- the administrator, for a link the operator made
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,
    redeemed_at      INTEGER NOT NULL DEFAULT 0,
    session_id       TEXT NOT NULL DEFAULT ''  -- the session the redemption made
);
CREATE INDEX IF NOT EXISTS idx_sign_in_links_principal ON sign_in_links(principal_id, seq);

-- A session: one browser signed in as one principal. Only the cookie
-- secret's SHA-256 is kept. A row is never deleted.
CREATE TABLE IF NOT EXISTS sessions (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    secret_sha256    TEXT NOT NULL UNIQUE,
    principal_id     TEXT NOT NULL,
    method           TEXT NOT NULL,          -- how it was signed in: link, …
    sign_in_link_id  TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,
    last_seen_at     INTEGER NOT NULL,
    revoked_at       INTEGER NOT NULL DEFAULT 0,
    revoked_by       TEXT NOT NULL DEFAULT '',
    revoke_reason    TEXT NOT NULL DEFAULT '',
    user_agent       TEXT NOT NULL DEFAULT '',
    remote_address   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_principal ON sessions(principal_id, seq);

-- One Sign in with Discord round trip: the state Discord carries back and the
-- browser-binding cookie, both kept only as hashes. Spent once, ten minutes.
CREATE TABLE IF NOT EXISTS discord_attempts (
    seq                   INTEGER PRIMARY KEY AUTOINCREMENT,
    state_sha256          TEXT NOT NULL UNIQUE,
    browser_secret_sha256 TEXT NOT NULL,
    purpose               TEXT NOT NULL,          -- sign_in or link
    linking_principal_id  TEXT NOT NULL DEFAULT '',
    linking_session_id    TEXT NOT NULL DEFAULT '',
    return_to             TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    expires_at            INTEGER NOT NULL,
    consumed_at           INTEGER NOT NULL DEFAULT 0
);
