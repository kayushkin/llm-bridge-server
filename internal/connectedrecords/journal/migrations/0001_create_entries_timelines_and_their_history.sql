-- journal-store: the user's own writing, its history, the ids it mentions,
-- and the dated items of its timelines. Times are Unix seconds, 0 meaning
-- unset, as the API has always served them.
--
-- Every row carries owner_principal_id, principal-store's id of the person who
-- keeps it. A child row carries its parent's owner, and its foreign key runs
-- over (owner_principal_id, parent), so a child can never sit under another
-- owner's record. Every uniqueness rule is per owner: two people may each keep
-- an import of the same source file, or each publish a post called "hello".

-- An entry's public id is entry_ and its number (FormatRecordID), so the
-- number is the only id stored. Nothing here is hard-deleted: DELETE stamps
-- deleted_at, and every update, delete and restore first copies the whole row
-- into entry_revisions.
CREATE TABLE entries (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    kind               text NOT NULL,
    title              text NOT NULL DEFAULT '',
    body               text NOT NULL DEFAULT '',
    written_at         bigint NOT NULL,
    visibility         text NOT NULL,
    published_at       bigint NOT NULL DEFAULT 0,
    slug               text NOT NULL DEFAULT '',
    tags               jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags) = 'array'),
    about_ref          text NOT NULL DEFAULT '',
    source             text NOT NULL DEFAULT '',
    source_ref         text NOT NULL DEFAULT '',
    source_hash        text NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    deleted_at         bigint NOT NULL DEFAULT 0,
    -- What the q filter searches: title, body and tags, read as the FTS5
    -- tokenizer the standalone store searched with read them. Every ASCII
    -- punctuation mark separates words, so well-being is "well being" and
    -- tags' JSON brackets and quotes drop away, and the simple configuration
    -- neither stems nor drops common words. search_query.go reads a query the
    -- same way.
    search_document    tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            title || ' ' || body || ' ' || tags::text,
            '!"#$%&''()*+,-./:;<=>?@[\]^_`{|}~', '                                '))
    ) STORED,
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX entries_written_index   ON entries (owner_principal_id, deleted_at, written_at DESC, seq DESC);
CREATE INDEX entries_kind_index      ON entries (kind, deleted_at);
CREATE INDEX entries_about_index     ON entries (about_ref) WHERE about_ref <> '';
CREATE INDEX entries_published_index ON entries (visibility, deleted_at, published_at DESC);
CREATE INDEX entries_tags_index      ON entries USING gin (tags jsonb_path_ops);
CREATE INDEX entries_search_index    ON entries USING gin (search_document);
-- An import is found again by where it came from, never by its title, and
-- only among the importing owner's own entries.
CREATE UNIQUE INDEX entries_source_index
    ON entries (owner_principal_id, source, source_ref) WHERE source <> '';
-- A public site finds a post by slug, so two of one owner's live published
-- entries cannot share one. Drafts and private entries may hold any slug until
-- they publish.
CREATE UNIQUE INDEX entries_published_slug_index
    ON entries (owner_principal_id, slug) WHERE visibility = 'published' AND deleted_at = 0;

-- The state of an entry before each update, delete, restore, import update or
-- promote-todo: every column of entries, plus why and when it was replaced.
CREATE TABLE entry_revisions (
    revision_id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    entry_seq          bigint NOT NULL,
    reason             text NOT NULL,
    replaced_at        bigint NOT NULL,
    kind               text NOT NULL,
    title              text NOT NULL,
    body               text NOT NULL,
    written_at         bigint NOT NULL,
    visibility         text NOT NULL,
    published_at       bigint NOT NULL,
    slug               text NOT NULL,
    tags               jsonb NOT NULL,
    about_ref          text NOT NULL,
    source             text NOT NULL,
    source_ref         text NOT NULL,
    source_hash        text NOT NULL,
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    deleted_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, entry_seq) REFERENCES entries (owner_principal_id, seq)
);
CREATE INDEX entry_revisions_entry_index ON entry_revisions (entry_seq, revision_id DESC);

-- Every [text](ref:<id>) link in an entry's body, rewritten whole on every
-- write. entity_ref is the truth; label is only what the link showed when it
-- was written. entity_type is the one reference-store type whose pattern
-- matches the ref, or '' (registry not read yet, no match, or several).
CREATE TABLE mentions (
    owner_principal_id text NOT NULL,
    entry_seq          bigint NOT NULL,
    position           integer NOT NULL,
    line               integer NOT NULL,
    entity_ref         text NOT NULL,
    entity_type        text NOT NULL DEFAULT '',
    label              text NOT NULL DEFAULT '',
    PRIMARY KEY (entry_seq, position),
    FOREIGN KEY (owner_principal_id, entry_seq) REFERENCES entries (owner_principal_id, seq)
);
CREATE INDEX mentions_ref_index  ON mentions (entity_ref);
CREATE INDEX mentions_type_index ON mentions (entity_type);

-- One dated thing on a timeline (an entry of kind timeline). Soft-deleted like
-- entries, and every update, delete and restore first copies the whole row
-- into timeline_item_revisions. Its public id is timeline_item_ and its number.
CREATE TABLE timeline_items (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    timeline_seq       bigint NOT NULL,
    occurred_at        bigint NOT NULL,
    ends_at            bigint NOT NULL DEFAULT 0,
    precision          text NOT NULL,
    title              text NOT NULL,
    body               text NOT NULL DEFAULT '',
    sources            jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(sources) = 'array'),
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    deleted_at         bigint NOT NULL DEFAULT 0,
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, timeline_seq) REFERENCES entries (owner_principal_id, seq)
);
CREATE INDEX timeline_items_timeline_index ON timeline_items (timeline_seq, deleted_at, occurred_at, seq);
CREATE INDEX timeline_items_time_index     ON timeline_items (deleted_at, occurred_at, seq);

-- timeline_seq here is the item's timeline as it was, kept as a value.
CREATE TABLE timeline_item_revisions (
    revision_id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text NOT NULL,
    item_seq           bigint NOT NULL,
    reason             text NOT NULL,
    replaced_at        bigint NOT NULL,
    timeline_seq       bigint NOT NULL,
    occurred_at        bigint NOT NULL,
    ends_at            bigint NOT NULL,
    precision          text NOT NULL,
    title              text NOT NULL,
    body               text NOT NULL,
    sources            jsonb NOT NULL,
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    deleted_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, item_seq) REFERENCES timeline_items (owner_principal_id, seq)
);
CREATE INDEX timeline_item_revisions_item_index ON timeline_item_revisions (item_seq, revision_id DESC);

-- The ref: links in a timeline item's body, then its sources, rewritten whole
-- on every write to the item. Kept apart from mentions, whose line numbers are
-- lines of an entry's body.
CREATE TABLE timeline_item_mentions (
    owner_principal_id text NOT NULL,
    item_seq           bigint NOT NULL,
    position           integer NOT NULL,
    entity_ref         text NOT NULL,
    entity_type        text NOT NULL DEFAULT '',
    label              text NOT NULL DEFAULT '',
    PRIMARY KEY (item_seq, position),
    FOREIGN KEY (owner_principal_id, item_seq) REFERENCES timeline_items (owner_principal_id, seq)
);
CREATE INDEX timeline_item_mentions_ref_index ON timeline_item_mentions (entity_ref);
