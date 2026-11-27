-- The registry of principals: who may log in, hold a grant, be assigned a card
-- or file a ticket. Times are Unix seconds, 0 meaning unset, as the API has
-- always served them.
--
-- A principal's public id is principal_ and its number (FormatRecordID), so
-- the number is the only id stored, and every other table points at it by
-- number. There is no hard delete of a principal anywhere: other stores join
-- on its id, and disabled_at is the only removal.
CREATE TABLE principals (
    seq              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- One of kind.go's kinds; checked in Go, which names the vocabulary.
    kind             text NOT NULL,
    display_name     text NOT NULL,
    -- Free text, trimmed. Not unique and never a join key.
    email            text NOT NULL DEFAULT '',
    disabled_at      bigint NOT NULL DEFAULT 0,
    created_at       bigint NOT NULL,
    updated_at       bigint NOT NULL,
    -- The declared working week as JSON of Availability; '' means none, and
    -- none means unknown, which is never available.
    availability     text NOT NULL DEFAULT '',
    is_administrator boolean NOT NULL DEFAULT false,
    -- What the q filter searches: the display name's words with weight A and
    -- the email's with weight B, which is how a column filter
    -- (display_name:data) finds one field only. The store writes it with every
    -- name and email (searchDocumentSQL), from the words searchTextOf makes:
    -- split at every ASCII punctuation mark, lower-cased and with diacritics
    -- taken off, as the FTS5 tokenizer the standalone store searched with
    -- read them, so priya@northwind-eng.example is "priya northwind eng
    -- example" and José is "jose". Postgres has no immutable way to take off
    -- diacritics, so this is not a generated column.
    search_document  tsvector NOT NULL
);
CREATE INDEX principals_kind_index     ON principals (kind, disabled_at);
CREATE INDEX principals_disabled_index ON principals (disabled_at);
CREATE INDEX principals_search_index   ON principals USING gin (search_document);

-- Who is in which group. Go checks that group_seq is a group and member_seq a
-- kind that acts as a caller (no nested groups), because a CHECK cannot look
-- across rows.
CREATE TABLE group_members (
    group_seq  bigint NOT NULL REFERENCES principals (seq),
    member_seq bigint NOT NULL REFERENCES principals (seq),
    created_at bigint NOT NULL,
    PRIMARY KEY (group_seq, member_seq)
);
CREATE INDEX group_members_member_index ON group_members (member_seq);

-- An absence: a half-open [starts_at, ends_at) beside the declared week. Only a
-- human has rows here; checked in Go. This table is hard-deleted: nothing
-- joins on a time_off id.
CREATE TABLE time_off (
    seq           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    principal_seq bigint NOT NULL REFERENCES principals (seq),
    starts_at     bigint NOT NULL,
    ends_at       bigint NOT NULL,
    note          text NOT NULL DEFAULT '',
    created_at    bigint NOT NULL
);
CREATE INDEX time_off_principal_index ON time_off (principal_seq, starts_at);

-- A sign-in identity linked to one human: (provider, tenant_id, subject) is
-- what a login proves, never an email. Written by an administrator, never by
-- the login. No row is deleted: unlinked_at is the only removal, and
-- linked_by, unlinked_by and unlink_reason are the record of who decided.
CREATE TABLE identity_bindings (
    seq             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    principal_seq   bigint NOT NULL REFERENCES principals (seq),
    provider        text NOT NULL,
    tenant_id       text NOT NULL,
    subject         text NOT NULL,
    linked_at       bigint NOT NULL,
    linked_by_seq   bigint NOT NULL REFERENCES principals (seq),
    link_note       text NOT NULL DEFAULT '',
    unlinked_at     bigint NOT NULL DEFAULT 0,
    -- Null while the binding is active.
    unlinked_by_seq bigint REFERENCES principals (seq),
    unlink_reason   text NOT NULL DEFAULT ''
);
-- One active binding per identity. The Go check names the existing row; this
-- index is what holds when two links race.
CREATE UNIQUE INDEX identity_bindings_active_identity_index
    ON identity_bindings (provider, tenant_id, subject) WHERE unlinked_at = 0;
CREATE INDEX identity_bindings_principal_index ON identity_bindings (principal_seq);
