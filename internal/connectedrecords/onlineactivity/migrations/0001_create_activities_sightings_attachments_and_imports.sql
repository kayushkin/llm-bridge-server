-- activity-store: what a person did online, one activity per thing done at
-- one time. Times are epoch seconds, 0 meaning unset.
--
-- A record's public id is its type's prefix and its number (FormatRecordID):
-- activity_000001, attachment_000001, import_000001. Only the number is
-- stored. A revision's id is its number alone, as the standalone store gave it.
--
-- Every row carries owner_principal_id, the principal-store id of the person
-- whose activity it is. A child row carries its parent's owner, and its
-- foreign key names both, so a child can never hang on another owner's parent.
--
-- Nothing a person can cite is hard-deleted: DELETE stamps deleted_at, and
-- every delete, restore and purge first copies the whole activity into
-- revisions. A purge blanks what the activity said, in the row, its search
-- document and every revision, and keeps the row so a citation still resolves.

-- One imported file, found again among its owner's imports by its hash:
-- importing the same file twice changes nothing. finished_at is 0 while an
-- import is still sending; running the importer again on the same file
-- resumes it.
CREATE TABLE imports (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    kind               text   NOT NULL,
    sha256             text   NOT NULL,
    source_machine     text   NOT NULL DEFAULT '',
    source_path        text   NOT NULL DEFAULT '',
    imported_at        bigint NOT NULL,
    finished_at        bigint NOT NULL DEFAULT 0,
    -- ImportCounts as JSON, summed over every batch.
    counts             text   NOT NULL DEFAULT '{}',
    UNIQUE (owner_principal_id, sha256),
    UNIQUE (owner_principal_id, seq)
);

-- One thing done at one time.
CREATE TABLE activities (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    product            text   NOT NULL,
    action             text   NOT NULL,
    action_text        text   NOT NULL DEFAULT '',
    title              text   NOT NULL DEFAULT '',
    url                text   NOT NULL DEFAULT '',
    occurred_at        bigint NOT NULL,
    latitude           double precision,
    longitude          double precision,
    -- Details as JSON, kept as the import wrote it.
    details            text   NOT NULL DEFAULT '{}',
    source             text   NOT NULL,
    source_ref         text   NOT NULL,
    -- The import that first brought it in; null only for a row the standalone
    -- store held with none.
    import_seq         bigint,
    -- sha256 of url and occurred_at, first 128 bits; '' without a url. A
    -- record from another source, or another export of the same product, with
    -- the same key in the same owner's activities is the same activity. A
    -- purge keeps it, so a later import still finds the row.
    dedupe_key         text   NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    deleted_at         bigint NOT NULL DEFAULT 0,
    purged_at          bigint NOT NULL DEFAULT 0,
    -- The words of the title, url and details' text, and only the words: the
    -- store writes it with each new activity and empties it on purge, so, as
    -- the standalone store's contentless FTS5 index did, it keeps no readable
    -- copy of a purged activity.
    search_document    tsvector NOT NULL DEFAULT ''::tsvector,
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES imports (owner_principal_id, seq)
);
CREATE INDEX activities_owner_time_index ON activities (owner_principal_id, deleted_at, occurred_at, seq);
CREATE INDEX activities_time_index ON activities (deleted_at, occurred_at, seq);
CREATE INDEX activities_product_index ON activities (owner_principal_id, product, occurred_at);
CREATE INDEX activities_dedupe_index ON activities (owner_principal_id, dedupe_key) WHERE dedupe_key <> '';
CREATE INDEX activities_search_index ON activities USING gin (search_document);

-- Every source record that described an activity, the first included. An
-- import finds a record again here by (source, source_ref) among its owner's.
-- seq only orders sightings seen in the same second.
CREATE TABLE sightings (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    activity_seq       bigint NOT NULL,
    source             text   NOT NULL,
    source_ref         text   NOT NULL,
    product            text   NOT NULL,
    action             text   NOT NULL,
    import_seq         bigint NOT NULL,
    occurred_at        bigint NOT NULL,
    seen_at            bigint NOT NULL,
    UNIQUE (owner_principal_id, source, source_ref),
    FOREIGN KEY (owner_principal_id, activity_seq) REFERENCES activities (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES imports (owner_principal_id, seq)
);
CREATE INDEX sightings_activity_index ON sightings (activity_seq, seen_at, seq);
CREATE INDEX sightings_import_index ON sightings (import_seq, activity_seq);

-- A file that came with an activity. Its bytes are file-store's: file_id is
-- file-store's id of them, '' once the attachment is purged.
CREATE TABLE attachments (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    activity_seq       bigint NOT NULL,
    file_name          text   NOT NULL,
    content_type       text   NOT NULL,
    sha256             text   NOT NULL,
    size_bytes         bigint NOT NULL,
    file_id            text   NOT NULL,
    created_at         bigint NOT NULL,
    purged_at          bigint NOT NULL DEFAULT 0,
    CHECK ((purged_at = 0) = (file_id <> '')),
    UNIQUE (activity_seq, sha256),
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, activity_seq) REFERENCES activities (owner_principal_id, seq)
);

-- An activity as it was before a delete, restore or purge, as JSON.
CREATE TABLE revisions (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    activity_seq       bigint NOT NULL,
    reason             text   NOT NULL,
    revised_at         bigint NOT NULL,
    snapshot           text   NOT NULL,
    FOREIGN KEY (owner_principal_id, activity_seq) REFERENCES activities (owner_principal_id, seq)
);
CREATE INDEX revisions_activity_index ON revisions (activity_seq, id);
