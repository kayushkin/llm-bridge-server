# notes

The operator's notes, ranked list entries and workspaces, in the Postgres
module `notes` (role `notes_module`), served by llm-bridge-server under
`/notes/`. It was noteboard until 2026-11-27, when noteboard's todos became
issues (`internal/tasktracking/issues`, routes `/issues/items`); the module
was renamed from `noteboard` at the first start of a server that knows, with
its records kept, and its todos moved to issues
(`internal/tasktracking/noteboardsplit`).

This package is only the module: its schema and role, the migrations that
build its `items` and `item_revisions` tables, and the `Holding` that keeps
`note`, `rank` and `workspace` items and refuses a todo with a 400 naming
`/issues/items`. The store, the routes and the recurrence rules are
`internal/tasktracking/items`, shared with issues; its README has the routes.

A ranked list is the items that share a `list_id`; an issue joins one through
a `rank` entry whose `links` hold the issue's id. A workspace is an agent's
durable working memory, which a scheduler agent job reads first and rewrites
last (`scheduler.notes_url`).

## License

MIT, as `internal/tasktracking/items`.
