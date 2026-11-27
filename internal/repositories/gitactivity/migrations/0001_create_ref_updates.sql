-- One row per ref that moved, as the git hook saw it. Commits and branches
-- themselves stay in git; this table holds only who moved what, and when.
CREATE TABLE ref_updates (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recorded_at_unix_nano bigint NOT NULL,
    -- llm-bridge-server's session id, or '' for a git command no session ran.
    session_id            text   NOT NULL,
    hook                  text   NOT NULL,
    git_subcommand        text   NOT NULL,
    -- repo-store's id for the repository, or NULL when repo-store has no repo
    -- at repository_path. Never guessed from a name. Not a foreign key: a
    -- deleted repo's updates keep the id it had.
    repo_store_id         bigint,
    repository_path       text   NOT NULL,
    worktree_path         text   NOT NULL,
    ref                   text   NOT NULL,
    old_sha               text   NOT NULL,
    new_sha               text   NOT NULL
);

CREATE INDEX ref_updates_by_session ON ref_updates (session_id, recorded_at_unix_nano);
CREATE INDEX ref_updates_by_repository ON ref_updates (repository_path, recorded_at_unix_nano);
CREATE INDEX ref_updates_by_new_sha ON ref_updates (new_sha);
-- A repo's graph reads every update of that repo in time order. Most rows
-- belong to clones repo-store does not know (NULL), so without this index
-- each graph scans the whole table for a few hundred rows.
CREATE INDEX ref_updates_by_repo_store_id ON ref_updates (repo_store_id, recorded_at_unix_nano, id);
