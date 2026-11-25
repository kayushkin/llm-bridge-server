-- activity-store schema. Create-only and idempotent: Open() runs this whole
-- file on every boot. A new column goes in an additive migration in store.go,
-- never in an edit to a CREATE TABLE below.
--
-- Nothing a person can cite is hard-deleted: DELETE stamps deleted_at, and
-- every delete, restore and purge first copies the whole row into revisions.
-- A purge blanks what the activity said, in the row and in every revision,
-- and keeps the row so a citation still resolves.

PRAGMA foreign_keys = ON;

-- One imported file, found again by its hash: importing the same file twice
-- changes nothing. finished_at is 0 while an import is still sending; running
-- the importer again on the same file resumes it.
CREATE TABLE IF NOT EXISTS imports (
    id              TEXT PRIMARY KEY,              -- import_000001
    seq             INTEGER NOT NULL UNIQUE,
    kind            TEXT NOT NULL,                 -- vocabulary.go ImportKinds
    sha256          TEXT NOT NULL UNIQUE,
    source_machine  TEXT NOT NULL DEFAULT '',
    source_path     TEXT NOT NULL DEFAULT '',
    imported_at     INTEGER NOT NULL,
    finished_at     INTEGER NOT NULL DEFAULT 0,
    counts          TEXT NOT NULL DEFAULT '{}'     -- ImportCounts as JSON, summed over every batch
);

-- One thing done at one time.
CREATE TABLE IF NOT EXISTS activities (
    id           TEXT PRIMARY KEY,                 -- activity_000001
    seq          INTEGER NOT NULL UNIQUE,
    product      TEXT NOT NULL,
    action       TEXT NOT NULL,                    -- vocabulary.go Actions
    action_text  TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    occurred_at  INTEGER NOT NULL,                 -- epoch seconds, UTC
    latitude     REAL,
    longitude    REAL,
    details      TEXT NOT NULL DEFAULT '{}',       -- Details as JSON
    source       TEXT NOT NULL,                    -- vocabulary.go Sources
    source_ref   TEXT NOT NULL,
    import_id    TEXT REFERENCES imports(id),
    -- sha256 of url and occurred_at, first 128 bits; '' without a url. A record from another
    -- source, or another export of the same product, with the same key is the
    -- same activity. A purge keeps it, so a later import still finds the row.
    dedupe_key   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    deleted_at   INTEGER NOT NULL DEFAULT 0,
    purged_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_activities_time ON activities(deleted_at, occurred_at, seq);
CREATE INDEX IF NOT EXISTS idx_activities_product ON activities(product, occurred_at);
CREATE INDEX IF NOT EXISTS idx_activities_dedupe ON activities(dedupe_key) WHERE dedupe_key != '';

-- Every source record that described an activity, the first included. An
-- import finds a record again here by (source, source_ref).
CREATE TABLE IF NOT EXISTS sightings (
    activity_id  TEXT NOT NULL REFERENCES activities(id),
    source       TEXT NOT NULL,
    source_ref   TEXT NOT NULL,
    product      TEXT NOT NULL,
    action       TEXT NOT NULL,
    import_id    TEXT NOT NULL REFERENCES imports(id),
    occurred_at  INTEGER NOT NULL,
    seen_at      INTEGER NOT NULL,
    PRIMARY KEY (source, source_ref)
);
CREATE INDEX IF NOT EXISTS idx_sightings_activity ON sightings(activity_id);
CREATE INDEX IF NOT EXISTS idx_sightings_import ON sightings(import_id, activity_id);

-- A file that came with an activity, kept under <data dir>/attachments.
CREATE TABLE IF NOT EXISTS attachments (
    id            TEXT PRIMARY KEY,                -- attachment_000001
    seq           INTEGER NOT NULL UNIQUE,
    activity_id   TEXT NOT NULL REFERENCES activities(id),
    file_name     TEXT NOT NULL,
    content_type  TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    size_bytes    INTEGER NOT NULL,
    stored_path   TEXT NOT NULL,                   -- relative to the data dir; '' once purged
    created_at    INTEGER NOT NULL,
    purged_at     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (activity_id, sha256)
);

-- An activity as it was before a delete, restore or purge.
CREATE TABLE IF NOT EXISTS revisions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    activity_id  TEXT NOT NULL REFERENCES activities(id),
    reason       TEXT NOT NULL,                    -- vocabulary.go RevisionReasons
    revised_at   INTEGER NOT NULL,
    snapshot     TEXT NOT NULL                     -- the Activity as JSON
);
CREATE INDEX IF NOT EXISTS idx_revisions_activity ON revisions(activity_id, id);

-- Full-text search over what each activity says. rowid is activities.seq.
-- Contentless: the index keeps no copy of the text, only what finds it. A
-- purge deletes the activity's row here.
CREATE VIRTUAL TABLE IF NOT EXISTS activities_fts USING fts5(
    title, url, details, content = '', contentless_delete = 1, tokenize = 'unicode61 remove_diacritics 2'
);
