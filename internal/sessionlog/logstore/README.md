# log-store

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
- **Its own routes** are mounted at `/log-store/` with the same paths behind the
  prefix (`/log-store/api/v1/events`, `/log-store/health`, …), operator only:
  every session's events, with no owner on a row, and an ingest route that
  writes into any session.
- **Import:** `llm-bridge-server import-log-store -sqlite <copy of events.db>`
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
