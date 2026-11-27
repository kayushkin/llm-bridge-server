# gitactivity (formerly work-graph-store)

`internal/repositories/gitactivity`, package `gitactivity`. It was the standalone work-graph-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `gitactivity`. Its record ids did not change.

> **In llm-bridge-server.** This package is work-graph-store moved into the bridge: its routes are served under `/git-activity/` (operator routes), its ref updates are in the Postgres module `git_activity` (`work_graph` before 2026-11-27, renamed at start), it reads repo-store in process, and `llm-bridge-server import-git-activity -sqlite <copy>` copies a standalone store's file in. The bridge reads the git hook's spool only with `git_activity.ingest.enabled`; the hook itself (`cmd/work-graph-hook`) is unchanged.

Records which agent session moved which git branch, through a global git hook,
and serves each repo's commit graph with the session that made every commit.
See `AGENTS.md` for how it works and `CONTRACT.md` for the routes.
