-- journal-store schema. Create-only and idempotent: Open() runs this whole file
-- on every boot. A new column goes in an additive migration in store.go, never
-- in an edit to a CREATE TABLE below: an edited CREATE TABLE does nothing to a
-- database that already exists.

PRAGMA foreign_keys = ON;

-- One piece of the user's own writing. Nothing here is hard-deleted: DELETE
-- stamps deleted_at, and every update, delete and restore first copies the
-- whole row into entry_revisions.
CREATE TABLE IF NOT EXISTS entries (
    id            TEXT PRIMARY KEY,            -- entry_000001; see formatID
    seq           INTEGER NOT NULL UNIQUE,     -- monotonic; generates id, doubles as the FTS rowid
    kind          TEXT NOT NULL,               -- vocabulary.go: journal | post | note | about
    title         TEXT NOT NULL DEFAULT '',    -- may be empty only for kind journal
    body          TEXT NOT NULL DEFAULT '',    -- markdown
    written_at    INTEGER NOT NULL,            -- epoch seconds: the day the entry is about or was written
    visibility    TEXT NOT NULL,               -- vocabulary.go: private | draft | published
    published_at  INTEGER NOT NULL DEFAULT 0,  -- first time it became published; 0 = never
    slug          TEXT NOT NULL DEFAULT '',    -- unique among live published entries (index below)
    tags          TEXT NOT NULL DEFAULT '[]',  -- JSON array of strings
    about_ref     TEXT NOT NULL DEFAULT '',    -- kind about: the id of what it is about (person_000012)
    source        TEXT NOT NULL DEFAULT '',    -- imports: where it came from (samsung-notes)
    source_ref    TEXT NOT NULL DEFAULT '',    -- imports: its id there (a relative path)
    source_hash   TEXT NOT NULL DEFAULT '',    -- imports: the caller's hash of the source; same hash = skip
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    deleted_at    INTEGER NOT NULL DEFAULT 0   -- 0 = live
);
CREATE INDEX IF NOT EXISTS idx_entries_written    ON entries(deleted_at, written_at DESC, seq DESC);
CREATE INDEX IF NOT EXISTS idx_entries_kind       ON entries(kind, deleted_at);
CREATE INDEX IF NOT EXISTS idx_entries_about      ON entries(about_ref) WHERE about_ref != '';
CREATE INDEX IF NOT EXISTS idx_entries_published  ON entries(visibility, deleted_at, published_at DESC);
-- An import is found again by where it came from, never by its title.
CREATE UNIQUE INDEX IF NOT EXISTS idx_entries_source
    ON entries(source, source_ref) WHERE source != '';
-- A public site finds a post by slug, so two live published entries cannot
-- share one. Drafts and private entries may hold any slug until they publish.
CREATE UNIQUE INDEX IF NOT EXISTS idx_entries_published_slug
    ON entries(slug) WHERE visibility = 'published' AND deleted_at = 0;

-- The state of an entry before each update, delete, restore or import update:
-- every column of entries, plus why and when it was replaced.
CREATE TABLE IF NOT EXISTS entry_revisions (
    revision_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id      TEXT NOT NULL REFERENCES entries(id),
    reason        TEXT NOT NULL,               -- update | delete | restore | import | promote-todo
    replaced_at   INTEGER NOT NULL,
    kind          TEXT NOT NULL,
    title         TEXT NOT NULL,
    body          TEXT NOT NULL,
    written_at    INTEGER NOT NULL,
    visibility    TEXT NOT NULL,
    published_at  INTEGER NOT NULL,
    slug          TEXT NOT NULL,
    tags          TEXT NOT NULL,
    about_ref     TEXT NOT NULL,
    source        TEXT NOT NULL,
    source_ref    TEXT NOT NULL,
    source_hash   TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    deleted_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entry_revisions_entry ON entry_revisions(entry_id, revision_id DESC);

-- Every [text](ref:<id>) link in an entry's body, rewritten whole on every
-- write. entity_ref is the truth; label is only what the link showed when it
-- was written. entity_type is filled from kanban-store's entity-type registry
-- when exactly one registered pattern matches the ref, and left '' otherwise
-- (registry not fetched yet, no match, or several matches).
CREATE TABLE IF NOT EXISTS mentions (
    entry_id     TEXT NOT NULL REFERENCES entries(id),
    position     INTEGER NOT NULL,             -- 0-based order of the link in the body
    line         INTEGER NOT NULL,             -- 1-based line the link starts on
    entity_ref   TEXT NOT NULL,
    entity_type  TEXT NOT NULL DEFAULT '',
    label        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (entry_id, position)
);
CREATE INDEX IF NOT EXISTS idx_mentions_ref  ON mentions(entity_ref);
CREATE INDEX IF NOT EXISTS idx_mentions_type ON mentions(entity_type);

-- One dated thing on a timeline (an entry of kind timeline). Soft-deleted
-- like entries, and every update, delete and restore first copies the whole
-- row into timeline_item_revisions.
CREATE TABLE IF NOT EXISTS timeline_items (
    id           TEXT PRIMARY KEY,             -- timeline_item_000001
    seq          INTEGER NOT NULL UNIQUE,
    timeline_id  TEXT NOT NULL REFERENCES entries(id),
    occurred_at  INTEGER NOT NULL,             -- epoch seconds
    ends_at      INTEGER NOT NULL DEFAULT 0,   -- 0 = a moment, not a span
    precision    TEXT NOT NULL,                -- vocabulary.go: minute | day | month | year
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',     -- markdown
    sources      TEXT NOT NULL DEFAULT '[]',   -- JSON array of the ids the item rests on
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    deleted_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_timeline_items_timeline ON timeline_items(timeline_id, deleted_at, occurred_at, seq);
CREATE INDEX IF NOT EXISTS idx_timeline_items_time     ON timeline_items(deleted_at, occurred_at, seq);

CREATE TABLE IF NOT EXISTS timeline_item_revisions (
    revision_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id      TEXT NOT NULL REFERENCES timeline_items(id),
    reason       TEXT NOT NULL,                -- update | delete | restore
    replaced_at  INTEGER NOT NULL,
    timeline_id  TEXT NOT NULL,
    occurred_at  INTEGER NOT NULL,
    ends_at      INTEGER NOT NULL,
    precision    TEXT NOT NULL,
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    sources      TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    deleted_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_timeline_item_revisions_item ON timeline_item_revisions(item_id, revision_id DESC);

-- The ref: links in a timeline item's body, then its sources, rewritten whole
-- on every write to the item. Kept apart from mentions, whose line numbers
-- are lines of an entry's body.
CREATE TABLE IF NOT EXISTS timeline_item_mentions (
    item_id      TEXT NOT NULL REFERENCES timeline_items(id),
    position     INTEGER NOT NULL,
    entity_ref   TEXT NOT NULL,
    entity_type  TEXT NOT NULL DEFAULT '',
    label        TEXT NOT NULL DEFAULT '',     -- '' for a source
    PRIMARY KEY (item_id, position)
);
CREATE INDEX IF NOT EXISTS idx_timeline_item_mentions_ref ON timeline_item_mentions(entity_ref);

-- Content-carrying (not external-content), so the triggers stay plain deletes
-- and inserts. rowid is entries.seq. tags is indexed as its JSON text; the
-- tokenizer drops the brackets and quotes.
CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(title, body, tags);

CREATE TRIGGER IF NOT EXISTS entries_fts_after_insert AFTER INSERT ON entries BEGIN
    INSERT INTO entries_fts(rowid, title, body, tags) VALUES (new.seq, new.title, new.body, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS entries_fts_after_delete AFTER DELETE ON entries BEGIN
    DELETE FROM entries_fts WHERE rowid = old.seq;
END;

CREATE TRIGGER IF NOT EXISTS entries_fts_after_update AFTER UPDATE ON entries BEGIN
    DELETE FROM entries_fts WHERE rowid = old.seq;
    INSERT INTO entries_fts(rowid, title, body, tags) VALUES (new.seq, new.title, new.body, new.tags);
END;
