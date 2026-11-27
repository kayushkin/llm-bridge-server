# project-store

Projects: what each is for, the goals it wants reached, and what it owns in
other stores by their ids.
`CONTRACT.md` is the route table.

## Where it runs

project-store is a module of llm-bridge-server. Its records live in the
Postgres module `projects` (role `projects_module`), built by the migrations in
`migrations/`, and the server answers its routes under `/projects/` with
the same paths behind the prefix. The server's `database.postgres_url`
(`LLMBRIDGE_POSTGRES_URL`) names the Postgres; with it empty the routes answer
503 `project_store_off`. Ids keep their form: `project_000001`, `goal_000001`,
`suggestion_000001` and `session_filing_000001` are `FormatRecordID` over the
row's identity number, and only an id of exactly that form names a record.

Every route is an operator route: the service token, an administrator, or an
administrator's session agent token. No row has an owner, and links name
sessions, people and notes.

`llm-bridge-server import-project-store -sqlite <file>` copies a standalone
project-store's SQLite file (a copy made with `sqlite3 .backup`) into an empty
module, keeping every id, number and time, and numbers on from the last number
the standalone store handed out.

## What it calls

- **noteboard, in the same process** (`NoteboardModule` over the noteboard
  module's store): a note link's title, the items a project's note links name,
  and the todo a next step makes.
- **this server's own routes, over HTTP with the service token**: the sessions
  a link names and the session filer reads (`/sessions/…`, `/session-briefs`)
  and the one-shot router (`/oneshot`). The server's own address is taken from
  `listen_address`. The filer's passes belong to bridge-worker in the end,
  another process, so they call the routes rather than the server's code.
- **the stores that stay their own services**, over HTTP at the server's
  settings: repo-store (`repo_store.url`), kanban-store (`kanban_store.url`
  and `kanban_store.service_token`), scheduler (`scheduler.url`), healthcheck
  (`healthcheck.url`), principal-store (`principal_store.url`) and
  work-graph-store (`work_graph_store.url`). A call to a store whose address is
  not set is 502, naming the route it was for.

## The session filer

`POST /projects/session-filings/run` runs one pass, as before. With
`project_store.session_filer.enabled` on (off by default) the server also runs
a pass every `project_store.session_filer.interval` (default 5m), logging each
pass and every session it could not judge. Its role, minimum confidence, wait
before filing without a brief, window and sessions per pass are the
`project_store.session_filer.*` settings, and the suggestions' role is
`project_store.suggestion_model_role`; all are read at start.

## Tests

`go test ./internal/tasktracking/projects/` with
`POSTGRESTEST_ADMINISTRATOR_URL` set (`scripts/test-postgres.sh start` from the
repo root). `server_test.go` runs every route against fake owners.
