-- people-store: the people in one person's life, their handles, how they
-- relate, and the guesses that some handles are one person. Times are Unix
-- seconds, 0 meaning unset, as the API has always served them.
--
-- Every row carries owner_principal_id, principal-store's id of the person
-- who keeps the record. A child row carries its parent's owner, and a
-- composite foreign key on (owner_principal_id, parent) makes a child that
-- points at another owner's parent impossible. Every uniqueness rule is per
-- owner, because two people may each keep a record of the same person or
-- handle.
--
-- A record's public id is its prefix and its number (person_000001), so the
-- number in seq is the only id stored.

-- A person. Other records (journal entries, messages) join on the id, so there
-- is no hard delete: deleted_at is the only removal, and restore undoes it.
CREATE TABLE persons (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    display_name         text NOT NULL,
    birthday             text NOT NULL DEFAULT '',   -- '' | 'YYYY-MM-DD' | '--MM-DD' (year unknown)
    how_met              text NOT NULL DEFAULT '',
    tags                 jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags) = 'array'),
    principal_id         text NOT NULL DEFAULT '',   -- '' | the principal this person logs in as
    created_at           bigint NOT NULL,
    updated_at           bigint NOT NULL,
    deleted_at           bigint NOT NULL DEFAULT 0,  -- 0 = live
    -- The values and labels of the person's identities, in the order they
    -- were attached. It comes from another table, so the store rewrites it in
    -- the same transaction as every change to the person's identities.
    identity_search_text text NOT NULL DEFAULT '',
    -- What GET /persons?q= searches. Every ASCII punctuation mark separates
    -- words, as SQLite's FTS5 tokenizer did before the move, so
    -- @telegram_1:chat.example.com is "telegram 1 chat example com"; the
    -- simple configuration neither stems nor drops common words.
    search_document      tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            display_name || ' ' || how_met || ' ' || tags::text || ' ' || identity_search_text,
            '!"#$%&''()*+,-./:;<=>?@[\]^_`{|}~', '                                '))
    ) STORED,
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX persons_owner_deleted_index ON persons (owner_principal_id, deleted_at);
CREATE INDEX persons_principal_index     ON persons (principal_id);
CREATE INDEX persons_tags_index          ON persons USING gin (tags jsonb_path_ops);
CREATE INDEX persons_search_index        ON persons USING gin (search_document);

-- A handle that reaches a person. UNIQUE (owner, kind, value) is the rule that
-- one handle belongs to at most one of an owner's persons. Values are stored
-- normalized, so the rule holds across spellings. Removing an identity deletes
-- its row; the person's revisions keep the old list.
CREATE TABLE identities (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    person_seq         bigint NOT NULL,
    kind               text NOT NULL,                -- checked in Go against IdentityKinds
    value              text NOT NULL,
    label              text NOT NULL DEFAULT '',
    linked_by          text NOT NULL DEFAULT '',     -- one of LinkSources
    reason             text NOT NULL DEFAULT '',
    linked_at          bigint NOT NULL DEFAULT 0,    -- when the link was made; an import may set it before created_at
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, person_seq) REFERENCES persons (owner_principal_id, seq),
    UNIQUE (owner_principal_id, kind, value)
);
CREATE INDEX identities_person_index ON identities (person_seq, seq);

-- "from_person is the kind of to_person": (A, B, parent) reads "A is B's
-- parent". Each pair is stored once; a person's list shows the other direction
-- with the inverse kind.
CREATE TABLE relationships (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    from_person_seq    bigint NOT NULL,
    to_person_seq      bigint NOT NULL,
    kind               text NOT NULL,                -- checked in Go against RelationshipKinds
    note               text NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, from_person_seq) REFERENCES persons (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, to_person_seq) REFERENCES persons (owner_principal_id, seq),
    CHECK (from_person_seq <> to_person_seq),
    UNIQUE (owner_principal_id, from_person_seq, to_person_seq, kind)
);
CREATE INDEX relationships_from_index ON relationships (from_person_seq);
CREATE INDEX relationships_to_index   ON relationships (to_person_seq);

-- A source's guess that some handles are one person. Nothing is linked until
-- someone accepts it. payload is kept as the text the store wrote, so the
-- importer's source_detail comes back as it was sent.
CREATE TABLE suggestions (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    source             text NOT NULL,                -- checked in Go against SuggestionSources
    payload            text NOT NULL CHECK (jsonb_typeof(payload::jsonb) = 'object'),
    status             text NOT NULL CHECK (status IN ('proposed', 'accepted', 'rejected', 'superseded')),
    person_seq         bigint,                       -- the person an accept created or extended, or a supersede named
    created_at         bigint NOT NULL,
    decided_at         bigint NOT NULL DEFAULT 0,
    FOREIGN KEY (owner_principal_id, person_seq) REFERENCES persons (owner_principal_id, seq),
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX suggestions_owner_status_index ON suggestions (owner_principal_id, status, seq);

-- Every handle any of an owner's suggestions ever proposed, whatever became
-- of it. An importer source never proposes a handle that is in here under any
-- of the owner's suggestions, which is how a second run proposes nothing twice
-- and a rejected guess stays rejected.
CREATE TABLE suggestion_handles (
    owner_principal_id text NOT NULL,
    suggestion_seq     bigint NOT NULL,
    kind               text NOT NULL,
    value              text NOT NULL,
    PRIMARY KEY (suggestion_seq, kind, value),
    FOREIGN KEY (owner_principal_id, suggestion_seq) REFERENCES suggestions (owner_principal_id, seq)
);
CREATE INDEX suggestion_handles_handle_index ON suggestion_handles (owner_principal_id, kind, value);

-- "This handle is not this person": the owner removed it from them, moved it
-- away from them, or rejected a suggestion that would have put it on them. A
-- source that does not override refusals gets a 409 for that pair; the
-- operator linking the pair again deletes the row.
CREATE TABLE identity_refusals (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    kind               text NOT NULL,
    value              text NOT NULL,
    person_seq         bigint NOT NULL,
    refused_by         text NOT NULL,                -- one of LinkSources
    reason             text NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, person_seq) REFERENCES persons (owner_principal_id, seq),
    UNIQUE (owner_principal_id, kind, value, person_seq)
);
CREATE INDEX identity_refusals_person_index ON identity_refusals (person_seq);

-- The review agent's queue, per owner: handles the owner talks with that
-- nobody has reviewed. A claim keeps a handle out of the queue for
-- ReviewClaimSeconds so two sessions never take the same one; a review keeps
-- it out for good.
CREATE TABLE handle_reviews (
    owner_principal_id text NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    kind               text NOT NULL,
    value              text NOT NULL,
    reviewed_by        text NOT NULL,                -- one of LinkSources
    reviewed_at        bigint NOT NULL,
    PRIMARY KEY (owner_principal_id, kind, value)
);
CREATE TABLE handle_review_claims (
    owner_principal_id text NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    kind               text NOT NULL,
    value              text NOT NULL,
    claimed_by         text NOT NULL,                -- one of LinkSources
    claimed_at         bigint NOT NULL,
    PRIMARY KEY (owner_principal_id, kind, value)
);

-- What a source saw of one handle, for one owner: when it was last active and
-- how much it talks. Suggestions sum it over their handles at read time. An
-- importer rewrites a row whole on each run.
CREATE TABLE handle_activity (
    owner_principal_id text NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    kind               text NOT NULL,
    value              text NOT NULL,
    source             text NOT NULL,                -- checked in Go against SuggestionSources
    last_activity_at   bigint NOT NULL DEFAULT 0,    -- 0 = none seen
    message_count      bigint NOT NULL DEFAULT 0,
    direct_chat        boolean NOT NULL DEFAULT false,
    observed_at        bigint NOT NULL,
    PRIMARY KEY (owner_principal_id, kind, value)
);

-- The person, with its identities, as it was just before each change. The
-- snapshot is JSON text.
CREATE TABLE revisions (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    person_seq         bigint NOT NULL,
    action             text NOT NULL,                -- see RevisionActions
    snapshot           text NOT NULL CHECK (jsonb_typeof(snapshot::jsonb) = 'object'),
    created_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, person_seq) REFERENCES persons (owner_principal_id, seq)
);
CREATE INDEX revisions_person_index ON revisions (person_seq, seq);
