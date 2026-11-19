// --- Canonical types from llm-bridge (single source of truth) ---
//
// These are auto-generated from Go types in llm-bridge/msg/.
// See llm-bridge/ARCHITECTURE.md for the generation workflow.
//
// DO NOT duplicate these types locally. If you need a new API type,
// add the Go struct to llm-bridge/msg/server.go, run generate-ts.sh,
// and import it here.

import type {
  TokenUsage,
  Cost,
  Event,
  Instance,
  InstanceCredential,
  InstanceStatus,
  Machine,
  ManagedSession,
  HarnessInfo,
  HarnessDefaults,
  BridgePrefs,
  MaterializedMessage,
  MaterializedTool,
  ResultEvent,
  SessionInfo,
  ToolInfo,
  MCPServerInfo,
  HookEvent,
  HookResolution,
  APICallEvent,
  APISpendTotalEvent,
  CreateSessionRequest,
  CreateMachineRequest,
  UpdateMachineRequest,
  Signal,
  SignalAnswer,
  SignalOption,
  SignalCustomerReplyDraft,
} from '@kayushkin/llm-bridge-types'

export {
  ErrCodeBudgetExceeded,
  HookSourceHook,
  HookSourcePermission,
  HookSourceUserInput,
  PermissionModeAsk,
  PermissionModeAskAll,
  PermissionModeAuto,
  PermissionModeBlockAll,
  PermissionModeBypass,
  PermissionModeCustom,
  PermissionModePlan,
  PermissionModeRead,
  SignalKindNotification,
  SignalKindQuestion,
  SignalSeverityInfo,
  SignalSeverityWarn,
  SignalSourceDerived,
  SignalSourceTool,
  SignalStateAcknowledged,
  SignalStateAnswered,
  SignalStateDismissed,
  SignalStateOpen,
  SignalSurfaceChat,
  SignalSurfaceKanban,
} from '@kayushkin/llm-bridge-types'

// Re-export canonical types for consumers.
export type {
  TokenUsage,
  Cost,
  Event,
  InstanceCredential,
  InstanceStatus,
  Machine,
  ManagedSession,
  HarnessInfo,
  HarnessDefaults,
  BridgePrefs,
  MaterializedMessage,
  MaterializedTool,
  ResultEvent,
  SessionInfo,
  ToolInfo,
  MCPServerInfo,
  HookEvent,
  HookResolution,
  APICallEvent,
  APISpendTotalEvent,
  CreateSessionRequest,
  CreateMachineRequest,
  UpdateMachineRequest,
  Signal,
  SignalAnswer,
  SignalOption,
  SignalCustomerReplyDraft,
}

// Re-export with backward-compatible aliases where names differ.
export type { Instance as BridgeInstance }
export type { ManagedSession as BridgeSession }

// --- UI-specific types (not in llm-bridge) ---

// Fetch function type — consumers provide their own auth'd fetch
export type FetchFn = (url: string, opts?: RequestInit) => Promise<Response>

// ToolEvent matches the canonical MaterializedTool shape (input as object,
// not stringified). The streaming and history paths both deliver tools in
// this form so renderers can read fields directly.
//
// tool_id is the harness-native tool_use id (e.g. Anthropic's toolu_…) — the
// same id that task_progress SystemEvents carry, so consumers can correlate
// a tool invocation to its narration bubbles.
export interface ToolEvent {
  tool_id: string
  tool: string
  input?: Record<string, unknown>
  output?: string
  error?: boolean
  /** The input's strings were shortened for the page (chat-core `toolInputTruncated`).
   *  A renderer must not treat them as the whole input — a diff of shortened
   *  `old_string`/`new_string` would show an edit that never happened. */
  input_truncated?: boolean
}

// MessageMeta extends the canonical ResultEvent with client-side enrichments.
// The server sends ResultEvent as the "meta" field on materialized messages;
// the UI adds tools[], toolCalls, and rawStats during streaming.
export type MessageMeta = Partial<ResultEvent> & {
  tools?: ToolEvent[]
  toolCalls?: number
  rawStats?: Record<string, unknown>
}

// Message is the UI's working message type. It extends the server's
// MaterializedMessage with client-side tracking fields (id, sessionId, etc.).
// The server returns MaterializedMessage; the UI normalizes to this shape.
export interface Message {
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
  thinking?: string
  tools?: ToolEvent[]
  meta?: MessageMeta
  raw?: Record<string, unknown>
  done?: boolean
  // id is the canonical bridge-server MessageID (msg_<ULID>) once known.
  // Before /send returns, optimistic user messages key off clientId instead.
  id?: string
  clientId?: string         // optimistic-only id, replaced by canonical id on /send response
  harnessMessageId?: string // harness-native id, mirrors the server's harness_message_id
  lastEventRowId?: number   // highest SSE RowID applied to this message — dedupes replays
  orchestrator?: string
  agent?: string
  sessionId?: string
}

// LogRow is the flat event-log view of a session. One row per server event,
// with rows that share a bridge message_id coalesced together (stream deltas
// accumulate text, tool_call/tool_result merge, result adds stats). Rows
// without a message_id (system, session_state, session_info) stand alone.
//
// See useBridgeSession's reducer for the coalesce rules.
export type LogRowActor = 'user' | 'assistant' | 'system'

// LogRowKind narrows what a row represents. Rows only coalesce events of the
// same kind under a shared message_id (tool_call/tool_result pair by tool_id).
// Stream events split into 'text' or 'thinking' rows based on their delta.type.
export type LogRowKind =
  | 'user_message'
  | 'text'
  | 'thinking'
  | 'tool'
  | 'result'
  | 'error'
  | 'system'
  | 'session_state'
  | 'session_info'
  | 'plan'
  | 'approval'    // deprecated — emitted by legacy ApprovalEvent path; see 'hook' below
  | 'hook'        // canonical permission/observation surface (HookEvent)
  | 'stream'      // stream events with no delta (block-boundary markers)
  | 'block'       // finished content blocks not specialized to text/thinking (e.g. images, refusals)
  // Observability / derived signals. Hidden from chat by default (see
  // DEFAULT_HIDDEN_TYPES in components/chat/persistence.ts) but surfaced
  // as their own FilterBar toggles so users can opt in.
  | 'api_call'         // raw per-call telemetry from the harness (OTel-derived for claudecode)
  | 'api_spend_total'  // running per-call spend total, server-derived
  | 'usage_total'      // running per-turn usage total, server-derived
  | 'turn_complete'    // coalesced turn summary, server-derived
  | 'other'

export interface LogRow {
  // Stable React key: <messageId>_<kind> when coalescing, tool_<toolId> for
  // tool rows, otherwise "evt_<eventId>".
  key: string

  // Displayed IDs.
  clientId?: string
  clientRequestId?: string    // caller's per-turn id, stamped on every event in the turn
  turnId?: string             // bridge-minted per-turn id, covers user_message → result
  messageId?: string          // canonical bridge-server MessageID (msg_<ULID>)
  harnessMessageId?: string   // harness-native completion id (Anthropic msg_…)
  toolUseId?: string          // harness-native tool_use id (toolu_…) on SystemEvent rows that narrate a tool call

  // Internal — tracks which event rows contributed, used to dedup SSE replay.
  eventIds: number[]

  // Header metadata.
  actor: LogRowActor
  kind: LogRowKind            // what this row represents, drives grouping + display
  eventType: string           // raw type of the first event for this row
  subtype?: string            // populated for system/thinking events
  timestamp: string

  // Coalesced body payloads.
  text?: string               // user text OR streamed text_delta OR result text
  thinking?: string           // accumulated thinking text
  tools?: ToolEvent[]         // tool_call + tool_result pairs
  usage?: TokenUsage          // per-completion or per-turn token usage
  meta?: MessageMeta          // full result payload (tokens, cost, duration…)
  systemMessage?: string      // SystemEvent.Message
  systemFields?: Record<string, unknown>  // typed SystemEvent fields (attempt, etc.)
  stateTransition?: { from?: string; to: string; reason?: string }
  sessionInfo?: SessionInfo
  errorMessage?: string
  hook?: HookEvent           // populated for kind='hook' rows; carries phase, decision, resolution
  apiCall?: APICallEvent     // populated for kind='api_call' rows; one upstream model API call
  apiSpendTotal?: APISpendTotalEvent  // populated for kind='api_spend_total' rows; running per-call spend

  // Raw events that composed this row (for "show raw" expand).
  events: Array<Record<string, unknown>>

  done?: boolean              // terminal row (result, error, user_message)
}

// SessionUIState mirrors the canonical msg.SessionState enum from llm-bridge,
// with two UI-only additions: 'empty' (no session selected) and 'placeholder'
// (skeleton row). The deprecated 'running' is still accepted for back-compat
// with sessions whose row was last written by the pre-2026-05-06 schema, and
// gets projected to 'tool_running' at render time.
export type SessionUIState =
  | 'empty'
  | 'placeholder'
  // Pre-flight.
  | 'starting'
  // Active.
  | 'model_generating'
  | 'tool_running'
  | 'compacting'
  // The turn is over, but subagents or backgrounded commands it started are
  // still running in the harness. Active, yet not a turn in flight: nothing is
  // streaming and there is no turn to stop, so it is in none of the
  // turn-in-flight list (STREAMING_STATES in BridgeChat.tsx).
  | 'background_tasks_running'
  // Blocked on user.
  | 'awaiting_permission'
  | 'awaiting_user'
  // Self-healing wait.
  | 'rate_limited'
  | 'paused'
  // Quiet.
  | 'idle'
  // Terminal.
  | 'completed'
  | 'error'
  | 'aborted'
  | 'disconnected'
  // Deprecated — still emitted by sessions on the old vocabulary; rendered
  // as 'tool_running'/'awaiting_permission' until the row gets rewritten.
  | 'running'
  | 'waiting_on_approval'

export type ActivityKind =
  | { kind: 'idle' }
  | { kind: 'thinking' }
  | { kind: 'streaming' }
  | { kind: 'tool'; name: string }

// --- SSE types ---

// EventData is the canonical bridge Event, extended with event_id which
// log-store injects when replaying history (not present on live SSE events).
export type EventData = Event & { event_id?: number }

export interface BridgeEvent {
  id?: string                   // SSE `id:` line, stringified row id
  type: string                  // SSE `event:` line; mirrors data.type for live events
  data: EventData
}

// --- Hook return types ---

// BudgetHalt is the client's record that bridge-server's spend ceiling has
// stopped this session. It is set from either half of the server-side gate:
//
//   - the mid-turn interrupt, which arrives as an EventError carrying
//     ErrCodeBudgetExceeded on the session's SSE stream;
//   - the between-turns refusal, a 402 with a JSON body naming both dollar
//     figures, returned by send / resume / mode-switch.
//
// spendUSD and maxBudgetUSD are optional because only the 402 half carries
// them. The error event carries a sentence and no numbers, so a halt raised
// that way leaves them undefined and the surface falls back to `message` —
// the server's own words — rather than inventing a figure.
export interface BudgetHalt {
  /** Bridge session id the halt belongs to. A halt never outlives its own
   * session: switching sessions clears it. */
  sessionId: string
  /** The server's own description of the halt. Always present. */
  message: string
  /** Spend recorded against the ceiling at the moment of refusal, in USD.
   * Undefined when the halt came from the mid-turn error event. */
  spendUSD?: number
  /** The ceiling that was breached, in USD. Undefined when the halt came
   * from the mid-turn error event. */
  maxBudgetUSD?: number
}

export interface UseBridgeSessionReturn {
  sessions: ManagedSession[]
  activeSession: ManagedSession | null
  logRows: LogRow[]
  uiState: SessionUIState
  getSessionUIState: (session: ManagedSession) => SessionUIState
  activity: ActivityKind
  connected: boolean
  compacting: boolean
  error: string | null
  loadingHistory: boolean
  createSession: (opts: Partial<CreateSessionRequest> & Pick<CreateSessionRequest, 'harness'>) => Promise<ManagedSession | null>
  selectSession: (id: string) => void
  // attachTokens caches the per-session pty attach token returned by
  // POST /sessions and POST /sessions/{id}/mode. Keyed by bridge id.
  // Empty for sessions not created or mode-switched in this browser tab —
  // the server does not expose a refresh endpoint, so cross-tab attach
  // is unsupported in v1.
  attachTokens: Record<string, string>
  // switchMode flips a live session between events and pty mode. The
  // bridge-server kills the current harness process and respawns in the
  // new mode using --resume so history is preserved. Returns the new
  // attach token on a successful pty switch (also cached in attachTokens).
  switchMode: (sessionId: string, mode: 'events' | 'pty') => Promise<string | null>
  // refreshAttachToken fetches the per-hub attach token from the
  // server (GET /sessions/{id}/attach-token) and caches it. Used by
  // BridgeAttach to recover the token after a page refresh wipes
  // the in-memory map. Returns null when no live hub exists.
  refreshAttachToken: (sessionId: string) => Promise<string | null>
  // explicitSessionId targets a session the caller just created, before the
  // hook's activeSessionId state has settled (used by the lazily-started
  // "pending" new chat). Omit to send to the active session.
  send: (text: string, explicitSessionId?: string) => void
  interrupt: () => void
  resume: () => void
  stop: () => void
  compact: (summary?: string) => void
  fork: (displayName?: string) => void
  renameSession: (bridgeID: string, displayName: string) => Promise<void>
  sendConfig: (config: { model?: string; effort?: string; disabled_tools?: string[]; max_budget?: number }, explicitSessionId?: string) => void
  refreshSessions: () => void

  // budgetHalt is set while the active session is stopped at its spend
  // ceiling, and null otherwise. Drives the chat's BudgetBanner. Null for every
  // session with no ceiling, and for every server that predates the gate —
  // neither can produce the 402 nor the error code that sets it.
  budgetHalt: BudgetHalt | null
  // raiseBudgetCeiling sets a new ceiling on the halted session and clears
  // the halt. This is the escape hatch out of a budget halt, and it works on
  // a session whose process the gate already killed: bridge-server persists
  // the ceiling itself and only forwards to the harness when there is one.
  // Resolves to the error text when the server refused, null on success.
  raiseBudgetCeiling: (maxBudgetUSD: number, explicitSessionId?: string) => Promise<string | null>

  // Awaiting-resolution HookEvents for the active session: events whose
  // phase="awaiting_resolution" has not yet been closed by a matching
  // phase="completed". Hydrated on session select via /hooks/pending and
  // kept in sync by the SSE stream. Drives the sticky permission banner.
  pendingHooks: HookEvent[]
  // resolveHook posts a decision to the bridge-server's
  // /sessions/{id}/hooks/{request_id}/resolve endpoint and clears the
  // matching pendingHook entry optimistically.
  resolveHook: (input: {
    requestId: string
    behavior: 'allow' | 'deny'
    updatedInput?: unknown
    message?: string
    resolvedBy?: string
  }) => Promise<void>
}
