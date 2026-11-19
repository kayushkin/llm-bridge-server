# chat-core wire contract

Authoritative shape of the traffic between the Go backend (llm-bridge-server `:8160` +
log-store `:8175`) and this client. The TypeScript source of truth is `src/net/types.ts`;
the Go structs must serialize to exactly these JSON shapes. Keep both in lockstep.

## Conventions
- Timestamps: RFC3339 **with offset** (e.g. `2026-07-27T14:03:11-07:00`). Never naive.
- JSON field names: camelCase on the wire (Go structs use `json:"sessionId"` tags).
- All responses are gzip/br-compressed by nginx (SSE excluded). Handlers write plain JSON.
- Nothing is lossy. Dedup is expressed as annotation on `Entry` (`duplicate`, `primary`,
  `groupId`), never by omitting an event. The raw Timeline view must be able to reconstruct
  every stored event from the payload.
- `Entry.origin` (`live` | `page`) is **not on the wire.** The client stamps it — the fold and
  the optimistic send write `live`, merging a server page writes `page` — and it is stored in
  the IndexedDB cache with the entry. A server must not send it.

## Endpoints (all additive; existing endpoints unchanged)

### `GET /sessions/summary?limit=100&before=<cursor>`
Projected sidebar list, newest first, paginated. Omits `info` / `harness_config`. Each row
carries `spendUsd`, the session's spend so far in US dollars (`SessionSummary.spendUsd`); the
list stream's upsert carries the same as `spend_usd`. Each row also carries `workingDir`, the
directory the session was started in (`''` when none was recorded); the upsert carries it as
`working_dir`.
→ `SummaryResponse` `{ sessions: SessionSummary[], next: string|null, revision: string }`.
Also sets `ETag: <revision>`; honors `If-None-Match` → `304`.
`revision` = max(updatedAt) across the table (or a monotonic change counter).

### `POST /sessions/summary`
The same query as the GET, re-encoded as a JSON body, and the REQUIRED encoding for the
two id lookups (`session_ids`, `manager_session_ids`). Their lists are unbounded — one id
per loaded session — and the query-string encoding reached 93 KB on a real sidebar, which
nginx answers not with a refusal but by destroying the whole HTTP/2 connection (GOAWAY at
~11.5 KB of URL, measured), killing every other stream in flight. `ApiClient.getSummary`
switches encodings on the presence of either lookup; a caller never chooses.
Body: `{ limit?, before?, harnesses?, statuses?, types?, purposes?, modes?, machines?,
session_ids?, manager_session_ids? }` — list fields plural (the GET spells them as one
repeated singular parameter). Same `SummaryResponse`, same server-side response cache;
present-but-empty id lists are a 400 exactly as on the GET, and unknown fields are a 400
(strict decoder). No conditional-GET ride on this encoding — the one thing given up.

### `GET /sessions/recent-bundle?n=20&turns=30`
The N most-recent sessions, each with summary + last `turns` turns materialized, in ONE
response. → `RecentBundleResponse` `{ [sessionId]: { summary, model } }`.
Server-assembled and response-cached (keyed by the max updatedAt across the N), invalidated
by the store mutation Notifier.

### `POST /sessions/validators` — body `{ ids: string[] }`
→ `ValidatorsResponse` `{ [sessionId]: { maxEventId, eventCount, updatedAt } }`.
The cheap staleness check: a cached model is fresh iff its validator matches.
POST body for the same reason as `POST /sessions/summary`: the id list is one entry per
cached session, and a query string that grows with the cache walks toward nginx's
HTTP/2 connection-kill cliff (~11.5 KB of URL). `GET /sessions/validators?ids=a,b,c`
still exists and answers identically; the client no longer sends it. Absent/empty ids
→ `{}` on both encodings (never a 400 — an empty check has an obviously right answer).

### `GET /sessions/{id}/messages?limit=30&before=<eventId>`
Materialized tail (or a page older than `before`). → `MessagesResponse { model }`.
`model.more=true` if older turns remain. Never unbounded — default returns the last `limit`
turns only.

### `GET /sessions/search?q=<text>`
Full-text content search across session transcripts. → `SearchResponse`
`{ sessionIds: string[], hits?: { sessionId, snippet? }[] }` — the ids whose materialized
transcript text matched `q`. The client folds `sessionIds` into the search-filter path
(C6) so a query matches transcript content, not just the display name. This is an ASYNC
AUGMENTATION of the instant local name filter — the client never blocks name-matching on
it, and a response for a superseded query is dropped (the hit set is pinned to its query).

### `POST /sessions/{id}/files?filename=<name>` — body the bytes, `Content-Type` their type
Shares a file into the session; answers the `SessionFile`, and the stream gets a
`session_file` event for it. `GET /sessions/{id}/files/{file_id}/content[?inline=true]` reads
it back with file-store's headers. LOUD: a refusal (413 too large, 400 a name that is not a
plain name, 503 no file-store) throws.

### `POST /sessions/{id}/actions/{action_id}/run`
Confirms a session action the agent offered: llm-bridge-server runs it and answers the
`SessionAction` as `running`; the run's end arrives as a `session_action` event. LOUD: 409
when it already ran or when what it would run changed since it was offered. `GET
/sessions/{id}/actions` lists every action's newest record. The agent offers one with
`POST /sessions/{id}/actions` (its `bridge-button` command); the client never does.

### `POST /sessions/{id}/interrupt`
Interrupt/stop the running turn. The client treats this as a LOUD call: any non-2xx throws
and MUST surface (never a swallowed fake-idle). In particular the server returns **409**
("nothing was stopped") while a tool still holds the turn — until the server-side gate fix
ships — and the client must show that, not optimistically mark the session idle.

### `GET /sessions/{id}`
The full per-session detail — the canonical `msg.ManagedSession` (snake_case). Unlike the
summary list it carries the heavy `info` blob. The client (`ApiClient.getSessionDetail`)
projects the summary fields via `summaryFromManaged` and maps the snake_case `info` →
camelCase `SessionInfo`; `info` is `null` when the harness has reported none yet. Backs
`useSessionInfo` (lazy, cached per id). The wire `info` is `msg.SessionInfo`:

| wire (snake_case, `msg.SessionInfo`) | client (`SessionInfo`, camelCase) |
| --- | --- |
| `system_prompt` | `systemPrompt?` |
| `append_system_prompt` | `appendSystemPrompt?` |
| `working_dir` | `workingDir?` |
| `model` | `model?` |
| `permission_mode` | `permissionMode?` |
| `tools[]` `{name, description?}` (`msg.ToolInfo`) | `tools?: { name; description? }[]` (`ToolInfo`) |
| `slash_commands[]` | `slashCommands?` |
| `agents[]` | `agents?` |
| `skills[]` | `skills?` |
| `mcp_servers[]` `{name, status?}` (`msg.MCPServerInfo`) | `mcpServers?: { name; status? }[]` (`McpServerInfo`) |

Every info field is copied explicitly (never spread) so a wire rename fails the type-check at
the mapping site instead of leaking a snake_case key through.

The detail also carries the session's `harness_config` bag, which the summary list omits.
On the Go side `ManagedSession.HarnessConfig` is an **opaque `json.RawMessage`** — a
per-harness config map — so `getSessionDetail` surfaces the bridge's own well-known keys in
camelCase (`harnessConfigFromWire`) and passes every OTHER key through unchanged (the index
signature on `HarnessConfig`), keeping the layer lossless. `harnessConfig` is `null` when the
wire omits `harness_config`. The well-known keys are the ones the bridge itself reads/writes
(llm-bridge-server `permission_mode.go`, `hooks_resolve.go`):

| wire (snake_case, `harness_config`) | client (`HarnessConfig`, camelCase) |
| --- | --- |
| `permission_mode` | `permissionMode?` |
| `disable_network` | `disableNetwork?` |
| `permission_mode_custom` `{approval?, sandbox?}` | `permissionModeCustom?: { approval?; sandbox? }` (`HarnessConfigCustom`) |
| `model` | `model?` |
| `effort` | `effort?` |
| *(any other key)* | *(carried through unchanged — opaque bag)* |

`ManagedSessionDetail` is `{ sessionId, summary, info, harnessConfig }` — `sessionId` is
surfaced at the top level (it also lives on `summary`). `useManagedSession` reads this; the
interactive permission-mode selector reads `harnessConfig.permissionMode`.

### `PUT /sessions/{id}/permission-mode`
Set a session's per-session permission mode. Body `{ "mode": "<PermissionMode>" }` where
mode ∈ ask | auto | bypass | plan | read | ask_all | block_all | custom (validated
server-side → 400 on an invalid value; 404 on an unknown session). The bridge persists it
into `harness_config.permission_mode`; the prehook reads it **live**, so the change takes
effect on the session's NEXT tool call without a restart. `ApiClient.setPermissionMode` treats
this as a LOUD call (throws on any non-2xx) so `useManagedSession`'s optimistic cache update
can revert. (The server also accepts optional `disable_network` and `permission_mode_custom`
fields on this PUT; the client currently sends only `mode` per the required signature.)

## Settings / controls bar (the chat page's `bc-controls-bar`)

These back the settings-bar controls. Every mutation is a LOUD `ApiClient` call (throws on
any non-2xx). Endpoints + bodies are the exact canonical ones bridge-ui uses — verified
against `bridge-ui/src/useBridgeSession.ts` (line refs below) and the Go handlers in
`llm-bridge-server/internal/server/{sessions.go,mode_switch.go,models.go,health.go}`.

### `POST /sessions` and `POST /sessions/{id}/fork`
Both return the canonical **snake_case `msg.ManagedSession`** — its id field is `session_id`
(there is no `sessionId` on this shape). `ApiClient.createSession` / `fork` therefore read
`session_id` from the canonical key and surface it camelCase as `CreatedSession.sessionId`.
Fork body: `{ "display_name": "", "type": "interactive" }` (empty name → server derives
"<parent> (fork)"). Create is unchanged (`type`/`purpose`/`origin` + optional
`instance_id`/`harness`). (useBridgeSession.ts:908 create, :1143 fork.)

### `POST /sessions/{id}/compact`
Compact the context. Body `{ "summary": "..." }` when a caller supplies one, else `{}`. The
POST only ACKs (`compact_ack` system event); the real completion is the `compact_boundary`
system event on the session stream — `useSessionControls` clears its `compacting` flag on
that entry (or a 180s safety timeout), never on the POST resolving. (useBridgeSession.ts:1129.)

### `POST /sessions/{id}/mode`
Switch a live session between I/O modes. Body `{ "mode": "events" | "pty" }`. The server
kills + respawns the harness via `--resume` (history preserved) and, for a pty switch,
returns an `attach_token` sibling. chat-core passes the response through unchanged (pty
attach-token management is a bridge-ui concern). Gated in the UI on `HarnessMeta.pty`.
(useBridgeSession.ts:973; handler `mode_switch.go`.)

### `POST /sessions/{id}/config`
Apply per-session runtime knobs. Body `{ model?, effort?, max_budget?, disabled_tools? }`
(`msg.ConfigSessionRequest`; only the provided fields are sent — never a null for the rest).
This is the canonical path for BOTH the controls-bar model/effort **pre-start** settings
(applied right after create — bridge-ui's create call carries no model/effort) and changing
them on a **live** session. `ApiClient.setConfig(id, SessionConfig)` maps the camelCase
`SessionConfig` (`maxBudget`/`disabledTools`) to the snake_case wire body. (useBridgeSession.ts:1176.)

### `GET /harnesses`
The registered harness types + capabilities — the canonical registry the controls bar gates
on. Returns `msg.HarnessInfo[]` (snake_case; nil slice → JSON `null`).
`ApiClient.getHarnesses` maps each to the camelCase `HarnessMeta`:

| wire (snake_case, `msg.HarnessInfo`) | client (`HarnessMeta`, camelCase) |
| --- | --- |
| `name` / `label` / `emoji` / `tint` / `available` | same |
| `capabilities` (`model`,`effort`,`compact`,`fork`,`system_prompt`,`tools`) | `capabilities` |
| `hook_events` | `hookEvents` |
| `supported_providers` | `supportedProviders` (scopes the model picker) |
| `supported_permission_modes` | `supportedPermissionModes` |
| `pty` | `pty` (gates the events/pty toggle) |
| `supports_disable_network` | `supportsDisableNetwork` |

`useHarnessCapabilities(harnessId) → Set<string>` reads this — never a hardcoded per-harness
allowlist. (bridge-ui reads the same endpoint: `BridgeChat.tsx:304`; handler `health.go:249`.)

### `GET /models`
The model-store registry. Returns model rows (snake_case: `id`, `provider`, `name`,
`max_tokens`, `input_cost`, `output_cost`, `enabled`; the handler also embeds credential
status, ignored here; nil slice → JSON `null`). `ApiClient.getModels` drops `enabled=false`
rows and projects each to a `ModelOption { value: id, label: "<name> ($in/$out)", provider }`
(label falls back to just the name/id when cost is unreported — never a fabricated cost).
`useModels(harnessId?)` filters these to the harness's `supportedProviders` exactly as
bridge-ui's `harnessModels` does; no harness (or one declaring no providers) → all enabled
models. (bridge-ui: `BridgeChat.tsx:260`; handler `models.go`.)

## Cost / context: `Entry.usage` + `TurnModel.aggregates` (Phase 2)
These ride on the materialized model from `GET /sessions/{id}/messages`, populated by
log-store's spend/context materializer. Unlike `SessionInfo` they are **camelCase already on
the wire** — no client mapping — so the TS shapes below ARE the wire shapes. Both MAY be
absent; absence reads as zero, never a fabricated figure.

- `Entry.usage?` — per-entry token usage, mirrored 1:1 from the source event's
  `msg.TokenUsage`. Absent on events with no usage (user_message, most system entries). Read
  by the UI directly off the entry; there is no per-entry hook.
  ```
  usage?: { inputTokens?; outputTokens?; cacheReadTokens?; cacheWriteTokens? }
  ```
  Provenance: `msg.TokenUsage` (`InputTokens` / `OutputTokens` / `CacheReadTokens` /
  `CacheWriteTokens`). On the materialized model the field is emitted camelCase per the
  convention above — no client mapping.
- `TurnModel.aggregates?` — the loaded page's cost/context roll-up. **Undefined when the
  spend/context events fall outside the loaded page.** `useSessionCost` and `useContextUsage`
  read from here and return zeros when it (or a member) is absent.
  ```
  aggregates?: {
    totalUsd?;                          // APISpendTotalEvent.TotalUSD
    byModel?: Record<string, number>;   // APISpendTotalEvent.ByModel
    byQuerySource?: Record<string, number>; // APISpendTotalEvent.ByQuerySource
    contextTokens?; contextLimit?;      // context-window figures; pct = tokens/limit*100
  }
  ```
  Emitted camelCase on the materialized wire (no client mapping); the `msg` names are
  provenance for the canonical source of each figure.

## The `Entry` / dedup model (non-destructive)
Every stored event → exactly one `Entry`. Materialization on the server (log-store
`materialize.go`) groups entries into turns and sets the annotations:
- `duplicate`: hidden in the collapsed Turns view; ALWAYS shown in the raw Timeline view.
- `primary`: the single copy shown for its `groupId` in the collapsed view.
- `groupId`: entries with identical logical content across sources share this (e.g. the
  harness assistant text and its ~1s-late OTel copy). The per-message "sources" badge counts
  a group's members and lists their `source`.

Dedup is **source+count**, never positional (the OTel copy can arrive after the reply). The
client keeps this logic for the LIVE TAIL only (`reduce/otelDedup.ts`); the server owns it
for settled history. A shared fixture (including a 1s-late dual-emit case) pins both to the
same output.

### OTel / error / system `Entry` fields (mapped from the canonical wire)
These optional fields ride on `Entry` (camelCase), mapped 1:1 from the source event —
never invented. log-store's `Entry` struct and the live-tail reducer populate the same set.
- `recovered?: boolean` — an assistant text block surfaced from the OTel copy after the live
  stream produced nothing that turn (`extensions.recovered`; handler.go
  `flushRecoveredAssistant`). It is NOT a duplicate — there is no live copy to collapse it
  against, so it renders as the assistant message. The flag is a presentation marker only and
  never gates visibility. On the settled path such a block is kept **visible/primary** when no
  Result or harness assistant text supersedes it in the same turn.
- `code?: string`, `retryable?: boolean`, `statusCode?: number` — the canonical
  `msg.ErrorEvent{Code,Message,Retryable,StatusCode}` fields on a kind `'error'` entry (wire:
  `error.code` / `error.retryable` / `error.status_code`). `code` values `TURN_IDLE_TIMEOUT` and
  `PROCESS_DIED` are **turn terminators** (transition the session out of running/holding);
  `api_error` / `api_retries_exhausted` are **informational** chips and must NOT clear the
  running state. Unknown codes render generically.
- `subtype?: string` — the `SystemEvent.subtype` on a kind `'system'` entry (e.g.
  `subagent_completed`, `compact_boundary`). An unknown subtype renders generically, never as
  an error.
- `taskId?`, `toolUseId?`, `taskStatus?`, `taskSummary?`, `taskOutputFile?`, `taskType?`,
  `subagentType?`, `lastToolName?` — the `msg.SystemEvent` subagent fields on a `task_*` entry.
  `taskStatus` reaching a terminal value is the ONLY thing that ever says a subagent finished;
  it emits no result event of its own. An unrecognized status is NOT terminal, so a status a
  harness adds later cannot close a task that is still running.
- `subagentSessionId?: string` — the bridge session id of the subagent a `task_*` entry
  describes, and the id a client follows to read what it did. **Server-owned**: llm-bridge-server
  mints the session and stamps this, because it is the only party that knows the id — a harness
  knows only its own task id. Empty is a real answer, not a gap: a backgrounded shell task
  (`taskType` `local_bash`) never gets a session.
- `harnessParentId?: string` — set when the entry is a **subagent's own work rather than this
  session's** (`Event.harness_parent_id`: the tool_use_id of the Task call that spawned it).
  The server routes a subagent's frames into the subagent's own session, so a session's model
  should almost never hold one; the exception is the fail-safe that keeps a frame whose
  `task_started` was missed on the parent rather than dropping it. A view of what THIS session
  did must leave those out.
- `toolResultImages?: ToolResultImage[]` — the images a tool result carried
  (`ToolResultEvent.Content`): a screenshot, an image file the agent read. log-store lists them
  by `index` and `mediaType` only, never their bytes; the client stamps each with the tool
  result's own `eventId`, and `ApiClient.toolResultImageUrl` turns that into
  `GET /sessions/{id}/entries/{eventId}/images/{index}`. A live-folded entry carries
  `base64Data` instead and needs no URL. An image held by URL is not drawn.
- `sessionFile?: SessionFile` — kind `'file'`: a file shared into the session
  (`session_file` event, `msg.SessionFile`), role `user` or `assistant` by `shared_by`.
  `ApiClient.shareSessionFile` shares one, `ApiClient.sessionFileContentUrl` reads it back.
- `sessionAction?: SessionAction` — kind `'action'`: a session action (`session_action`
  event, `msg.SessionAction`) as that event recorded it. Every change is its own event
  carrying the whole record: the entry whose record is `offered` (role `assistant`) is where
  the button goes, and each later one (role `system`) is a step of its one run.
  `newestSessionActions(entries)` gives each action's current record;
  `ApiClient.runSessionAction` confirms one. The agent places a button by writing its id
  (`session_action_000007`) in its reply: `parseRefChips` gives it kind `'action'`, and
  `sessionActionIdsPlacedInProse(entries)` names the ids placed that way.
  A `run_command` carries the reviewer's `review` (approve, caution, reject — a reject cannot
  be run); `model_call` and `background_agent` carry the agent's `maximum_cost_usd`, and a
  `model_call` its spend in `cost_usd`; a `background_agent` names its session in
  `result_session_id` while it still runs. `output` is markdown for `model_call` and
  `background_agent`, and for a `run_command` whose `result_format` is `markdown`.

### Session status (`SessionStatus`) — decided by the server, ordered by `as_of`
What a session is doing right now is `msg.SessionStatus`, **passed through in its own
snake_case** everywhere it appears: `state`, `generating` (`thinking`|`text`), `tools[]`
(`tool_id`, `name`, `summary`, `started_at`), `subagents[]` (`task_id`, `task_type`,
`subagent_type`, `description`, `last_tool_name`, `started_at`, `session_id`), `rate_limit`,
`turn_started_at`, `changed_at`, and **`as_of`** — the llm-bridge-server event row id the
status is current as of.

It arrives four ways — a `session_status` event on the session's own stream, `session.status`
on a list-stream upsert, `status` on a `/sessions/summary` row, and the IndexedDB cache — in no
guaranteed order. **The larger `as_of` wins** (`store/sessionStatus.ts`); arrival order decides
nothing. `SessionSummary.state` is always `status.state`. A row with no status (cached before
2026-09-17) reads as `{state, as_of: 0}`.

The client derives no status from the transcript. The activity fold, the transcript-derived
activity, the last-turn tool/subagent scan and the F1 terminal-state reconcile
(`effectiveState`, `terminalStateFromTail`) were all deleted on 2026-09-17 in its favor. The
one transcript read left beside a status line is the harness's in-progress todo
(`store/inProgressTodo.ts`), which is conversation, not status.

## SSE
- `GET /session-events` — one global stream of list deltas (`hello`/`upsert`/`delete`/`signal`).
  The client sends `Last-Event-ID` on a reconnect. The `hello` answers with `resume`:
  `replayed` (the missed frames follow), `gap` (more were missed than the server's 512-frame
  buffer holds) or `none`. On anything but `replayed` after a reconnect, the client re-reads
  every row it holds (`SyncEngine.refreshHeldSessions`) — a lost upsert is otherwise a session
  whose status stays wrong until its next change.
- `GET /sessions/{id}/events` — per-session stream for the ACTIVE session only; resumes via
  `Last-Event-ID`. Warm-but-inactive sessions are refreshed via validator sweeps, not live
  streams. Their STATUS needs neither: it rides the list stream.
