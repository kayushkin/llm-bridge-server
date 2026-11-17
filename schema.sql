-- Hands out each id number once, so an id written on a card never comes to
-- mean a different project.
CREATE TABLE IF NOT EXISTS id_sequences (
    name TEXT PRIMARY KEY,
    next INTEGER NOT NULL
);

-- A project: what it is for. What happens in it is not stored here; it is
-- worked out from what the project owns (project_links) and from the cards
-- kanban-store links to it.
CREATE TABLE IF NOT EXISTS projects (
    id                 TEXT PRIMARY KEY,          -- project_000001
    name               TEXT NOT NULL,
    kind               TEXT NOT NULL,             -- project | stream
    parent_id          TEXT REFERENCES projects (id),
    goal               TEXT NOT NULL DEFAULT '',
    done_when          TEXT NOT NULL DEFAULT '',
    stage              TEXT NOT NULL,
    -- The share of all spend the user means this project to take, 0 to 1;
    -- NULL when nobody set one.
    spend_target_share REAL,
    created_by         TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    archived_at        INTEGER NOT NULL DEFAULT 0
);

-- What a project owns, each thing named by the id its own store gave it.
-- label is the owner's name for it when linked, for display only.
CREATE TABLE IF NOT EXISTS project_links (
    id          INTEGER PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects (id),
    entity_type TEXT NOT NULL,
    entity_ref  TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    UNIQUE (project_id, entity_type, entity_ref)
);
CREATE INDEX IF NOT EXISTS project_links_by_entity ON project_links (entity_type, entity_ref);

-- Every change to a project's own fields, oldest first: how its stage moved
-- and who moved it.
CREATE TABLE IF NOT EXISTS project_changes (
    id         INTEGER PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects (id),
    field      TEXT NOT NULL,
    old_value  TEXT NOT NULL,
    new_value  TEXT NOT NULL,
    changed_by TEXT NOT NULL DEFAULT '',
    changed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS project_changes_by_project ON project_changes (project_id, id);

-- Next steps a model proposed for a project, kept until a person accepts one
-- (it becomes a noteboard todo linked to the project, note_id) or dismisses
-- it. A later round of suggestions supersedes the proposals still undecided.
CREATE TABLE IF NOT EXISTS suggestions (
    id         TEXT PRIMARY KEY,                  -- suggestion_000001
    project_id TEXT NOT NULL REFERENCES projects (id),
    title      TEXT NOT NULL,
    why        TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL,                     -- proposed | accepted | dismissed | superseded
    model      TEXT NOT NULL DEFAULT '',          -- the model that answered
    requested_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    decided_by TEXT NOT NULL DEFAULT '',
    decided_at INTEGER NOT NULL DEFAULT 0,
    note_id    TEXT NOT NULL DEFAULT ''           -- noteboard todo made on accept
);
CREATE INDEX IF NOT EXISTS suggestions_by_project ON suggestions (project_id, status, id);

-- What the session filer concluded about one chat session, each time it
-- judged it: filed it under a project by itself, proposed a project (or none)
-- for a person to decide, or found nothing to file. A row is never rewritten
-- to a new verdict: a later judgment of the same session is a new row, and
-- the proposal before it that nobody decided on becomes superseded.
CREATE TABLE IF NOT EXISTS session_filings (
    id            TEXT PRIMARY KEY,               -- session_filing_000001
    session_id    TEXT NOT NULL,                  -- llm-bridge-server's id, br_…
    session_label TEXT NOT NULL DEFAULT '',       -- the session's name when judged, for display only
    evidence      TEXT NOT NULL,                  -- brief | name | none
    goal          TEXT NOT NULL DEFAULT '',       -- the brief's overall goal as judged; '' without a brief
    repo_store_ids TEXT NOT NULL DEFAULT '[]',    -- JSON array: repo-store ids of the repos the session moved refs in
    project_id    TEXT NOT NULL DEFAULT '',       -- the project picked; '' when none fits
    confidence    REAL NOT NULL DEFAULT 0,        -- the model's odds for project_id, 0 to 1
    why           TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',       -- the model that answered; '' when no model was asked
    status        TEXT NOT NULL,                  -- filed | proposed | accepted | dismissed | superseded | nothing_to_file
    link_id       INTEGER NOT NULL DEFAULT 0,     -- the project_links row a filed or accepted filing made or found
    filed_project_id TEXT NOT NULL DEFAULT '',    -- the project of link_id: project_id when filed, the person's choice when accepted
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS session_filings_by_session ON session_filings (session_id, id);
CREATE INDEX IF NOT EXISTS session_filings_by_status ON session_filings (status, id);
