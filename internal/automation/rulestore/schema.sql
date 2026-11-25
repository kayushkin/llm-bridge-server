-- rule-store schema. Create-only and idempotent: Open() executes this whole
-- file on every boot. A new column goes in ensureColumns in store.go, never in
-- an edit to a CREATE TABLE below, which would be a no-op on a database that
-- already exists.

PRAGMA foreign_keys = ON;

-- A rule: "when a subject from this source changes, if these conditions hold,
-- do these effects". Its definition lives in rule_revisions; this row says
-- which revision is in force and how. Never deleted: retired is the end.
CREATE TABLE IF NOT EXISTS rules (
    id                 TEXT PRIMARY KEY,           -- rule_000001
    seq                INTEGER NOT NULL UNIQUE,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    source             TEXT NOT NULL,              -- served vocabulary; see vocabulary.go
    state              TEXT NOT NULL,              -- draft | active | paused | retired
    active_revision_id TEXT NOT NULL DEFAULT '',   -- '' until first activated
    mode               TEXT NOT NULL DEFAULT '',   -- shadow; '' until first activated
    created_by         TEXT NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

-- One version of a rule's definition. Immutable: an edit is a new revision,
-- so every run can say exactly which text decided it.
CREATE TABLE IF NOT EXISTS rule_revisions (
    id          TEXT PRIMARY KEY,                  -- rule_revision_000001
    seq         INTEGER NOT NULL UNIQUE,
    rule_id     TEXT NOT NULL REFERENCES rules(id),
    revision    INTEGER NOT NULL,                  -- 1, 2, 3 within the rule
    definition  TEXT NOT NULL,                     -- JSON of Definition, validated on write
    note        TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (rule_id, revision)
);

-- Every activate, pause and retire: who, when, which revision, which mode,
-- why. The rules row holds the current state; this holds how it got there.
CREATE TABLE IF NOT EXISTS rule_activations (
    id           TEXT PRIMARY KEY,                 -- rule_activation_000001
    seq          INTEGER NOT NULL UNIQUE,
    rule_id      TEXT NOT NULL REFERENCES rules(id),
    action       TEXT NOT NULL,                    -- activate | pause | retire
    revision_id  TEXT NOT NULL DEFAULT '',
    mode         TEXT NOT NULL DEFAULT '',
    acted_by     TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    acted_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rule_activations_rule ON rule_activations(rule_id, seq);

-- The latest copy of each subject rule-store has seen, and how many times its
-- state has changed while rule-store watched. The source owns the subject;
-- this is a cache that the next seed overwrites. state_episode is what lets a
-- rule fire once each time a subject enters a state, since the source records
-- no "entered this state at".
CREATE TABLE IF NOT EXISTS subjects (
    subject_type      TEXT NOT NULL,               -- session
    subject_id        TEXT NOT NULL,
    state             TEXT NOT NULL,
    state_episode     INTEGER NOT NULL,            -- 1 on first sight, +1 on each state change
    state_observed_at INTEGER NOT NULL,            -- when rule-store first saw this state; not when it began
    snapshot          TEXT NOT NULL,               -- the source's JSON, verbatim
    observed_at       INTEGER NOT NULL,
    PRIMARY KEY (subject_type, subject_id)
);
CREATE INDEX IF NOT EXISTS idx_subjects_state ON subjects(subject_type, state);

-- One rule revision deciding about one subject in one state episode. Unique on
-- that, so a replayed feed frame, a sweep and a restart cannot decide twice.
-- Only matches are written: a row per non-match would be one per session per
-- sweep.
CREATE TABLE IF NOT EXISTS rule_runs (
    id               TEXT PRIMARY KEY,             -- rule_run_000001
    seq              INTEGER NOT NULL UNIQUE,
    rule_id          TEXT NOT NULL REFERENCES rules(id),
    rule_revision_id TEXT NOT NULL REFERENCES rule_revisions(id),
    subject_type     TEXT NOT NULL,
    subject_id       TEXT NOT NULL,
    trigger_key      TEXT NOT NULL,                -- "<state>#<episode>"
    mode             TEXT NOT NULL,                -- shadow
    cause            TEXT NOT NULL,                -- feed | sweep
    facts            TEXT NOT NULL,                -- JSON: what the conditions were decided on
    planned_effects  TEXT NOT NULL,                -- JSON: the exact owner calls the effects are
    outcome          TEXT NOT NULL,                -- shadowed
    decided_at       INTEGER NOT NULL,
    UNIQUE (rule_revision_id, subject_type, subject_id, trigger_key)
);
CREATE INDEX IF NOT EXISTS idx_rule_runs_rule ON rule_runs(rule_id, seq);
CREATE INDEX IF NOT EXISTS idx_rule_runs_subject ON rule_runs(subject_type, subject_id);
