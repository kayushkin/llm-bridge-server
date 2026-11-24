# work-graph-store

> **In llm-bridge-server.** This package is work-graph-store moved into the bridge: its routes are served under `/work-graph-store/` (operator routes), its ref updates are in the Postgres module `work_graph`, it reads repo-store in process, and `llm-bridge-server import-work-graph-store -sqlite <copy>` copies a standalone store's file in. The bridge reads the git hook's spool only with `work_graph_store.ingest.enabled`; the hook itself (`cmd/work-graph-hook`) is unchanged.

Records which agent session moved which git branch, through a global git hook,
and serves each repo's commit graph with the session that made every commit.
See `AGENTS.md` for how it works and `CONTRACT.md` for the routes.
