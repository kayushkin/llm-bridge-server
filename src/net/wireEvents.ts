import type { SessionAction, SessionFile, SessionStatus } from './types.js';
// The raw SSE/event wire shapes the live-tail reducer consumes. These mirror the
// canonical llm-bridge `msg.Event` JSON (snake_case on the wire, as emitted by
// llm-bridge-server) — deliberately narrow to the fields chat-core reads. The
// materialized, render-ready shapes live in `types.ts`; this file is the *input*
// side (streaming events) that `reduce/TurnReducer.ts` folds into a TurnModel.

/** SSE envelope: `event:`/`data:`/`id:` frame from a bridge event stream. */
export interface WireEvent {
  /** The SSE `id:` line — llm-bridge-server's OWN event row id (bridge.db),
   *  used for Last-Event-ID resume against that same server. ⚠️ NOT the
   *  log-store row id the materialized page's `eventId` carries: the two
   *  stores number the same events independently, and no numeric comparison
   *  across them is meaningful. (This comment previously claimed the id WAS
   *  the log-store row id; the whole client was built on that false premise —
   *  see dash docs/chat-turns-per-message.md.) */
  id?: string;
  /** The SSE `event:` line; mirrors `data.type` for live events. */
  type: string;
  data: WireEventData;
}

/** Canonical bridge Event payload (the `data` of a WireEvent), narrowed. */
export interface WireEventData {
  /** Injected by log-store on replay; the monotonic row id. */
  event_id?: number;
  type?: string;
  timestamp?: string;
  message_id?: string;
  turn_id?: string;
  harness_message_id?: string;
  client_request_id?: string;
  /** Who wrote a user_message (msg.Event.SenderPrincipalID), and their name then. */
  sender_principal_id?: string;
  sender_display_name?: string;
  /** The tool_use_id of the Task call that spawned the subagent this event came
   *  from. Present only on a subagent's OWN frames, and the only thing that
   *  separates them from the parent's: Claude Code runs a subagent inside the
   *  parent's process and stamps every frame with the parent's session id.
   *
   *  The bridge server normally routes these frames into the subagent's own
   *  session, so a parent's timeline should not see them. When one does arrive
   *  (its task_started was missed, and the server keeps it on the parent rather
   *  than dropping it), this is what identifies it as somebody else's work. */
  harness_parent_id?: string;

  /** llm-bridge-claudecode tags the OTel copy of a prompt/response here
   *  (`extensions.source = "otel"`) so consumers can tell the two sources apart.
   *  `recovered` marks an assistant text block surfaced from the OTel copy after the
   *  live stream produced nothing that turn (handler.go flushRecoveredAssistant). */
  extensions?: { source?: string; recovered?: boolean };

  stream?: { delta?: { type?: string; text?: string; thinking?: string } };
  block?: {
    block?: {
      type?: string;
      text_block?: { text?: string };
      thinking_block?: { text?: string };
    };
  };
  thinking?: { text?: string; subtype?: string };
  tool_call?: { tool_id?: string; name?: string; input?: unknown };
  tool_result?: {
    tool_id?: string;
    name?: string;
    output?: unknown;
    is_error?: boolean;
    /** The result's non-text blocks — `ToolResultEvent.Content`, canonical content
     *  blocks. Only images are read. */
    content?: Array<{
      type?: string;
      image_block?: { source?: { kind?: string; media_type?: string; data?: string } };
    }>;
  };
  /** Body of a `session_file` event: a file shared into the session. */
  session_file?: SessionFile;
  /** Body of a `session_action` event: a session action's whole record, each time it
   *  changes. */
  session_action?: SessionAction;
  result?: { text?: string; usage?: unknown; is_error?: boolean };
  // Mirrors the canonical msg.ErrorEvent JSON (snake_case on the wire). `code` values
  // TURN_IDLE_TIMEOUT / PROCESS_DIED are turn terminators; api_error /
  // api_retries_exhausted are informational. `retryable` / `status_code` drive chip styling.
  error?: { code?: string; message?: string; retryable?: boolean; status_code?: number };
  /** Mirrors the canonical `msg.SystemEvent` JSON (snake_case on the wire).
   *
   *  The task_* fields describe a harness subagent. `task_id` names it,
   *  `tool_use_id` ties it back to the tool call that spawned it, and
   *  `task_status` is the only thing that ever says it finished — a subagent
   *  emits no result event of its own. `task_summary` is the subagent's own
   *  report, which is the payload the notification exists to deliver.
   *
   *  These were previously read off the untyped `raw` blob by whoever needed
   *  them, which is why the completion never rendered: nothing looked. */
  system?: {
    subtype?: string;
    message?: string;
    tool_use_id?: string;
    task_id?: string;
    description?: string;
    last_tool_name?: string;
    task_status?: string;
    task_summary?: string;
    task_output_file?: string;
    /** The message a server held instead of sending, because it was about to
     *  restart, on `message_held_for_restart`, `held_message_delivered` and
     *  `held_message_delivery_failed` (llm-bridge `SystemEvent.HeldMessageID`). */
    held_message_id?: string;
    /** What kind of background work the task is (`local_agent`, `local_bash`, …).
     *  Only `task_started` carries it — Claude Code backgrounds a shell command
     *  through the same task frames it uses for a subagent, and without the kind a
     *  consumer cannot tell an agent from `sleep 2`. */
    task_type?: string;
    /** The agent role a subagent was spawned as (`Explore`, `general-purpose`, …).
     *  Only `task_started` carries it. */
    subagent_type?: string;
    /** The bridge session id of the subagent this task spawned — what a client
     *  follows to read what the subagent actually did.
     *
     *  Set by llm-bridge-server, which mints the session and so is the only
     *  party that knows the id; a harness only ever knows its own task id.
     *  Empty is a real answer, not a gap: a backgrounded shell task never gets
     *  a session. */
    subagent_session_id?: string;
  };
  state?: { state?: string; previous?: string; reason?: string };
  /** Body of a `session_status` event — the session's whole status. */
  status?: SessionStatus;
  /** Body of a `session_info` event — the harness's whole SessionInfo, which
   *  replaces the one the session carries. */
  info?: SessionInfoWire;
  hook?: HookEventWire;
}

/** snake_case `msg.HookEvent`, narrowed to the fields the pending-permission surface
 *  reads. A hook whose resolver is out-of-band (a human, usually) emits
 *  `phase="awaiting_resolution"` and parks the tool call until a decision arrives at
 *  `POST /sessions/{id}/hooks/{request_id}/resolve`; the matching `phase="completed"`
 *  carries the same `request_id`. Those two phases are the whole protocol the banner
 *  needs — `started`/`progress` are observation-only.
 *
 *  `source` picks the card: `permission_prompt` (and the empty default) is a tool-gating
 *  ask answered allow/deny; `user_input` is the model asking the human a structured
 *  question, whose answer rides back in the resolve body's `updated_input`. */
export interface HookEventWire {
  event?: string;
  matcher?: string;
  tool_name?: string;
  hook_id?: string;
  source?: string;
  phase?: string;
  request_id?: string;
  /** Raw JSON the harness passed the hook — the tool input, in the harness's own
   *  dialect. Pass-through: chat-core never reshapes it. */
  input?: unknown;
  decision?: string;
}

/** A managed-session row as it arrives on the global list SSE (`/session-events`) and
 *  from `GET /sessions/{id}`. snake_case, as emitted by llm-bridge-server.
 *
 *  The first block is what `summaryFromManaged` (sync/sse.ts) projects onto
 *  `SessionSummary` — the sidebar row. The second block is the per-session identity,
 *  lineage and spend the server already writes on every one of these rows and that
 *  mostly only `ManagedSessionDetail` surfaces; apart from the two lineage ids
 *  (`harness_session_id`, `manager_session_id`, which the live-status surface joins
 *  on), it is deliberately NOT projected onto `SessionSummary`, because the sidebar
 *  list query must not widen to carry it. */
export interface ManagedSessionWire {
  session_id: string;
  state?: string;
  /** `msg.SessionStatus`, passed through untouched — see `SessionStatus`. */
  status?: SessionStatus;
  harness?: string;
  instance_id?: string;
  type?: string;
  purpose?: string;
  mode?: string;
  folder_name?: string;
  display_name?: string;
  agent_id?: string;
  principal_id?: string;
  updated_at?: string;
  created_at?: string;

  /** Which service or script created this session (frontend-dash, autoworker, …). */
  origin?: string;
  /** OS process id of the harness process, while one is running. */
  pid?: number;
  /** The harness's OWN session id, which rotates on resume/fork. Never equal to
   *  `session_id` — the server discards an event that claims it is. Projected onto
   *  `SessionSummary` (with `manager_session_id`) because it is the lineage join the
   *  live-status surface needs: a promoted subagent session's value is
   *  `agent-<task_id>`, the server-chosen dedupe key that ties it back to the
   *  parent's `task_started` narration. */
  harness_session_id?: string;
  /** ⚠️ Deprecated on the server (`msg.ManagedSession.ParentID`): this is the FORK
   *  parent's `harness_session_id`, a harness UUID fed to `--fork` — not a session id
   *  and not a general parent. `forked_from_session_id` is its honest replacement.
   *  Carried so a consumer can still read old rows; label it for what it is. */
  parent_id?: string;
  /** The session this one was forked from, as a bridge session id. */
  forked_from_session_id?: string;
  /** The managing session in the team tree (bridge session id); empty = top-level.
   *  Projected onto `SessionSummary` — see `harness_session_id` above. */
  manager_session_id?: string;
  /** The directory the harness runs in. Empty inherits the instance's, then the
   *  machine's — an empty string is "inherit", never "unknown". */
  working_dir?: string;
  /** Total derived API spend for this session, in US dollars. Absent when the server
   *  predates the spend gate — absent is NOT zero, and must not render as $0.00. */
  spend_usd?: number;
  /** The server-side spend ceiling. ⚠️ ZERO MEANS NO CEILING, not "halt now" — the
   *  same convention as Claude Code's `--max-budget-usd`. */
  max_budget_usd?: number;
}

/** snake_case `msg.ToolInfo` — a tool the agent reports (SessionInfoWire.tools[]). */
export interface ToolInfoWire {
  name: string;
  description?: string;
}

/** snake_case `msg.MCPServerInfo` — an MCP server the agent reports. */
export interface McpServerInfoWire {
  name: string;
  status?: string;
}

/** snake_case `msg.SessionInfo` as it rides on a ManagedSession's `info` field
 *  (`GET /sessions/{id}`). Mapped to the camelCase `SessionInfo` by
 *  `ApiClient.getSessionDetail` — this is the single source for that mapping. */
export interface SessionInfoWire {
  system_prompt?: string;
  append_system_prompt?: string;
  working_dir?: string;
  model?: string;
  permission_mode?: string;
  effort?: string;
  tools?: ToolInfoWire[];
  slash_commands?: string[];
  agents?: string[];
  skills?: string[];
  mcp_servers?: McpServerInfoWire[];
}

/** The raw approval/sandbox knobs carried under `harness_config.permission_mode_custom`
 *  (the power-user "custom" mode escape hatch). Codex consumes `approval` + `sandbox`;
 *  other harnesses define their own subset. snake-free already — these are the leaf
 *  fields of `msg` permissionModeCustomConfig. */
export interface HarnessConfigCustomWire {
  approval?: string;
  sandbox?: string;
}

/** snake_case `harness_config` as it rides on a ManagedSession (`GET /sessions/{id}`).
 *  On the Go side this is an OPAQUE `json.RawMessage` — a per-harness config bag — so
 *  the well-known bridge keys are surfaced explicitly here and the index signature
 *  preserves any harness-specific knob the bridge has not (yet) named. The keys below
 *  are the canonical ones the bridge itself reads/writes (permission_mode.go,
 *  hooks_resolve.go): `permission_mode` (the per-session gate), `disable_network`
 *  (sandbox network gate), `permission_mode_custom` (raw approval/sandbox knobs),
 *  plus `model` / `effort` (per-harness defaults snapshot). Nothing is invented. */
export interface HarnessConfigWire {
  permission_mode?: string;
  disable_network?: boolean;
  permission_mode_custom?: HarnessConfigCustomWire;
  model?: string;
  effort?: string;
  [k: string]: unknown;
}

/** The full per-session detail from `GET /sessions/{id}` — the canonical
 *  ManagedSession (a superset of the list-projection `ManagedSessionWire`) plus the
 *  snake_case `info` blob the summary list omits and the opaque `harness_config` bag. */
export type ManagedSessionDetailWire = ManagedSessionWire & {
  info?: SessionInfoWire | null;
  harness_config?: HarnessConfigWire | null;
};

/** snake_case `msg.HarnessInfo` as returned by `GET /harnesses` — one registered
 *  harness type and its capabilities. Mapped to the camelCase `HarnessMeta` by
 *  `ApiClient.getHarnesses` (the single source of truth for that mapping). This is the
 *  canonical registry the controls bar gates on; the client never hardcodes a
 *  per-harness allowlist. Narrowed to the fields chat-core reads. */
export interface HarnessInfoWire {
  name: string;
  label: string;
  emoji: string;
  tint?: string;
  available: boolean;
  binary?: string;
  /** The feature set the controls bar gates each control on: e.g. `model`, `effort`,
   *  `compact`, `fork`, `system_prompt`, `tools`. */
  capabilities: string[];
  hook_events?: string[];
  /** Provider ids this harness can run models from — scopes the model picker. */
  supported_providers?: string[];
  supported_permission_modes?: string[];
  pty: boolean;
  supports_disable_network?: boolean;
}

/** snake_case model row as returned by `GET /models` (model-store `Model` fields; the
 *  handler embeds credential status too, which chat-core ignores). Projected to the
 *  camelCase `ModelOption` by `ApiClient.getModels`, which also drops `enabled=false`
 *  rows. */
export interface StoreModelWire {
  id: string;
  provider: string;
  name?: string;
  /** The model's short nickname (`opus-4.6` for `claude-opus-4-6`, whose full `name` is
   *  `Claude Opus 4.6`), for a dense picker that keeps the full name in the tooltip.
   *  Optional because model-store lets the column stay empty: a model whose nickname has
   *  not been set yet simply has none, and the server sends no value rather than a made-up
   *  one. */
  short_name?: string;
  max_tokens?: number;
  input_cost?: number;
  output_cost?: number;
  enabled?: boolean;
}

/** Frame on the global session-list stream (`GET /session-events`). */
export type SessionListFrame =
  | { type: 'hello'; resume: 'none' | 'replayed' | 'gap' }
  | { type: 'upsert'; session: ManagedSessionWire }
  | { type: 'delete'; sessionId: string }
  /** A session's open questions moved: one was raised, answered, dismissed or
   *  superseded. Carries no signal payload on purpose — it says WHICH session
   *  changed, and the reader re-reads the open set through the signals API
   *  rather than trusting a second encoding of it on this stream. */
  | { type: 'signal'; sessionId: string }
  /** A chat's brief or one of its loose ends moved. Carries no rows, like
   *  `signal`: the reader re-reads through GET /session-briefs. */
  | { type: 'session_brief'; sessionId: string };
