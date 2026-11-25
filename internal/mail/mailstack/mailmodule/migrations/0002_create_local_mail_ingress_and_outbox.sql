-- The local side of mailstack, which the standalone store kept in
-- local-mail.db: mail held for local accounts, which this module owns
-- outright, its bodies and earlier revisions, which file-store file holds each
-- attachment, the sources imports came from, ingress credentials and their
-- receipts, purge tombstones and the event outbox.
--
-- Times are Unix milliseconds and flags are 0 or 1, as the store always wrote
-- them. A NULL body or byte count is a representation the source left out,
-- apart from an empty one.

CREATE TABLE local_messages (
    account_id          text COLLATE "C" NOT NULL,
    id                  text COLLATE "C" NOT NULL,
    folder_id           text NOT NULL DEFAULT 'INBOX',
    message_id          text NOT NULL DEFAULT '',
    thread_id           text NOT NULL DEFAULT '',
    in_reply_to         text NOT NULL DEFAULT '',
    references_ids      text NOT NULL DEFAULT '[]',
    subject             text NOT NULL DEFAULT '',
    from_name           text NOT NULL DEFAULT '',
    from_email          text NOT NULL DEFAULT '',
    to_addrs            text NOT NULL DEFAULT '[]',
    cc_addrs            text NOT NULL DEFAULT '[]',
    bcc_addrs           text NOT NULL DEFAULT '[]',
    date_ms             bigint NOT NULL,
    snippet             text NOT NULL DEFAULT '',
    unread              integer NOT NULL DEFAULT 1 CHECK (unread IN (0, 1)),
    flagged             integer NOT NULL DEFAULT 0 CHECK (flagged IN (0, 1)),
    text_bytes          bigint,
    html_bytes          bigint,
    provider_message_id text NOT NULL DEFAULT '',
    source_revision     text NOT NULL DEFAULT '',
    outlook_store_id    text NOT NULL DEFAULT '',
    outlook_folder_id   text NOT NULL DEFAULT '',
    outlook_folder_path text NOT NULL DEFAULT '',
    revision            bigint NOT NULL DEFAULT 1,
    content_sha256      text NOT NULL DEFAULT '',
    created_at          bigint NOT NULL,
    updated_at          bigint NOT NULL,
    PRIMARY KEY (account_id, id)
);
-- Listing is newest first within one folder, and the cursor walks that order.
CREATE INDEX local_messages_listing_index ON local_messages (account_id, folder_id, date_ms DESC, id DESC);
CREATE INDEX local_messages_message_id_index ON local_messages (account_id, message_id);
-- The idempotency key of an import. Partial, because appended messages have
-- no provider id and share the empty string.
CREATE UNIQUE INDEX local_messages_provider_message_id_index
    ON local_messages (account_id, provider_message_id) WHERE provider_message_id <> '';

CREATE TABLE local_message_bodies (
    account_id text NOT NULL,
    id         text NOT NULL,
    body_text  text,
    body_html  text,
    PRIMARY KEY (account_id, id)
);

-- One row per export file or other unit imported at once, so an importer can
-- skip a source whose bytes it has already imported.
CREATE TABLE local_import_sources (
    account_id    text NOT NULL,
    source_ref    text COLLATE "C" NOT NULL,
    source_sha256 text NOT NULL,
    message_count integer NOT NULL,
    imported_at   bigint NOT NULL,
    PRIMARY KEY (account_id, source_ref)
);

-- A revision an import replaced: its metadata as it was served, and both
-- bodies. metadata_json is kept as the text written, byte for byte.
CREATE TABLE local_message_revisions (
    account_id    text NOT NULL,
    id            text NOT NULL,
    revision      bigint NOT NULL,
    metadata_json text NOT NULL,
    body_text     text,
    body_html     text,
    replaced_at   bigint NOT NULL,
    PRIMARY KEY (account_id, id, revision)
);

-- Which file-store file holds each attachment of each revision. The bytes are
-- file-store's; file_id is never handed out.
CREATE TABLE local_message_attachments (
    account_id           text NOT NULL,
    id                   text NOT NULL,
    revision             bigint NOT NULL,
    attachment_id        text NOT NULL,
    position             integer NOT NULL,
    source_attachment_id text NOT NULL DEFAULT '',
    filename             text NOT NULL,
    content_type         text NOT NULL,
    size_bytes           bigint NOT NULL,
    sha256               text NOT NULL,
    content_id           text NOT NULL DEFAULT '',
    is_inline            integer NOT NULL DEFAULT 0 CHECK (is_inline IN (0, 1)),
    file_id              text NOT NULL,
    PRIMARY KEY (account_id, id, revision, attachment_id)
);

-- A sender's right to push deliveries into named local accounts. Only the
-- token's sha256 is kept.
CREATE TABLE ingress_credentials (
    credential_id text PRIMARY KEY,
    token_sha256  text NOT NULL UNIQUE,
    source        text NOT NULL,
    note          text NOT NULL DEFAULT '',
    created_at    bigint NOT NULL,
    revoked_at    bigint NOT NULL DEFAULT 0
);
CREATE TABLE ingress_credential_accounts (
    credential_id text NOT NULL,
    account_id    text COLLATE "C" NOT NULL,
    PRIMARY KEY (credential_id, account_id)
);

-- One row per delivery answered, whatever the answer. A delivery id is the
-- sender's; the same one sent again is answered from here.
CREATE TABLE delivery_receipts (
    receipt_id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    credential_id       text NOT NULL,
    source              text NOT NULL,
    delivery_id         text NOT NULL,
    account_id          text NOT NULL,
    provider_message_id text NOT NULL,
    source_revision     text NOT NULL,
    content_sha256      text NOT NULL,
    status              text NOT NULL,
    message_id          text NOT NULL DEFAULT '',
    revision            bigint NOT NULL DEFAULT 0,
    detail              text NOT NULL DEFAULT '',
    received_at         bigint NOT NULL,
    UNIQUE (source, delivery_id)
);

-- A purge, remembered by the source's id, so the same message delivered or
-- imported again is refused instead of coming back.
CREATE TABLE message_purges (
    account_id          text NOT NULL,
    provider_message_id text NOT NULL,
    message_id          text NOT NULL,
    reason              text NOT NULL,
    purged_at           bigint NOT NULL,
    PRIMARY KEY (account_id, provider_message_id)
);

-- The outbox. A row is written in the same transaction as the change it
-- announces; seq is the cursor and the consumer's idempotency key.
CREATE TABLE message_events (
    seq                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind                text NOT NULL,
    origin              text NOT NULL,
    account_id          text NOT NULL,
    id                  text NOT NULL,
    provider_message_id text NOT NULL DEFAULT '',
    revision            bigint NOT NULL DEFAULT 0,
    created_at          bigint NOT NULL
);
CREATE INDEX message_events_account_index ON message_events (account_id, seq);
