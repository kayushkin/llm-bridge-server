// Wire types — the shared contract between llm-bridge-server / log-store (Go) and this
// client. Keep this file and docs/WIRE.md in lockstep with the Go structs. Timestamps are
// RFC3339 strings WITH offset (never naive). Nothing here is lossy: every event/source that
// exists in the store is representable and transmitted; dedup is expressed as ANNOTATION
// (`duplicate` / `primary` / `groupId`), never omission.

/** Projected session-list row for the sidebar. Deliberately omits the heavy `info` /
 *  `harness_config` blobs — those are fetched lazily per session when actually needed. */
export interface SessionSummary {
  sessionId: string;
  state: string;
  harness: string;
  instanceId: string;
  type: string;
  purpose: string;
  mode: string;
  folderName: string;
  displayName: string;
  agentId: string;
  /** Who the session was started as: a principal-store id (`principal_000001`),
   *  or '' for a session with no principal. Set at creation; a UI resolves the
   *  name live from principal-store rather than carrying a copy. */
  principalId: string;
  updatedAt: string; // RFC3339 + offset
  createdAt: string; // RFC3339 + offset
  /** The harness's OWN session id (rotates on resume/fork). For a promoted subagent
   *  session this is `agent-<task_id>` — the server-chosen key that ties the row back
   *  to the parent's `task_started` narration, and the join the live-status surface
   *  uses to link a running subagent line to its session. Empty until the harness
   *  reports one. */
  harnessSessionId: string;
  /** The managing session in the team tree (bridge session id); empty = top-level.
   *  A promoted subagent session carries its parent here. */
  managerSessionId: string;
  /** What the session has spent so far, in US dollars (llm-bridge-server's
   *  `spendUsd` on a summary row, `spend_usd` on a list-stream upsert). Absent
   *  when the row came from somewhere that did not carry it — a row cached
   *  before the field was kept — and absent is unknown, not zero. */
  spendUsd?: number;
  /** The directory the session was started in, llm-bridge-server's `workingDir` on a
   *  summary row and `working_dir` on a list-stream upsert; '' when none was recorded.
   *  Absent when the row came from somewhere that did not carry it (a row cached before
   *  2026-10-31), and absent is unknown, not "no directory". */
  workingDir?: string;
  /** Everything the session is doing right now, as llm-bridge-server decided it,
   *  with the event row id it is current as of. `state` above is `status.state` —
   *  the store keeps the two equal (`store/sessionStatus.ts`). Optional on the type
   *  only because a row cached before 2026-09-17 has none; the store fills it in on
   *  the way in, so a row READ from the store always has one. */
  status?: SessionStatus;
}

/**
 * A session's whole status — llm-bridge `msg.SessionStatus`, field for field and in
 * its own snake_case.
 *
 * ⚠️ Deliberately NOT re-keyed to camelCase like the types around it. The same JSON
 * arrives four ways — on a `session_status` event, on a session-list upsert, on a
 * `/sessions/summary` row and out of the IndexedDB cache — and a converter would have
 * to sit on all four. One shape with no converter is one thing that cannot drift.
 *
 * This replaced three client-side derivations (the live activity fold, the
 * transcript-derived activity, and the tool/subagent scan of the last turn) that
 * each rebuilt part of this from a transcript copy, and could not say which of two
 * disagreeing facts was newer. `as_of` can: see `newerSessionStatus`.
 */
export interface SessionStatus {
  state: string;
  /** `thinking` or `text` while `state` is `model_generating`; absent otherwise. */
  generating?: 'thinking' | 'text';
  /** Tool calls issued and not yet answered, oldest first. */
  tools?: SessionStatusTool[];
  /** Harness tasks (subagents, backgrounded shells) still running, oldest first. */
  subagents?: SessionStatusSubagent[];
  /** The provider's last rate-limit verdict when it was not `allowed`. */
  rate_limit?: { status: string; limit_type?: string; resets_at?: number };
  /** RFC3339. Absent between turns. */
  turn_started_at?: string;
  /** RFC3339. When `state` last changed. */
  changed_at?: string;
  /** The llm-bridge-server event row id this status is current as of. Every status
   *  the server emits has its own, so of two statuses for one session the larger
   *  `as_of` is the newer. 0 = synthesized from a bare `state` (an old cached row). */
  as_of: number;
}

export interface SessionStatusTool {
  tool_id: string;
  name: string;
  /** One human line for the call's input (`cat thing.txt`), written by the server. */
  summary?: string;
  started_at: string;
}

export interface SessionStatusSubagent {
  task_id: string;
  /** `local_agent`, `local_bash`, … — a backgrounded shell is not an agent. */
  task_type?: string;
  subagent_type?: string;
  description?: string;
  last_tool_name?: string;
  started_at: string;
  /** The task's own bridge session, when the server promoted one. */
  session_id?: string;
}

/** Cheap staleness currency. A cached TurnModel is fresh iff its validator equals the
 *  server's for that session. Comparing these avoids shipping any messages. */
export interface Validator {
  maxEventId: number;
  eventCount: number;
  updatedAt: string; // RFC3339 + offset
}

export type EntrySource = 'harness' | 'otel';

export type EntryKind =
  | 'text' // assistant/user prose
  | 'thinking'
  | 'tool_call'
  | 'tool_result'
  | 'system'
  | 'result'
  | 'error'
  | 'file' // a file shared into the session, by the user or by its agent
  | 'action' // a button the agent offered, or a later step of its one run
  | 'meta'; // catch-all for anything not specially rendered

export type Role = 'user' | 'assistant' | 'system' | 'tool';

/**
 * One renderable atom, mapped 1:1 to a stored event. This is the unit of the
 * non-destructive dedup model:
 *  - EVERY event becomes exactly one Entry — nothing is dropped.
 *  - `duplicate=true` means "hidden from the collapsed Turns view", NOT deleted. The raw
 *    Timeline view renders all entries regardless.
 *  - Entries that represent the same logical content across sources (e.g. a harness
 *    assistant message and its ~1s-late OTel copy) share a `groupId`; exactly one of them
 *    has `primary=true`. The sources badge counts a group's members.
 */
/** Per-entry token usage, mirrored 1:1 from the source event (camelCase on the wire —
 *  log-store populates these directly, no client mapping). MAY be absent when the event
 *  carried no usage (e.g. a user_message or a system entry). Never invented. */
export interface EntryUsage {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheWriteTokens?: number;
}

/** One image of a tool result. `index` is its position in `ToolResultEvent.Content`.
 *
 *  Its bytes are in one of two places, and exactly one of these is set:
 *   - `base64Data` on a LIVE-folded entry, which holds the whole event;
 *   - `eventId` on a PAGE entry: log-store's row id of the tool_result event, which
 *     `ApiClient.toolResultImageUrl` turns into the image route. A page lists images
 *     without their bytes, because a screenshot is ~100 KB of base64 and the page is
 *     what `raw` was dropped from to keep small. It is stamped here rather than read
 *     off the entry because a call and its result merge into one row, and the row's
 *     own `eventId` is then no longer the result's. */
export interface ToolResultImage {
  index: number;
  mediaType: string;
  base64Data?: string;
  eventId?: number;
}

/** Who shared a file into a session — `msg.SessionFileSharer`. */
export type SessionFileSharer = 'user' | 'agent';

/** A file shared into a session — the canonical `msg.SessionFile`, snake_case as
 *  llm-bridge-server and log-store send it. `path` is where the session's agent reads
 *  the same bytes; the browser reads them through `ApiClient.sessionFileContentUrl`. */
export interface SessionFile {
  file_id: string;
  session_id: string;
  filename: string;
  media_type: string;
  size_bytes: number;
  shared_by: SessionFileSharer;
  path: string;
  created_at: string;
}

/** What a session action does when a person confirms it — `msg.SessionActionType`. */
export type SessionActionType =
  | 'deploy'
  | 'run_scheduler_job'
  | 'run_command'
  | 'model_call'
  | 'background_agent'
  | 'send_message' // no longer offered since 2026-09-28; older records still carry it
  | 'fork_and_send'
  | 'new_session_and_send'
  | 'send_to_session'
  | 'stop_session';

/** How the chat draws an action's output — `msg.SessionActionResultFormat`. */
export type SessionActionResultFormat = 'text' | 'markdown';

/** A reviewer model's reading of a `run_command`, made when it was offered —
 *  `msg.SessionActionReview`. A `reject` cannot be run. */
export interface SessionActionReview {
  verdict: 'approve' | 'caution' | 'reject';
  reasons: string;
  model: string;
  reviewed_at: string;
}

/** Where a session action is in its one run — `msg.SessionActionState`. */
export type SessionActionState = 'offered' | 'running' | 'succeeded' | 'failed' | 'outcome_unknown';

/** What the agent asked for — `msg.SessionActionOffer`. `type` decides which fields
 *  are set. */
export interface SessionActionOffer {
  label: string;
  type: SessionActionType;
  repo_id?: number;
  scheduler_job_id?: number;
  message?: string;
  shell_command?: string;
  working_directory?: string;
  result_format?: SessionActionResultFormat;
  model?: string;
  maximum_cost_usd?: number;
  /** The other session a send_to_session or stop_session acts on. */
  target_session_id?: string;
  /** Text boxes the button shows (run_command and send_to_session). */
  inputs?: SessionActionInput[];
}

/** One text box on a button — `msg.SessionActionInput`. What the person types is
 *  sent with the confirm and reaches the command as `$BRIDGE_INPUT_<name>`, or takes
 *  the place of `$BRIDGE_INPUT_<name>` in a send_to_session message; the server
 *  stores it nowhere. A secret box is a password field (run_command only). */
export interface SessionActionInput {
  name: string;
  label: string;
  secret?: boolean;
}

/** A button an agent put in its session and the record of its run — the canonical
 *  `msg.SessionAction`. `command` is llm-bridge-server's statement of exactly what a
 *  confirm runs. Every change arrives as a `session_action` event carrying the whole
 *  record, so the newest record for an `action_id` is its current state. */
export interface SessionAction {
  action_id: string;
  session_id: string;
  offer: SessionActionOffer;
  command: string;
  state: SessionActionState;
  offered_at: string;
  run_by_principal_id?: string;
  started_at?: string;
  finished_at?: string;
  output?: string;
  error?: string;
  result_session_id?: string;
  review?: SessionActionReview;
  cost_usd?: number;
}

/** Where the client got an entry: folded from the live stream (or born in the
 *  client, as the optimistic prompt is), or read from a server page. */
export type EntryOrigin = 'live' | 'page';

export interface Entry {
  id: string; // stable per-entry key (message_id + role, or synthesized from event_id)
  turnId: string; // groups entries into a turn
  role: Role;
  kind: EntryKind;
  source: EntrySource;
  eventId: number; // the log-store event row id — monotonic, used for ordering + resume
  ts: string; // RFC3339 + offset

  /** Where this entry came from. The CLIENT stamps it — `'live'` where the fold or the
   *  optimistic send creates the entry, `'page'` where a server page is merged in; the
   *  server never sends it. It is a field on the entry, not a lookup in the tail's
   *  frame-id map, because the entry outlives that map: every streamed batch writes
   *  the model to IndexedDB and a reload reads it back with the map empty. Judged by
   *  the map, a cached live prompt (`msg_…_user`) looked like old history, the merge
   *  kept it beside the page's copy (`e_…`), and the prompt was drawn again after the
   *  answer (measured 2026-09-17 on br_1789665111110648544). */
  origin?: EntryOrigin;

  /** Canonical bridge-assigned id of the chat message this entry belongs to
   *  (`message_id` on every event; `messageId` on the materialized wire). The
   *  per-message Turns view groups and keys rows by it — see dash
   *  docs/chat-turns-per-message.md. Carried as a field even on entries
   *  whose key already encodes it, and especially on TOOL entries (keyed
   *  `tool_<toolId>`, which does not encode it) so no consumer parses id
   *  shapes. Absent on bookkeeping entries (system, session_state, …) that
   *  belong to no message bubble. */
  messageId?: string;

  /** Token usage for this entry, when the source event reported it. camelCase on the
   *  wire; read directly by the UI (`entry.usage`) — there is no per-entry hook. */
  usage?: EntryUsage;

  // Rendered payload (kind-dependent; unused fields omitted):
  text?: string;
  toolName?: string;
  toolInput?: unknown;
  toolResult?: unknown;
  /** Set on a page's entries when the tool input or output was SHORTENED for the page
   *  (`payload=preview`): `toolInput` / `toolResult` hold at most 2 KB of each string,
   *  the matching `*Truncated` is true when a longer original exists, and `*Bytes` is
   *  the original's size. `ApiClient.getEntry` returns the entry with both in full.
   *  Absent on live-stream entries, which always carry the whole payload. */
  toolInputBytes?: number;
  toolInputTruncated?: boolean;
  toolResultBytes?: number;
  toolResultTruncated?: boolean;
  /** tool entries only: the source event carried no tool_id, so this entry can
   *  never be paired with its counterpart — a call here will never receive a
   *  result, and a result here will never find its call. OTel-derived tool
   *  stand-ins arrive this way by design. The edge must not render such a call
   *  as still running: it is not pending, it is unknowable. */
  unpairable?: boolean;
  /** Set when this entry is a subagent's own work rather than this session's:
   *  the tool_use_id of the Task call that spawned it (`harness_parent_id` on
   *  the wire).
   *
   *  The bridge server routes a subagent's frames into the subagent's own
   *  session, so a session's own model should almost never hold one. The
   *  exception is the server's fail-safe: a frame whose task_started was missed
   *  stays on the parent rather than being dropped. A view of what THIS session
   *  did must leave those out — they are another session's rows, sitting here
   *  because there was nowhere better to put them. */
  harnessParentId?: string;
  /** The canonical tool id — `toolId` on the materialized wire, `tool_id` on the
   *  live event. Pairs a call with its result.
   *
   *  ⚠️ Absent is a REAL answer and not a gap: the OTel exporter emits no tool id
   *  at all (measured — 363 of 644 `tool_result` entries on one real session, every
   *  one of them `source: 'otel'`, and their `raw` carries none either). That is the
   *  unpairable case, and a consumer must render it as unknowable rather than as
   *  still-running. */
  toolId?: string;
  /** Whether the tool failed — `ToolResultEvent.IsError`. */
  toolError?: boolean;
  /** The images a tool result carried (`ToolResultEvent.Content`): a screenshot, an
   *  image file the agent read. Each names where its bytes are — see
   *  `ToolResultImage`. Absent when the result carried none. */
  toolResultImages?: ToolResultImage[];
  /** kind `'file'` only: the file shared into the session, as the canonical record. */
  sessionFile?: SessionFile;
  /** kind `'action'` only: the session action as this event recorded it. The entry
   *  whose record is `offered` is where the button was put; later entries are the
   *  steps of its run. */
  sessionAction?: SessionAction;
  /** The caller-minted per-turn id, used to correlate an optimistic user row with
   *  the real `user_message`.
   *
   *  ⚠️ Empty on this box today, on every session measured: the bridge stamps it
   *  only when a caller supplies one and nothing here does. So the correlation
   *  falls through to its normalized-text fallback, which cannot tell two identical
   *  prompts apart. Carried as a field so it starts working the day something does
   *  send one, rather than being invented here. */
  clientRequestId?: string;
  /** Who wrote a user message: principal-store's id, and their name when it was
   *  written, for display only. Absent when the server recorded no sender — the
   *  message came from a script, an agent, or before senders were recorded. */
  senderPrincipalId?: string;
  senderDisplayName?: string;
  /** The canonical `Event.Type`. Read by the terminal-state selector for the event
   *  types that carry no `kind` of their own. */
  eventType?: string;

  /** The original event payload, for the Raw pane and audit.
   *
   *  ⚠️ Absent on the DEFAULT page and present only on `/messages/raw`. It was 78.9%
   *  of a measured 9.91 MB page and the Turns view never rendered a byte of it, so
   *  it is fetched only when Raw is open. Nothing outside the Raw pane may read it:
   *  the four fields above exist precisely so nothing has to. On a LIVE-folded entry
   *  it is still populated from the SSE frame, so its presence says nothing about
   *  which page shape a consumer is holding. */
  raw?: unknown;

  // Provenance / kind-specific fields (mapped from the canonical wire, never invented):
  /** true when this assistant text was recovered from the OTel copy after the live
   *  stream produced nothing (`extensions.recovered`). Never gates visibility. */
  recovered?: boolean;
  /** error entries (kind 'error'): the canonical ErrorEvent fields. `code` values like
   *  TURN_IDLE_TIMEOUT / PROCESS_DIED are turn terminators; api_error /
   *  api_retries_exhausted are informational chips. */
  code?: string;
  retryable?: boolean;
  statusCode?: number;
  /** system entries (kind 'system'): the SystemEvent subtype (e.g. subagent_completed,
   *  compact_boundary). An unknown subtype must render generically, never as an error. */
  subtype?: string;
  /** system entries describing a harness subagent (subtype `task_*`).
   *
   *  `taskId` names the subagent and is what the timeline groups its rows under;
   *  `toolUseId` ties it back to the tool call that spawned it. `taskStatus` is
   *  the only signal that the subagent finished — it emits no result event of
   *  its own — and `taskSummary` is its own report of what it did, which is the
   *  whole point of the notification. `taskOutputFile` is the transcript path,
   *  diagnostic only: never read it to reconstruct the summary. */
  taskId?: string;
  toolUseId?: string;
  taskStatus?: string;
  taskSummary?: string;
  taskOutputFile?: string;
  /** system entries about a message the server held while it waited to restart
   *  (subtypes `message_held_for_restart`, `held_message_delivered`,
   *  `held_message_delivery_failed`). The three frames of one message share it. */
  heldMessageId?: string;
  /** What kind of background work the task is (`local_agent`, `local_bash`, …).
   *  Only `task_started` carries it; without it a background `sleep 2` is
   *  indistinguishable from an agent. */
  taskType?: string;
  /** The agent role a subagent was spawned as (`Explore`, `general-purpose`, …).
   *  Only `task_started` carries it. */
  subagentType?: string;
  /** The bridge session id of the subagent a `task_*` entry describes — the id a
   *  client follows to read what that subagent did. Minted and stamped by
   *  llm-bridge-server, which owns it.
   *
   *  Empty means there is no session to link to, which is a real answer: a
   *  backgrounded shell task gets the same task frames a subagent does and
   *  deliberately never gets one. */
  subagentSessionId?: string;
  /** The tool the subagent last ran, from `task_progress` — the only live view a
   *  parent has into what its subagent is doing. */
  lastToolName?: string;

  // Non-destructive dedup annotations:
  duplicate: boolean; // hidden in collapsed Turns view; always shown in raw Timeline
  primary: boolean; // the copy shown for its group in the collapsed view
  groupId?: string; // members share identical logical content across sources
}

export interface Turn {
  id: string;
  role: Role;
  ts: string;
  entryIds: string[]; // order within the turn, by eventId
}

/** Rolled-up cost/context figures for a session's materialized model, populated by
 *  log-store's spend/context materializer (camelCase on the wire, mirrored directly).
 *  MAY be undefined — or carry undefined members — when the spend/context events fall
 *  outside the currently loaded page; consumers must treat absence as zero, never
 *  fabricate a figure. `useSessionCost` / `useContextUsage` read from here. */
export interface TurnAggregates {
  totalUsd?: number;
  byModel?: Record<string, number>;
  byQuerySource?: Record<string, number>;
  contextTokens?: number;
  contextLimit?: number;
}

/** The render-ready, fully-annotated model for one session. Holds EVERY entry; the
 *  collapsed Turns view is a pure selector over `entries` (filter !duplicate), the raw
 *  Timeline view renders all of `entries`. */
export interface TurnModel {
  /** Which sources each dedup group was built from, keyed by groupId — computed by
   *  log-store over EVERY stored entry before the duplicates were dropped from the
   *  projected page. It is the only record of a pairing whose other half the page
   *  does not carry: a prompt's OTel echo is deleted on the wire, so the sources
   *  badge cannot be derived from the entries alone (it was, and on a projected page
   *  it read "1 source" on exactly the messages it exists for). Optional because a
   *  log-store that predates it sends none. */
  sourceGroups?: Record<string, string[]>;
  sessionId: string;
  turns: Turn[];
  entries: Record<string, Entry>; // id -> Entry (all sources, all copies)
  validator: Validator;
  more: boolean; // true if older turns exist beyond this page (paginate with `before`)
  /** Cost/context roll-up for the loaded page; undefined when the materializer has not
   *  attached one (spend/context events outside the page). Read via the cost/context hooks. */
  aggregates?: TurnAggregates;
}

/** A tool the harness reports as available to the agent (SessionInfo.tools[]).
 *  `description` is optional — the harness only reports what the agent exposes. */
export interface ToolInfo {
  name: string;
  description?: string;
}

/** An MCP server connection reported by the agent (SessionInfo.mcpServers[]). */
export interface McpServerInfo {
  name: string;
  status?: string;
}

/** What the harness knows about a session at start: the configured system prompt,
 *  working dir, model, permission mode, and the tools / slash commands / sub-agents /
 *  skills / MCP servers the underlying agent reports. camelCase after the client maps
 *  it; the WIRE is snake_case `msg.SessionInfo` (system_prompt, working_dir, …), mapped
 *  explicitly in `ApiClient.getSessionDetail`. Fetched lazily per session via
 *  `useSessionInfo`; absent fields stay absent (never guessed). */
export interface SessionInfo {
  systemPrompt?: string;
  appendSystemPrompt?: string;
  workingDir?: string;
  model?: string;
  permissionMode?: string;
  /** The reasoning effort the agent reports it runs at — its own resolved level,
   *  so it is set even when nothing chose one and the agent used its default. */
  effort?: string;
  tools?: ToolInfo[];
  slashCommands?: string[];
  agents?: string[];
  skills?: string[];
  mcpServers?: McpServerInfo[];
}

/** `HookEvent.source` — a hook that emits `permission_prompt` is the harness asking
 *  whether a tool call may proceed. The empty source means the same thing to this
 *  client: an allow/deny card. */
export const HOOK_SOURCE_PERMISSION = 'permission_prompt';

/** `HookEvent.source` — the model is asking the human a structured question (Claude
 *  Code's AskUserQuestion, ACP's request_user_input). The answer rides back as the
 *  resolve body's `updatedInput`, so "allow" carries data and bypass never applies:
 *  these always park for a human. */
export const HOOK_SOURCE_USER_INPUT = 'user_input';

/** A hook parked on a human decision — one `awaiting_resolution` HookEvent that no
 *  matching `completed` has closed yet. camelCase of `HookEventWire`. Hydrated from
 *  `GET /sessions/{id}/hooks/pending` and kept live by the session SSE; a session with
 *  one of these has a tool call frozen mid-turn and shows no other sign of it, which is
 *  why the banner is not optional.
 *
 *  `input` is the harness's own raw tool input, carried through untouched. */
export interface PendingHook {
  requestId: string;
  event: string;
  phase: string;
  source: string;
  toolName?: string;
  matcher?: string;
  hookId?: string;
  input?: unknown;
}

/** A decision for one parked hook, as `POST /sessions/{id}/hooks/{request_id}/resolve`
 *  takes it. `updatedInput` REPLACES the tool input wholesale — it is how a
 *  `user_input` hook's answers reach the model. */
export interface HookResolveInput {
  requestId: string;
  behavior: 'allow' | 'deny';
  updatedInput?: unknown;
  message?: string;
  /** Audit label for who decided; defaults to `user`. */
  resolvedBy?: string;
}

/** The raw approval/sandbox knobs under `harnessConfig.permissionModeCustom` (the
 *  power-user "custom" permission mode). camelCase of `HarnessConfigCustomWire`. */
export interface HarnessConfigCustom {
  approval?: string;
  sandbox?: string;
}

/** The three axes `PUT /sessions/{id}/permission-mode` writes in one body. camelCase
 *  of the request the bridge accepts (`mode` / `disable_network` /
 *  `permission_mode_custom`); the mapping to snake_case happens in `ApiClient`.
 *
 *  `mode` is required — the endpoint validates it server-side and rejects an empty
 *  one. The other two are optional and **absent means "leave the stored value alone"**,
 *  not "false" and not "clear": a caller that renders no network checkbox must not
 *  silently switch the sandbox back on by changing the mode. To CLEAR the custom knobs,
 *  send `permissionModeCustom` with both fields empty — that is the server's own
 *  "empty struct clears" contract, and it is the only way to say it. */
export interface SessionPermissionState {
  mode: string;
  disableNetwork?: boolean;
  permissionModeCustom?: HarnessConfigCustom;
}

/** A session's per-harness config bag (`harness_config` on the wire; opaque
 *  `json.RawMessage` on the Go side). The bridge's own well-known keys are surfaced in
 *  camelCase; the index signature carries any harness-specific knob through unchanged so
 *  the layer stays transparent and lossless. Fields:
 *   - `permissionMode` — the per-session prehook gate (ask / auto / bypass / plan / …);
 *     what the interactive permission-mode selector reads and writes.
 *   - `disableNetwork` — the sandbox "no outbound network" gate (harnesses that support it).
 *   - `permissionModeCustom` — raw approval/sandbox knobs for the "custom" mode.
 *   - `model` / `effort` — the per-harness defaults snapshotted at session create.
 *  Absent fields stay absent; nothing is invented. */
export interface HarnessConfig {
  permissionMode?: string;
  disableNetwork?: boolean;
  permissionModeCustom?: HarnessConfigCustom;
  model?: string;
  effort?: string;
  [k: string]: unknown;
}

/** The full per-session detail from `GET /sessions/{id}` (the canonical ManagedSession),
 *  with its snake_case `info` mapped to camelCase `SessionInfo` and `harness_config`
 *  mapped to `HarnessConfig`. `info` / `harnessConfig` are null when the harness has not
 *  reported / carries none. `sessionId` is surfaced at the top level (it also lives on
 *  `summary`) so a consumer holding just the detail can identify the session.
 *
 *  The nine fields after `harnessConfig` are the identity, lineage and spend the endpoint
 *  has always returned and this type used to drop at the boundary. They live HERE and not
 *  on `summary`: `SessionSummary` is the sidebar row, and widening it would widen the
 *  session-list query for data only one session at a time ever needs. `spendUsd` is the
 *  one exception, on both: the server's summary rows carry it, and a view that sums spend
 *  over many sessions reads it from the list.
 *
 *  Every one of them is optional and ABSENT MEANS ABSENT — the server omits what it has
 *  no value for, and a reader must render that as unknown rather than substituting a
 *  zero, an empty string or a dollar figure nobody reported. */
export interface ManagedSessionDetail {
  sessionId: string;
  summary: SessionSummary;
  info: SessionInfo | null;
  harnessConfig: HarnessConfig | null;

  /** Which service or script created this session (frontend-dash, autoworker, …). */
  origin?: string;
  /** OS process id of the harness process, while one is running. */
  pid?: number;
  /** The harness's OWN session id, which rotates on resume/fork. Never equal to
   *  `sessionId`. */
  harnessSessionId?: string;
  /** ⚠️ The FORK parent's `harnessSessionId` — a harness UUID, not a session id.
   *  Deprecated on the server in favor of `forkedFromSessionId`; do not label it
   *  simply "parent" in a UI, because it is not one. */
  forkParentHarnessSessionId?: string;
  /** The session this one was forked from, as a bridge session id. */
  forkedFromSessionId?: string;
  /** The managing session in the team tree (bridge session id); absent = top-level. */
  managerSessionId?: string;
  /** The directory the harness runs in. Absent inherits the instance's, then the
   *  machine's — it means "inherit", never "unknown". */
  workingDir?: string;
  /** Total derived API spend for this session, in US dollars. Absent when the server
   *  predates the spend gate; absent is NOT zero. */
  spendUsd?: number;
  /** The server-side spend ceiling. ⚠️ ZERO MEANS NO CEILING, not "halt now". */
  maxBudgetUsd?: number;
}

/** A registered harness type and its capabilities, from `GET /harnesses` (camelCase of
 *  `msg.HarnessInfo`). `capabilities` is the CANONICAL per-harness feature set the
 *  controls bar gates each control on (`model` / `effort` / `compact` / `fork` /
 *  `system_prompt` / `tools`); `supportedProviders` scopes the model picker to the
 *  providers this harness can run. Single source of truth — never a hardcoded
 *  per-harness allowlist. Absent optional fields stay absent; nothing is invented. */
export interface HarnessMeta {
  name: string;
  label: string;
  emoji: string;
  tint?: string;
  available: boolean;
  capabilities: string[];
  hookEvents?: string[];
  supportedProviders?: string[];
  supportedPermissionModes?: string[];
  pty: boolean;
  supportsDisableNetwork?: boolean;
}

/** One selectable model for the controls-bar picker, projected from `GET /models`
 *  (only `enabled` rows). `value` is the model id (exactly what the config POST sends),
 *  `label` is display text including per-million cost when reported, `provider` scopes
 *  the option to a harness's `supportedProviders`, and `shortName` is the model's dense
 *  nickname for a picker too narrow to show the full label. */
export interface ModelOption {
  value: string;
  label: string;
  provider: string;
  /** The model's short nickname (`opus-4.6`), empty when the registry has none for it.
   *
   *  REQUIRED, not optional, even though the wire field it comes from is optional and the
   *  value is often the empty string. The projection that builds this type writes a fresh
   *  object literal rather than spreading the wire row, so an optional field would let any
   *  construction site quietly omit it and still typecheck — the field would then be
   *  `undefined` at runtime in exactly the paths nobody remembered to update. Requiring it
   *  turns each of those omissions into a compile error at the site that has the wire row
   *  in hand and can actually answer the question. The empty string carries "no nickname"
   *  perfectly well and costs no extra type. */
  shortName: string;
}

/** The per-session runtime knobs applied via `POST /sessions/{id}/config`. camelCase
 *  here; mapped to the snake_case wire body (`model` / `effort` / `max_budget` /
 *  `disabled_tools`) by `ApiClient.setConfig`. `model` / `effort` are the controls-bar
 *  pre-start settings (applied right after create) and can also be changed on a live
 *  session; absent fields are omitted from the request (never sent as null). */
export interface SessionConfig {
  model?: string;
  effort?: string;
  maxBudget?: number;
  disabledTools?: string[];
}

// ---- Endpoint response shapes ----

export interface SummaryResponse {
  sessions: SessionSummary[];
  next: string | null; // cursor for the next page, or null
  revision: string; // ETag-equivalent; also returned as the ETag header
}

/** The axes `GET /sessions/summary` can narrow on, server-side.
 *
 *  These are exactly the six faceted axes of `FilterState`, so a `FilterState` is
 *  structurally assignable here and no mapping table is needed. `folder` and
 *  `search` are deliberately absent: the endpoint has no folder parameter, and
 *  transcript search is a different endpoint with its own ranking.
 *
 *  Semantics match the chips: an absent or empty axis constrains nothing, values
 *  within one axis are OR'd, and axes are AND'd. The server implements the same
 *  rule (log-store `internal/store/event_page.go`), so the two cannot drift into meaning
 *  different things by one filter. */
export interface SessionSummaryFilterAxes {
  harness?: string[];
  status?: string[];
  type?: string[];
  purpose?: string[];
  mode?: string[];
  machine?: string[];
  /** Principal ids the sessions were started as; '' is "started as no principal". */
  principal?: string[];
}

/** One principal sessions were started as, among those the caller may see. */
export interface SessionPrincipal {
  /** principal-store's id, or '' for sessions started as no principal. */
  principalId: string;
  /** principal-store's name, read when asked; for display only. '' for no principal. */
  displayName: string;
  sessionCount: number;
}

/** The axis names, which are also the query parameter names. Iterated rather than
 *  written out at each call site so adding an axis is one edit. */
export const SUMMARY_FILTER_AXES = [
  'harness',
  'status',
  'type',
  'purpose',
  'mode',
  'machine',
  'principal',
] as const satisfies readonly (keyof SessionSummaryFilterAxes)[];

/** True when the filter constrains nothing, so a caller can tell an unfiltered
 *  request from a filtered one without inspecting each axis. */
export function isEmptySummaryFilter(filter: SessionSummaryFilterAxes | undefined): boolean {
  if (!filter) return true;
  return SUMMARY_FILTER_AXES.every((axis) => (filter[axis]?.length ?? 0) === 0);
}

/** recent-bundle: warms the N most-recent sessions in one round trip. */
export type RecentBundleResponse = Record<
  string,
  {
    summary: SessionSummary;
    model: TurnModel;
    /** Where to resume this session's event stream — see `StreamResumePoint`. Optional
     *  for the same reason it is on `MessagesResponse`: an older server does not send
     *  one, and a client without one connects without a resume point. */
    stream?: StreamResumePoint;
  }
>;

export type ValidatorsResponse = Record<string, Validator>;

export interface MessagesResponse {
  model: TurnModel;
  /** Where to resume this session's event stream so the page and the stream meet
   *  exactly. Absent from an older llm-bridge-server, which is why every reader treats
   *  it as optional and falls back to connecting without one. */
  stream?: StreamResumePoint;
}

/**
 * The point in a session's event stream that the page it arrived with already covers.
 *
 * ⚠️ `head` is an llm-bridge-server event row id — the id space of the SSE `id:` line and
 * of `Last-Event-ID`. It is NOT the log-store row id that `Entry.eventId` carries. The
 * two stores number the same events independently and comparing across them is
 * meaningless; that confusion has already cost this client a bug where every reconnect
 * silently replayed nothing. It is a separate field with a separate name for that reason
 * — never derive one from the other.
 *
 * Why it exists: the server replays the ENTIRE current turn to a client that connects
 * without a resume point, because it cannot know what that client already has. Measured
 * 2026-08-26 on three real sessions, that replay was 88, 177 and 360 frames — up to
 * 1,154 KB — all of it content the page had just delivered. With a resume point the same
 * connects replay nothing.
 *
 * `head: 0` is a real answer, not a missing one: the session has no stored events, and
 * the stream should be opened without a resume point.
 */
export interface StreamResumePoint {
  head: number;
}

/** `GET /folders` exactly as the gateway sends it (`msg.FolderList`). The field
 *  is literally `folder_order` and its ORDER is the payload — the server keeps a
 *  deliberate folder order and this is the only place it is stated. */
export interface FolderListWire {
  folder_order: string[] | null;
}

/** One content-search hit exactly as log-store sends it (`store.SearchHit`):
 *  the matching session and how many of its events matched. There is no snippet
 *  on the wire — an earlier version of this file declared one, and a `hits` array
 *  wrapped in an envelope, neither of which the backend has ever sent. */
export interface SearchHitWire {
  session_id: string;
  match_count: number;
}

/** One content-search hit, camelCased. `matchCount` is the only ranking signal the
 *  backend offers, so it is carried rather than dropped. */
export interface SearchHit {
  sessionId: string;
  matchCount: number;
}

/** `GET /sessions/search?q=` — the sessions whose materialized transcript text
 *  matches `q`. This is the async augmentation of the instant local name filter:
 *  the client folds `sessionIds` into the search-filter path so a query matches
 *  transcript content, not just the display name.
 *
 *  The server responds with a BARE ARRAY of `SearchHitWire`, not an object. This
 *  type is what `ApiClient.search` returns after mapping that array; it is not the
 *  wire shape. `sessionIds` is ordered by descending `matchCount`. */
export interface SearchResponse {
  sessionIds: string[];
  hits: SearchHit[];
  /** The hit cap this response was fetched under — echoed back so a caller can
   *  report the bound it is subject to without re-deriving it from the request. */
  limit: number;
  /** True when the backend returned a FULL page, i.e. `hits.length === limit`.
   *  The search endpoint reports no total, so a full page is the only evidence
   *  that hits were dropped; there is no way to say how many. Callers must read
   *  this as "at least this many, probably more", never as an exact count. */
  truncated: boolean;
}
