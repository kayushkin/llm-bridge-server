-- How people sign in, and the sessions that result. Times are Unix seconds, 0
-- meaning unset, as the API has always served them. A link's or session's
-- public id is its prefix and its number (FormatRecordID): sign_in_link_000001,
-- session_000001. No secret is stored, only its SHA-256.

-- A sign-in link: a one-time secret that signs one principal in.
CREATE TABLE sign_in_links (
    seq           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    secret_sha256 text NOT NULL UNIQUE,
    principal_id  text NOT NULL,
    sent_by       text NOT NULL,            -- operator, discord, phone or host_shell
    made_by       text NOT NULL DEFAULT '', -- the administrator, for a link the operator made
    created_at    bigint NOT NULL,
    expires_at    bigint NOT NULL,
    redeemed_at   bigint NOT NULL DEFAULT 0,
    session_id    text NOT NULL DEFAULT ''  -- the session the redemption made
);
CREATE INDEX sign_in_links_by_principal ON sign_in_links (principal_id, seq);

-- A session: one browser signed in as one principal. A row is never deleted.
CREATE TABLE sessions (
    seq             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    secret_sha256   text NOT NULL UNIQUE,
    principal_id    text NOT NULL,
    method          text NOT NULL,          -- sign_in_link or discord
    sign_in_link_id text NOT NULL DEFAULT '',
    created_at      bigint NOT NULL,
    expires_at      bigint NOT NULL,
    last_seen_at    bigint NOT NULL,
    revoked_at      bigint NOT NULL DEFAULT 0,
    revoked_by      text NOT NULL DEFAULT '',
    revoke_reason   text NOT NULL DEFAULT '',
    user_agent      text NOT NULL DEFAULT '',
    remote_address  text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_by_principal ON sessions (principal_id, seq);

-- One Sign in with Discord round trip: the state Discord carries back and the
-- browser-binding cookie, both kept only as hashes. Spent once, ten minutes.
CREATE TABLE discord_attempts (
    seq                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    state_sha256          text NOT NULL UNIQUE,
    browser_secret_sha256 text NOT NULL,
    purpose               text NOT NULL,          -- sign_in or link
    linking_principal_id  text NOT NULL DEFAULT '',
    linking_session_id    text NOT NULL DEFAULT '',
    return_to             text NOT NULL,
    created_at            bigint NOT NULL,
    expires_at            bigint NOT NULL,
    consumed_at           bigint NOT NULL DEFAULT 0
);
