import { createStore, type StoreApi } from 'zustand/vanilla';
import type {
  Entry,
  HarnessConfig,
  HarnessMeta,
  ManagedSessionDetail,
  ModelOption,
  PendingHook,
  SearchHit,
  SessionInfo,
  SessionStatus,
  SessionSummary,
  Turn,
  TurnModel,
} from '../net/types.js';
import type { WireEvent } from '../net/wireEvents.js';
import { sessionInfoFromWire } from '../net/sessionInfo.js';
import {
  annotateTail,
  carryForwardAggregates,
  foldEventWithoutAnnotating,
  initTailState,
  mergeMaterializedPage,
  normalizeText,
  reseedTailKeepingFoldHistory,
  type TailState,
} from '../reduce/TurnReducer.js';
import { foldHookEvent } from './pendingHooks.js';
import { isRunningState } from './sessionStates.js';
import { newerSessionStatus, withNewestStatus } from './sessionStatus.js';
import { budgetHaltFromEvent, type BudgetHalt } from './budgetHalt.js';
import { DraftStore, defaultDraftStorage, type DraftStorageLike } from './draftStorage.js';
import { FilterStore, PERSISTED_FILTER_AXES } from './filterStorage.js';
import { defaultWebStorage, type WebStorageLike } from './webStorage.js';

// L1 hot store (decision D2). Zustand vanilla store held in `Map`s for the
// working set. ACTIONS ARE THE ONLY MUTATION PATH — both the SyncEngine and the
// UI go through them, so the optimistic and reconciled paths converge on one
// model. Selector subscriptions (see react/hooks.ts) mean only components whose
// slice changed re-render.

export type ConnState = 'idle' | 'connecting' | 'open' | 'closed';

/**
 * Sidebar filter state. Each faceted axis (`harness`, `status`, `type`, `purpose`,
 * `mode`, `machine`) is now MULTI-SELECT: a `string[]` where an empty array means
 * "no filter on this axis". Matching is OR **within** an axis (any selected value
 * matches) and AND **across** axes (every non-empty axis must match) — see
 * `matchesFilter`. `folder` and `search` keep their scalar semantics.
 *
 * The `machine` axis matches `SessionSummary.instanceId`: the summary carries no
 * machine field, and instanceId is the value the dash resolves to a machine display
 * name (via bridge-ui `useBridgeMachines`). So the dash passes instanceId values here
 * (grouped by machine on its side) — `selectFacets().machine` is likewise keyed by
 * instanceId.
 */
export interface FilterState {
  harness: string[];
  status: string[];
  type: string[];
  purpose: string[];
  mode: string[];
  machine: string[];
  /** Principal ids the sessions were started as (`SessionSummary.principalId`);
   *  '' selects the sessions started as no principal. */
  principal: string[];
  folder: string | null;
  search: string;
}

export const EMPTY_FILTER: FilterState = {
  harness: [],
  status: [],
  type: [],
  purpose: [],
  mode: [],
  machine: [],
  principal: [],
  folder: null,
  search: '',
};

/** Where a new chat is aimed and what it starts configured with.
 *
 *  `instanceId` / `harness` say WHERE the session is created; the other four are
 *  runtime settings the create call deliberately does not take (bridge-ui parity —
 *  `POST /sessions` carries the target only). They ride on the pending pane and are
 *  applied by a single `POST /sessions/{id}/config` right after the real session is
 *  lazily created on first send.
 *
 *  Two sources feed the settings and they are the same shape on purpose: the
 *  controls bar's pre-start picks, and the caller's saved per-harness defaults
 *  (dash resolves those from `bridge-prefs`). A pre-start pick beats a saved
 *  default; that precedence belongs to the caller, not here. */
export interface NewSessionOpts {
  instanceId?: string;
  harness?: string;
  /** Who the session is started as: a principal-store id (`principal_000001`), sent
   *  on the create itself as `principal_id`, not on the follow-up config call. The
   *  server checks it and, at spawn, offers only what the principal's grants name.
   *  Absent means no principal — every session before the field existed. */
  principalId?: string;
  model?: string;
  effort?: string;
  /** ⚠️ ZERO IS A REAL VALUE and means NO CEILING on the server — never fold it in
   *  with absent. Absent means "the server decides"; 0 means "do not stop me". */
  maxBudget?: number;
  /** An EMPTY ARRAY is also a real value: "disable nothing", which is not the same
   *  answer as absent ("inherit whatever the harness does by default"). */
  disabledTools?: string[];
}

/** A pending (not-yet-created) session pane — 0 network until first send. Carries the
 *  `NewSessionOpts` its lazy create and follow-up config call will use. */
export interface PendingSession extends NewSessionOpts {
  clientId: string;
}

/** Content-search augmentation (C6). The set of session ids whose materialized
 *  transcript text matched `query`, fetched async via `ApiClient.search`. `query`
 *  pins the hits to the filter they were fetched for, so a stale set is never
 *  folded into a newer search. The instant local name match never waits on this.
 *
 *  `query` is always the TRIMMED search string. `filter.search` keeps whatever the
 *  user typed, spaces and all; every comparison against it here and in
 *  `matchesFilter` trims first. They used to be compared raw against a trimmed
 *  `query`, so a query with a leading or trailing space failed the equality check
 *  and its hits were dropped on arrival with no error. */
export interface ContentHits {
  query: string;
  /** How many events of that session's transcript matched, by session id — the
   *  whole `SearchHit` list, keyed for lookup rather than reduced to membership.
   *
   *  This used to be a bare `Set<string>` of the ids, which threw away the only
   *  ranking signal the backend offers: `ApiClient.search` sorts the hits by
   *  descending `match_count` and the store dropped the counts one call later, so
   *  the sidebar ordered content hits by recency and a session with one incidental
   *  match outranked the session the query was actually about.
   *
   *  A `Map` answers membership with `.has` and size with `.size` exactly as the
   *  Set did, so nothing that only asked "did this session match?" had to change. */
  matchCountBySessionId: Map<string, number>;
  /** How many hits the backend returned for `query` — `matchCountBySessionId.size`,
   *  kept explicitly because it is a count of what the SERVER found, while the
   *  number of rows the list can actually paint is bounded by the loaded window and
   *  is always ≤ this. */
  hitCount: number;
  /** True when the backend filled a whole page, so there are more matches it never
   *  sent. See `SearchResponse.truncated` — this is "at least", not a total. */
  truncated: boolean;
}

export interface ChatState {
  sessions: Map<string, SessionSummary>;
  turnsBySession: Map<string, TurnModel>;
  /** Lazily-fetched per-session detail (`ManagedSessionDetail`: summary + info +
   *  harnessConfig), keyed by session id — the ONE cache behind `useManagedSession`,
   *  `useSessionInfo` (which reads its `info`) and the permission-mode selector. A
   *  present key means it was fetched. Populated on first use, never on the hot path. */
  sessionDetail: Map<string, ManagedSessionDetail>;
  /** Session ids whose full-detail fetch is in flight (drives `useManagedSession` loading). */
  sessionDetailLoading: Set<string>;
  /** Internal live-tail reducer state per session (not for direct UI reads). */
  tails: Map<string, TailState>;
  /** Sessions whose UNPROJECTED page (`/messages/raw`) has been loaded.
   *
   *  `turnsBySession.has(id)` cannot answer this. The default page is projected —
   *  duplicates dropped and `raw` stripped — so a session loaded for the Turns view
   *  has a model that the Raw view and the Timeline would render incompletely, with
   *  nothing in the model itself saying so. This is that missing fact.
   *
   *  One-directional by design: the raw page is a SUPERSET, so a Turns view reading a
   *  raw-loaded model is already correct and never refetches. */
  rawTurnsLoaded: Set<string>;
  /** Hooks parked on a human decision, `sessionId -> requestId -> PendingHook`. A
   *  session with an entry here has a tool call frozen mid-turn; nothing else in the
   *  model shows it, so this map is the ONLY thing standing between a permission ask
   *  and a chat that hangs with no visible cause. Hydrated per session from
   *  `GET /sessions/{id}/hooks/pending` and kept live by the session SSE. */
  pendingHooks: Map<string, Map<string, PendingHook>>;
  /** Sessions stopped at their spend ceiling, `sessionId -> BudgetHalt`. A halted
   *  session refuses every send, resume and mode switch with a 402 until its ceiling
   *  moves, and nothing else in the model records that — the session just stops
   *  answering. Set from the 402 a refused request throws and from the mid-turn
   *  `budget_exceeded` error event; cleared when the ceiling is raised. */
  budgetHalts: Map<string, BudgetHalt>;
  /** Statuses that arrived for a session this client has no row for YET.
   *
   *  The open session's stream and the first list page start together, and the stream
   *  is often first: it replays the current turn, `session_status` events included,
   *  before the row exists. Dropping those left a row that arrived LATER but was
   *  OLDER (the list lags a seconds-old session's whole first turn) showing its stale
   *  state until the next status change. They wait here instead, and every path that
   *  stores a row weighs the waiting status against the row's own by `as_of`. */
  statusAwaitingRow: Map<string, SessionStatus>;
  /** What the sidebar ORDERS by, `sessionId -> RFC3339 stamp`. NOT `updatedAt`:
   *  the server bumps that on every event, so with several sessions running at
   *  once the rows leapfrogged each other continuously and the list could not be
   *  read while it was working. This stamp moves on exactly three facts:
   *
   *   - a session is seen for the FIRST time (seeded from its `updatedAt`, so a
   *     new session enters at its recency position — the top);
   *   - a session's TURN STARTS — its state enters the running set, which is what
   *     the user's own send or any other new work does first;
   *   - a session's TURN ENDS — its state leaves the running set — meaning the
   *     response's final text has landed in the chat.
   *
   *  Either change of state can arrive as a list upsert (`upsertSession`) or as a
   *  `session_status` on the session's live tail (`applySessionStatus`).
   *  Everything in between — stream deltas, tool calls, the running state moving
   *  between `model_generating` and `tool_running` — and renames leave the stamp
   *  where it was, and the row where it was.
   *
   *  Monotonic per session (advances by max), because the same ending can arrive
   *  on both wires and on a replayed stream, in any order. `updatedAt` itself is
   *  untouched: it is the server's fact, shown in the header and used for cache
   *  eviction, and this map exists precisely so ordering can differ from it. */
  listOrderStampBySession: Map<string, string>;
  activeId: string | null;
  filter: FilterState;
  /** Content-search hits for the current `filter.search`, or null when none have
   *  been fetched (or the query changed and the prior set was invalidated). */
  contentHits: ContentHits | null;
  /** The TRIMMED query a content search is currently running for — counting its
   *  debounce wait, because from the user's side the search has already been asked
   *  for — or null when none is running.
   *
   *  `contentHits === null` is NOT the same fact and cannot stand in for it: hits
   *  are also null before anyone has typed, and they stay null after a search that
   *  failed. Deriving "searching" from them would leave the sidebar saying
   *  "searching…" forever the first time the gateway refuses a query. */
  contentSearchInFlight: string | null;
  /** Why the last content search for the live query failed, or null. A failed
   *  transcript search is not zero matches — the list still shows every local name
   *  match — so it is reported rather than folded into an empty hit set. */
  contentSearchError: string | null;
  /** The folders the server holds, IN THE ORDER IT KEEPS THEM (`GET /folders`).
   *
   *  Not derived from the loaded sessions. A folder is a row of its own: it exists
   *  before anything is filed into it and it survives the last session leaving it,
   *  so a list scraped off the session window can neither order the folders the way
   *  the user arranged them nor represent an empty one at all. `visibleSessions`
   *  reads this to seed its groups.
   *
   *  Empty until the first `/folders` response lands (or if that read fails), which
   *  is a real state and not a bug: grouping falls back to the loaded sessions'
   *  own folder names in recency order rather than to an empty sidebar. */
  folders: string[];
  connState: ConnState;
  listLoading: boolean;
  /** The opaque cursor for the page of sessions OLDER than everything loaded, taken
   *  from the last summary response's `next`. `null` means there is no older page —
   *  either the server said so (a short page), or no page has landed yet. This is the
   *  only "is the sidebar truncated?" signal that exists: the summary endpoint reports
   *  no total, so a non-null cursor means "at least one more page", never "N more". */
  olderSessionsCursor: string | null;
  /** True while the older-sessions page fetch is in flight. Guards the fetch against a
   *  second scroll/click firing before the first lands. */
  olderSessionsLoading: boolean;
  turnsLoading: Set<string>;
  moreBySession: Map<string, boolean>;
  /** Unsent composer text, keyed by session id — plus one entry under the pending
   *  pane's key (`PENDING_DRAFT_KEY` in react/hooks.ts) for a chat that has no session
   *  yet. Persisted to localStorage by `draftStorage.ts` and read back synchronously
   *  at store construction, so a reload does not eat a half-typed message. */
  drafts: Map<string, string>;
  sending: Set<string>;
  pending: PendingSession | null;

  /** The harness registry from `GET /harnesses` — the canonical source the controls bar
   *  gates on (`capabilities`) and scopes the model picker with (`supportedProviders`).
   *  `null` means "not fetched yet"; `useHarnessCapabilities` / `useModels` fetch it once
   *  on first use, never on the hot path. */
  harnesses: HarnessMeta[] | null;
  /** True while the one-shot `GET /harnesses` fetch is in flight. */
  harnessesLoading: boolean;
  /** The model registry from `GET /models` (enabled rows only), projected to
   *  `ModelOption`s. `null` means "not fetched yet"; `useModels` fetches it once on first
   *  use. */
  models: ModelOption[] | null;
  /** True while the one-shot `GET /models` fetch is in flight. */
  modelsLoading: boolean;

  actions: ChatActions;
}

export interface ChatActions {
  setConn(state: ConnState): void;
  setListLoading(loading: boolean): void;

  /** Replace the loaded window with `list` and re-anchor the older-sessions cursor to
   *  `olderSessionsCursor`. Replace, not merge, is deliberate: the first page IS the
   *  window, so a session the server no longer returns must leave the sidebar. That
   *  also means any older page loaded earlier is dropped — correct, because its cursor
   *  is re-anchored in the same call and the user can page down again. */
  setSessions(list: SessionSummary[], olderSessionsCursor?: string | null): void;
  /** Merge one page of OLDER sessions into the loaded window and advance the cursor.
   *  Merges (never replaces) so the newer pages already on screen survive, and clears
   *  `olderSessionsLoading`. */
  appendOlderSessions(list: SessionSummary[], olderSessionsCursor: string | null): void;
  /** Merge sessions into the window without touching the paging cursor — a lookup by
   *  id, or a page for a filter that is no longer the one being paged. */
  mergeSessions(list: SessionSummary[]): void;
  /** Replace the paging cursor: the first page of a newly set filter has landed. */
  setOlderSessionsCursor(olderSessionsCursor: string | null): void;
  setOlderSessionsLoading(loading: boolean): void;
  upsertSession(summary: SessionSummary): void;
  /** Apply a status that arrived on its own — a `session_status` event on the open
   *  session's stream. Kept only if it is newer (`as_of`) than the one the session
   *  already carries; `state` follows it. Reaches a session the list does not hold
   *  (one opened by id) through its cached detail. A no-op for an unknown session. */
  applySessionStatus(sessionId: string, status: SessionStatus): void;
  /** Replace a session's `info` with one that arrived live — a `session_info` event.
   *  The harness sends its whole SessionInfo each time (the claudecode harness sends
   *  one after init and again when Claude Code reports a new effort), and the server
   *  stores it the same way. A no-op until the session's detail has been fetched: the
   *  fetch returns the stored info, which is at least as new. */
  applySessionInfo(sessionId: string, info: SessionInfo): void;
  removeSession(sessionId: string): void;

  /** Replace the folder list with what `GET /folders` returned, order intact.
   *  The server's answer is the whole truth here — this never merges with what the
   *  loaded sessions happen to mention, or a folder deleted on the server would
   *  live on in the sidebar for as long as one session still pointed at it. */
  setFolders(folders: string[]): void;

  setActive(sessionId: string | null): void;

  /** Install a freshly materialized page as the session's model.
   *
   *  Replaces everything EXCEPT `aggregates`, which is last-value-wins session state
   *  rather than a property of the page: log-store computes it only from the events the
   *  page returned and omits it entirely when that page held none, so a page without
   *  spend events on it must not be allowed to report "$0.00" for a session that has
   *  spent money. See `carryForwardAggregates`. */
  setTurns(
    sessionId: string,
    model: TurnModel,
    opts?: {
      raw?: boolean;
    },
  ): void;
  applyTailEvent(sessionId: string, event: WireEvent): void;
  /** Fold a batch of live frames and notify subscribers ONCE — see the implementation
   *  for why the batch is load-bearing rather than a convenience. */
  applyTailEvents(sessionId: string, events: WireEvent[]): void;
  setTurnsLoading(sessionId: string, loading: boolean): void;
  /** Merge an OLDER page in front of the loaded model (backwards pagination). Same
   *  `aggregates` rule as `setTurns`, in the other direction: the older page can fill a
   *  roll-up that was never known, but never overwrite the one already on screen. */
  prependOlder(sessionId: string, older: TurnModel): void;

  /** Cache a session's fetched FULL detail (summary + info + harnessConfig); clears the
   *  detail-loading flag. Backs `useManagedSession`. */
  setSessionDetail(sessionId: string, detail: ManagedSessionDetail): void;
  setSessionDetailLoading(sessionId: string, loading: boolean): void;
  /** Optimistically merge a patch into a cached detail's `harnessConfig` (e.g. a
   *  permission-mode change). No-op if the detail isn't cached yet. Returns the prior
   *  `harnessConfig` (or null) so the caller can revert on a failed PUT. */
  patchHarnessConfig(sessionId: string, patch: Partial<HarnessConfig>): HarnessConfig | null;

  /** Replace a session's parked-hook set (the `/hooks/pending` hydration). */
  setPendingHooks(sessionId: string, hooks: PendingHook[]): void;
  /** Insert or refresh one parked hook (an `awaiting_resolution` event, or the revert
   *  of a refused resolve). */
  upsertPendingHook(sessionId: string, hook: PendingHook): void;
  /** Drop one parked hook (a `completed` event, or an optimistic resolve). */
  clearPendingHook(sessionId: string, requestId: string): void;

  /** Record that a session stopped at its spend ceiling. Replaces any halt already
   *  held for that session: raising the ceiling and breaching the new one later is a
   *  different halt with different figures. */
  setBudgetHalt(halt: BudgetHalt): void;
  /** Drop a session's spend halt — the ceiling moved and the session can run again.
   *  No-op when that session has no halt. */
  clearBudgetHalt(sessionId: string): void;

  appendOptimisticUser(sessionId: string, text: string, clientId: string): void;
  /** Remove the optimistic user row `appendOptimisticUser` added, when the send it
   *  was betting on failed. Without this a refused message stays on screen looking
   *  sent. No-op when the row is already gone (the real `user_message` landed and
   *  reconciled it). */
  dropOptimisticUser(sessionId: string, clientId: string): void;

  setFilter(patch: Partial<FilterState>): void;
  openFolder(folder: string): void;
  /** Record async content-search hits for a query. `query` must be the TRIMMED
   *  search string; it is compared against the trimmed live filter, so a late or
   *  stale response can't override a newer search. `truncated` says the backend
   *  had more matches than it returned.
   *
   *  Takes the whole `SearchHit[]`, not just the ids: `match_count` is the only
   *  ranking signal the search endpoint reports, and the sidebar cannot order
   *  content-only hits without it. */
  setContentHits(query: string, hits: SearchHit[], truncated?: boolean): void;
  /** Record that a content search for the TRIMMED `query` has been asked for. Call
   *  it when the query is typed, not when the request leaves — the debounce wait is
   *  part of the wait the user is looking at. `null` means no search is running.
   *  Clears any previous error. */
  startContentSearch(query: string | null): void;
  /** Record that the content search for `query` ended with no hits to fold in:
   *  `error` non-null when it failed, null when it was canceled before firing.
   *  Ignored unless `query` is still the one in flight, so a late failure can never
   *  clear the search that replaced it. */
  endContentSearch(query: string, error: string | null): void;

  /** Record unsent composer text and persist it. Setting `''` deletes the persisted
   *  copy — an empty box is the absence of a draft, not a draft that is empty. */
  setDraft(sessionId: string, text: string): void;
  setSending(sessionId: string, sending: boolean): void;

  openPending(opts?: NewSessionOpts): PendingSession;
  /** Change settings on the pending pane that is ALREADY open, without re-keying it.
   *
   *  `openPending` is not a substitute, for two reasons that survive reading it:
   *
   *    - it REPLACES rather than merges — the pane it returns is built from the opts it
   *      was handed and nothing else, so recording a model pick through it would drop
   *      the instance, harness, ceiling and disabled-tool list the pane already carried;
   *    - it sets `activeId` to null, because opening a new chat takes the focus.
   *      Touching a select must not knock the user out of a session they clicked into.
   *
   *  It also mints a fresh `clientId`, which is the pane's identity and should not churn
   *  every time a select moves. (Note that the draft is NOT at risk here, contrary to
   *  what `9fd9df44` recorded: an unsent pending pane's text is keyed by the CONSTANT
   *  `PENDING_DRAFT_KEY`, not by `clientId`, so re-opening never moved it. The two
   *  reasons above are enough on their own.)
   *
   *  A key the patch does not mention is left alone. `model` and `effort` given as an
   *  EMPTY STRING are a deliberate clear — that is the value the placeholder option of a
   *  select carries, and "no model chosen" has to be expressible or a pre-start pick
   *  could never be taken back. `maxBudget` and `disabledTools` test `!== undefined`
   *  instead, because 0 (no ceiling) and [] (disable nothing) are real answers.
   *
   *  Returns the patched pane, or null when no pending pane is open — a pane that does
   *  not exist cannot be configured, and inventing one here would open a new chat as a
   *  side effect of touching a select. */
  patchPending(patch: NewSessionOpts): PendingSession | null;
  clearPending(): void;

  /** Cache the harness registry (`GET /harnesses`); clears the loading flag. */
  setHarnesses(list: HarnessMeta[]): void;
  setHarnessesLoading(loading: boolean): void;
  /** Cache the model registry (`GET /models`, enabled rows); clears the loading flag. */
  setModels(list: ModelOption[]): void;
  setModelsLoading(loading: boolean): void;
}

export type ChatStoreApi = StoreApi<ChatState>;

/** One shared empty map for sessions with no parked hook. Reusing the reference keeps
 *  `usePendingPermissions`'s selector stable, so a session that never parks a hook never
 *  re-renders the banner. */
export const EMPTY_HOOKS: ReadonlyMap<string, PendingHook> = new Map();

/** Whether a session's state has just crossed into or out of the running set —
 *  a turn starting or a turn ending, the two state changes that move a session's
 *  sidebar order stamp. A first-seen session has no previous state and is seeded
 *  by the caller instead. */
function turnStartedOrEndedBetween(previousState: string | undefined, nextState: string): boolean {
  return previousState !== undefined && isRunningState(previousState) !== isRunningState(nextState);
}

function getOrInitTail(state: ChatState, sessionId: string): TailState {
  const existing = state.tails.get(sessionId);
  if (existing) return existing;
  return initTailState(sessionId, state.turnsBySession.get(sessionId));
}

/**
 * How much transcript the in-memory store keeps warm, measured in characters of
 * payload rather than in sessions.
 *
 * BYTES, NOT A SESSION COUNT, and that is the whole point. Transcript size varies by
 * more than 10× between sessions on this box — measured 2026-08-25 across the 40 most
 * recent, one `messages?limit=30` page is 1.06 MB of JSON at the median and 10.1 MB at
 * the worst, and a single session's last TEN turns can be 4.2 MB. A bound counted in
 * sessions is therefore the same mistake one layer up that caused the original bug:
 * `ApiClient.DEFAULT_MESSAGE_TURNS` bounds the request to 30 MESSAGES, and 30 messages
 * carrying big tool results is 10 MB. Counting the wrong unit is how the heap reached
 * the hundreds of megabytes that crashed the tab.
 *
 * A byte budget adapts instead of guessing. Someone working across a dozen ordinary
 * sessions keeps all twelve warm and instant; someone opening three enormous ones keeps
 * three. Either way the ceiling is the same and the tab cannot be grown out of memory.
 *
 * This is the L1 working set, NOT a cache — the IndexedDB layer below holds
 * `DEFAULT_CACHE_LIMIT` (50) sessions, so an evicted session repaints from there rather
 * than from the network.
 *
 * 32M characters is roughly 32 MB of text, which holds ~30 median sessions or ~3 of the
 * worst — comfortably more than anyone flips between, and a fraction of the heap the
 * unbounded store reached.
 */
export const DEFAULT_TURN_RETENTION_BYTES = 32_000_000;

/**
 * Sessions kept warm no matter what the byte budget says.
 *
 * Without a floor the budget alone has a bad failure mode: three 10 MB sessions
 * overshoot 32M characters, so switching between the two the user is actually working
 * across would evict and refetch on every switch — the slowest possible behavior
 * arriving exactly when the transcripts are biggest and refetching hurts most.
 *
 * Four covers "the thing I am doing", "the thing I was doing", and the two either side.
 * The worst case it admits is bounded and known: four times the biggest transcript.
 */
export const DEFAULT_TURN_RETENTION_MIN_SESSIONS = 4;

export interface CreateChatStoreOptions {
  /** Characters of transcript payload to keep warm in memory — see
   *  `DEFAULT_TURN_RETENTION_BYTES`. */
  turnRetentionBytes?: number;
  /** Sessions kept warm regardless of the byte budget — see
   *  `DEFAULT_TURN_RETENTION_MIN_SESSIONS`. The active session is always kept, so even
   *  a floor of 0 retains one. */
  turnRetentionMinSessions?: number;
  /** Where composer drafts are persisted. Defaults to the browser's `localStorage`
   *  (and to no persistence anywhere it does not exist — node, SSR, a test). Pass
   *  `null` to turn persistence off explicitly, or a `DraftStorageLike` to point it
   *  somewhere else. */
  draftStorage?: DraftStorageLike | null;
  /** Where the sidebar's filter selection is persisted. Same defaulting as
   *  `draftStorage`, and kept as a separate option so a caller can persist one and
   *  not the other. */
  filterStorage?: WebStorageLike | null;
}

/** The session window with `list` merged in: one Map copy for the whole page — a
 *  per-row `upsertSession` loop would copy it once per row. A row that arrived live
 *  over SSE while the page was in flight is newer than the page, so its fields stay
 *  on top; a new row takes its order stamp from its own `updatedAt`. */
/** What `withNewestStatus` should weigh an incoming row against: the row already
 *  held, or — when there is none — a status that arrived before any row did. */
function heldOrAwaiting(
  state: Pick<ChatState, 'statusAwaitingRow'>,
  sessionId: string,
  row: SessionSummary | undefined,
): SessionSummary | undefined {
  if (row) return row;
  const status = state.statusAwaitingRow.get(sessionId);
  // Only `status` is read off a held row that is not a real one.
  return status ? ({ status } as SessionSummary) : undefined;
}

function mergedSessions(
  state: Pick<ChatState, 'sessions' | 'listOrderStampBySession' | 'statusAwaitingRow'>,
  list: SessionSummary[],
): Pick<ChatState, 'sessions' | 'listOrderStampBySession'> {
  const sessions = new Map(state.sessions);
  const listOrderStampBySession = new Map(state.listOrderStampBySession);
  for (const s of list) {
    const prev = sessions.get(s.sessionId);
    // `prev`'s fields stay on top, except the status, which is decided by `as_of`
    // and not by which of the two arrived first.
    sessions.set(s.sessionId, withNewestStatus(heldOrAwaiting(state, s.sessionId, prev), s, true));
    if (!listOrderStampBySession.has(s.sessionId)) {
      listOrderStampBySession.set(s.sessionId, s.updatedAt);
    }
  }
  return { sessions, listOrderStampBySession };
}

export function createChatStore(options: CreateChatStoreOptions = {}): ChatStoreApi {
  const draftStore = new DraftStore(
    options.draftStorage === undefined ? defaultDraftStorage() : options.draftStorage,
  );
  const filterStore = new FilterStore(
    options.filterStorage === undefined ? defaultWebStorage() : options.filterStorage,
  );
  // Read synchronously, BEFORE the store exists, so the drafts are in the very first
  // state the UI ever sees. An async hydrate would land after the composer is already
  // typeable and would race the user's keystrokes.
  const persistedDrafts = draftStore.load();
  // Same reasoning, and it matters more here: a filter applied one paint late means
  // the list is drawn unfiltered and then rows are pulled out from under a user who
  // has already started reading it. `search` and `folder` are never restored — see
  // filterStorage.ts.
  const persistedFilterAxes = filterStore.load();
  const turnRetentionBytes = options.turnRetentionBytes ?? DEFAULT_TURN_RETENTION_BYTES;
  const turnRetentionMinSessions =
    options.turnRetentionMinSessions ?? DEFAULT_TURN_RETENTION_MIN_SESSIONS;

  return createStore<ChatState>((set, get) => {
    // Session ids most-recently-used first — the eviction order for the two heavy
    // maps. Kept in the closure rather than in `ChatState` because it is bookkeeping
    // that no component reads; putting it in state would wake every subscriber on
    // every switch to publish a fact none of them use.
    //
    // Recency of USE, not of insertion. `turnsBySession` is a Map and its own key
    // order is insertion order — the sessions opened longest ago — so an evictor
    // reading the Map's keys drops exactly the sessions the user is flipping between
    // and keeps the ones they have finished with.
    let recency: string[] = [];

    /** Move `sessionId` to the front of the eviction order. */
    function touch(sessionId: string): void {
      if (recency[0] === sessionId) return;
      recency = [sessionId, ...recency.filter((id) => id !== sessionId)];
    }

    /**
     * Roughly how much memory a model's payload occupies, in characters.
     *
     * An estimate on purpose. `JSON.stringify` would be exact and costs a full
     * serialization of a multi-megabyte object on every page that lands — paying the
     * parse twice to measure what we only need to compare against a budget. This walks
     * the entries once and adds up the fields that actually carry the weight: rendered
     * text and string tool results. Everything else is a per-entry constant.
     *
     * `toolResult` is only counted when it is a string, which is what a shell or file
     * tool returns and where the megabytes live. A structured result is charged the flat
     * overhead rather than serialized to find out.
     */
    function estimateSize(model: TurnModel): number {
      let size = 0;
      for (const entry of Object.values(model.entries)) {
        size += entry.text?.length ?? 0;
        if (typeof entry.toolResult === 'string') size += entry.toolResult.length;
        // Ids, timestamps, roles, kinds — small, but there is one set per entry and a
        // transcript can hold thousands, so they are not nothing.
        size += 200;
      }
      return size;
    }

    /** Estimated size per retained session, so the budget is not recomputed over the
     *  whole working set every time one session changes. */
    const sizeBySession = new Map<string, number>();

    /**
     * Evict least-recently-used transcripts until the working set fits the byte budget,
     * and return the trimmed maps.
     *
     * Three things are never evicted, in order of precedence:
     *
     *  1. The ACTIVE session. It is the one on screen; dropping it blanks the transcript
     *     being read and costs a refetch to put it back.
     *  2. The `turnRetentionMinSessions` most recently used. This is what makes flipping
     *     between the handful of sessions someone is working across instant even when
     *     those sessions are individually enormous — see the constant for why a budget
     *     alone gets that case exactly backwards.
     *  3. Anything that still fits inside the budget.
     *
     * ONLY the transcript goes. The summary stays (the sidebar needs every row it has
     * loaded, and a row is a few hundred bytes against a megabyte of transcript), the
     * draft stays (unsent user text — losing it is data loss), and so do the parked hooks
     * and the budget halt. This is a memory bound, not a delete.
     *
     * The model and its tail always go together. `TailState` holds the model plus
     * `turnIndex`, `entryEventIds` and `seenEventIds`, so dropping `turnsBySession` alone
     * frees nothing — the tail still points at the same object graph.
     */
    function evictBeyondBudget(
      turnsBySession: Map<string, TurnModel>,
      tails: Map<string, TailState>,
    ): {
      turnsBySession: Map<string, TurnModel>;
      tails: Map<string, TailState>;
      rawTurnsLoaded: Set<string>;
    } {
      const activeId = get().activeId;
      // Most-recently-used first. Anything `recency` never saw goes on the end as a
      // victim candidate: a missed `touch` must not quietly reintroduce unbounded growth.
      const order = [
        ...recency.filter((id) => turnsBySession.has(id)),
        ...[...turnsBySession.keys()].filter((id) => !recency.includes(id)),
      ];

      const victims: string[] = [];
      let spent = 0;
      let kept = 0;
      for (const id of order) {
        const size = sizeBySession.get(id) ?? estimateSize(turnsBySession.get(id) as TurnModel);
        sizeBySession.set(id, size);
        const pinned = id === activeId || kept < turnRetentionMinSessions;
        if (pinned || spent + size <= turnRetentionBytes) {
          spent += size;
          kept++;
          continue;
        }
        victims.push(id);
      }
      if (victims.length === 0) {
        return { turnsBySession, tails, rawTurnsLoaded: get().rawTurnsLoaded };
      }
      // `rawTurnsLoaded` goes with the model. It is a claim about what is IN MEMORY,
      // not about what was once fetched — leave it behind and the guard in `useTurns`
      // reads "already have the raw page" for a session whose model has been evicted,
      // so no fetch fires and the pane renders empty.
      //
      // RETURNED rather than `set()` here, and that is not style. Every caller finishes
      // with one `set({ ...evictBeyondBudget(...), … })`, so a write from inside would
      // be silently overwritten by whatever the caller had read before eviction ran —
      // which is exactly how the first version of this failed its own test.
      let rawTurnsLoaded = get().rawTurnsLoaded;
      let clearedAnyRaw = false;
      for (const id of victims) {
        turnsBySession.delete(id);
        tails.delete(id);
        sizeBySession.delete(id);
        if (rawTurnsLoaded.has(id)) {
          if (!clearedAnyRaw) {
            rawTurnsLoaded = new Set(rawTurnsLoaded);
            clearedAnyRaw = true;
          }
          rawTurnsLoaded.delete(id);
        }
      }
      recency = recency.filter((id) => turnsBySession.has(id));
      return { turnsBySession, tails, rawTurnsLoaded };
    }

    /** Record a session's fresh size and re-apply the budget. Called from every door a
     *  transcript can enter by, so no single path can grow the working set unchecked. */
    function retain(
      sessionId: string,
      turnsBySession: Map<string, TurnModel>,
      tails: Map<string, TailState>,
    ): {
      turnsBySession: Map<string, TurnModel>;
      tails: Map<string, TailState>;
      rawTurnsLoaded: Set<string>;
    } {
      touch(sessionId);
      const model = turnsBySession.get(sessionId);
      if (model) sizeBySession.set(sessionId, estimateSize(model));
      return evictBeyondBudget(turnsBySession, tails);
    }

    const actions: ChatActions = {
      setConn(connState) {
        set({ connState });
      },
      setListLoading(listLoading) {
        set({ listLoading });
      },

      setSessions(list, olderSessionsCursor = null) {
        const sessions = new Map<string, SessionSummary>();
        // The stamps are rebuilt with the list — a session no longer in it keeps no
        // stamp — but a stamp already held wins over re-seeding from `updatedAt`,
        // or every list refresh would snap the frozen order back to raw recency.
        const prior = get().listOrderStampBySession;
        const held = get().sessions;
        const listOrderStampBySession = new Map<string, string>();
        for (const s of list) {
          // The list is replaced, but a status already held may be newer than the
          // page's: a live event can land while the page is in flight.
          sessions.set(s.sessionId, withNewestStatus(heldOrAwaiting(get(), s.sessionId, held.get(s.sessionId)), s));
          listOrderStampBySession.set(s.sessionId, prior.get(s.sessionId) ?? s.updatedAt);
        }
        set({
          sessions,
          listOrderStampBySession,
          listLoading: false,
          olderSessionsCursor,
          olderSessionsLoading: false,
        });
      },

      appendOlderSessions(list, olderSessionsCursor) {
        set({
          ...mergedSessions(get(), list),
          olderSessionsCursor,
          olderSessionsLoading: false,
        });
      },

      mergeSessions(list) {
        if (list.length === 0) return;
        set(mergedSessions(get(), list));
      },

      setOlderSessionsCursor(olderSessionsCursor) {
        set({ olderSessionsCursor });
      },

      setOlderSessionsLoading(olderSessionsLoading) {
        set({ olderSessionsLoading });
      },

      upsertSession(summary) {
        const sessions = new Map(get().sessions);
        const prev = sessions.get(summary.sessionId);
        // Newest `as_of` wins, not newest arrival: this upsert may be older than a
        // `session_status` the open session's own stream has already delivered.
        summary = withNewestStatus(heldOrAwaiting(get(), summary.sessionId, prev), summary);
        sessions.set(summary.sessionId, summary);
        // The order stamp moves on exactly three upserts: a session seen for the first
        // time (it enters at its recency position), a turn starting, and a turn
        // ending. Every other upsert — and the server sends one per event while a
        // session works — changes the row, not the order.
        const priorStamps = get().listOrderStampBySession;
        const current = priorStamps.get(summary.sessionId);
        let stamp = current ?? summary.updatedAt;
        if (turnStartedOrEndedBetween(prev?.state, summary.state) && summary.updatedAt > stamp) {
          stamp = summary.updatedAt;
        }
        let listOrderStampBySession = priorStamps;
        if (stamp !== current) {
          listOrderStampBySession = new Map(priorStamps);
          listOrderStampBySession.set(summary.sessionId, stamp);
        }
        set({ sessions, listOrderStampBySession });
      },

      applySessionInfo(sessionId, info) {
        const prev = get().sessionDetail.get(sessionId);
        if (!prev) return;
        const sessionDetail = new Map(get().sessionDetail);
        sessionDetail.set(sessionId, { ...prev, info });
        set({ sessionDetail });
      },

      applySessionStatus(sessionId, status) {
        const listed = get().sessions.get(sessionId);
        if (listed) {
          if (newerSessionStatus(listed.status, status) !== status) return;
          if (listed.status?.as_of === status.as_of) return; // the same event, replayed
          const sessions = new Map(get().sessions);
          sessions.set(sessionId, { ...listed, status, state: status.state });
          // A turn starting or ending moves the sidebar order stamp, exactly as it
          // does when the change arrives as a list upsert — whichever wire carries it
          // first.
          let listOrderStampBySession = get().listOrderStampBySession;
          const changedAt = status.changed_at;
          if (changedAt && turnStartedOrEndedBetween(listed.state, status.state)) {
            const current = listOrderStampBySession.get(sessionId);
            if (current === undefined || changedAt > current) {
              listOrderStampBySession = new Map(listOrderStampBySession);
              listOrderStampBySession.set(sessionId, changedAt);
            }
          }
          set({ sessions, listOrderStampBySession });
          return;
        }
        const detail = get().sessionDetail.get(sessionId);
        if (!detail) {
          // No row of any kind yet. Keep the status for the row that is on its way.
          const waiting = get().statusAwaitingRow.get(sessionId);
          if (newerSessionStatus(waiting, status) !== status || waiting?.as_of === status.as_of) return;
          const statusAwaitingRow = new Map(get().statusAwaitingRow);
          statusAwaitingRow.set(sessionId, status);
          set({ statusAwaitingRow });
          return;
        }
        const held = detail.summary.status;
        if (newerSessionStatus(held, status) !== status || held?.as_of === status.as_of) return;
        const sessionDetail = new Map(get().sessionDetail);
        sessionDetail.set(sessionId, {
          ...detail,
          summary: { ...detail.summary, status, state: status.state },
        });
        set({ sessionDetail });
      },

      removeSession(sessionId) {
        const sessions = new Map(get().sessions);
        const wasInList = sessions.delete(sessionId);
        // A session opened by id (reference chip, old deeplink) has no list row but
        // IS tracked — its summary lives in `sessionDetail`, and `activeSummary`
        // falls back to it (`sessionSummaryFor`). Early-returning on the list Map
        // alone would leave that fallback serving a deleted session. Only an id
        // neither map knows is a true no-op.
        if (!wasInList && !get().sessionDetail.has(sessionId)) return;
        const turnsBySession = new Map(get().turnsBySession);
        turnsBySession.delete(sessionId);
        const tails = new Map(get().tails);
        tails.delete(sessionId);
        const rawTurnsLoaded = new Set(get().rawTurnsLoaded);
        rawTurnsLoaded.delete(sessionId);
        const sessionDetail = new Map(get().sessionDetail);
        sessionDetail.delete(sessionId);
        const sessionDetailLoading = new Set(get().sessionDetailLoading);
        sessionDetailLoading.delete(sessionId);
        const pendingHooks = new Map(get().pendingHooks);
        pendingHooks.delete(sessionId);
        const listOrderStampBySession = new Map(get().listOrderStampBySession);
        listOrderStampBySession.delete(sessionId);
        const statusAwaitingRow = new Map(get().statusAwaitingRow);
        statusAwaitingRow.delete(sessionId);
        // The one authoritative "this session is gone" signal there is, so it is the
        // one place a draft can be dropped for a reason rather than for age. Persist
        // the removal too, or the draft comes straight back on the next reload.
        const drafts = new Map(get().drafts);
        const hadDraft = drafts.delete(sessionId);
        if (hadDraft) draftStore.save(drafts);
        set({
          sessions,
          turnsBySession,
          tails,
          rawTurnsLoaded,
          pendingHooks,
          listOrderStampBySession,
          statusAwaitingRow,
          sessionDetail,
          sessionDetailLoading,
          drafts,
          activeId: get().activeId === sessionId ? null : get().activeId,
        });
      },

      setFolders(folders) {
        set({ folders });
      },

      setActive(activeId) {
        // Nothing about the session's STATUS is touched here, and that is the point.
        // A label used to be rebuilt from the transcript on every switch, because the
        // only live source was the open session's stream and a kept label went stale.
        // The status now rides the session row, which the list stream keeps current
        // for every session, so the row being switched TO is already right.
        // Selecting a session is the strongest statement of use there is, and it is the
        // only one that arrives for a session already warm in memory — switching back to
        // a loaded session fetches nothing, so without this its recency would still read
        // as whenever it was last fetched and it would be evicted out from under a user
        // flipping between two sessions.
        if (activeId) touch(activeId);
        set({ activeId });
      },

      setTurns(sessionId, incoming, opts) {
        // MERGE, never replace (dash docs/chat-turns-per-message.md §6). This used
        // to swap the whole model for the incoming page and rebuild the tail from the
        // page alone — the "everything resets" bug: live-only content vanished and the
        // fold history was forgotten. `mergeMaterializedPage` joins on the tail's
        // event-id sets (the entry-id spaces of the two paths never overlap), keeps
        // what the page cannot answer for — including the reasoning carve-out that
        // `carryForwardReasoning` used to apply as a separate pass — and reuses held
        // entry objects for unchanged content so row memos keep hitting.
        //
        // `carryForwardAggregates` still runs first: a page carrying no cost/context
        // roll-up does not get to erase the one already known for this session.
        const prior = get().turnsBySession.get(sessionId);
        // Re-annotated AFTER the merge: the page's dedup flags were computed over the
        // page alone and the kept live entries' over the stream alone, so a copy on
        // each side of the join (a page-side OTel prompt echo and a live-side bridge
        // copy, say) has never been paired. One pass over the merged model pairs them.
        const tail = annotateTail(
          mergeMaterializedPage(get().tails.get(sessionId), carryForwardAggregates(prior, incoming)),
        );
        const model = tail.model;
        const turnsBySession = new Map(get().turnsBySession);
        turnsBySession.set(sessionId, model);
        const tails = new Map(get().tails);
        tails.set(sessionId, tail);
        const moreBySession = new Map(get().moreBySession);
        moreBySession.set(sessionId, model.more);
        const turnsLoading = new Set(get().turnsLoading);
        turnsLoading.delete(sessionId);
        const retained = retain(sessionId, turnsBySession, tails);
        // Marked AFTER eviction, off the set eviction returned — marking first and
        // spreading second would put the mark back for a session eviction had just
        // dropped. A projected page never CLEARS the flag: the raw page is a superset,
        // so once a session has been read in full, a later Turns-view fetch merging
        // over it leaves it still complete.
        let rawTurnsLoaded = retained.rawTurnsLoaded;
        if (opts?.raw && !rawTurnsLoaded.has(sessionId)) {
          rawTurnsLoaded = new Set(rawTurnsLoaded);
          rawTurnsLoaded.add(sessionId);
        }
        set({
          ...retained,
          moreBySession,
          turnsLoading,
          rawTurnsLoaded,
        });
      },

      applyTailEvent(sessionId, event) {
        actions.applyTailEvents(sessionId, [event]);
      },

      /**
       * Fold a BATCH of live frames and notify subscribers ONCE.
       *
       * The batch is the point, not a convenience. Zustand notifies every subscriber
       * synchronously on every `set`, so each selector in the app re-ran once per
       * frame — and a session open replays its whole current turn, which is hundreds
       * of frames. Measured 2026-08-26 across eight cold opens on the real dashboard:
       * 12,751ms of main-thread blocking, of which 98% disappeared when the per-session
       * SSE was cut off entirely (261ms). Neither the page size nor the entry count was
       * implicated — clipping tool payloads (6.17MB -> 4.02MB) moved the total 3%, and
       * removing every tool entry (6.17MB -> 0.54MB) moved it not at all.
       *
       * Relying on React's auto-batching alone was not enough: it coalesces the
       * RENDERS, and leaves every selector still running once per frame underneath.
       *
       * The per-frame semantics are unchanged — each frame is folded in order, through
       * the same reducer, with the same optimistic-row strip and the same idempotent
       * no-op check. Only the notification is shared.
       */
      applyTailEvents(sessionId, events) {
        if (events.length === 0) return;
        const state = get();
        // Parked hooks are folded FIRST and independently of the turn reducer. A hook
        // event moves no turn, so the reducer returns the same tail and the early
        // return below would drop it — and with it the only signal that a tool call is
        // frozen waiting on a human.
        let hooks = state.pendingHooks.get(sessionId) ?? EMPTY_HOOKS;
        for (const event of events) hooks = foldHookEvent(hooks, event);
        if (hooks !== (state.pendingHooks.get(sessionId) ?? EMPTY_HOOKS)) {
          const pendingHooks = new Map(state.pendingHooks);
          pendingHooks.set(sessionId, hooks as Map<string, PendingHook>);
          set({ pendingHooks });
        }
        // A spend halt is folded FIRST for the same reason: the gate interrupts the
        // turn and emits an error event, and an error event that the turn reducer
        // treats as a no-op would hit the early return below and be lost — leaving a
        // session that stopped mid-answer with no visible cause.
        for (const event of events) {
          const halt = budgetHaltFromEvent(sessionId, event);
          if (halt) actions.setBudgetHalt(halt);
        }
        // The status is folded FIRST for a third version of the same reason: a
        // `session_status` event moves no turn, so behind the early return below it
        // would be dropped. The NEWEST one in the batch is the only one that matters,
        // and `newerSessionStatus` decides whether even that one is news — the stream
        // replays the whole current turn on every open, and a replayed status is older
        // than what the session row already carries.
        let incomingStatus: SessionStatus | undefined;
        for (const event of events) {
          const status = event.type === 'session_status' ? event.data.status : undefined;
          if (status) incomingStatus = newerSessionStatus(incomingStatus, status);
        }
        if (incomingStatus) actions.applySessionStatus(sessionId, incomingStatus);
        // Folded ahead of the early return for the same reason: `session_info` moves no
        // turn. Events arrive in order, so the last one in the batch is the newest.
        let incomingInfo: SessionInfo | undefined;
        for (const event of events) {
          if (event.type === 'session_info' && event.data.info) incomingInfo = sessionInfoFromWire(event.data.info);
        }
        if (incomingInfo) actions.applySessionInfo(sessionId, incomingInfo);
        // Whether this session had no transcript at all before this frame. That is the
        // only case where a live frame grows the RETAINED SET rather than one member of
        // it, and so the only case that has to re-run the budget.
        const wasCold = !state.turnsBySession.has(sessionId);
        const before = getOrInitTail(state, sessionId);
        let next = before;
        // Folded WITHOUT annotating, once per frame, then annotated once below.
        // `annotateOTelDuplicates` rebuilds every entry in the model, so doing it per
        // frame is O(frames x entries) — and a session open replays hundreds of frames
        // onto a model holding a thousand entries.
        for (const event of events) {
          let tail = next;
          // Strip a matching optimistic user row when the real user_message lands,
          // so the two don't double-show (both are harness-sourced, so the OTel
          // annotator won't collapse them). Correlation prefers the client request id,
          // then falls back to a normalized-text match (bug-1 hardening) so a server
          // prompt that came back trimmed/normalized still reconciles.
          if (event.type === 'user_message') {
            tail = stripOptimisticUser(tail, event);
          }
          const folded = foldEventWithoutAnnotating(tail, event);
          // ⚠️ Compare against the tail BEFORE the strip, not after. When this event's
          // id is already in seenEventIds (a repair page landed first — the normal case
          // now that the merge accumulates seen ids), applyEvent no-ops and returns the
          // stripped tail unchanged. Comparing against the stripped one then silently
          // DISCARDED the strip, leaving the optimistic row alive as a duplicate "You"
          // row (observed live 2026-08-24).
          if (folded !== next) next = folded;
        }
        if (next === before) return; // every frame was an idempotent no-op
        next = annotateTail(next);
        const tails = new Map(state.tails);
        tails.set(sessionId, next);
        const turnsBySession = new Map(state.turnsBySession);
        turnsBySession.set(sessionId, next.model);
        // A live frame is the hot path — one per streamed token — so it does NOT
        // re-measure the transcript or re-run the budget. It cannot need to: the only
        // session that streams is the ACTIVE one, which is pinned, and the set of
        // retained sessions is unchanged unless this frame is the first thing that
        // session ever held. The size estimate is dropped rather than updated, so the
        // next eviction pass recomputes it from the grown model instead of trusting a
        // stale number.
        touch(sessionId);
        if (wasCold) {
          set(retain(sessionId, turnsBySession, tails));
        } else {
          sizeBySession.delete(sessionId);
          set({ tails, turnsBySession });
        }
      },

      setTurnsLoading(sessionId, loading) {
        const turnsLoading = new Set(get().turnsLoading);
        if (loading) turnsLoading.add(sessionId);
        else turnsLoading.delete(sessionId);
        set({ turnsLoading });
      },

      setSessionDetail(sessionId, detail) {
        const sessionDetail = new Map(get().sessionDetail);
        // The detail's summary is a session row like any other: a status already held
        // for this session — on an earlier detail, or waiting for a row — may be newer.
        const held = heldOrAwaiting(get(), sessionId, get().sessionDetail.get(sessionId)?.summary);
        sessionDetail.set(sessionId, { ...detail, summary: withNewestStatus(held, detail.summary) });
        const sessionDetailLoading = new Set(get().sessionDetailLoading);
        sessionDetailLoading.delete(sessionId);
        set({ sessionDetail, sessionDetailLoading });
      },

      setSessionDetailLoading(sessionId, loading) {
        const sessionDetailLoading = new Set(get().sessionDetailLoading);
        if (loading) sessionDetailLoading.add(sessionId);
        else sessionDetailLoading.delete(sessionId);
        set({ sessionDetailLoading });
      },

      patchHarnessConfig(sessionId, patch) {
        const prev = get().sessionDetail.get(sessionId);
        if (!prev) return null;
        const prevConfig = prev.harnessConfig;
        const nextConfig: HarnessConfig = { ...(prevConfig ?? {}), ...patch };
        const sessionDetail = new Map(get().sessionDetail);
        sessionDetail.set(sessionId, { ...prev, harnessConfig: nextConfig });
        set({ sessionDetail });
        return prevConfig;
      },

      prependOlder(sessionId, older) {
        const cur = get().turnsBySession.get(sessionId);
        if (!cur) {
          get().actions.setTurns(sessionId, older);
          return;
        }
        const entries = { ...older.entries, ...cur.entries };
        const seen = new Set(cur.turns.map((t) => t.id));
        const turns: Turn[] = [...older.turns.filter((t) => !seen.has(t.id)), ...cur.turns];
        // Spread `cur` first so every field the merge does not explicitly decide — today
        // that is `aggregates`, tomorrow whatever TurnModel grows next — comes from the
        // model already on screen rather than silently vanishing. `older` can still fill
        // a roll-up `cur` never had, but never overwrite one: an older page's figures are
        // by construction the staler answer. See `carryForwardAggregates`.
        const sourceGroups =
          older.sourceGroups || cur.sourceGroups
            ? { ...(older.sourceGroups ?? {}), ...(cur.sourceGroups ?? {}) }
            : undefined;
        const merged: TurnModel = carryForwardAggregates(older, {
          ...cur,
          sessionId,
          turns,
          entries,
          validator: cur.validator,
          more: older.more,
          ...(sourceGroups ? { sourceGroups } : {}),
        });
        const turnsBySession = new Map(get().turnsBySession);
        turnsBySession.set(sessionId, merged);
        const tails = new Map(get().tails);
        // Keeps every surviving entry's folded event-id set — a plain reseed collapsed
        // them to one id each, leaving a Last-Event-ID replay free to re-fold text the
        // entry already held.
        tails.set(sessionId, reseedTailKeepingFoldHistory(sessionId, merged, get().tails.get(sessionId)));
        const moreBySession = new Map(get().moreBySession);
        moreBySession.set(sessionId, older.more);
        set({ ...retain(sessionId, turnsBySession, tails), moreBySession });
      },

      setPendingHooks(sessionId, hooks) {
        const pendingHooks = new Map(get().pendingHooks);
        const forSession = new Map<string, PendingHook>();
        for (const hook of hooks) forSession.set(hook.requestId, hook);
        pendingHooks.set(sessionId, forSession);
        set({ pendingHooks });
      },

      upsertPendingHook(sessionId, hook) {
        const pendingHooks = new Map(get().pendingHooks);
        const forSession = new Map(pendingHooks.get(sessionId) ?? EMPTY_HOOKS);
        forSession.set(hook.requestId, hook);
        pendingHooks.set(sessionId, forSession);
        set({ pendingHooks });
      },

      clearPendingHook(sessionId, requestId) {
        const forSession = get().pendingHooks.get(sessionId);
        if (!forSession?.has(requestId)) return;
        const next = new Map(forSession);
        next.delete(requestId);
        const pendingHooks = new Map(get().pendingHooks);
        pendingHooks.set(sessionId, next);
        set({ pendingHooks });
      },

      appendOptimisticUser(sessionId, text, clientId) {
        const state = get();
        const tail = getOrInitTail(state, sessionId);
        const next = appendOptimistic(tail, text, clientId);
        const tails = new Map(state.tails);
        tails.set(sessionId, next);
        const turnsBySession = new Map(state.turnsBySession);
        turnsBySession.set(sessionId, next.model);
        set({ tails, turnsBySession });
      },

      dropOptimisticUser(sessionId, clientId) {
        const state = get();
        const tail = state.tails.get(sessionId);
        if (!tail) return;
        const next = removeOptimistic(tail, clientId);
        if (next === tail) return;
        const tails = new Map(state.tails);
        tails.set(sessionId, next);
        const turnsBySession = new Map(state.turnsBySession);
        turnsBySession.set(sessionId, next.model);
        set({ tails, turnsBySession });
      },

      setBudgetHalt(halt) {
        const budgetHalts = new Map(get().budgetHalts);
        budgetHalts.set(halt.sessionId, halt);
        set({ budgetHalts });
      },

      clearBudgetHalt(sessionId) {
        const state = get();
        if (!state.budgetHalts.has(sessionId)) return;
        const budgetHalts = new Map(state.budgetHalts);
        budgetHalts.delete(sessionId);
        set({ budgetHalts });
      },

      setFilter(patch) {
        const filter = { ...get().filter, ...patch };
        // A changed search query invalidates the prior content-search hits until
        // the async augmentation returns for the new query (fails safe: local name
        // matching still runs instantly).
        const nextState: Partial<ChatState> = { filter };
        if (patch.search !== undefined) {
          const cur = get().contentHits;
          // Trimmed on both sides: `contentHits.query` is always trimmed, so an
          // untrimmed compare invalidated a still-valid hit set on every keystroke
          // that only added whitespace.
          if (!cur || cur.query !== filter.search.trim()) nextState.contentHits = null;
        }
        set(nextState);
        // Persist only when the patch actually named a persisted axis. Keying off the
        // PATCH rather than diffing the result is what keeps every keystroke in the
        // search box from rewriting the filter record — search is not persisted, and
        // a save per keystroke would be a stringify per keystroke for no change at all.
        if (PERSISTED_FILTER_AXES.some((axis) => patch[axis] !== undefined)) {
          filterStore.save(filter);
        }
      },

      openFolder(folder) {
        set({ filter: { ...get().filter, folder } });
      },

      setContentHits(query, hits, truncated = false) {
        // Drop a stale response whose query no longer matches the live filter.
        // `query` is trimmed by the caller, so trim this side too — comparing it
        // raw dropped the hits for any query the user typed with a stray space.
        if (get().filter.search.trim() !== query) return;
        // Keyed by session id, keeping the count. Duplicate ids collapse the same
        // way the old Set collapsed them; last one wins, which for a sorted list
        // is the smaller count and is the conservative read.
        const matchCountBySessionId = new Map<string, number>();
        for (const h of hits) matchCountBySessionId.set(h.sessionId, h.matchCount);
        // Hits landing ends the search they were asked for, and clears any failure
        // recorded for it. Both are unconditional because the drop above has already
        // established that `query` IS the live query — a second guard on
        // `contentSearchInFlight` was tried here and proved unreachable.
        //
        // Clearing the error is not redundant with `startContentSearch`. A query can
        // have two requests out at once (a trailing space re-fires it under the same
        // trimmed query); if the first fails and the second succeeds, this is the only
        // thing that takes the failure notice back down.
        set({
          contentHits: {
            query,
            matchCountBySessionId,
            hitCount: matchCountBySessionId.size,
            truncated,
          },
          contentSearchInFlight: null,
          contentSearchError: null,
        });
      },

      startContentSearch(query) {
        set({ contentSearchInFlight: query, contentSearchError: null });
      },

      endContentSearch(query, error) {
        if (get().contentSearchInFlight !== query) return;
        set({ contentSearchInFlight: null, contentSearchError: error });
      },

      setDraft(sessionId, text) {
        const drafts = new Map(get().drafts);
        drafts.set(sessionId, text);
        set({ drafts });
        // Synchronous, on every keystroke. See draftStorage.ts for why this is not
        // debounced: the words at risk are the ones typed just before the reload.
        draftStore.save(drafts);
      },

      setSending(sessionId, sending) {
        const s = new Set(get().sending);
        if (sending) s.add(sessionId);
        else s.delete(sessionId);
        set({ sending: s });
      },

      openPending(opts) {
        // Absent keys, not `undefined` ones: `'instanceId' in pending` is what the
        // create path reads to decide whether a target was chosen at all.
        // `maxBudget` and `disabledTools` test `!== undefined` rather than
        // truthiness — 0 (no ceiling) and [] (disable nothing) are both real
        // answers a truthiness check would silently drop back to "inherit".
        const pending: PendingSession = {
          clientId: `pending_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`,
          ...(opts?.instanceId ? { instanceId: opts.instanceId } : {}),
          ...(opts?.harness ? { harness: opts.harness } : {}),
          ...(opts?.principalId ? { principalId: opts.principalId } : {}),
          ...(opts?.model ? { model: opts.model } : {}),
          ...(opts?.effort ? { effort: opts.effort } : {}),
          ...(opts?.maxBudget !== undefined ? { maxBudget: opts.maxBudget } : {}),
          ...(opts?.disabledTools !== undefined ? { disabledTools: opts.disabledTools } : {}),
        };
        set({ pending, activeId: null });
        return pending;
      },

      patchPending(patch) {
        const current = get().pending;
        // No pane, nothing to patch. Deliberately NOT "open one": a select in a
        // controls bar must not be able to start a new chat by being touched.
        if (!current) return null;

        const next: PendingSession = { ...current };
        // Same absent-key discipline as `openPending`, plus one rule it does not need:
        // an empty string CLEARS. `openPending` folds '' into "never mentioned" because
        // its caller resolves saved defaults and has no reason to pass a blank; a select
        // does, every time the user picks the placeholder row back.
        const setOrDelete = (key: 'instanceId' | 'harness' | 'model' | 'effort') => {
          const value = patch[key];
          if (value === undefined) return;
          if (value) next[key] = value;
          else delete next[key];
        };
        setOrDelete('instanceId');
        setOrDelete('harness');
        setOrDelete('model');
        setOrDelete('effort');
        if (patch.maxBudget !== undefined) next.maxBudget = patch.maxBudget;
        if (patch.disabledTools !== undefined) next.disabledTools = patch.disabledTools;

        // A patch that changes nothing does not re-render. The value identity is what
        // `usePendingSession` subscribes to, so writing an equal-but-new object would
        // wake every consumer of the pane for no reason — and a caller that patches
        // from an effect would loop.
        const keys = new Set([...Object.keys(current), ...Object.keys(next)]);
        let changed = false;
        for (const k of keys) {
          const a = current[k as keyof PendingSession];
          const b = next[k as keyof PendingSession];
          if (Array.isArray(a) && Array.isArray(b)) {
            if (a.length !== b.length || a.some((v, i) => v !== b[i])) { changed = true; break; }
          } else if (a !== b) { changed = true; break; }
        }
        if (!changed) return current;

        // `activeId` is untouched: patching settings is not selecting anything, and
        // `openPending`'s `activeId: null` is about OPENING a pane, not configuring one.
        set({ pending: next });
        return next;
      },

      clearPending() {
        set({ pending: null });
      },

      setHarnesses(list) {
        set({ harnesses: list, harnessesLoading: false });
      },
      setHarnessesLoading(harnessesLoading) {
        set({ harnessesLoading });
      },
      setModels(list) {
        set({ models: list, modelsLoading: false });
      },
      setModelsLoading(modelsLoading) {
        set({ modelsLoading });
      },
    };

    return {
      sessions: new Map(),
      turnsBySession: new Map(),
      sessionDetail: new Map(),
      sessionDetailLoading: new Set(),
      tails: new Map(),
      rawTurnsLoaded: new Set(),
      pendingHooks: new Map(),
      budgetHalts: new Map(),
      statusAwaitingRow: new Map(),
      listOrderStampBySession: new Map(),
      activeId: null,
      filter: { ...EMPTY_FILTER, ...persistedFilterAxes },
      contentHits: null,
      contentSearchInFlight: null,
      contentSearchError: null,
      folders: [],
      connState: 'idle',
      listLoading: false,
      olderSessionsCursor: null,
      olderSessionsLoading: false,
      turnsLoading: new Set(),
      moreBySession: new Map(),
      drafts: persistedDrafts,
      sending: new Set(),
      pending: null,
      harnesses: null,
      harnessesLoading: false,
      models: null,
      modelsLoading: false,
      actions,
    };
  });
}

// --- optimistic-entry helpers (live-tail only) ---

function appendOptimistic(tail: TailState, text: string, clientId: string): TailState {
  const entryId = `optim_${clientId}`;
  const turnId = `optimturn_${clientId}`;
  const nowIso = new Date().toISOString();
  const maxEventId = tail.model.validator.maxEventId;
  const entry: Entry = {
    id: entryId,
    turnId,
    role: 'user',
    kind: 'text',
    source: 'harness',
    eventId: maxEventId + 0.5, // orders after the last real event, before the next
    ts: nowIso,
    origin: 'live',
    text,
    duplicate: false,
    primary: true,
    raw: { optimistic: true, clientId },
  };
  const entries = { ...tail.model.entries, [entryId]: entry };
  const turn: Turn = { id: turnId, role: 'user', ts: nowIso, entryIds: [entryId] };
  const turns = [...tail.model.turns, turn];
  const model: TurnModel = { ...tail.model, entries, turns };
  const turnIndex = new Map(tail.turnIndex);
  turnIndex.set(turnId, turns.length - 1);
  return { ...tail, model, turnIndex };
}

/**
 * Reconcile the optimistic user row against the canonical `user_message` event.
 * Bug-1 hardening: correlate by `client_request_id` FIRST (an exact, id-based
 * match), then fall back to a NORMALIZED text match (trim + collapse whitespace)
 * so a server prompt echoed back trimmed/normalized still collapses the optimistic
 * copy instead of leaving both rows alive. The empty-text case is guarded — a
 * canonical event with no text is never matched against an optimistic row by text,
 * which would otherwise drop an unrelated row or leave both alive.
 */
function stripOptimisticUser(tail: TailState, event: WireEvent): TailState {
  const clientId = event.data.client_request_id;
  const normText = normalizeText(event.data.result?.text ?? '');

  let removedId: string | null = null;
  // 1. Prefer an id-based correlation when the server echoes the client request id.
  if (clientId) {
    for (const [id, entry] of Object.entries(tail.model.entries)) {
      const raw = entry.raw as { optimistic?: boolean; clientId?: string } | undefined;
      if (raw?.optimistic && raw.clientId === clientId) {
        removedId = id;
        break;
      }
    }
  }
  // 2. Fall back to a normalized-text match, guarding empty text (no blind match).
  if (!removedId && normText) {
    for (const [id, entry] of Object.entries(tail.model.entries)) {
      const raw = entry.raw as { optimistic?: boolean } | undefined;
      if (raw?.optimistic && normalizeText(entry.text ?? '') === normText) {
        removedId = id;
        break;
      }
    }
  }
  if (!removedId) return tail;
  return removeEntryAndItsTurn(tail, removedId);
}

/** Drop the optimistic row for `clientId`, or report the tail unchanged when there is
 *  none. The id-based half of `stripOptimisticUser`, addressed directly: the caller
 *  here IS the code that minted the client id, so there is nothing to correlate. */
function removeOptimistic(tail: TailState, clientId: string): TailState {
  const entryId = `optim_${clientId}`;
  if (!tail.model.entries[entryId]) return tail;
  return removeEntryAndItsTurn(tail, entryId);
}

/** Remove one entry and the turn it was the whole of, re-deriving the turn index.
 *  Shared by the reconcile path (the real message landed) and the failure path (the
 *  send was refused), so the two can never drift into removing it differently. */
function removeEntryAndItsTurn(tail: TailState, entryId: string): TailState {
  const entries = { ...tail.model.entries };
  const removed = entries[entryId];
  delete entries[entryId];
  const turns = tail.model.turns.filter((t) => t.id !== removed?.turnId);
  const turnIndex = new Map<string, number>();
  turns.forEach((t, i) => turnIndex.set(t.id, i));
  const model: TurnModel = { ...tail.model, entries, turns };
  return { ...tail, model, turnIndex };
}
