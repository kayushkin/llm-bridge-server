-- The registry of repositories and their detected signature (language,
-- framework, build and test tags): what a repo is, apart from any session or
-- worktree. Times are Unix seconds, as the API has always served them.
--
-- A repo's id is its identity number. Other stores keep it (work-graph-store's
-- ref updates, project-store's links, the deploy ledger below), so a renamed
-- repo keeps it: PUT /repos/{id}/identity rewrites the name and path in place.
CREATE TABLE repos (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          text NOT NULL UNIQUE,
    path          text NOT NULL DEFAULT '',
    git_remote    text NOT NULL DEFAULT '',
    description   text NOT NULL DEFAULT '',
    -- Who authors the repo: 'ours', 'vendored', or '' when nobody has said.
    -- Never detected and never defaulted to a real value. Written only by
    -- PUT /repos/{id}/provenance, so a re-POST that refreshes tags cannot
    -- blank it.
    provenance    text NOT NULL DEFAULT '' CHECK (provenance IN ('', 'ours', 'vendored')),
    -- JSON arrays of strings, written as the API takes them; '' is none.
    tags          text NOT NULL DEFAULT '',
    tag_overrides text NOT NULL DEFAULT '',
    created_at    bigint NOT NULL,
    updated_at    bigint NOT NULL
);
CREATE INDEX repos_path_index ON repos (path);
CREATE INDEX repos_provenance_index ON repos (provenance);

-- One row per deploy of a repo: which commit went out, from where, and who did
-- it. Appended by the shared deploy gate after a deploy.sh succeeds, and never
-- updated or deleted.
CREATE TABLE deployments (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id         bigint NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    -- The full sha of HEAD in the tree that was built.
    commit_sha      text NOT NULL,
    -- The branch that tree was on; '' for a detached HEAD.
    branch          text NOT NULL,
    -- What the gate resolved the default branch to.
    default_branch  text NOT NULL,
    -- The tree it was built in.
    source_path     text NOT NULL,
    -- The commit is contained in origin/<default_branch>.
    on_default      boolean NOT NULL,
    -- Tracked files were modified at build time.
    dirty           boolean NOT NULL,
    -- Not empty: the gate refused and was overridden, for this reason.
    override_reason text NOT NULL DEFAULT '',
    -- A JSON array: what the gate objected to, empty for a clean deploy.
    refusals        text NOT NULL DEFAULT '[]',
    -- The session or agent that ran the deploy, as the gate could tell.
    deployed_by     text NOT NULL DEFAULT '',
    created_at      bigint NOT NULL
);
CREATE INDEX deployments_repo_index ON deployments (repo_id, id DESC);

-- One row per GitHub pull request of a repo, as GitHub last told the sync.
-- GitHub owns the pull request; this is the copy kept so nothing else calls
-- GitHub for it. A row is rewritten when GitHub reports a change and never
-- deleted by the sync.
CREATE TABLE pull_requests (
    repo_id    bigint NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    -- GitHub's number, unique within the repo.
    number     bigint NOT NULL,
    title      text NOT NULL,
    -- open, closed (without a merge) or merged.
    state      text NOT NULL CHECK (state IN ('open', 'closed', 'merged')),
    is_draft   boolean NOT NULL,
    -- The GitHub login of whoever opened it.
    author     text NOT NULL,
    head_ref   text NOT NULL,
    base_ref   text NOT NULL,
    head_sha   text NOT NULL,
    -- The pull request's page on GitHub.
    url        text NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    -- 0: not merged, or not closed.
    merged_at  bigint NOT NULL DEFAULT 0,
    closed_at  bigint NOT NULL DEFAULT 0,
    -- When the sync last wrote the row.
    synced_at  bigint NOT NULL,
    PRIMARY KEY (repo_id, number)
);
CREATE INDEX pull_requests_updated_index ON pull_requests (updated_at DESC);

-- Where each repo's last successful pull-request sync started, so the next
-- one asks GitHub only for what changed since. github_url is the page that
-- sync read: when a repo's remote moves to another GitHub repository the
-- cursor belongs to the old one, and the next sync starts over.
CREATE TABLE pull_request_syncs (
    repo_id     bigint PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    github_url  text NOT NULL,
    started_at  bigint NOT NULL,
    finished_at bigint NOT NULL
);
