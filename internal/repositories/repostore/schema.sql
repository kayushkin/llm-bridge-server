-- repo-store schema
-- Canonical registry of repositories and their detected "signature" (a set of
-- language/framework/build/test tags). This is stable, session-independent
-- metadata: "what IS this repo." It is deliberately NOT operational state —
-- ephemeral worktrees, builds, and previews are forge's concern. The bundle
-- resolver reads a repo's effective tags to select a session bundle.

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS repos (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT UNIQUE NOT NULL,           -- canonical name, e.g. "dash"
    path          TEXT NOT NULL DEFAULT '',       -- canonical local path
    git_remote    TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',

    -- Who authors this repository: 'ours', 'vendored', or '' when nobody has
    -- stated it. Never detected and never defaulted to a real value: a
    -- fleet-wide count that silently assumes 'ours' counts third-party code as
    -- work the host can do. Written only by PUT /repos/{id}/provenance, so a
    -- re-POST that refreshes tags cannot blank it.
    provenance    TEXT NOT NULL DEFAULT '',

    tags          TEXT NOT NULL DEFAULT '',       -- JSON array: detected signature tags
    tag_overrides TEXT NOT NULL DEFAULT '',       -- JSON array: manual additive tags

    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_repos_path ON repos(path);

CREATE INDEX IF NOT EXISTS idx_repos_provenance ON repos(provenance);

-- One row per deploy of a repo: which commit went out, from where, and who did
-- it. Appended by the shared deploy gate (healthcheck scripts/deploy-gate.sh)
-- after a deploy.sh succeeds, and never updated or deleted.
--
-- This is not the session-scoped state the header rules out. A worktree or a
-- preview belongs to one run and dies with it; "commit abc123 of dash has been
-- live since Tuesday" is a fact about the repo that outlives every session, and
-- the next agent to deploy needs it before it builds.
CREATE TABLE IF NOT EXISTS deployments (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id         INTEGER NOT NULL,
    commit_sha      TEXT NOT NULL,             -- full sha of HEAD in the tree that was built
    branch          TEXT NOT NULL,             -- branch that tree was on; '' for a detached HEAD
    default_branch  TEXT NOT NULL,             -- what the gate resolved the default branch to
    source_path     TEXT NOT NULL,             -- the tree it was built in
    on_default      INTEGER NOT NULL,          -- the commit is contained in origin/<default_branch>
    dirty           INTEGER NOT NULL,          -- tracked files were modified at build time
    override_reason TEXT NOT NULL DEFAULT '',  -- non-empty = the gate refused and was overridden, with this reason
    refusals        TEXT NOT NULL DEFAULT '[]',-- JSON array: what the gate objected to (empty for a clean deploy)
    deployed_by     TEXT NOT NULL DEFAULT '',  -- session or agent that ran the deploy, as the gate could tell
    created_at      INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_deployments_repo ON deployments(repo_id, id DESC);

-- One row per GitHub pull request of a repo, as GitHub last told the
-- background sync (pull_request_sync.go). GitHub owns the pull request; this is
-- the copy repo-store keeps so that nothing else on the box calls GitHub for
-- it. A row is rewritten whenever GitHub reports a change and is never
-- deleted by the sync, so a pull request that leaves GitHub stays here as it
-- was last seen.
CREATE TABLE IF NOT EXISTS pull_requests (
    repo_id    INTEGER NOT NULL,
    number     INTEGER NOT NULL,              -- GitHub's number, unique within the repo
    title      TEXT NOT NULL,
    state      TEXT NOT NULL,                 -- 'open' | 'closed' | 'merged' (closed with a merge time)
    is_draft   INTEGER NOT NULL,
    author     TEXT NOT NULL,                 -- GitHub login of whoever opened it
    head_ref   TEXT NOT NULL,
    base_ref   TEXT NOT NULL,
    head_sha   TEXT NOT NULL,
    url        TEXT NOT NULL,                 -- the pull request's page on GitHub
    created_at INTEGER NOT NULL,              -- unix seconds, as GitHub reports each
    updated_at INTEGER NOT NULL,
    merged_at  INTEGER NOT NULL DEFAULT 0,    -- 0 = not merged
    closed_at  INTEGER NOT NULL DEFAULT 0,    -- 0 = not closed
    synced_at  INTEGER NOT NULL,              -- when the sync last wrote this row
    PRIMARY KEY (repo_id, number),
    FOREIGN KEY (repo_id) REFERENCES repos(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_pull_requests_updated ON pull_requests(updated_at DESC);

-- Where each repo's last successful pull-request sync started, so the next
-- one asks GitHub only for what changed since. github_url is the page that sync
-- read: when a repo's remote moves to another GitHub repository, the cursor
-- belongs to the old one, and the next sync starts over.
CREATE TABLE IF NOT EXISTS pull_request_syncs (
    repo_id     INTEGER PRIMARY KEY,
    github_url  TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id) ON DELETE CASCADE
);
