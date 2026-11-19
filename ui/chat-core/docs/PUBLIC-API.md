# chat-core public API

The surface dash's chat page consumes. The client implementation (`src/react/*`, `src/store/*`)
must export exactly these; the dash page imports only these. Kept stable so the page and the
library can be built in parallel.

```ts
import type { ReactNode } from 'react';
import type {
  SessionSummary, TurnModel, Entry, Turn,
  SessionInfo, ToolInfo, McpServerInfo, ManagedSessionDetail,
  HarnessConfig, HarnessConfigCustom, Facets,
  HarnessMeta, ModelOption, SessionConfig,
} from '@kayushkin/chat-core';

// ---- Provider ----
export interface ChatProviderProps {
  fetch: typeof fetch;          // dash passes its cookie-credentialed apiFetch
  basePath: string;             // '/api/bridge'
  noteboardBasePath?: string;   // '/api/noteboard' — omit and note/todo ref chips
                                // say lookup is not configured, never guess a path
  referenceStoreBasePath?: string; // '/api/bridge/reference-store' — the list of
                                // reference types and lookups for the signed-in person
  recentN?: number;             // warm-cache size, default 20
  turnsPerBundle?: number;      // last-N turns per bundled session, default 30
  sessionsPerPage?: number;     // sidebar sessions per page, default 100
  cache?: boolean;              // enable IndexedDB persistence, default true
  keepDraftsAndFiltersInBrowser?: boolean; // drafts and filters in localStorage,
                                // default true; a second provider on the page
                                // (another server's sessions) passes false
  children: ReactNode;
}
export function ChatProvider(props: ChatProviderProps): JSX.Element;

// ---- Selectors / hooks (all read from the in-memory store; no network on the hot path) ----

// Liveness of the global session-list SSE stream, as SyncEngine reports it. Separates
// "still connecting" from "you genuinely have no sessions", and says when the client has
// stopped receiving updates at all.
//
// The four values are not evenly likely: 'idle' is the window between mount and
// SyncEngine.start() (the boot prime runs first); 'connecting' covers the first attach AND
// every backoff reconnect, so a dropped stream reads as connecting; 'closed' is set only by
// SyncEngine.stop(), i.e. on unmount. The honest test for "updates are flowing" is
// `=== 'open'`, NOT `!== 'closed'`.
export function useConnState(): ConnState;
export type ConnState = 'idle' | 'connecting' | 'open' | 'closed';

// Sidebar list, already filtered + grouped + sorted by the current filter/folder state.
// `effectiveState(sessionId)` returns the tail-reconciled state for a row's status dot: a
// session the server still reports as running/holding but whose warm tail is terminal reads
// as completed/failed (F1 self-heal). Cold sessions return their raw summary state.
// `facets` are cross-axis counts over the FULL loaded session set (independent of the
// active filter), so the sidebar can render every available option + count and offer
// multi-select. Memoized on the sessions Map identity. (A `useFacets()` hook was NOT
// added — facets ride on `useSessionList` since the sidebar renders both together.)
//
// The list is ONE PAGE deep on boot (`sessionsPerPage`, default 100), because this box
// carries thousands of sessions and loading them all is the unbounded list the rewrite
// exists to kill. `moreSessions` is true while the server holds older sessions the client
// has not loaded, and `loadOlderSessions()` pulls the next cursor page and MERGES it —
// a sidebar that renders neither leaves every session past the first page unreachable.
// `total` counts the loaded-and-filter-passing rows and is never a server total: the
// summary endpoint reports no count, so "at least one more page" is the strongest honest
// claim. Filtering and `facets` are computed over the loaded window only, so paging is
// also what widens what a filter can reach.
export function useSessionList(): {
  groups: { folder: string; sessions: SessionSummary[] }[];
  total: number;
  loading: boolean;
  effectiveState: (sessionId: string) => string;
  facets: Facets;
  moreSessions: boolean;
  loadingOlderSessions: boolean;
  loadOlderSessions: () => void;
};

// Active session id + setter. select() is synchronous: it swaps the active id and renders
// from cache immediately (sub-10ms); any needed fetch/reconcile happens in the background.
// `summary.state` is reconciled against the warm tail (see effectiveState), so a stale
// running/holding state clears once the session is open.
export function useActiveSession(): {
  id: string | null;
  select: (id: string) => void;
  summary: SessionSummary | null;
};

// The pending (not-yet-created) session pane, or null. `openPending` sets `activeId` to
// null, so "nothing is selected" and "a new chat is open, unsent" look identical through
// useActiveSession alone — a UI that cannot tell them apart draws a freshly opened new
// chat as an empty pane. The value carries the instance/harness the first send will create
// the session on, so a header can name the target before the session exists, and it goes
// back to null the moment the real session is created.
export function usePendingSession(): PendingSession | null;

// Turns for a session. `view: 'turns'` = collapsed (dupes hidden); `view: 'raw'` = every
// entry incl. duplicates, ordered by eventId. `sourcesFor(entryId)` returns the group's
// members for the sources badge. Never throws on a cold session — returns loading + triggers
// a background tail fetch.
export function useTurns(sessionId: string | null, view?: 'turns' | 'raw'): {
  turns: Turn[];
  entries: Record<string, Entry>;
  visibleEntryIds: (turnId: string) => string[]; // respects `view`
  sourcesFor: (entryId: string) => Entry[];       // all copies in the entry's group
  loading: boolean;
  more: boolean;
  loadOlder: () => void;
};

// Composer + turn controls for a session (or the pending/new pane). Optimistic:
// append() shows the user text instantly and reconciles the message id after POST.
// Creating the real session is lazy on first send for a pending pane.
//
// `send()` returns void and cannot throw, but it is no longer silent. It used to drop
// every failure — a `.finally()` with no `.catch` on the send path and a bare
// `.catch(() => {})` on the create path — so a refused message stayed on screen looking
// sent, the typed text was gone, and the spend-ceiling 402 was discarded before anything
// could read it. A failed send now puts the text back in the box (only when the box is
// still empty, so a message typed meanwhile is not clobbered), removes the optimistic row
// that was never sent, sets `error`, and records a spend halt when that is what the
// refusal was — read it back with useBudgetHalt(). On the create path the restore goes
// to the session the send TARGETED, not to the pending pane: createSession succeeded and
// setActive already moved the pane, so restoring under the pending key would put the text
// in a box nobody is looking at.
//
// `draft`/`setDraft` PERSIST. The draft map is written to localStorage on every keystroke
// (store/draftStorage.ts) and read back synchronously inside createChatStore, so a reload
// does not eat a half-typed message and the text is there in the first paint. The pending
// pane's draft is keyed under PENDING_DRAFT_KEY and cleared on send, so a sent new chat
// does not reappear in the next one. Setting '' deletes the persisted copy. The record is
// bounded by recency (MAX_PERSISTED_DRAFTS drafts, MAX_DRAFT_AGE_MS old) and by
// removeSession, NOT by "is this session in the sidebar" — the sidebar is paged, so a
// draft for a session outside the loaded window is still live. Pass
// `createChatStore({draftStorage})` to redirect or disable it (null); it is off wherever
// there is no working localStorage.
//
// `stop()` interrupts the running turn (POST /sessions/{id}/interrupt). It is a LOUD
// control: it throws on a non-2xx (e.g. the 409 the server returns while a tool still
// holds the turn), sets `error`, and does NOT optimistically mark the session idle —
// a failed stop must be visible, never swallowed into a fake-idle. `interrupting` is
// true for the request's duration. `paused` reflects a parked/held session (the
// explicit 'paused' state; never gate on a bare `state === 'running'` — `tool_running`
// is also busy). `error` is the last send/stop/resume error message, or null.
//
// `resume()` restarts a session whose harness process is gone (POST
// /sessions/{id}/resume), LOUD on the same terms as stop(). `resuming` is true for the
// request's duration. `resumable` is the gate: the server decides resumability from its
// live process registry (a session WITH a process gets a 409), and the client-visible
// proxy for "no process" is the state set in RESUMABLE_STATES — currently
// aborted/disconnected/completed (`completed` since 2026-10-19: the server now keeps a
// reaped finished session's state as `completed` instead of overwriting it). It is
// deliberately NOT `paused`: nothing emits
// `msg.SessionPaused` today, so a control gated on it never renders. Interrupt also
// leaves the process registered, so an interrupted session is not resumable — it is
// idle, and sending to it continues it.
export function useComposer(sessionId: string | null): {
  send: (text: string) => void;
  draft: string;
  setDraft: (t: string) => void;
  sending: boolean;
  stop: () => Promise<void>;
  interrupting: boolean;
  paused: boolean;
  resume: () => Promise<void>;
  resuming: boolean;
  resumable: boolean;
  error: string | null;
};

// Filters + folders (client-side over the loaded list; switching is sub-10ms).
// `set({ search })` matches the display name INSTANTLY/locally; it also fires an async
// content search (GET /sessions/search) whose hit ids are folded into the list path
// when they arrive, so the filter matches transcript text too — without ever blocking
// the local name filter on the network (C6).
//
// The six faceted axes PERSIST. They are written to localStorage whenever a patch names
// one (store/filterStorage.ts) and read back synchronously inside createChatStore, so a
// reload paints the list already filtered rather than filtering it one frame later. The
// shape stored is chat-core's own INCLUSION arrays under `chat-core:filters` — NOT
// bridge-ui's exclusion keys, which mean the opposite and shared an origin with the chat page while dash served both.
// `search` and `folder` are deliberately never persisted: a restored query re-runs a
// transcript search on every load, and a restored folder can point at a deleted one.
// Pass `createChatStore({filterStorage})` to redirect or disable it (null); it is off
// wherever localStorage is not usable.
export function useFilters(): {
  filter: FilterState;
  set: (patch: Partial<FilterState>) => void;
  openFolder: (folder: string) => void;
};

// Optimistic mutations. Each updates the store first, POSTs in the background, reverts on error.
// `newSession` opens the pending pane (0 network). `instanceId`/`harness` say WHERE; the other
// four are PRE-START settings that ride on the pending pane and are applied by ONE
// POST /sessions/{id}/config right after the real session is lazily created on first send —
// matching bridge-ui, whose create call carries none of them (application is best-effort on
// that optimistic path; the LOUD path for a live change is `useSessionControls().setConfig`).
//
// The caller resolves the settings and chat-core invents none. Two sources feed them and they
// are one shape on purpose: the controls bar's pre-start picks, and the caller's saved
// per-harness defaults (dash reads `bridge-prefs.defaults[harness]`). A pre-start pick beats a
// saved default, and that precedence belongs to the caller.
//
// ⚠️ Two falsy values are real answers and are carried, not dropped: `maxBudget: 0` means NO
// ceiling on the server (never "halt now"), and `disabledTools: []` means "disable nothing"
// (which absent does not). Absent fields are omitted from the config body rather than sent as
// null, and a pane with no settings at all makes no config call — see `pendingSessionConfig`.
export interface NewSessionOpts {
  instanceId?: string;
  harness?: string;
  model?: string;
  effort?: string;
  maxBudget?: number;
  disabledTools?: string[];
}

export function useSessionActions(): {
  newSession: (opts?: NewSessionOpts) => void;
  archive: (id: string) => void;
  unarchive: (id: string) => void;
  rename: (id: string, name: string) => void;
}

// The config body a lazily-created session owes its pending pane, or null when it owes none
// and the call should be skipped. Pure and exported so the two falsy-but-real values above can
// be pinned without a browser.
export function pendingSessionConfig(opts: NewSessionOpts | null | undefined): SessionConfig | null;

// ---- Settings / controls bar (the chat page's `bc-controls-bar`) ----

// Live-session controls for the settings bar: compact / fork / switch-mode / model+effort.
// All are LOUD — the underlying ApiClient methods throw on any non-2xx; each control surfaces
// the message on `error` and RETHROWS rather than faking a success/idle. `compacting` is set
// on compact() and cleared only when the canonical `compact_boundary` system entry lands on
// the session stream (or a 180s safety timeout) — the POST only ACKs, so completion is never
// faked (mirrors bridge-ui). `forking` is true for the fork request; on success it navigates
// the store to the new fork (its summary arrives via the list SSE). `setConfig` is the live
// model/effort change (POST /config). `error` is the last control error, or null.
export function useSessionControls(sessionId: string | null): {
  compact: (summary?: string) => Promise<void>;
  fork: (displayName?: string) => Promise<void>;
  switchMode: (mode: 'events' | 'pty') => Promise<void>;
  setConfig: (config: SessionConfig) => Promise<void>;
  compacting: boolean;
  forking: boolean;
  error: string | null;
}

// The capability set the controls bar gates each control on:
// `capabilities.has('model' | 'effort' | 'compact' | 'fork' | 'system_prompt' | 'tools')`.
// Sourced from the CANONICAL `GET /harnesses` registry (never a hardcoded per-harness
// allowlist), fetched once on first use and cached/shared in the store. Returns an empty set
// until it loads or when the harness is unknown, so a control simply stays hidden.
export function useHarnessCapabilities(harnessId: string | null): Set<string>;

// The models for the controls-bar picker, from the CANONICAL `GET /models` registry (enabled
// rows only), filtered to the harness's `supportedProviders` exactly as bridge-ui's
// `harnessModels` does. Pass no harnessId (or a harness declaring no providers) for every
// enabled model. Fetched once on first use, cached/shared; returns `[]` until it loads.
// `value` is the model id (what the config POST sends).
export function useModels(harnessId?: string | null): ModelOption[];

// A registered harness type + capabilities, from GET /harnesses (camelCase of msg.HarnessInfo).
export interface HarnessMeta {
  name: string; label: string; emoji: string; tint?: string; available: boolean;
  capabilities: string[];            // the controls-bar gate set (single source of truth)
  hookEvents?: string[];
  supportedProviders?: string[];     // scopes the model picker
  supportedPermissionModes?: string[];
  pty: boolean;                      // gates the events/pty ModeToggle
  supportsDisableNetwork?: boolean;
}
export interface ModelOption { value: string; label: string; provider: string; }
export interface SessionConfig { model?: string; effort?: string; maxBudget?: number; disabledTools?: string[]; };

// ---- Session info + cost/context (Phase 2) ----

// Session detail info (system prompt, model, permission mode, tools, slash commands,
// sub-agents, skills, MCP servers). LAZY: fetches GET /sessions/{id} on first use, caches
// the result in the store keyed by id, and returns the cache thereafter — a cached `null`
// means the harness has reported no info yet and is NOT re-fetched. Never blocks the hot
// path (the fetch is backgrounded; the store update re-renders). `loading` is true only
// while the first fetch is in flight.
export function useSessionInfo(sessionId: string | null): {
  info: SessionInfo | null;
  loading: boolean;
};

// A session's rolled-up cost from the cached/active model's `TurnModel.aggregates`. PURE
// selector — no network. Every field is 0/{} when aggregates are absent (the spend events
// fell outside the loaded page).
export function useSessionCost(sessionId: string | null): {
  totalUsd: number;
  byModel: Record<string, number>;
  byQuerySource: Record<string, number>;
};

// A session's context-window usage from `TurnModel.aggregates`. PURE selector — no network.
// `pct = tokens/limit*100`, or 0 when the limit is missing; zeros when aggregates are absent.
export function useContextUsage(sessionId: string | null): {
  tokens: number;
  limit: number;
  pct: number;
};

// Per-entry token usage is read DIRECTLY off the entry (`entry.usage`) — there is no
// per-entry hook. It may be absent when the source event carried no usage.
export interface SessionInfo {
  systemPrompt?: string;
  appendSystemPrompt?: string;
  workingDir?: string;
  model?: string;
  permissionMode?: string;
  tools?: ToolInfo[];
  slashCommands?: string[];
  agents?: string[];
  skills?: string[];
  mcpServers?: McpServerInfo[];
}
export interface ToolInfo { name: string; description?: string; }
export interface McpServerInfo { name: string; status?: string; }

// ---- Managed-session detail + permission mode (chat-page follow-up) ----

// Full per-session detail (summary + info + harnessConfig) PLUS a permission-mode
// mutator — what the interactive SessionPermissionMode selector consumes. LAZY: fetches
// GET /sessions/{id} on first use, caches the ManagedSessionDetail in the store keyed by
// id, returns the cache thereafter. Never blocks the hot path (backgrounded fetch;
// the store update re-renders). `loading` is true only while the first fetch is in flight.
//
// `setPermissionMode(mode)` OPTIMISTICALLY patches the cached detail's
// `harnessConfig.permissionMode`, then PUTs /sessions/{id}/permission-mode. On a non-2xx
// it REVERTS the cached detail and rethrows — a failed change must be visible, never
// silently kept. Resolves once persisted. `mode` is a canonical PermissionMode value
// (ask | auto | bypass | plan | read | ask_all | block_all | custom).
export function useManagedSession(sessionId: string | null): {
  session: ManagedSessionDetail | null;
  loading: boolean;
  setPermissionMode: (mode: string) => Promise<void>;
};

// The hooks this session has parked on a human decision, plus the verb that answers one.
// A permission ask FREEZES the tool call and produces no other visible sign — no error, no
// state the composer reads, nothing in the turn list — so a client without this surface
// simply hangs. Hydrates GET /sessions/{id}/hooks/pending on session change (the session
// SSE resumes from Last-Event-ID and never replays a hook parked before the client
// attached), then the live stream keeps the set current: phase="awaiting_resolution"
// inserts, the matching phase="completed" clears. Both directions are idempotent.
//
// resolve() POSTs /sessions/{id}/hooks/{request_id}/resolve, clearing the card
// optimistically and RESTORING it on a non-2xx before rethrowing — the tool call is still
// parked when the server refuses, so an emptied banner would be a lie. `updatedInput`
// replaces the tool input wholesale: it is how a source="user_input" hook's answers reach
// the model.
export function usePendingPermissions(sessionId: string | null): {
  pending: PendingHook[];
  resolve: (input: HookResolveInput) => Promise<void>;
};

// The session's spend halt, plus the one control that lifts it.
//
// llm-bridge-server stops a session that has spent its ceiling and then refuses every
// send, resume and mode switch with a 402 (writeRefusalIfOverBudget,
// internal/server/sessions.go). Neither half of that produces anything else a client can
// see, so without this surface a halted session reads as a hung one — the wrong
// conclusion, because it is fine and waiting on a number.
//
// halt is null for every session under its ceiling, every session without one, and every
// server that predates the gate. It is set from two places, and only one of them carries
// numbers: the 402 body names both dollar figures, the mid-turn error event carries a
// sentence and none.
//
// raiseCeiling POSTs /sessions/{id}/config with the new max_budget and clears the halt.
// It REPORTS the server's refusal text rather than throwing, and clears nothing when the
// server refused — silence would read as "raised" and the next send would be refused
// again.
export function useBudgetHalt(sessionId: string | null): {
  halt: BudgetHalt | null;
  raiseCeiling: (maxBudgetUSD: number) => Promise<string | null>;
};

export interface BudgetHalt {
  sessionId: string;
  message: string;      // always present: the server's own words
  spendUSD?: number;    // the 402 half only
  maxBudgetUSD?: number;// the 402 half only
}

// Every non-2xx throws this. `message` is the exact text it always was, so anything
// rendering e.message is unchanged; `status` and `body` are there because some refusals
// mean something specific and picking the JSON back out of an English string is guessing.
export class ApiError extends Error {
  readonly status: number;
  readonly body: string;   // response text verbatim, '' when unreadable
  readonly method: string;
  readonly path: string;
}

// One hook parked on a decision. `input` is the harness's own raw tool input, carried
// through untouched. `source` picks the card: HOOK_SOURCE_PERMISSION ("permission_prompt",
// and the empty default) is an allow/deny tool gate; HOOK_SOURCE_USER_INPUT ("user_input")
// is the model asking the human a structured question.
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
export interface HookResolveInput {
  requestId: string;
  behavior: 'allow' | 'deny';
  updatedInput?: unknown;
  message?: string;
  resolvedBy?: string;   // audit label; defaults to "user"
}

// The full detail carried by useManagedSession / ApiClient.getSessionDetail. `sessionId`
// is surfaced at the top level (also on `summary`). `info` / `harnessConfig` are null
// when the harness has reported / carries none.
export interface ManagedSessionDetail {
  sessionId: string;
  summary: SessionSummary;
  info: SessionInfo | null;
  harnessConfig: HarnessConfig | null;
}
// The per-harness config bag (wire `harness_config`, opaque json.RawMessage on the Go
// side). Bridge-owned well-known keys are surfaced camelCase; the index signature carries
// any unnamed harness-specific knob through unchanged (lossless). `permissionMode` is what
// the selector reads/writes; `disableNetwork` + `permissionModeCustom` are the sandbox /
// custom-mode knobs; `model` / `effort` are the per-harness defaults snapshot.
export interface HarnessConfig {
  permissionMode?: string;
  disableNetwork?: boolean;
  permissionModeCustom?: HarnessConfigCustom;
  model?: string;
  effort?: string;
  [k: string]: unknown;
}
export interface HarnessConfigCustom { approval?: string; sandbox?: string; }

// Prefetch hint (call on sidebar row hover) — warms a cold session so the click is instant.
export function usePrefetch(): (sessionId: string) => void;

// FilterState (BREAKING vs Phase 1): the faceted axes are now MULTI-SELECT `string[]`
// (was `string | null`), and a new `machine` axis is added. An EMPTY array means "no
// filter on this axis". Matching is OR *within* an axis (any selected value matches)
// and AND *across* axes (every non-empty axis must match). `folder` and `search` keep
// their scalar semantics. Dash consumers must migrate: `set({ harness: 'codex' })` →
// `set({ harness: ['codex'] })`, and read/toggle these as arrays.
//
// The `machine` axis matches `SessionSummary.instanceId` — the summary carries NO
// machine field, and instanceId is the value the dash resolves to a machine display
// name (bridge-ui `useBridgeMachines`). So the dash passes instanceId values here
// (grouped by machine on its side); `selectFacets().machine` is likewise keyed by
// instanceId. See "Canonical field flagged" in the handoff note.
//
// The `type` axis has ONE exception to "empty means no filter": with nothing selected
// on it, `matchesFilter` still drops the types in `DEFAULT_HIDDEN_SESSION_TYPES`
// (currently `external` — a session that ran outside the bridge and was imported from
// the harness's on-disk history, so it is not a chat anyone opened). Selecting any
// type makes the array rule alone, so the default is user-toggleable and nothing is
// persisted on the user's behalf.
export const DEFAULT_HIDDEN_SESSION_TYPES: ReadonlySet<string>;
export interface FilterState {
  harness: string[];
  status: string[];
  type: string[];
  purpose: string[];
  mode: string[];
  machine: string[];   // matches SessionSummary.instanceId (no machine field on summary)
  folder: string | null; // e.g. 'archive' — unchanged scalar
  search: string;         // unchanged
}

// Cross-axis facet counts over the FULL loaded session set (NOT the filtered list), so
// the sidebar can show every available option with its count and support cross-axis
// selection. `status` counts `SessionSummary.state`; `machine` counts `instanceId`.
// Empty-string axis values are skipped. Exposed via `useSessionList().facets` (below)
// and as the pure selector `selectFacets(state)`.
export interface Facets {
  harness: Record<string, number>;
  status: Record<string, number>;
  type: Record<string, number>;
  purpose: Record<string, number>;
  mode: Record<string, number>;
  machine: Record<string, number>;
}
export function selectFacets(state: ChatState): Facets;

// ---- Timeline pane selector (Path A) ----
// A pure, memoized transform of a materialized model into the event-granular,
// turn→task-grouped structure the Timeline pane renders. Returns DATA, not JSX, so
// the pane stays presentation-only and never re-derives. Memoized on model identity.
// Mirrors the grouping semantics of bridge-ui Timeline.tsx (group by turn, sub-group
// tool/thinking/result/error, respect task_* scoping); being the raw audit surface it
// represents every entry, ordered by eventId.
export function selectTimeline(model: TurnModel | undefined): TimelineView;

export interface TimelineView {
  items: TimelineItem[];          // flat, ordered — every event
  turns: TimelineTurnGroup[];     // turn → task grouped tree
  count: number;
}
export interface TimelineTurnGroup { turnId: string; header: TimelineItem; children: TimelineNode[]; }
export type TimelineNode =
  | { type: 'item'; item: TimelineItem }
  | { type: 'task'; taskId: string; header: TimelineItem; children: TimelineItem[] };
export interface TimelineItem {
  key: string; entryId: string; turnId: string; taskId?: string;
  icon: string; label: string; detail?: string; fullText?: string;
  ts: string; tone: TimelineTone;
}
export type TimelineTone =
  | 'turn' | 'task-start' | 'thinking' | 'tool' | 'tool-done' | 'tool-err'
  | 'result' | 'error' | 'system' | 'text';

// ---- Reference chips ----
// Pure matcher + a remark transformer (dependency-free). The React renderer,
// `RefChip`, is bridge-ui's since 2026-09-10 — this package ships no components;
// it exports the context (`ChatContext`, `useChatContext`) those components read.
// Wire into ReactMarkdown:  remarkPlugins={[remarkRefChips]} components={{ 'ref-chip': RefChip }}
//
// Verbatim nodes are skipped, for opposite reasons: `link` (a linkified id keeps its link
// — only remark-gfm makes a bare URL a link, so without it an id in a query string is cut
// out of the address) and `code`, the FENCED block, which holds a payload a reader copies
// out. `inlineCode` is NOT skipped: a single-backtick span is prose emphasis and setting
// an id apart with backticks is how people write one. In a mixed span the non-reference
// part stays code — `todo: <uuid>` becomes a code span reading "todo: " plus the chip.
//
// The matcher chips bare session ids (br_/herald-/autoworker- snowflakes) anywhere;
// noteboard uuids behind a cue word — note/workspace (kind 'note') or todo/item/card
// (kind 'todo'), each also with an `_id` suffix; a bare uuid as kind 'uuid', which the
// chip classifies through reference-store; `session_action_…` ids as kind 'action';
// and, when the host passes `resolvableIdPatterns`, every id matching one of them as kind
// 'registered' (`project_000001`, `prediction_000001`, …), classified the same way. The
// patterns come from reference-store's list (`useResolvableIdPatterns`, which reads its
// `GET /types` once per client), so no store prefix is written in this package.
// The cue only says where to LOOK; the loaded item's own `type` is the authority and the
// chip relabels itself from it.
//
// ⚠️ bridge-ui's RefChip reads ChatProvider context (it resolves ids against llm-bridge
// and noteboard through the loaders below), so it must be mounted inside one.
export function parseRefChips(value: string, options?: RefChipOptions): RefSegment[];
export function remarkRefChips(options?: RefChipOptions): (tree: unknown) => void;
export interface RefChipOptions { resolvableIdPatterns?: readonly string[] | null }
export function useResolvableIdPatterns(): readonly string[] | null;   // null until loaded
// reference-store through the bridge (ChatProvider's `referenceStoreBasePath`): the one
// list of reference types, and lookups for the signed-in person. A match from it carries
// `label`, `page_url`, or `refused: true` with nothing else. It is the only resolver: with
// no `referenceStoreBasePath`, an id with no chip of its own stays plain text.
export function useReferenceTypes(): ReadonlyMap<string, ReferenceType> | null;   // null until loaded
export class ReferenceStoreClient { constructor(config: { fetch: typeof fetch; basePath: string }); resolve(ids: string[]): Promise<ResolveResponse>; types(): Promise<ReferenceType[]> }
export function isResolvableReferenceType(type: ReferenceType): boolean;
export interface ReferenceType { type: string; service: string; description: string; get?: string; id_patterns?: string[]; search?: string; search_results?: string; label_field?: string; summary?: ReferenceSummaryField[]; page?: string; page_site?: string; glyph?: string; access: 'grant' | 'owner' }
export interface ReferenceSummaryField { field: string; label: string; format: ReferenceFieldFormat }
export type ReferenceFieldFormat = 'text' | 'number' | 'unix_seconds' | 'unix_milliseconds' | 'iso_time' | 'date' | 'cents' | 'probability' | 'url' | 'reference' | 'boolean';
// A record on another bridge server is written `<id>@<server_id>` (`server_` and a ULID):
// the chip segment, and the markdown chip's properties, then carry `serverId`.
export const ChatContext: React.Context<ChatContextValue | null>;
export function useChatContext(): ChatContextValue;   // throws outside ChatProvider
export type RefKind = 'session' | 'note' | 'todo' | 'uuid' | 'action' | 'registered';

// Detail loaders behind the chip panels. Every one dedupes by id through a 30s promise
// cache, because one id can mount dozens of chips in a single transcript. A rejection is
// never cached, so the next chip retries.
export function useSessionRefDetail(sessionId: string): RefDetailState<ManagedSessionDetail>;
export function useNoteboardRefDetail(itemId: string): RefDetailState<NoteboardItem>;
// The "fully load the history" affordance: fetches ONLY once `enabled` is true (the user
// expanded the panel), always with a limit on the wire. `model.more` means older turns
// exist beyond the window; this does not paginate.
export function useSessionRefTranscript(sessionId: string, enabled: boolean): RefDetailState<TurnModel>;
export function clearRefDetailCache(): void;   // tests only
export const REF_TRANSCRIPT_TURNS: number;     // 30
export interface RefDetailState<T> { data: T | null; error: string | null; loading: boolean }

// ---- Session signals (the open questions a session is waiting on) ----
// A signal is the canonical record of anything a session surfaces to a human: a question
// that needs an answer, or a notification that needs at most an acknowledgement
// (llm-bridge `msg/signal.go`). The cards that render one — `SignalCard`,
// `SignalRequestCard`, `SessionSignals`, `SignalRequestList` — are bridge-ui's since
// 2026-09-10, on the verbs and reads below; this package ships the data.
//
// Answering is ONE door: POST /signals/{id}/answer, keyed by signal id, whichever producer
// raised the question and whether or not the session is still running. The client used to
// choose the transport — refetch the parked tool input and post it to the hook route, or
// post text to /send — on evidence it does not have: a requestId says a park EXISTED, not
// that it is still live. The server picks now, and the title-keyed pairing a parked hook
// needs is derived where the parked input already lives. Dismiss is the SIGNAL-level verb
// (POST /signals/{id}/resolve {state:'dismissed'}), and the server reads it the same way:
// it denies a live parked call and closes a dead one. Acknowledge is that verb with
// `acknowledged`, which the server refuses for a QUESTION on purpose — a question nobody
// answered has not been handled.
//
// How a card answers:
//   pick-one     — plain buttons that SEND on the click, with no Submit and no radios: a
//                  radio holds a choice until a Submit, and there is none to hold it for.
//                  Only when the request holds this question and no other, because one
//                  AskUserQuestion call resolves ONCE with every answer together.
//   pick-many    — checkboxes plus a Submit, driven off `allowMultipleOptions` and never
//                  inferred from the option count. A pick-many answer is comma-joined,
//                  which is what the tool's own schema takes.
//   any option   — rewritable in place. The editor opens on the LABEL (the words you are
//                  amending; a machine `value` cannot be amended into a sentence) and the
//                  rewritten text is what goes on the wire. An untouched pick still sends
//                  `value || label`.
//   freeform     — a textarea, for every question that has no editable option to rewrite
//                  instead. Never gated on `allowFreeform`: `signalFromWire` defaults that
//                  to FALSE when the key is absent, so honoring it fails closed onto an
//                  unanswerable card.
//
// `compact` trims chrome (descriptions, body) for tight surfaces and never the means of
// answering. `startCollapsedToAnswers` opens a card on its answers alone, with a
// disclosure for the question — for a chat pane, whose transcript already carries the
// question directly above. That one also drops the freeform box wherever the question HAS
// options, because each option is editable. ⚠️ A host's composer is not the fallback: a
// bare POST /sessions/{id}/send deliberately leaves a tool-parked question open, because
// the harness is blocked on its hook and not on stdin.
//
// A 404 from /signals means this bridge-server predates the feature: the read answers
// `null` (never `[]`, which would say "deployed and quiet") and every surface renders
// nothing rather than erroring. Reads dedupe through a 30s promise cache like the chip
// loaders, and every resolve announces in-process so other mounted surfaces refetch.
export function useOpenSignals(sessionId?: string): OpenSignalsState;
export function clearOpenSignalsCache(): void;   // tests only
export interface OpenSignalsState {
  signals: Signal[]; requests: SignalRequest[];
  available: boolean;      // false once the server 404s — no signals route here
  loading: boolean; error: string | null; reload: () => void;
  resolve: (request: SignalRequest, answersBySignalId: Readonly<Record<string, SignalAnswer>>) => Promise<void>;
}
// The reads take every surface (GET /signals?state=open, no `surface`): an autonomous
// session's signals go to the `kanban` surface, and the chat lists them too, apart.
// Whose a signal or a waiting session is to answer is decided by SESSION TYPE, in one
// place: interactive and herald are the operator's; autonomous, system, external and an
// unknown or empty type are background. A surface that marks or counts "needs you" uses
// the operator group only; background is listed separately with its own count.
export function useOpenSignalsByAttention(): OpenSignalsByAttentionState;
export interface OpenSignalsByAttentionState {
  operator: SignalAttentionGroup; background: SignalAttentionGroup;   // { signals, requests }
  sessionTypeOf: (signal: Signal) => string;   // what the split used; '' = unknown
  available: boolean; loading: boolean; error: string | null; reload: () => void;
}
export function isOperatorSessionType(sessionType: string): boolean;     // '' and unknown → false
export function isAwaitingHumanState(state: string): boolean;            // awaiting_user | awaiting_permission
export function isSessionAwaitingOperator(session: { type: string; state: string }): boolean;
// The signal's own `session_type`, else the session row's type, else '' (background).
export function sessionTypeOfSignal(signal: Signal, sessionTypeOf: (sessionId: string) => string | undefined): string;
export function splitSignalsByAttention(signals: readonly Signal[], sessionTypeOf: (sessionId: string) => string | undefined): SignalsByAttention;
// The verbs, for a host rendering its own answer UI. Submit stays disabled until
// `everyQuestionAnswered` — one AskUserQuestion call resolves once, so a partial submit
// would answer some questions and discard the rest (`answerSignalRequest` enforces it too).
export function answerSignalRequest(api: ApiClient, request: SignalRequest, answersBySignalId: Readonly<Record<string, SignalAnswer>>): Promise<void>;
// What one composed answer becomes on the wire: the picked options comma-joined, or the
// typed text. Empty means unanswered — options OR text, never both.
export function answerTextOf(answer: SignalAnswerDraft | undefined): string;
export function acknowledgeSignal(api: ApiClient, signalId: string): Promise<void>;
export function dismissSignal(api: ApiClient, signalId: string): Promise<void>;
export function subscribeToSignalChanges(listener: () => void): () => void;
export function everyQuestionAnswered(request: SignalRequest, answersBySignalId: Readonly<Record<string, SignalAnswer>>): boolean;
export function questionsIn(request: SignalRequest): Signal[];
export function answerTextOf(answer: SignalAnswer | undefined): string;  // option OR text, never both
// Grouping key is request_id: one AskUserQuestion call mints one signal per question and
// resolves as a unit, so the REQUEST is what gets answered. A signal with no request_id
// gets a group of its own.
export function groupSignalsByRequest(signals: readonly Signal[]): SignalRequest[];
export function signalFromWire(w: SignalWire): Signal;
export interface SignalRequest { requestId: string; sessionId: string; signals: Signal[] }

// ---- NoteboardClient ----
// A SECOND backend, not part of the bridge gateway, so it gets its own client rather than
// a method on ApiClient. Read-only and deliberately narrow: ref chips are the only reason
// chat-core talks to noteboard. `getItem` throws loud on non-2xx; a DELETED item still
// resolves (noteboard's delete is reversible), carrying `deleted_at`.
export class NoteboardClient {
  constructor(config: NoteboardClientConfig);
  getItem(id: string): Promise<NoteboardItem>;
}

// ---- ApiClient additions ----
// interrupt() fails LOUD (throws on non-2xx, incl. the 409 "nothing was stopped");
// search() returns the session ids whose transcript text matched, for filter folding.
// getSessionDetail() GETs the full ManagedSession from GET /sessions/{id} and maps its
// snake_case `info` → camelCase `SessionInfo` (info=null when the harness reported none)
// AND its snake_case `harness_config` → camelCase `HarnessConfig` (null when absent); it
// backs useSessionInfo + useManagedSession. setPermissionMode() PUTs
// /sessions/{id}/permission-mode with `{ mode }` (fails loud on non-2xx). All throw loud
// on non-2xx like the rest.
// class ApiClient {
//   interrupt(id: string): Promise<unknown>;
//   search(q: string): Promise<SearchResponse>;
//   getSessionDetail(id: string): Promise<ManagedSessionDetail>;  // { sessionId, summary, info, harnessConfig }
//   setPermissionMode(id: string, mode: string): Promise<unknown>;
//   getPendingHooks(id: string): Promise<PendingHook[]>;                      // GET /sessions/{id}/hooks/pending — unwraps msg.Event.hook, keeps resolvable awaiting entries
//   resolveHook(id: string, input: HookResolveInput): Promise<unknown>;       // POST /sessions/{id}/hooks/{request_id}/resolve — { behavior, resolved_by, updated_input?, message? }
// }
```

### ApiClient — controls-bar additions (all LOUD: throw on any non-2xx)

Mirror the exact canonical bridge endpoints (verified against bridge-ui `useBridgeSession.ts`).
`createSession`/`fork` read the id from the canonical `session_id` wire key (POST /sessions and
/fork return the snake_case `msg.ManagedSession`; there is no `sessionId` on that shape).

```ts
// class ApiClient {
//   createSession(opts?: { instanceId?; harness? }): Promise<CreatedSession>; // maps session_id → sessionId
//   compact(id: string, summary?: string): Promise<unknown>;                   // POST /sessions/{id}/compact — {} or { summary }
//   fork(id: string, displayName?: string): Promise<CreatedSession>;           // POST /sessions/{id}/fork — { display_name, type:'interactive' }
//   switchMode(id: string, mode: 'events'|'pty'): Promise<unknown>;            // POST /sessions/{id}/mode — { mode }
//   setConfig(id: string, config: SessionConfig): Promise<unknown>;           // POST /sessions/{id}/config — { model?, effort?, max_budget?, disabled_tools? }
//   getHarnesses(): Promise<HarnessMeta[]>;                                     // GET /harnesses (canonical harness registry)
//   getModels(): Promise<ModelOption[]>;                                       // GET /models (enabled rows → ModelOption)
//   listSessionActions(id: string): Promise<SessionAction[]>;                 // GET /sessions/{id}/actions
//   runSessionAction(id: string, actionId: string): Promise<SessionAction>;   // POST /sessions/{id}/actions/{action_id}/run — 409 if already run or changed
// }
```

Session actions — buttons an agent put in its chat (`SessionAction`, `SessionActionOffer`,
`SessionActionState`, `SessionActionType`; entry kind `'action'`):
`newestSessionActions(entries) → Map<action_id, SessionAction>` gives each action's current
record, since every change is its own entry.
`sessionActionIdsPlacedInProse(entries) → Set<action_id>` names the actions whose id the agent
wrote in its reply text, where the chat draws the button; `RefKind` `'action'` is that id in text.

Pure, framework-free selectors (exported for non-React consumers/tests):
`harnessCapabilities(harnesses, harnessId) → Set<string>` and
`modelsForHarness(models, harnesses, harnessId?) → ModelOption[]` — both read the canonical
registry lists, never a hardcoded allowlist.

Notes for implementers:
- All hooks read via Zustand selector subscriptions so only components whose slice changed
  re-render.
- `select()` / filter changes / `newSession` / `archive` must never await the network on the
  path that updates the UI.
- `useTurns(view:'raw')` and `sourcesFor` are the audit surface — they must expose every
  stored copy (see WIRE.md non-destructive dedup).
- Pure reconcile helpers are also exported for non-React consumers/tests:
  `terminalStateFromTail(model)` and `effectiveState(state, sessionId)` /
  `activeSummaryEffective(state)` (see WIRE.md "Client terminal-state reconcile").
