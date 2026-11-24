-- prediction-store schema. Create-only and idempotent: Open() executes this
-- whole file on every boot. New columns go in the ensureColumn loop in Open(),
-- never in an edit to a CREATE TABLE above — an edited CREATE TABLE is a no-op
-- against a database that already exists.

PRAGMA foreign_keys = ON;

-- A prediction is a resolvable claim plus the odds you would bet at.
-- `probability` is the CURRENT number and is denormalized from the newest row
-- in `estimates`; `initial_probability` is frozen at the first estimate so the
-- gap between them measures how well you update, not just how well you guess.
CREATE TABLE IF NOT EXISTS predictions (
    id                  TEXT PRIMARY KEY,           -- prediction_000142
    seq                 INTEGER NOT NULL UNIQUE,    -- monotonic; generates id
    claim               TEXT NOT NULL,
    resolution_criteria TEXT NOT NULL,              -- required: what settles it
    category            TEXT NOT NULL,
    tags                TEXT NOT NULL DEFAULT '',   -- JSON array
    provenance          TEXT NOT NULL,              -- measured|read|inferred|guessed
    author              TEXT NOT NULL DEFAULT '',
    probability         REAL NOT NULL,              -- newest estimate
    initial_probability REAL NOT NULL,              -- first estimate, never moves
    due_at              INTEGER NOT NULL DEFAULT 0, -- resolve-by; 0 = no deadline
    status              TEXT NOT NULL DEFAULT 'open',  -- open|resolved|void
    outcome             TEXT NOT NULL DEFAULT '',      -- true|false; '' while open
    resolved_at         INTEGER NOT NULL DEFAULT 0,
    resolution_note     TEXT NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    deleted_at          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_predictions_status     ON predictions(status, deleted_at);
CREATE INDEX IF NOT EXISTS idx_predictions_due        ON predictions(due_at, status);
CREATE INDEX IF NOT EXISTS idx_predictions_category   ON predictions(category);
CREATE INDEX IF NOT EXISTS idx_predictions_provenance ON predictions(provenance);
CREATE INDEX IF NOT EXISTS idx_predictions_author     ON predictions(author);
CREATE INDEX IF NOT EXISTS idx_predictions_created    ON predictions(created_at);
CREATE INDEX IF NOT EXISTS idx_predictions_resolved   ON predictions(resolved_at);

-- Append-only. Every revision of the number, with the evidence that moved it.
-- There is no update and no delete path: rewriting your old odds is how a
-- calibration record becomes a flattering one.
CREATE TABLE IF NOT EXISTS estimates (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    prediction_id TEXT NOT NULL REFERENCES predictions(id) ON DELETE CASCADE,
    probability   REAL NOT NULL,
    rationale     TEXT NOT NULL DEFAULT '',
    provenance    TEXT NOT NULL DEFAULT '',
    author        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_estimates_prediction ON estimates(prediction_id, created_at);

-- What the prediction is about, by id in the owning store. `entity_type` is a
-- type from kanban-store's registry (GET :8305/api/entity-types); `label` is
-- carried for display and is never read back to find the row.
CREATE TABLE IF NOT EXISTS links (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    prediction_id TEXT NOT NULL REFERENCES predictions(id) ON DELETE CASCADE,
    entity_type   TEXT NOT NULL,
    entity_ref    TEXT NOT NULL,
    label         TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    UNIQUE (prediction_id, entity_type, entity_ref)
);
CREATE INDEX IF NOT EXISTS idx_links_prediction ON links(prediction_id);
CREATE INDEX IF NOT EXISTS idx_links_entity     ON links(entity_type, entity_ref);

-- Content-carrying (not external-content) so the triggers below can stay simple
-- deletes and inserts rather than fts5 'delete' commands.
CREATE VIRTUAL TABLE IF NOT EXISTS predictions_fts USING fts5(
    claim, resolution_criteria, tags, resolution_note
);

CREATE TRIGGER IF NOT EXISTS predictions_fts_after_insert AFTER INSERT ON predictions BEGIN
    INSERT INTO predictions_fts(rowid, claim, resolution_criteria, tags, resolution_note)
    VALUES (new.seq, new.claim, new.resolution_criteria, new.tags, new.resolution_note);
END;

CREATE TRIGGER IF NOT EXISTS predictions_fts_after_delete AFTER DELETE ON predictions BEGIN
    DELETE FROM predictions_fts WHERE rowid = old.seq;
END;

CREATE TRIGGER IF NOT EXISTS predictions_fts_after_update AFTER UPDATE ON predictions BEGIN
    DELETE FROM predictions_fts WHERE rowid = old.seq;
    INSERT INTO predictions_fts(rowid, claim, resolution_criteria, tags, resolution_note)
    VALUES (new.seq, new.claim, new.resolution_criteria, new.tags, new.resolution_note);
END;
