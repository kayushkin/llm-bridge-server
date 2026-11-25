-- message-store: every chat message the user sent or received, one row each,
-- in time order, whatever app carried it. Times are unix milliseconds, UTC.
--
-- A record's public id is its type's prefix and its number (FormatRecordID):
-- conversation_000001, chat_message_000001, conversation_day_000001,
-- message_import_000001, message_account_000001. Only the number is stored.
-- A retired number (a superseded message, a merged conversation) is kept in
-- its own table and never handed out again: the identity columns never repeat
-- a number, and an import continues each numbering after the highest number
-- ever used, retired ones included.
--
-- Every row carries owner_principal_id, the principal-store id of the person
-- whose messages these are. A row that points at another row carries that
-- row's owner, and its foreign key names both, so nothing can hang on another
-- owner's records.

-- One import run, written first so the rows it writes can name it.
CREATE TABLE imports (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    importer           text   NOT NULL,            -- the command that ran (message-import-skype…)
    description        text   NOT NULL DEFAULT '', -- what it read, without private paths
    started_at         bigint NOT NULL,
    finished_at        bigint NOT NULL DEFAULT 0,
    counts             text   NOT NULL DEFAULT '{}', -- JSON ImportCounts, summed over batches
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX imports_owner_index ON imports (owner_principal_id, seq);

-- One chat: a direct conversation or a small group, on one platform.
CREATE TABLE conversations (
    seq                bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    platform           text    NOT NULL,            -- vocabulary.go Platforms
    source             text    NOT NULL,            -- the id namespace source_ref lives in (discord, skype…)
    source_ref         text    NOT NULL,            -- the chat's id there
    title              text    NOT NULL DEFAULT '', -- as the platform names it; empty for most direct chats
    is_group           boolean NOT NULL DEFAULT false,
    parent_source_ref  text    NOT NULL DEFAULT '', -- a thread's channel, in the same source; '' for anything else
    created_at         bigint  NOT NULL,
    updated_at         bigint  NOT NULL,
    UNIQUE (owner_principal_id, source, source_ref),
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX conversations_owner_platform_index ON conversations (owner_principal_id, platform);

-- Who is in a conversation, by the handle the platform uses. person_id is
-- people-store's id, set here once; a message's sender person is read through
-- this row, so naming a handle names every message it sent.
CREATE TABLE participants (
    owner_principal_id text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    conversation_seq   bigint  NOT NULL,
    handle             text    NOT NULL,            -- platform user id, phone number, Skype name…
    display_name       text    NOT NULL DEFAULT '', -- for display only; never joined on
    person_id          text    NOT NULL DEFAULT '', -- person_000001, or '' when nobody is known
    is_self            boolean NOT NULL DEFAULT false,
    PRIMARY KEY (conversation_seq, handle),
    FOREIGN KEY (owner_principal_id, conversation_seq) REFERENCES conversations (owner_principal_id, seq)
);
CREATE INDEX participants_person_index ON participants (owner_principal_id, person_id) WHERE person_id <> '';
CREATE INDEX participants_handle_index ON participants (owner_principal_id, handle);

CREATE TABLE messages (
    seq                 bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id  text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    conversation_seq    bigint  NOT NULL,
    sent_at             bigint  NOT NULL,
    sender_handle       text    NOT NULL DEFAULT '', -- participants.handle; '' for a system line
    kind                text    NOT NULL,            -- vocabulary.go MessageKinds
    body                text    NOT NULL DEFAULT '',
    body_source         text    NOT NULL DEFAULT '', -- vocabulary.go BodySources: whose text body is
    -- attachments, embeds and buttons are JSON arrays kept as the text the
    -- store wrote, byte for byte: an import compares them as text to decide
    -- whether a message changed.
    attachments         text    NOT NULL DEFAULT '[]',
    reply_to_source_ref text    NOT NULL DEFAULT '',
    call_seconds        integer NOT NULL DEFAULT 0,
    edited              boolean NOT NULL DEFAULT false,
    deleted_on_platform boolean NOT NULL DEFAULT false,
    embeds              text    NOT NULL DEFAULT '[]',
    buttons             text    NOT NULL DEFAULT '[]',
    source              text    NOT NULL,
    source_ref          text    NOT NULL,
    source_hash         text    NOT NULL DEFAULT '',
    -- The import that last wrote the row; NULL only for a row the standalone
    -- store held with none.
    import_seq          bigint,
    created_at          bigint  NOT NULL,
    updated_at          bigint  NOT NULL,
    -- What the q filter searches: the body, read as the FTS5 tokenizer the
    -- standalone store searched with read it. Every ASCII punctuation mark
    -- separates words, and the simple configuration neither stems nor drops
    -- common words. search_query.go reads a query the same way.
    search_document     tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', translate(body,
            '!"#$%&''()*+,-./:;<=>?@[\]^_`{|}~', '                                '))
    ) STORED,
    UNIQUE (owner_principal_id, source, source_ref),
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, conversation_seq) REFERENCES conversations (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES imports (owner_principal_id, seq)
);
CREATE INDEX messages_owner_time_index ON messages (owner_principal_id, sent_at, seq);
CREATE INDEX messages_conversation_time_index ON messages (conversation_seq, sent_at, seq);
CREATE INDEX messages_conversation_sender_index ON messages (conversation_seq, sender_handle);
-- A reply names the message it answers by source_ref; the day-involvement check looks it up.
CREATE INDEX messages_conversation_source_ref_index ON messages (conversation_seq, source_ref);
CREATE INDEX messages_search_index ON messages USING gin (search_document);

-- One conversation on one local day: what a journal-store transcript entry
-- used to hold. journal_entry_id is the transcript entry that now points
-- here, so a timeline that cites the entry still lands on these messages.
CREATE TABLE conversation_days (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    conversation_seq   bigint NOT NULL,
    day                text   NOT NULL,            -- YYYY-MM-DD, local to where the user lived
    time_zone          text   NOT NULL,            -- IANA zone the day is local to
    starts_at          bigint NOT NULL,            -- unix ms, inclusive
    ends_at            bigint NOT NULL,            -- unix ms, exclusive
    journal_source     text   NOT NULL DEFAULT '', -- the transcript's source in journal-store
    journal_source_ref text   NOT NULL DEFAULT '', -- and its source_ref
    journal_entry_id   text   NOT NULL DEFAULT '', -- entry_000001 once linked
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    UNIQUE (conversation_seq, day),
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, conversation_seq) REFERENCES conversations (owner_principal_id, seq)
);
CREATE INDEX conversation_days_journal_index ON conversation_days (owner_principal_id, journal_source, journal_source_ref) WHERE journal_source <> '';
CREATE INDEX conversation_days_entry_index ON conversation_days (owner_principal_id, journal_entry_id) WHERE journal_entry_id <> '';
CREATE INDEX conversation_days_owner_day_index ON conversation_days (owner_principal_id, day, seq);

-- A message that turned out to be another row's copy, filed under a second id
-- namespace. Its row is kept here whole and removed from messages; its old
-- number is message_seq, never handed out again, and it answers redirected to
-- the message that holds it now.
CREATE TABLE superseded_messages (
    message_seq        bigint PRIMARY KEY,         -- the retired chat_message_ number
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    source             text   NOT NULL,
    source_ref         text   NOT NULL,
    superseded_by_seq  bigint NOT NULL,            -- the message that holds it now
    import_seq         bigint NOT NULL,            -- the import that retired it
    superseded_at      bigint NOT NULL,
    row_json           text   NOT NULL,            -- every column of the retired row, as it was
    UNIQUE (owner_principal_id, source, source_ref),
    FOREIGN KEY (owner_principal_id, superseded_by_seq) REFERENCES messages (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES imports (owner_principal_id, seq)
);
CREATE INDEX superseded_messages_by_index ON superseded_messages (superseded_by_seq);

-- Every version of a message an import changed, as it was before the change.
-- message_seq is the message's number when the revision was written; it may
-- since have been retired, and its revisions stay under it.
CREATE TABLE message_revisions (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    message_seq        bigint NOT NULL,
    import_seq         bigint NOT NULL,            -- the import that changed it
    revised_at         bigint NOT NULL,
    row_json           text   NOT NULL,            -- every column of the row before the change
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES imports (owner_principal_id, seq)
);
CREATE INDEX message_revisions_message_index ON message_revisions (owner_principal_id, message_seq, seq);

-- A conversation that turned out to be another one. Its messages, days and
-- participants moved to the one it merged into; its row and participants are
-- kept here whole, its number is conversation_seq, never handed out again,
-- and its (source, source_ref) resolves to the survivor on every later import.
CREATE TABLE conversation_merges (
    conversation_seq   bigint  PRIMARY KEY,        -- the merged conversation_ number
    owner_principal_id text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    source             text    NOT NULL,
    source_ref         text    NOT NULL,
    merged_into_seq    bigint  NOT NULL,           -- the conversation that holds its messages now
    rule               text    NOT NULL,           -- vocabulary.go ConversationMergeRules: why
    import_seq         bigint  NOT NULL,           -- the import that merged it
    merged_at          bigint  NOT NULL,
    messages_moved     integer NOT NULL,
    row_json           text    NOT NULL,           -- its row and participants, as they were
    UNIQUE (owner_principal_id, source, source_ref),
    FOREIGN KEY (owner_principal_id, merged_into_seq) REFERENCES conversations (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES imports (owner_principal_id, seq)
);
CREATE INDEX conversation_merges_into_index ON conversation_merges (merged_into_seq);

-- An account of ours on a platform: the owner's own, or a bot the owner runs.
-- A conversation belongs to an account when the account's handle is one of
-- its participants on the same platform.
CREATE TABLE accounts (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    platform           text   NOT NULL,            -- vocabulary.go Platforms
    handle             text   NOT NULL,            -- the account's user id there, as participants.handle holds it
    display_name       text   NOT NULL DEFAULT '', -- for display only; never joined on
    kind               text   NOT NULL,            -- vocabulary.go AccountKinds
    credential_id      text   NOT NULL DEFAULT '', -- auth-store's id of the credential that signs in as the account
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    UNIQUE (owner_principal_id, platform, handle),
    UNIQUE (owner_principal_id, seq)
);
