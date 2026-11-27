# sessionlog (formerly log-store)

`internal/sessionlog`, package `sessionlog`. It was the standalone log-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `sessionlog`. Its record ids did not change.

The session event log of llm-bridge-server: every `msg.Event` of every session,
the turns each is placed in, the stored reading pages, search and per-session
token totals. It came into this repo with its history from the standalone
log-store (github.com/kayushkin/log-store) and runs inside the bridge, as the
`session_log` package of the consolidation plan.

## Where it runs

- **Storage:** Postgres, in the module's own schema `session_log` and role
  `session_log_module` (`store.Module()`, migrations in `store/migrations/`),
  through the bridge's `postgresstorage` package and the setting
  `database.postgres_url` (`LLMBRIDGE_POSTGRES_URL`). The store keeps two pools:
  a writer of one connection, which the turn index relies on (see
  `store/turn_index.go`), and a reader of eight whose transactions are read only.
- **The bridge's own calls are in process.** The harness manager writes and
  reads events through `server.Server`'s `PushEvent`, `GetTurnState`,
  `ListEvents` and `SessionsHoldingHarnessSessionID`, the same methods the HTTP
  client in `client/` offers; the bridge's session routes (`/sessions/{id}/messages`,
  `/messages/raw`, `/entries/{eventId}`, `/entries/{eventId}/images/{index}`,
  `/sessions/search`, `/sessions/aggregates`, `/sessions/validators`,
  `/sessions/recent-bundle`) answer from its handler in process.
- **Its own routes** are mounted at `/session-log/` with the same paths behind the
  prefix (`/session-log/api/v1/events`, `/session-log/health`, …), operator only:
  every session's events, with no owner on a row, and an ingest route that
  writes into any session.
- **Import:** `llm-bridge-server import-session-log -sqlite <copy of events.db>`
  copies every table of a standalone log-store's SQLite file, with every event
  id, body and time, by COPY in one transaction. It refuses a module that holds
  any row, and a file whose projection backfills have not finished.
- **Logstack:** result statistics go to logstack at `session_log.logstack_url`
  (`LLMBRIDGE_SESSION_LOG_LOGSTACK_URL`); empty forwards nothing.

The standalone service's command, unit, deploy script and settings
(`LOG_STORE_LISTEN_ADDR`, `LOG_STORE_DB_PATH`, `LOG_STORE_LOGSTACK_URL`, and its
`GET /settings`) are gone from this copy; the bridge's own settings describe it.

## API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/events` | Ingest a `msg.Event` (needs `bridge_session_id`). Returns `{"id": <rowID>}` (201) |
| `GET` | `/api/v1/sessions/{id}/messages?limit=&before=&payload=` | The reading page: `{model}` with the newest `limit` prompt turns, or those older than the turn holding event `before`. Duplicates dropped, no `raw`. `payload=preview` shortens tool strings to 2 KB. **`limit` or `before` is required** (400 otherwise) |
| `GET` | `/api/v1/sessions/{id}/messages/raw?limit=&before=` | Same turns, unprojected: every event as an entry with its `raw` source, duplicates annotated. At most 5,000 events: older turns are left out first |
| `GET` | `/api/v1/sessions/{id}/entries/{eventId}` | One entry of the reading page with full tool payloads; 404 if the event is not one |
| `GET` | `/api/v1/sessions/{id}/entries/{eventId}/images/{index}` | The bytes of one image a tool result carries, at its position in `tool_result.content` (an entry's `toolResultImages` lists them). PNG, JPEG, GIF and WebP are served as themselves, anything else as a download; always `nosniff` and a sandbox CSP |
| `GET` | `/api/v1/sessions/bundle?ids=&turns=&payload=` | Reading pages for several sessions |
| `GET` | `/api/v1/sessions/validators?ids=` | `{maxEventId, eventCount, updatedAt}` per session, from the turn index |
| `GET` | `/api/v1/sessions/{id}/events?after=N&types=` | Raw events with row id > N, `event_id` spliced in |
| `GET` | `/api/v1/sessions/{id}/turn-state` | Whether a turn is in flight |
| `GET` | `/api/v1/sessions/search?q=&since=&until=&limit=` | Sessions whose events contain `q`, case ignored (`ILIKE`, `%` and `_` as wildcards, as SQLite's `LIKE`); `since`/`until` (RFC 3339) bound it to a window of events |
| `GET` | `/api/v1/sessions/aggregates?since=` | Per-session turn, duration and token totals from two records: the result events' input, output, cache read and cache write tokens (the main conversation's finished turns), and `api_call_usage`, the `api_call` events summed per `query_source` (every model call the harness reported, a subagent's included). With no `since`: every session's whole-life totals, and no cost — a session's whole-life cost is llm-bridge-server's `spend_usd`. With `since` (an RFC 3339 time): only the sessions that used anything from the clock hour holding that time on, with what each did from then on and `session_cost_usd`, how much its `session_cost` total grew in the window; 400 for a `since` that does not parse |
| `GET` | `/api/v1/sessions/by-harness-id?harness_session_id=` | Sessions holding a harness session id |
| `GET` | `/health` | `{"status": "ok"}` plus forwarder state |

### Stored turns

Pages are not rebuilt from events on each read. `store/turn_index.go` places
every event in its turn when it is stored; `server/stored_turns.go` builds a
turn once with the builder in `turnmodel.go`, stores the projected result, and rebuilds
it only when the turn gains an event, a dual-emitted copy changes what it pairs with
(`dedup.go`), or `materializerVersion` changes. Everything but `events` is derived and
can be dropped and rebuilt; sessions written before the index are indexed in the
background at startup, newest first.

### Per-session token totals

`sessions` holds each session's result-event sums and `session_api_call_usage` its
`api_call` events summed per query source; `StoreEvent` adds each new event to both
(`store/token_usage.go`). `session_usage_by_hour` and `session_api_call_usage_by_hour`
hold the same counts per session and clock hour (`store/usage_by_hour.go`), plus the
highest `session_cost` total seen in the hour, and `?since=` sums them. An event
belongs to the hour of its `events.created_at`.

In SQLite the cache columns and these tables were added after millions of events were
stored, and two background passes added the older events (`projection_backfills`). In
Postgres they exist from the module's first migration, so there is no such pass; the
import copies them from a file whose passes finished. A sessions row whose seed failed
is rebuilt from its events by `RebuildMissingSessionRows`, which the bridge runs at
start beside its work.

## Storage

`events` is the record: one row per event, its body in a `json` column, which keeps
the text exactly as it was sent and checks only that it is JSON (an ingest that is not
UTF-8 is refused with 400, since Postgres keeps no other text). It is indexed by
`(session_id, id)` and `(session_id, type, id)`. Session ids and query sources compare
by bytes (`COLLATE "C"`), as in SQLite. The other tables are derived from it — see
[Stored turns](#stored-turns).

## Client library

The `client` package is the HTTP client of the standalone service's routes and the
types the bridge's in-process calls share with it (`TurnState`, `HeldSession`,
`EventRejectedError`). The bridge's tests use it against fake log-stores.

## The only copy of session events

This module is the only copy of session events: the harness manager writes each event
once, synchronously and in order, and the id that write answers is the SSE event id
(`internal/sessionlog`, `SingleCopy`). Until the bridge's own records
moved to Postgres (the `bridge_core` module, `internal/store`) every event was also
kept in SQLite, in `bridge.db`'s `events` table, behind the setting
`session_log.single_copy`; that table, that path and the setting are gone, and the
table is archived history in the old file.

**The readers** (`TestSingleCopyReadersAnswerWhatTheConversationHolds`): the stream's
replay after `Last-Event-ID` and on connect, the stream head (`/messages` and the
recent bundle), the message id maps a respawn loads, the renamer's turn count and turn
texts, the brief pass's current-turn text and first message, last activity (startup
resume, the reaper, the external activity watcher), the reaper's and wakeup
redelivery's ScheduleWakeup lookup, wakeup redelivery's "did it fire", restart
readiness's background tasks, crash recovery's turn state, git discovery's tool
inputs, and the startup reconcile.

**One write per event, in two round trips, and one commit for every session's
waiting events.** `AppendEvent` reads the event's id, its session's turn index state
and any row its idempotency key names, then sends the insert, its turn and its totals
as one pgx batch. The writer is one goroutine on one connection, so ids commit in
order, and it writes every event waiting when it starts a write together, at most one
per session and 64 in all (`group_commit.go`): `BEGIN` and their first reads in one
round trip, their writes and `COMMIT` in the second, so with default durability the
sessions share one flush to disk instead of each waiting behind the others'. Events of
one session keep their order, each written after the one before committed. A group
that fails is rolled back and its events written one by one. The message ids, tool use
id and tool name are kept in columns beside the body, filled by the writer and by the
import.

**The rows written with their events.** Two events describe a row of the
`bridge_core` module, and each is written in the event's transaction, on this
module's writer connection, so the two commit together or not at all:

- a session status and its session row: `RequiredRow` locks the row
  (`SELECT … FOR NO KEY UPDATE`) in the same round trip as the event's first read,
  inside an explicit transaction whose `BEGIN` rides with that read and whose
  `COMMIT` rides with the batch, so it costs no round trip more; a session with no
  row gets no event (`ErrRequiredRowMissing`, `sql.ErrNoRows` to the caller), and the
  row write in the batch (`AlsoWrite`) carries the event's id as the status's `as_of`;
- a runner's numbered entry and its spawn's stored-through number, in the batch.

The statements come from the core module (`store.SessionRowLockForStatus`,
`store.SessionStatusRowWrite`, `store.RunnerSpawnAdvance`), which owns the tables,
and this module's role may run them and nothing more: at every start the core module
grants `session_log_module` `USAGE` on its schema, `SELECT (bridge_id, status)` and
`UPDATE (state, status, updated_at)` on `sessions`, and `SELECT (session_id,
spawn_id, last_sequence)` and `UPDATE (last_sequence)` on `runner_spawns`
(`store.GrantSessionLogWrites`). Both modules must be in one Postgres database.

**Retries.** A write is retried with backoff for up to 30 s while Postgres restarts.
Each status and runner entry still carries an idempotency key (a status's drawn per
write, a runner entry's `runner_entry:<spawn>:<sequence>`), so a retry after a commit
that was cut off finds the stored event, whose row committed with it, instead of
storing a second one. A write Postgres refused (a constraint, a permission) is not
retried.

**Numbering.** `import-bridge-database` records the highest id the archived `events`
table handed out (`archived_event_numbering` in `bridge_core`), and every start moves
this module's numbering above it once and records where it starts
(`single_copy_numbering`), so no `Last-Event-ID` or status `as_of` a client holds
names an event here; a `Last-Event-ID` below that replays the current turn, as a new
connection does.

**Durability.** `session_log.asynchronous_commit` (off by default) opens the writer
pool with `synchronous_commit` off: a crash of the server then loses nothing, and a
crash of Postgres or the machine loses up to three times `wal_writer_delay` (600 ms by
default) of events the stream may already have sent.

### Landing day

1. Stop the server, copy `bridge.db` (`sqlite3 bridge.db ".backup <archive>"`, or a
   copy without its `events` table that keeps `sqlite_sequence`), and keep the archive:
   it is the only copy of the events stored before.
2. Import log-store into this module (`llm-bridge-server import-session-log`) and the
   copy into `bridge_core` (`llm-bridge-server import-bridge-database -sqlite <copy>`),
   before the first start. The server refuses to start on an empty `bridge_core`
   while a file is at `database.path`.
3. Start. Never drop or rewrite the archived `events` table without the operator's
   word; `cmd/event-copy-parity` and `cmd/backfill-session-cost` still read the file,
   as tools.
