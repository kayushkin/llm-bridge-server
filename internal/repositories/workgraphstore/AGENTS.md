# About work-graph-store

## What it owns

`127.0.0.1:8319`, unit `work-graph-store.service`. Which agent session moved which git ref, in which repo and worktree, with which git command. Git keeps commits and branches; nothing kept which session made them, and here several sessions often work one branch, rebase each other's work, or merge it. From that link the store draws each repo's commit graph with every commit's maker and every branch's sessions on it. Repos are named by repo-store's id. `CONTRACT.md` is the route table.

# How it works

## The hook

git's global `core.hooksPath` is `~/.local/share/work-graph-store/git-hooks`, set by `deploy.sh`, which writes one script there for every hook name git knows. Each execs `~/bin/work-graph-hook <name>`. For `reference-transaction` (state `committed`) and `post-checkout` (branch checkouts) it appends one JSON line per ref to `~/.local/state/work-graph-store/ref-updates.jsonl`, carrying `LLM_BRIDGE_SESSION_ID`, the git subcommand read from its parent's `/proc/<pid>/cmdline`, the common git directory and the worktree. Then, for every hook, it runs the repository's own `<common dir>/hooks/<name>`, because a global hooksPath otherwise hides it. It never fails a git command: a failed record is printed to stderr, and the exit code is the repository hook's or 0. A failing `reference-transaction` hook in state `prepared` aborts the commit, so keep that path free of anything that can fail. A repo that sets its own `core.hooksPath` (`northwind-api`) is not recorded.

## Ingest

Every 2 s the service renames the spool aside, waits 200 ms for any hook mid-write, stores every line in one transaction with repo-store's id for the repository path, and deletes the file. A spool left aside by a crash is read first next time.

## Who made a commit

The earliest update that pointed `HEAD` or a local branch at the commit from `commit`, `cherry-pick`, `revert`, `am` or `commit-tree`; or from `merge`, `rebase` or `pull` only when it moved the ref from the commit's first parent, since those can also fast-forward onto commits made elsewhere. A rebased commit is a new commit, credited to whoever rebased. Commits from before the hook was installed, or fetched from elsewhere, have no maker.

# Working in this repo

## Tests and deploys

The wire types are rendered to TypeScript in `ts/model.ts` (`@kayushkin/work-graph-store-types`, which bridge-ui links) by `./generate-ts.sh` from the files `tygo.yaml` names; run it after changing a wire type and commit the result. Those files hold some internals too, and tygo renders them; nothing reads them.

`go test ./...` runs `hook_test.go`, which builds the real hook, makes commits in scratch repos as two sessions (worktree, rebase, fast-forward merge) and checks the credits. Deploy only with `./deploy.sh`; it replaces the hook binary by rename, since any git command on the host may be running it.
