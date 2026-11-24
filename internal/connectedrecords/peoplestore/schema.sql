-- people-store schema. Create-only and idempotent: Open() executes this whole
-- file on every boot. A new column goes in an additive migration in store.go,
-- never in an edit to a CREATE TABLE below: an edited CREATE TABLE does nothing
-- to a database that already exists.

PRAGMA foreign_keys = ON;

-- A person in the operator's own life: a friend, a relative, a coworker. Other
-- records (journal entries, messages) join on `id`, so there is no hard delete
-- anywhere in this file or the Go code. `deleted_at` is the only removal, and
-- POST /persons/{id}/restore undoes it.
CREATE TABLE IF NOT EXISTS persons (
    id            TEXT PRIMARY KEY,             -- person_000001; see formatID
    seq           INTEGER NOT NULL UNIQUE,      -- monotonic; makes the id and is the FTS rowid
    display_name  TEXT NOT NULL,
    birthday      TEXT NOT NULL DEFAULT '',     -- '' | 'YYYY-MM-DD' | '--MM-DD' (year unknown); see birthday.go
    how_met       TEXT NOT NULL DEFAULT '',
    tags          TEXT NOT NULL DEFAULT '[]',   -- JSON array of strings
    principal_id  TEXT NOT NULL DEFAULT '',     -- '' | principal-store's principal_000001; never a copy of its name
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    deleted_at    INTEGER NOT NULL DEFAULT 0    -- 0 = live
);
CREATE INDEX IF NOT EXISTS idx_persons_deleted ON persons(deleted_at);

-- A handle that reaches a person: an address, a number, a chat account.
-- UNIQUE(kind, value) is the rule that one handle belongs to at most one
-- person. Values are stored normalized (see identity.go), so the rule holds
-- across spellings. Removing an identity deletes its row: nothing joins on an
-- identity id, and the person's revisions keep the old list.
CREATE TABLE IF NOT EXISTS identities (
    id          TEXT PRIMARY KEY,               -- identity_000001
    seq         INTEGER NOT NULL UNIQUE,
    person_id   TEXT NOT NULL REFERENCES persons(id),
    kind        TEXT NOT NULL,                  -- enforced in Go against IdentityKinds
    value       TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (kind, value)
);
CREATE INDEX IF NOT EXISTS idx_identities_person ON identities(person_id);
-- store.go (migrate) adds three columns to identities, since 2026-10-01:
-- linked_by (one of LinkSources), reason, and linked_at (epoch seconds, when
-- the link was made, which an import may set earlier than created_at).

-- "from_person_id is the `kind` of to_person_id": (A, B, parent) reads "A is
-- B's parent". Each pair is stored once; listing a person's relationships
-- shows the other direction with the inverse kind (relationship_kind.go).
CREATE TABLE IF NOT EXISTS relationships (
    id              TEXT PRIMARY KEY,           -- relationship_000001
    seq             INTEGER NOT NULL UNIQUE,
    from_person_id  TEXT NOT NULL REFERENCES persons(id),
    to_person_id    TEXT NOT NULL REFERENCES persons(id),
    kind            TEXT NOT NULL,              -- enforced in Go against RelationshipKinds
    note            TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    CHECK (from_person_id <> to_person_id),
    UNIQUE (from_person_id, to_person_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_relationships_from ON relationships(from_person_id);
CREATE INDEX IF NOT EXISTS idx_relationships_to   ON relationships(to_person_id);

-- A proposal from an importer that some handles are one person. Nothing is
-- linked until someone accepts it.
CREATE TABLE IF NOT EXISTS suggestions (
    id          TEXT PRIMARY KEY,               -- suggestion_000001
    seq         INTEGER NOT NULL UNIQUE,
    source      TEXT NOT NULL,                  -- enforced in Go against SuggestionSources
    payload     TEXT NOT NULL,                  -- JSON SuggestionPayload
    status      TEXT NOT NULL,                  -- proposed | accepted | rejected
    person_id   TEXT NOT NULL DEFAULT '',       -- the person an accept created or extended
    created_at  INTEGER NOT NULL,
    decided_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(status, seq);

-- Legacy, read only by the one-time copy in store.go (migrate): until
-- 2026-10-01 every handle any suggestion proposed was kept here under
-- UNIQUE(kind, value), which let only one suggestion ever name a handle. The
-- review agent needs to suggest a handle an importer suggestion already holds,
-- so suggestion_handles below replaced it. Left in place, never written.
CREATE TABLE IF NOT EXISTS suggestion_identities (
    suggestion_id  TEXT NOT NULL REFERENCES suggestions(id),
    kind           TEXT NOT NULL,
    value          TEXT NOT NULL,
    UNIQUE (kind, value)
);
CREATE INDEX IF NOT EXISTS idx_suggestion_identities_suggestion ON suggestion_identities(suggestion_id);

-- Every handle any suggestion ever proposed, whatever became of it, with the
-- suggestion that proposed it. An importer source never proposes a handle that
-- is in here under any suggestion, which is how a second run proposes nothing
-- twice and a rejected guess stays rejected (suggestion.go, ProposeSuggestion).
CREATE TABLE IF NOT EXISTS suggestion_handles (
    suggestion_id  TEXT NOT NULL REFERENCES suggestions(id),
    kind           TEXT NOT NULL,
    value          TEXT NOT NULL,
    PRIMARY KEY (suggestion_id, kind, value)
);
CREATE INDEX IF NOT EXISTS idx_suggestion_handles_handle ON suggestion_handles(kind, value);

-- "This handle is not this person": the operator removed it from them, moved
-- it away from them, or rejected a suggestion that would have put it on them.
-- A source that does not override refusals (every one but the operator; see
-- LinkSources) gets a 409 for that pair. The operator linking the pair again
-- deletes the row.
CREATE TABLE IF NOT EXISTS identity_refusals (
    id          TEXT PRIMARY KEY,               -- refusal_000001
    seq         INTEGER NOT NULL UNIQUE,
    kind        TEXT NOT NULL,
    value       TEXT NOT NULL,
    person_id   TEXT NOT NULL REFERENCES persons(id),
    refused_by  TEXT NOT NULL,                  -- one of LinkSources
    reason      TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    UNIQUE (kind, value, person_id)
);
CREATE INDEX IF NOT EXISTS idx_identity_refusals_person ON identity_refusals(person_id);

-- The review agent's queue: handles the operator talks with (handle_activity
-- with messages or a one-to-one chat) that nobody has reviewed. A claim keeps a
-- handle out of the queue for ClaimSeconds so two review sessions never take
-- the same one; a review keeps it out for good.
CREATE TABLE IF NOT EXISTS handle_reviews (
    kind         TEXT NOT NULL,
    value        TEXT NOT NULL,
    reviewed_by  TEXT NOT NULL,                 -- one of LinkSources
    reviewed_at  INTEGER NOT NULL,
    PRIMARY KEY (kind, value)
);
CREATE TABLE IF NOT EXISTS handle_review_claims (
    kind        TEXT NOT NULL,
    value       TEXT NOT NULL,
    claimed_by  TEXT NOT NULL,                  -- one of LinkSources
    claimed_at  INTEGER NOT NULL,
    PRIMARY KEY (kind, value)
);

-- What a source saw of one handle: when it was last active and how much it
-- talks. A fact about the handle, not about any suggestion or person, so it is
-- keyed like identities, on the normalized (kind, value). Suggestions sum it
-- over their handles at read time (suggestion.go), which is how the review
-- queue puts people the operator talks to first. An importer rewrites a row
-- whole on each run (PUT /handle-activity).
CREATE TABLE IF NOT EXISTS handle_activity (
    kind              TEXT NOT NULL,
    value             TEXT NOT NULL,
    source            TEXT NOT NULL,                -- enforced in Go against SuggestionSources
    last_activity_at  INTEGER NOT NULL DEFAULT 0,   -- epoch seconds; 0 = none seen
    message_count     INTEGER NOT NULL DEFAULT 0,   -- messages this handle sent, as the source logged them
    direct_chat       INTEGER NOT NULL DEFAULT 0,   -- 1 when the operator has a one-to-one chat with this handle
    observed_at       INTEGER NOT NULL,             -- when this row was last written
    PRIMARY KEY (kind, value)
);

-- The person, with its identities, as it was just before each change.
CREATE TABLE IF NOT EXISTS revisions (
    id          TEXT PRIMARY KEY,               -- revision_000001
    seq         INTEGER NOT NULL UNIQUE,
    person_id   TEXT NOT NULL REFERENCES persons(id),
    action      TEXT NOT NULL,                  -- what replaced this snapshot; see RevisionActions
    snapshot    TEXT NOT NULL,                  -- JSON Person, identities included
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_revisions_person ON revisions(person_id, seq);

-- Search over a person's name, how they met, tags and identity values. Written
-- from Go (store.go, reindexPerson) in the same transaction as each change,
-- because the identity column is gathered from another table.
CREATE VIRTUAL TABLE IF NOT EXISTS persons_fts USING fts5(
    display_name, how_met, tags, identities
);
