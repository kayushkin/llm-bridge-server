import type { Entry, HarnessMeta, ModelOption, SessionStatus, SessionSummary, Turn, TurnModel } from '../net/types.js';
import { annotateOTelDuplicates } from '../reduce/otelDedup.js';
import { statusFromBareState } from './sessionStatus.js';
import { resultedToolIds, toolIdOf } from './toolPairing.js';
import type { ChatState, ContentHits, FilterState } from './ChatStore.js';

// Memoized selectors over the hot store. Kept pure + framework-free so they are
// testable in isolation and cheap enough to run on every render (the memo cache
// short-circuits when inputs are identity-equal). The React hooks bind to these.

export interface FolderGroup {
  folder: string;
  sessions: SessionSummary[];
}

/** Session types the list hides while the `type` axis carries no selection.
 *
 *  An `external` session ran outside the bridge entirely and was imported afterwards
 *  by scanning the harness's on-disk history. Nobody opened it in a UI, so listing it
 *  beside real conversations reports a `claude -p` one-shot as a human chat.
 *
 *  This is a rule rather than a seeded selection because the axis is INCLUSION-based:
 *  "everything except external" can only be written down by naming every other type,
 *  and that list would silently hide whatever type the server adds next. Selecting
 *  `external` on the axis shows those sessions, so the default stays user-toggleable
 *  and nothing is ever persisted on the user's behalf. */
export const DEFAULT_HIDDEN_SESSION_TYPES: ReadonlySet<string> = new Set(['external']);

/** True iff the session was spawned by another session — it names a manager.
 *
 *  The list hides such a session while the `purpose` axis carries no selection. A
 *  subagent is reached under the session that spawned it (`useSubagentSessions`), so
 *  listing it beside top-level conversations as well showed every one twice, and a
 *  Codex parent's reviewer and spawned threads crowded out the conversations around
 *  it. Selecting any purpose, `subagent` included, lists them again — the same escape
 *  the `type` axis gives `external`. */
export function isSpawnedSession(s: SessionSummary): boolean {
  return (s.managerSessionId ?? '') !== '';
}

/** True iff a session passes the current filter.
 *
 *  Each faceted axis (`harness`, `status`, `type`, `purpose`, `mode`, `machine`) is a
 *  multi-select `string[]`: an EMPTY array is "no filter" (matches everything), a
 *  non-empty array matches a session whose value is ANY of the selected ones (OR within
 *  the axis). Axes combine with AND — every non-empty axis must match. The `machine`
 *  axis matches `SessionSummary.instanceId` (there is no machine field on the summary;
 *  the dash resolves instanceId → machine and passes instanceId values here).
 *
 *  The `type` axis has ONE exception to "empty is no filter": with nothing selected on
 *  it, the types in `DEFAULT_HIDDEN_SESSION_TYPES` are still dropped. Select any type
 *  and the array rules alone. The `purpose` axis has the same kind of exception: with
 *  nothing selected on it, a session another session spawned (`isSpawnedSession`) is
 *  dropped.
 *
 *  `folder` is a scalar exact match; `search` matches the display name OR the session
 *  id case-insensitively AND, when `contentHits` is supplied (C6 content search), any
 *  session whose transcript text matched the same query. Name matching stays instant/local; the content-hit set is
 *  an async augmentation folded in when it arrives (see `useFilters`). */
export function matchesFilter(
  s: SessionSummary,
  f: FilterState,
  contentHits?: ContentHits | null,
): boolean {
  if (f.harness.length && !f.harness.includes(s.harness)) return false;
  if (f.status.length && !f.status.includes(s.state)) return false;
  if (f.type.length) {
    if (!f.type.includes(s.type)) return false;
  } else if (DEFAULT_HIDDEN_SESSION_TYPES.has(s.type)) {
    return false;
  }
  if (f.purpose.length) {
    if (!f.purpose.includes(s.purpose)) return false;
  } else if (isSpawnedSession(s)) {
    return false;
  }
  if (f.mode.length && !f.mode.includes(s.mode)) return false;
  if (f.machine.length && !f.machine.includes(s.instanceId)) return false;
  if (f.principal.length && !f.principal.includes(s.principalId)) return false;
  if (f.folder && s.folderName !== f.folder) return false;
  // Trimmed: the user's raw `filter.search` may carry surrounding spaces, which no
  // display name contains and which `contentHits.query` (always trimmed) never
  // matched. Comparing raw made a query with a stray space match nothing at all.
  const search = f.search.trim();
  if (search) {
    const q = search.toLowerCase();
    // Name OR id, always both. This used to read `s.displayName || s.sessionId`,
    // so the id was only searchable on a session that had no name — pasting a
    // `br_…` id matched nothing whenever the session it belonged to was named,
    // which is the one case where someone pastes an id on purpose.
    const nameHit =
      (s.displayName ?? '').toLowerCase().includes(q) || s.sessionId.toLowerCase().includes(q);
    const contentHit =
      !!contentHits &&
      contentHits.query === search &&
      contentHits.matchCountBySessionId.has(s.sessionId);
    if (!nameHit && !contentHit) return false;
  }
  return true;
}

/** How well a session answers `query`, lower is better — a port of bridge-ui's
 *  `rankOf` (bridge-ui `SessionList.tsx`).
 *
 *  `0` an exact session-id paste, `1` a name-or-id substring, `2` a content-only
 *  hit. Without the tiers a pasted id sinks under every transcript that merely
 *  mentions it. `query` must already be TRIMMED — the same string `matchesFilter`
 *  compares against `contentHits.query`. */
function searchRankOf(s: SessionSummary, query: string): number {
  const q = query.toLowerCase();
  const id = s.sessionId.toLowerCase();
  if (id === q) return 0;
  if ((s.displayName ?? '').toLowerCase().includes(q) || id.includes(q)) return 1;
  return 2;
}

/** Order the sessions matching an active `query`: rank tier, then how much of the
 *  transcript matched, then recency.
 *
 *  `matchCount` is the ranking the search endpoint already computed and
 *  `ApiClient.search` already sorted by; before this it reached the store and was
 *  discarded, so content hits came out in `byUpdatedDesc` order and a session with
 *  one incidental match outranked the session the query was about whenever it had
 *  been touched more recently.
 *
 *  A session with no content hit scores 0 here, which only ever compares against
 *  other rank-1 rows — a rank-2 row is a content hit by definition.
 *
 *  ⚠️ This orders sessions WITHIN a folder group. the chat page keeps its folder grouping
 *  while a query is active (bridge-ui flattens instead, in `SessionList.tsx`'s
 *  `searchActive` branch),
 *  so the best hit still sits under whatever folder sorts first. Whether the chat page
 *  should flatten on query is a product choice, tracked as its own todo. */
function bySearchRank(
  query: string,
  contentHits: ContentHits | null,
  stamps: ReadonlyMap<string, string>,
): (a: SessionSummary, b: SessionSummary) => number {
  const counts =
    contentHits && contentHits.query === query ? contentHits.matchCountBySessionId : null;
  const byStamp = byOrderStampDesc(stamps);
  return (a, b) => {
    const rank = searchRankOf(a, query) - searchRankOf(b, query);
    if (rank !== 0) return rank;
    const byMatches = (counts?.get(b.sessionId) ?? 0) - (counts?.get(a.sessionId) ?? 0);
    if (byMatches !== 0) return byMatches;
    // The same order-stamp tie-break the unsearched list uses, so equally-ranked
    // rows hold still while their sessions run.
    return byStamp(a, b);
  };
}

function byUpdatedDesc(a: SessionSummary, b: SessionSummary): number {
  if (a.updatedAt < b.updatedAt) return 1;
  if (a.updatedAt > b.updatedAt) return -1;
  return 0;
}

/** The stamp a session is ORDERED by: its sidebar order stamp, falling back to
 *  `updatedAt` only for a row no store action has seeded yet (every entry path
 *  seeds, so the fallback exists for safety, not as a second ordering). See
 *  `ChatState.listOrderStampBySession` for what moves the stamp and what
 *  deliberately does not. */
function orderStampOf(s: SessionSummary, stamps: ReadonlyMap<string, string>): string {
  return stamps.get(s.sessionId) ?? s.updatedAt;
}

/** Newest order stamp first; ties broken by `updatedAt`, then by id so the order
 *  is total and two sessions can never swap on a re-sort alone. */
function byOrderStampDesc(
  stamps: ReadonlyMap<string, string>,
): (a: SessionSummary, b: SessionSummary) => number {
  return (a, b) => {
    const as = orderStampOf(a, stamps);
    const bs = orderStampOf(b, stamps);
    if (as < bs) return 1;
    if (as > bs) return -1;
    const byUpdated = byUpdatedDesc(a, b);
    if (byUpdated !== 0) return byUpdated;
    return a.sessionId < b.sessionId ? -1 : a.sessionId > b.sessionId ? 1 : 0;
  };
}

// Identity-keyed memo: recompute only when (sessions, filter, contentHits, folders)
// change. contentHits is part of the key so a content-search response repaints;
// folders is, so the first `GET /folders` response after boot repaints rather than
// sitting behind a memo hit.
//
// One slot PER STORE, found by the store's own `sessions` Map, which every store
// creates for itself and replaces on every session mutation. A page can hold more than
// one store — bridge-ui mounts one per connected server beside this server's — and a
// single module-level slot let their reads evict each other, so every
// `useSyncExternalStore` check got a new array and React looped until "Maximum update
// depth exceeded" blanked the chat. The same holds for `reachCache` and `facetsCache`.
const visibleCache = new WeakMap<Map<string, SessionSummary>, {
  filter: FilterState;
  contentHits: ContentHits | null;
  folders: string[];
  stamps: Map<string, string>;
  result: FolderGroup[];
}>();

/** Filter → group-by-folder → order by the SERVER's folder list.
 *
 *  Group order is `state.folders`, which is what `GET /folders` returned — the
 *  order the user arranged, not a recency guess. Every folder the server names
 *  gets a group **even when it holds no visible session**, because an empty folder
 *  is a real row the sidebar has to be able to draw (and file into); ordering by
 *  freshest activity, as this did before, cannot represent one at all.
 *
 *  Three rules decide the layout:
 *
 *  1. The unfoldered bucket (`''`) goes FIRST, matching bridge-ui's sidebar — but
 *     only when something is actually in it. An empty NAMED folder is a server row
 *     and must render; an empty unfoldered bucket is the absence of a folder and
 *     would be a permanent blank header.
 *  2. Then every name in `state.folders`, in server order, empty or not.
 *  3. Then any folder a loaded session claims that the server list does not
 *     mention, ordered by newest session as before. `SessionSummary.folderName` is
 *     authoritative too, so a folder created since the last `/folders` read groups
 *     its sessions under their own name — it does not swallow them into the
 *     unfoldered bucket (which is what bridge-ui does) or drop them off the list.
 *
 *  With no folder list loaded — before the first response, or after a failed one —
 *  rule 3 covers everything and the result is exactly the old recency ordering.
 *
 *  Sessions within a group are ordered by their ORDER STAMP, newest first — the
 *  stamp of a session's last turn start or turn end, not its raw `updatedAt`. The
 *  server bumps `updatedAt` on every event, so ordering by it made concurrently
 *  running sessions leapfrog each other continuously; the stamp holds a row still
 *  while its turn runs (see `ChatState.listOrderStampBySession`). While a search
 *  query is active they are ordered by `bySearchRank` instead — id match, then
 *  name match, then how many transcript events matched, then the same stamp.
 *  Memoized on identity of the sessions Map + filter object + content-hit set +
 *  folder list + stamp map. */
export function visibleSessions(state: ChatState): FolderGroup[] {
  const { sessions, filter, contentHits, folders } = state;
  const stamps = state.listOrderStampBySession;
  const cached = visibleCache.get(sessions);
  if (
    cached &&
    cached.filter === filter &&
    cached.contentHits === contentHits &&
    cached.folders === folders &&
    cached.stamps === stamps
  ) {
    return cached.result;
  }
  const byFolder = new Map<string, SessionSummary[]>();
  for (const s of sessions.values()) {
    if (!matchesFilter(s, filter, contentHits)) continue;
    const folder = s.folderName || '';
    let arr = byFolder.get(folder);
    if (!arr) {
      arr = [];
      byFolder.set(folder, arr);
    }
    arr.push(s);
  }
  // Newest-first normally; while a query is active, by how well each session
  // answers it (see `bySearchRank`). Both comparators are total and fall back to
  // recency, so the ordering never depends on the Map's insertion order.
  const query = filter.search.trim();
  const order = query ? bySearchRank(query, contentHits, stamps) : byOrderStampDesc(stamps);
  for (const arr of byFolder.values()) arr.sort(order);

  const groups: FolderGroup[] = [];
  // 1. unfoldered, first, only when it has something in it.
  const unfoldered = byFolder.get('');
  if (unfoldered && unfoldered.length > 0) groups.push({ folder: '', sessions: unfoldered });
  // 2. every server folder, in server order, empty or not.
  const known = new Set<string>();
  for (const name of folders) {
    if (name === '' || known.has(name)) continue;
    known.add(name);
    groups.push({ folder: name, sessions: byFolder.get(name) ?? [] });
  }
  // 3. folders only the loaded sessions know about, newest group first.
  const unknown: FolderGroup[] = [];
  for (const [folder, arr] of byFolder) {
    if (folder === '' || known.has(folder)) continue;
    unknown.push({ folder, sessions: arr });
  }
  // Newest ORDER STAMP in the group, computed rather than read off `sessions[0]`:
  // that shortcut was only true while every group was sorted newest-first, and a
  // search-ranked group puts the best hit at index 0 instead. The stamp rather
  // than `updatedAt` for the groups' own ordering too — a folder holding a
  // running session would otherwise leapfrog its neighbors on every event, the
  // same churn the row ordering just gave up.
  const newestIn = (g: FolderGroup): string => {
    let newest = '';
    for (const s of g.sessions) {
      const stamp = orderStampOf(s, stamps);
      if (stamp > newest) newest = stamp;
    }
    return newest;
  };
  unknown.sort((a, b) => {
    const an = newestIn(a);
    const bn = newestIn(b);
    return an < bn ? 1 : an > bn ? -1 : 0;
  });
  groups.push(...unknown);

  visibleCache.set(sessions, { filter, contentHits, folders, stamps, result: groups });
  return groups;
}

/** Total visible session count across all groups. */
export function visibleCount(state: ChatState): number {
  return visibleSessions(state).reduce((n, g) => n + g.sessions.length, 0);
}

/** How much of a content search the sidebar is actually able to show.
 *
 *  `visibleSessions` can only ever render sessions present in `state.sessions`,
 *  the pages that have been loaded. A content hit for a session outside that
 *  window has its id in `contentHits.matchCountBySessionId` and no summary to paint, so it matches
 *  nothing and disappears — search returns fewer results than the server found
 *  and, until this selector existed, said nothing about it.
 *
 *  Measured on this host 2026-08-02: for a 100-hit page, 55–92% of hits fell
 *  outside the loaded window. This is the normal case, not an edge case. */
export interface ContentSearchReach {
  /** The trimmed query these numbers describe. */
  query: string;
  /** Sessions the backend matched on transcript text (capped — see `truncated`). */
  hitCount: number;
  /** Of those, how many are loaded AND survive the other filter axes, so are on
   *  screen. Counted against the rendered groups, not against `state.sessions`,
   *  so a hit filtered out by a chip is honestly reported as not shown. */
  shownHitCount: number;
  /** `hitCount - shownHitCount`: matches the server found that the list cannot
   *  paint. Zero means search is showing everything it was given. */
  hiddenHitCount: number;
  /** The backend had more matches than it returned; `hitCount` is a floor. */
  truncated: boolean;
}

const reachCache = new WeakMap<Map<string, SessionSummary>, {
  filter: FilterState;
  contentHits: ContentHits | null;
  result: ContentSearchReach | null;
}>();

/** The reach of the active content search, or null when none is active (no query,
 *  or the hits for it have not landed yet). */
export function selectContentSearchReach(state: ChatState): ContentSearchReach | null {
  const { sessions, filter, contentHits } = state;
  const cached = reachCache.get(sessions);
  if (cached && cached.filter === filter && cached.contentHits === contentHits) {
    return cached.result;
  }
  let result: ContentSearchReach | null = null;
  const query = filter.search.trim();
  if (query && contentHits && contentHits.query === query) {
    let shown = 0;
    for (const group of visibleSessions(state)) {
      for (const s of group.sessions)
        if (contentHits.matchCountBySessionId.has(s.sessionId)) shown++;
    }
    result = {
      query,
      hitCount: contentHits.hitCount,
      shownHitCount: shown,
      hiddenHitCount: Math.max(0, contentHits.hitCount - shown),
      truncated: contentHits.truncated,
    };
  }
  reachCache.set(sessions, { filter, contentHits, result });
  return result;
}

/** Cross-axis facet counts: for each faceted filter axis, a `value → count` map over
 *  the FULL loaded session set (NOT the already-filtered list), so the sidebar can show
 *  every available option with its count and offer cross-axis selection. `status` counts
 *  `SessionSummary.state`; `machine` counts `instanceId` (the summary has no machine
 *  field — see `matchesFilter`). Empty-string values are skipped (an unfiled/unknown axis
 *  value is not a facet). */
export interface Facets {
  harness: Record<string, number>;
  status: Record<string, number>;
  type: Record<string, number>;
  purpose: Record<string, number>;
  mode: Record<string, number>;
  machine: Record<string, number>;
  /** Keyed by `principalId`, with '' counting sessions started as no principal —
   *  unlike the other axes, where '' is an unknown value and is skipped. */
  principal: Record<string, number>;
}

function tally(map: Record<string, number>, value: string): void {
  if (!value) return;
  map[value] = (map[value] ?? 0) + 1;
}

// Identity memo keyed on the sessions Map: facets recompute only when the loaded set
// changes (the Map is replaced immutably on every session mutation), so `useStore` sees
// a stable reference between unrelated renders. One entry per store, for the reason
// given above `visibleCache`.
const facetsCache = new WeakMap<Map<string, SessionSummary>, Facets>();

/** Facet counts over every loaded session, independent of the active filter. */
export function selectFacets(state: ChatState): Facets {
  const { sessions } = state;
  const cached = facetsCache.get(sessions);
  if (cached) return cached;
  const result: Facets = {
    harness: {},
    status: {},
    type: {},
    purpose: {},
    mode: {},
    machine: {},
    principal: {},
  };
  for (const s of sessions.values()) {
    tally(result.harness, s.harness);
    tally(result.status, s.state);
    tally(result.type, s.type);
    tally(result.purpose, s.purpose);
    tally(result.mode, s.mode);
    tally(result.machine, s.instanceId);
    result.principal[s.principalId] = (result.principal[s.principalId] ?? 0) + 1;
  }
  facetsCache.set(sessions, result);
  return result;
}

/** A session's summary from the loaded list, or — when the list does not hold it —
 *  from the cached per-session detail (`sessionDetail`).
 *
 *  The fallback is what keeps a session opened BY ID usable. `setSessions` rebuilds
 *  the `sessions` Map from whatever page the refresh returns, so a session outside
 *  that page — every session reached through a `[session:…]` reference chip or a
 *  `?session=` deeplink older than the loaded window — is evicted from it on the
 *  next refresh. Measured on dash 2026-09-01: the header showed the deeplinked
 *  session's name until the refresh landed, then went blank (`—`) with the right
 *  transcript still underneath, because `activeSummary` read only the list Map.
 *  `sessionDetail` is warmed on open and never rebuilt by a list refresh, so it is
 *  the durable answer for exactly these sessions.
 *
 *  Order matters: the list row wins while it exists, because live SSE upserts land
 *  there and the cached detail is a point-in-time read. */
export function sessionSummaryFor(state: ChatState, sessionId: string): SessionSummary | null {
  return state.sessions.get(sessionId) ?? state.sessionDetail.get(sessionId)?.summary ?? null;
}

/** The active session's summary, or null. Falls back to the cached detail for a
 *  session the loaded list does not hold — see `sessionSummaryFor`. */
export function activeSummary(state: ChatState): SessionSummary | null {
  if (!state.activeId) return null;
  return sessionSummaryFor(state, state.activeId);
}

// There is no "effective state" any more. A running list state used to be overridden
// here when the warm transcript's tail looked finished, to cover a session row the
// server had stranded at `tool_running`. The server now writes state and status
// together on every change and reconciles a stranded row itself; measured on
// 2026-09-17, 1 of 64,000 rows read `tool_running` and it was genuinely running. A
// second opinion held by the client is a second place for the two to disagree.

const SYNTHESIZED_STATUS = new WeakMap<SessionSummary, SessionStatus>();

/**
 * A session's status — never undefined for a known session, null for an unknown one.
 *
 * Referentially stable while the summary is: a row with no `status` of its own (one
 * the store has not normalized, in a test fixture say) gets ONE synthesized object
 * per summary object, so a `useStore` subscription does not see a new snapshot on
 * every read.
 */
export function selectSessionStatus(state: ChatState, sessionId: string | null): SessionStatus | null {
  if (!sessionId) return null;
  const summary = sessionSummaryFor(state, sessionId);
  if (!summary) return null;
  if (summary.status) return summary.status;
  let synthesized = SYNTHESIZED_STATUS.get(summary);
  if (!synthesized) {
    synthesized = statusFromBareState(summary.state);
    SYNTHESIZED_STATUS.set(summary, synthesized);
  }
  return synthesized;
}

/** The materialized model for a session, or undefined if not warm. */
export function turnsFor(state: ChatState, sessionId: string | null): TurnModel | undefined {
  if (!sessionId) return undefined;
  return state.turnsBySession.get(sessionId);
}

// ---- Cost / context selectors (from TurnModel.aggregates; pure, no network) ----
//
// Both read the loaded model's roll-up. `aggregates` MAY be absent (the spend/context
// events fell outside the loaded page) — absence reads as zeros, never a fabricated
// figure.
//
// ## Why these memoize in a WeakMap and not a single slot
//
// Both are consumed through `useStore(store, s => selector(s, sessionId))`, i.e.
// `useSyncExternalStore`, whose contract is that `getSnapshot` returns a REFERENTIALLY
// STABLE value while the state is unchanged. React calls it during render, again after
// commit, and again on every store notification; if any two of those calls disagree by
// identity, it re-renders and asks again — forever.
//
// These selectors take a PARAMETER, so "the state is unchanged" is not enough to make a
// single slot safe. A one-entry cache keyed on the TurnModel's identity holds the answer
// for exactly one session, so the moment a second live caller reads a different
// sessionId — or the same caller alternates a real id with `null` while nothing is
// selected — the two evict each other, every call is a miss, every miss allocates, and
// each subscription sees its own snapshot change on every check. Note that `null` and an
// unloaded id both resolve to the key `undefined`, so a single slot cannot even tell
// "no session" apart from "a session whose model has not loaded".
//
// A WeakMap keyed on the TurnModel is the shape the problem actually has: one memo slot
// per model, so interleaved sessions cannot evict one another, and the entry dies with
// the model it describes rather than pinning it (the model is replaced immutably on
// every mutation, so a stale entry is unreachable the instant it is stale). `undefined`
// cannot be a WeakMap key, but it does not need to be: a session with no loaded model
// gets the shared `EMPTY_*` constant, which is referentially stable by construction.
//
// The other memos in this file (`visibleCache`, `reachCache`, `facetsCache`) take NO
// parameter beyond the state, so within one store there is only one live key; they keep
// one slot per store, keyed on its `sessions` Map, because a page can hold more than one
// store. `selectTimeline` is parameterized like `sessionCost` and `contextUsage` and is
// memoized the same way, for the same reason.

/** A session's rolled-up cost, or all-zeros when no aggregates are loaded. */
export interface SessionCost {
  totalUsd: number;
  byModel: Record<string, number>;
  byQuerySource: Record<string, number>;
}

/** A session's context-window usage, or all-zeros when no aggregates are loaded.
 *  `pct` is 0 when the limit is missing (never divide-by-zero). */
export interface ContextUsage {
  tokens: number;
  limit: number;
  pct: number;
}

const EMPTY_COST: SessionCost = { totalUsd: 0, byModel: {}, byQuerySource: {} };
const EMPTY_CONTEXT: ContextUsage = { tokens: 0, limit: 0, pct: 0 };

const costByModel = new WeakMap<TurnModel, SessionCost>();

/** Cost roll-up for a session from `TurnModel.aggregates`; zeros when absent.
 *
 *  Guaranteed stable: for an unchanged state, repeated calls with the same `sessionId`
 *  return the IDENTICAL object no matter which other sessionIds are read in between. */
export function sessionCost(state: ChatState, sessionId: string | null): SessionCost {
  const model = turnsFor(state, sessionId);
  if (!model) return EMPTY_COST;
  const cached = costByModel.get(model);
  if (cached) return cached;
  const agg = model.aggregates;
  const result: SessionCost = agg
    ? {
        totalUsd: agg.totalUsd ?? 0,
        byModel: agg.byModel ?? {},
        byQuerySource: agg.byQuerySource ?? {},
      }
    : EMPTY_COST;
  costByModel.set(model, result);
  return result;
}

const contextByModel = new WeakMap<TurnModel, ContextUsage>();

/** Context-window usage for a session from `TurnModel.aggregates`; zeros when absent.
 *  `pct = tokens/limit*100`, or 0 when the limit is missing/zero.
 *
 *  Same stability guarantee as `sessionCost`. */
export function contextUsage(state: ChatState, sessionId: string | null): ContextUsage {
  const model = turnsFor(state, sessionId);
  if (!model) return EMPTY_CONTEXT;
  const cached = contextByModel.get(model);
  if (cached) return cached;
  const agg = model.aggregates;
  const tokens = agg?.contextTokens ?? 0;
  const limit = agg?.contextLimit ?? 0;
  const result: ContextUsage =
    tokens === 0 && limit === 0
      ? EMPTY_CONTEXT
      : { tokens, limit, pct: limit > 0 ? (tokens / limit) * 100 : 0 };
  contextByModel.set(model, result);
  return result;
}

/** Turns for a session, or an empty array. */
export function turnList(state: ChatState, sessionId: string | null): Turn[] {
  return turnsFor(state, sessionId)?.turns ?? [];
}

/** Entry map for a session, or an empty record. */
export function entriesFor(state: ChatState, sessionId: string | null): Record<string, Entry> {
  return turnsFor(state, sessionId)?.entries ?? {};
}

/**
 * The entry ids to render for a turn, respecting the view:
 *  - 'turns' (collapsed): only `!duplicate` entries — the dedup selector.
 *  - 'raw'   (timeline):  every entry, ordered by eventId — the audit surface.
 * A pure selector over the full, annotated model; never a stored lossy form.
 */
export function visibleEntryIdsFor(
  model: TurnModel | undefined,
  turnId: string,
  view: 'turns' | 'raw',
): string[] {
  if (!model) return [];
  const turn = turnsById(model).get(turnId);
  if (!turn) return [];
  // TRUST the turn's own entry order — never re-sort by eventId. The model's
  // order is maintained by its producers (log-store materializes in event order,
  // the live reducer appends in arrival order, the merge interleaves the two),
  // and after a merge a turn holds entries from TWO id spaces (log-store page
  // ids and stream frame ids), so a numeric sort across them puts the newest
  // live entries first — the reported "narration arrives out of order".
  if (view === 'raw') return turn.entryIds;
  return turn.entryIds.filter((id) => !model.entries[id]?.duplicate);
}

/** A model's turns by id. `visibleEntryIdsFor` is called once per turn on every render,
 *  and finding the turn by scanning `model.turns` each time made a render quadratic in
 *  the turn count. Keyed on the model, which is replaced rather than edited, so an
 *  entry is never stale — see "Why these memoize in a WeakMap" above. */
const turnsByIdByModel = new WeakMap<TurnModel, Map<string, Turn>>();

function turnsById(model: TurnModel): Map<string, Turn> {
  let byId = turnsByIdByModel.get(model);
  if (!byId) {
    byId = new Map(model.turns.map((t) => [t.id, t]));
    turnsByIdByModel.set(model, byId);
  }
  return byId;
}

/** The dedup annotation of a model's entries, and each group's members. */
interface SourceIndex {
  byId: Map<string, Entry>;
  byGroup: Map<string, Entry[]>;
}

const sourceIndexByModel = new WeakMap<TurnModel, SourceIndex>();

/** All copies in an entry's dedup group (the sources badge). Recomputes the
 *  annotation over the model's entries to answer independent of storage.
 *
 *  The annotation runs ONCE per model. The Raw view asks this for every entry it draws,
 *  and recomputing a pass over every entry of the transcript each time made drawing a
 *  turn quadratic in its size: measured 2.8 s blocking the main thread on opening
 *  br_1789409869293194266 (one turn, ~1,250 entries) in the Raw view. Members come back
 *  in the same order `groupMembers` gives: the order of `model.entries`. */
export function sourcesForEntry(model: TurnModel | undefined, entryId: string): Entry[] {
  if (!model) return [];
  let index = sourceIndexByModel.get(model);
  if (!index) {
    const annotated = annotateOTelDuplicates(Object.values(model.entries));
    index = { byId: new Map(), byGroup: new Map() };
    for (const e of annotated) {
      index.byId.set(e.id, e);
      if (e.groupId) {
        const members = index.byGroup.get(e.groupId);
        if (members) members.push(e);
        else index.byGroup.set(e.groupId, [e]);
      }
    }
    sourceIndexByModel.set(model, index);
  }
  const entry = index.byId.get(entryId);
  if (!entry) return [];
  if (!entry.groupId) return [entry];
  return index.byGroup.get(entry.groupId) ?? [entry];
}

// ---- Timeline selector (Path A Timeline pane) ----
//
// A pure, memoizable transform of the materialized `entries` into the
// event-granular, turn→task-grouped structure the Timeline pane renders. It mirrors
// the grouping semantics of bridge-ui `Timeline.tsx` rowsToTimeline (group by turn,
// sub-group tool/thinking/result/error, respect task_* scoping) but returns DATA,
// not JSX — the pane stays presentation-only and never re-derives. Being the raw
// audit surface (see WIRE.md), it is event-granular: every entry is represented,
// ordered by eventId, so the timeline can reconstruct every stored event.

export type TimelineTone =
  | 'turn'
  | 'task-start'
  | 'task-done'
  | 'task-err'
  /** a task that was taken away rather than finishing or failing — the host
   *  process died with it in flight, or the harness reported it stopped. Not
   *  'task-err': nothing went wrong with the work, it was cut short. */
  | 'task-cancelled'
  | 'thinking'
  | 'tool'
  | 'tool-done'
  | 'tool-err'
  /** a tool call whose outcome can never be known: the source event carried no
   *  tool_id, so no result can ever be joined to it. Distinct from 'tool',
   *  which means still running. */
  | 'tool-unknown'
  | 'result'
  | 'error'
  | 'system'
  | 'text';

/** Task statuses that mean the subagent will do no more work.
 *
 * Mirrors msg.TaskStatusIsTerminal. An unrecognized status is NOT terminal, so
 * a status a harness adds later cannot close a task that is still running. */
const TERMINAL_TASK_STATUSES = new Set(['completed', 'failed', 'cancelled']);

/** Exported so the live-status derivation (`store/liveStatus.ts`) closes a task on
 *  exactly the same statuses the timeline does — one answer to "is it finished". */
export function isTerminalTaskStatus(status: string | undefined): boolean {
  return status !== undefined && TERMINAL_TASK_STATUSES.has(status);
}

/** One event-granular row in the timeline (presentation-agnostic). */
export interface TimelineItem {
  key: string;
  entryId: string;
  turnId: string;
  /** The task this row is ABOUT — set on the two rows that report a task's
   *  lifecycle, its spawn and its finish. It does not mean "this row happened
   *  inside that task": a task's own work is not in this session at all. */
  taskId?: string;
  /** The bridge session to follow to see what the task did. Only a task row
   *  carries one, and only when the task got a session — a backgrounded shell
   *  never does. */
  subagentSessionId?: string;
  /** What kind of background work a task row describes (`local_agent`,
   *  `local_bash`, …) and, for an agent, the role it was spawned as. Only the
   *  spawn row carries these; the harness sends them nowhere else. */
  taskType?: string;
  subagentType?: string;
  icon: string;
  label: string;
  detail?: string; // one-line preview
  fullText?: string; // untruncated source text
  ts: string;
  tone: TimelineTone;
}

/** A turn group: its header row (the user prompt / first event) + its rows, in
 *  the order they happened. Flat on purpose — see toTimelineItems. */
export interface TimelineTurnGroup {
  turnId: string;
  header: TimelineItem;
  children: TimelineItem[];
}

/** The full render-ready structure: the flat ordered items plus the turn→task tree. */
export interface TimelineView {
  items: TimelineItem[];
  turns: TimelineTurnGroup[];
  count: number;
}

function oneLine(s: string): string {
  return s.replace(/\s+/g, ' ').trim();
}

/** One line, capped. Returns the flattened string with an ellipsis when it had
 *  to cut. `oneLine` above is uncapped and stays that way — of its three
 *  callers only this one wants a cap; `makeItem` and `toTimelineItems` flatten
 *  row text that must survive whole, and a cap on the shared helper would
 *  silently truncate every one of those row types. */
function oneLineCapped(s: string, max: number): string {
  const flat = oneLine(s);
  return flat.length > max ? flat.slice(0, max) + '…' : flat;
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' && v.length > 0 ? v : undefined;
}

function safeJson(v: unknown): string {
  try {
    return JSON.stringify(v) ?? '';
  } catch {
    return '';
  }
}

function toolIsError(e: Entry): boolean {
  if (e.toolError !== undefined) return e.toolError;
  // Fallback for a log-store that predates the promotion — see toolPairing.ts.
  const tr = (e.raw as { tool_result?: { is_error?: boolean } } | undefined)?.tool_result;
  return tr?.is_error === true;
}

function toolText(e: Entry): string | undefined {
  const parts: string[] = [];
  if (e.toolInput !== undefined) {
    parts.push(typeof e.toolInput === 'string' ? e.toolInput : safeJson(e.toolInput));
  }
  if (e.toolResult !== undefined) {
    parts.push(typeof e.toolResult === 'string' ? e.toolResult : safeJson(e.toolResult));
  }
  const s = parts.filter(Boolean).join(' → ');
  return s.length > 0 ? s : undefined;
}

/** How long a tool's one-line preview may run before it is cut.
 *
 *  The timeline draws one row per event and the row is a flex line, so an
 *  uncapped preview is not merely ugly — a 40 KB `tool_result` string reaches
 *  the DOM in full and every row of a long session carries one. */
const TOOL_SNIPPET_MAX = 80;

/** The input keys worth showing, best first.
 *
 *  A tool's arguments are not equally identifying. `command` tells you what a
 *  Bash call did; `description` is the model's prose about it, and `file_path`
 *  says which file an Edit touched while its `old_string`/`new_string` are the
 *  bulk of the payload. Picking the first key that is present puts the
 *  identifying argument on the row and leaves the rest to `fullText`.
 *
 *  Ported from bridge-ui's `chat/utils.ts` `toolSnippet`, which the original
 *  chat's timeline has always used. Keep the order in step with it. */
const TOOL_SNIPPET_KEYS = [
  'command',
  'file_path',
  'path',
  'pattern',
  'url',
  'query',
  'description',
  'prompt',
] as const;

/** A TodoWrite call's input is a whole todo list, and every key on it is an
 *  array — so the generic path below can only ever say `todos[7]`. Count the
 *  statuses instead and name the one in progress, which is the single thing a
 *  reader wants from that row. */
function formatTodoWrite(todos: unknown): string | undefined {
  if (!Array.isArray(todos)) return undefined;
  let done = 0;
  let active = 0;
  let pending = 0;
  let current: string | undefined;
  for (const raw of todos) {
    if (!raw || typeof raw !== 'object') continue;
    const todo = raw as { status?: string; content?: string; activeForm?: string };
    if (todo.status === 'completed') done++;
    else if (todo.status === 'in_progress') {
      active++;
      current = todo.activeForm || todo.content || current;
    } else pending++;
  }
  const total = todos.length;
  const bits: string[] = [`${total} todo${total === 1 ? '' : 's'}`];
  const counts: string[] = [];
  if (done) counts.push(`${done}✓`);
  if (active) counts.push(`${active}⏺`);
  if (pending) counts.push(`${pending}○`);
  if (counts.length) bits.push(`(${counts.join(' ')})`);
  if (current) bits.push(`— ${oneLineCapped(current, 60)}`);
  return bits.join(' ');
}

/**
 * The one line that says what a tool call was ASKED to do.
 *
 * ⚠️ Reads the input and never the result, and that is the whole point of it.
 * `toolText` above joins both into `input → result`, which is what this row
 * used to show, and the result is the bigger half by orders of magnitude — a
 * Bash call reads `{"command":"npm run build","description":"…"} → {"stdout":"…`
 * and the command, the only part that identifies the row, is buried behind the
 * JSON punctuation of its own arguments. Whether the call SUCCEEDED is already
 * on the row twice, as the icon and as the tone, so the result was paying for
 * the line and answering a question the row had answered.
 *
 * `toolText` is still what `fullText` carries, so the result is one hover away
 * and nothing a reader could see before is gone.
 *
 * Returns '' when there is nothing worth saying — no input, or an input with no
 * keys. The caller leaves `detail` unset rather than drawing an empty span.
 */
function toolSnippet(e: Entry): string {
  const input = e.toolInput;
  if (input === undefined || input === null) return '';
  // A string input has no keys to prefer; it IS the argument.
  if (typeof input === 'string') return oneLineCapped(input, TOOL_SNIPPET_MAX);
  if (typeof input !== 'object') return oneLineCapped(String(input), TOOL_SNIPPET_MAX);

  const record = input as Record<string, unknown>;
  const keys = Object.keys(record);
  if (keys.length === 0) return '';

  if (e.toolName === 'TodoWrite') {
    const summary = formatTodoWrite(record.todos);
    if (summary) return summary;
  }

  for (const key of TOOL_SNIPPET_KEYS) {
    const value = record[key];
    if (typeof value === 'string' && value) {
      return `${key}=${oneLineCapped(value, TOOL_SNIPPET_MAX)}`;
    }
  }

  // No preferred key. Fall back to the first one the input actually has, which
  // at least names the shape of the call rather than dumping it.
  //
  // `keys` is non-empty — the guard above returned for the empty case — but the
  // index signature says otherwise under `noUncheckedIndexedAccess`, so the
  // check is written out rather than asserted away with `!`.
  const firstKey = keys[0];
  if (firstKey === undefined) return '';
  const first = record[firstKey];
  if (typeof first === 'string') return `${firstKey}=${oneLineCapped(first, TOOL_SNIPPET_MAX)}`;
  if (Array.isArray(first)) return `${firstKey}[${first.length}]`;
  return oneLineCapped(keys.join(','), TOOL_SNIPPET_MAX);
}

function makeItem(
  e: Entry,
  o: {
    icon: string;
    label: string;
    tone: TimelineTone;
    text?: string;
    /** The rendered one-liner, when it is not just `text` flattened.
     *
     *  Only tool rows pass this. For every other row the preview IS the source
     *  text with its whitespace collapsed, so deriving it is right; a tool row
     *  picks out the one identifying argument instead (`toolSnippet`) while
     *  `fullText` keeps the whole input-and-result for the hover. */
    detail?: string;
    keySuffix?: string;
  },
): TimelineItem {
  const item: TimelineItem = {
    // A task's spawn and finish can come off the SAME entry when the harness
    // repeats a terminal frame, so the key takes a suffix to stay unique.
    key: `tl_${e.id}${o.keySuffix ?? ''}`,
    entryId: e.id,
    turnId: e.turnId,
    icon: o.icon,
    label: o.label,
    ts: e.ts,
    tone: o.tone,
  };
  // `detail` and `fullText` are set together or not at all, so a row never
  // offers a hover that repeats what is already on screen — or, worse, a
  // hover with no visible row content behind it.
  const line = o.detail !== undefined ? oneLine(o.detail) : o.text ? oneLine(o.text) : '';
  if (line) {
    item.detail = line;
    item.fullText = o.text;
  }
  return item;
}

/** How a finished task is drawn, by its canonical terminal status.
 *
 *  A status with no entry here is not terminal and never reaches this table —
 *  isTerminalTaskStatus is the gate, and it mirrors msg.TaskStatusIsTerminal.
 */
/** Copy the task's identity onto a row. Both of a task's rows carry it, so a
 *  client can link to the subagent from the finish as readily as from the
 *  spawn. Absent fields stay absent — an empty subagentSessionId means there is
 *  no session, which is the honest answer for a backgrounded shell. */
function applyTaskFields(item: TimelineItem, e: Entry): void {
  if (e.taskId) item.taskId = e.taskId;
  if (e.subagentSessionId) item.subagentSessionId = e.subagentSessionId;
}

const TASK_FINISH_BY_STATUS: Record<string, { icon: string; label: string; tone: TimelineTone }> = {
  completed: { icon: '▣✓', label: 'Task finished', tone: 'task-done' },
  failed: { icon: '▣✗', label: 'Task failed', tone: 'task-err' },
  cancelled: { icon: '▣–', label: 'Task canceled', tone: 'task-cancelled' },
};

/** Flatten the model's entries into the ordered rows of what THIS session did.
 *
 *  Flat, and chronological, with no nesting. The timeline used to open a task
 *  "scope" and file every following row inside it, tracked as a single
 *  `currentTaskId` scalar set by the last task_started seen. With concurrent
 *  subagents — the normal case; one measured session ran four at once with
 *  their rows interleaved event by event — that scalar was wrong three ways at
 *  once. Rows landed under whichever task started most recently, one subagent's
 *  completion stamped its status and summary onto another's header, and the
 *  first task to finish closed the scope, dropping every still-running task's
 *  remaining rows out to the turn. Grouping was by CONTIGUOUS run as well, so
 *  interleaving shattered a task into repeated headers regardless.
 *
 *  None of it was recoverable by fixing the bookkeeping, because the premise was
 *  wrong: a subagent's work is not part of this session. The bridge server routes
 *  it into the subagent's own session, and a task row here links to it.
 *
 *  So a task contributes exactly two rows, where they happened: a spawn and a
 *  finish. Everything in between is the subagent's, and belongs on the other end
 *  of that link.
 */
function toTimelineItems(model: TurnModel): TimelineItem[] {
  // Model order (turns, then each turn's own entry order), NOT an eventId sort:
  // after a merge the model mixes log-store page ids with stream frame ids, and
  // sorting across the two spaces is meaningless. Entries outside any turn
  // (defensive: should not exist) go last, in map order.
  const inTurns: Entry[] = [];
  const seen = new Set<string>();
  for (const turn of model.turns) {
    for (const id of turn.entryIds) {
      const e = model.entries[id];
      if (e && !seen.has(id)) {
        inTurns.push(e);
        seen.add(id);
      }
    }
  }
  for (const id of Object.keys(model.entries)) {
    if (!seen.has(id)) inTurns.push(model.entries[id]!);
  }
  const entries = inTurns;

  // Which calls actually finished, by TOOL ID. The live reducer merges a result
  // onto its call, so `toolResult !== undefined` catches those — but the
  // server-materialized page (`GET /messages`) keeps call and result as separate
  // rows, and reading `toolResult` alone drew a ⚙ on every cold-loaded call,
  // forever. See store/toolPairing.ts.
  const resulted = resultedToolIds(entries);

  const out: TimelineItem[] = [];
  const seenTurn = new Set<string>();
  // One spawn row and one finish row per task. Claude Code repeats both — a
  // task_started can arrive twice, and a close is commonly narrated twice
  // (task_updated then task_notification, the second carrying the summary the
  // first lacks). Without these the timeline showed a task starting and
  // finishing several times over.
  const spawnedTasks = new Set<string>();
  const finishIdxByTask = new Map<string, number>();

  for (const e of entries) {
    // Somebody else's work. The bridge server routes a subagent's frames into
    // the subagent's own session; one that lands here anyway was kept on the
    // parent by the fail-safe for a frame whose task_started was missed, and
    // showing it would put another session's rows in this session's timeline.
    if (e.harnessParentId) continue;

    // User prompt → the turn header row.
    if (e.role === 'user') {
      const first = !seenTurn.has(e.turnId);
      seenTurn.add(e.turnId);
      out.push(makeItem(e, { icon: first ? '▶' : '»', label: 'Turn', tone: 'turn', text: e.text }));
      continue;
    }

    // A task's lifecycle: spawn and finish, and nothing else.
    if (e.kind === 'system' && e.subtype && e.subtype.startsWith('task_')) {
      // Keyed by task id, falling back to the entry when a frame names no task
      // — the adapter's catch-all branch forwards an unknown task_* subtype
      // with its correlators stripped, and two of those must not collapse onto
      // each other.
      const taskKey = e.taskId ?? `entry_${e.id}`;

      if (e.subtype === 'task_started') {
        if (spawnedTasks.has(taskKey)) continue;
        spawnedTasks.add(taskKey);
        const spawn = makeItem(e, {
          icon: '▣',
          label: 'Task started',
          tone: 'task-start',
          text: e.text || undefined,
        });
        applyTaskFields(spawn, e);
        spawn.taskType = e.taskType;
        spawn.subagentType = e.subagentType;
        out.push(spawn);
        continue;
      }

      if (!isTerminalTaskStatus(e.taskStatus)) {
        // task_progress and the non-terminal patches. This is the subagent
        // narrating its own tool calls to its parent — one row per call, 68 of
        // them for a single task in one measured session, three times the
        // parent's own output. It belongs to the subagent's session, and the
        // live status line already reads the newest one to say what a running
        // subagent is doing.
        continue;
      }

      const drawn = TASK_FINISH_BY_STATUS[e.taskStatus!];
      const existingIdx = finishIdxByTask.get(taskKey);
      if (existingIdx !== undefined) {
        // A second close for the same task: one row, moved to the LATEST one.
        //
        // Usually the two are milliseconds apart — task_updated carries the
        // status, task_notification follows with the report — and it makes no
        // visible difference which is used. But a task can genuinely resume:
        // measured on a real subagent, task_started → close → task_started
        // again → close again, thirty seconds apart. Anchoring on the first
        // close then drew "Task finished" above work the subagent had not done
        // yet.
        const existing = out.splice(existingIdx, 1)[0]!;
        for (const [key, idx] of finishIdxByTask) {
          if (idx > existingIdx) finishIdxByTask.set(key, idx - 1);
        }
        existing.ts = e.ts;
        existing.entryId = e.id;
        existing.icon = drawn?.icon ?? existing.icon;
        existing.label = drawn?.label ?? existing.label;
        existing.tone = drawn?.tone ?? existing.tone;
        const later = e.taskSummary || e.text;
        if (later) {
          existing.detail = oneLine(later);
          existing.fullText = later;
        }
        applyTaskFields(existing, e);
        finishIdxByTask.set(taskKey, out.length);
        out.push(existing);
        continue;
      }
      // The summary is the subagent's own report of what it did and is the
      // payload the notification exists to deliver; `text` is the reason a
      // close was derived rather than reported, which only the server sends.
      const finish = makeItem(e, {
        icon: drawn?.icon ?? '▣',
        label: drawn?.label ?? 'Task finished',
        tone: drawn?.tone ?? 'task-done',
        text: e.taskSummary || e.text || undefined,
        keySuffix: '_finish',
      });
      applyTaskFields(finish, e);
      finishIdxByTask.set(taskKey, out.length);
      out.push(finish);
      continue;
    }

    if (e.kind === 'thinking') {
      out.push(
        makeItem(e, {
          icon: '💭',
          label: 'Thinking',
          tone: 'thinking',
          text: e.text,
        }),
      );
      continue;
    }

    if (e.kind === 'tool_call' || e.kind === 'tool_result') {
      const toolId = toolIdOf(e);
      const done =
        e.kind === 'tool_result' ||
        e.toolResult !== undefined ||
        (toolId !== undefined && resulted.has(toolId));
      const err = toolIsError(e);
      // An unpairable call has no result coming — it was keyed by event id
      // because the source event carried no tool_id, so nothing can ever join
      // the two. Rendering it "running" promises an outcome that will never
      // arrive; that is the spinner that sat on screen forever. Say the
      // outcome is unknown instead. The reducer stamps `unpairable` when it
      // chooses the key; the server materializer stamps nothing, so the absent
      // tool id is read directly.
      const unresolvable = !done && !err && (e.unpairable === true || toolId === undefined);
      out.push(
        makeItem(e, {
          icon: err ? '✗' : done ? '✓' : unresolvable ? '–' : '⚙',
          label: e.toolName || 'tool',
          tone: err ? 'tool-err' : done ? 'tool-done' : unresolvable ? 'tool-unknown' : 'tool',
          // The row shows what the call was asked to do; the hover keeps the
          // whole input and result. See `toolSnippet`.
          //
          // ⚠️ `|| undefined` is load-bearing. A standalone `tool_result` entry
          // carries a result and NO input, so the snippet is empty for it —
          // and an empty string handed to `makeItem` as an explicit detail
          // would read as "this row has been given its preview" and leave the
          // row blank. Undefined means "no snippet to offer", which is what
          // sends `makeItem` back to `text` and keeps the result on screen.
          detail: toolSnippet(e) || undefined,
          text: toolText(e),
        }),
      );
      continue;
    }

    if (e.kind === 'result') {
      out.push(makeItem(e, { icon: '■', label: 'Done', tone: 'result', text: e.text }));
      continue;
    }

    if (e.kind === 'error') {
      out.push(
        makeItem(e, {
          icon: '⚠',
          label: 'Error',
          tone: 'error',
          text: e.text,
        }),
      );
      continue;
    }

    if (e.kind === 'system') {
      out.push(
        makeItem(e, {
          icon: 'ⓘ',
          label: e.subtype || 'System',
          tone: 'system',
          text: e.text,
        }),
      );
      continue;
    }

    if (e.kind === 'text' && e.text) {
      out.push(
        makeItem(e, { icon: '✎', label: 'Text', tone: 'text', text: e.text }),
      );
      continue;
    }
    // meta / empty entries carry nothing to render — skipped, as bridge-ui does.
  }
  return out;
}

function groupTurns(items: TimelineItem[]): TimelineTurnGroup[] {
  const groups: TimelineTurnGroup[] = [];
  let i = 0;
  while (i < items.length) {
    const turnId = items[i]!.turnId;
    const start = i;
    while (i < items.length && items[i]!.turnId === turnId) i++;
    const slice = items.slice(start, i);
    groups.push({ turnId, header: slice[0]!, children: slice.slice(1) });
  }
  return groups;
}

// Identity memo, one slot PER MODEL: the TurnModel is replaced immutably on every
// mutation, so referential equality is a correct and cheap staleness check, and a
// WeakMap keeps the entry alive exactly as long as the model it describes.
//
// This is parameterized like the cost/context selectors, so it carries their hazard too
// (see the long note above them): a single shared slot would mean two panes derived from
// two different models — a live session and a preview, say — thrashing each other out of
// the cache on every render and re-deriving the WHOLE transcript each time, which here
// costs a full sort-and-group over every entry rather than a five-field object.
const timelineByModel = new WeakMap<TurnModel, TimelineView>();

/** The answer for "no model at all". Shared so the empty case is referentially stable
 *  too — `undefined` cannot key a WeakMap, and a fresh empty object every call would
 *  re-render the pane forever on a session that has nothing to show. */
const EMPTY_TIMELINE: TimelineView = { items: [], turns: [], count: 0 };

/**
 * Build the event-granular, turn→task-grouped timeline for a materialized model.
 * Pure + memoized on the model's identity so the Timeline pane never re-derives.
 */
export function selectTimeline(model: TurnModel | undefined): TimelineView {
  if (!model) return EMPTY_TIMELINE;
  const cached = timelineByModel.get(model);
  if (cached) return cached;
  const items = toTimelineItems(model);
  const result: TimelineView = { items, turns: groupTurns(items), count: items.length };
  timelineByModel.set(model, result);
  return result;
}

// --- Harness capabilities + model picker (controls bar) ---
//
// Both read the CANONICAL registries fetched from GET /harnesses and GET /models —
// never a hardcoded per-harness allowlist. Pure over the lists (not ChatState) so they
// stay trivially testable and the React hooks can memoize on the stable slice identity.

/**
 * The capability set for a harness — the source the controls bar gates each control on
 * (`model` / `effort` / `compact` / `fork` / `system_prompt` / `tools`). Returns an empty
 * set when the registry has not loaded yet or the harness is unknown (a control simply
 * stays hidden — never guessed into existence).
 */
export function harnessCapabilities(
  harnesses: HarnessMeta[] | null,
  harnessId: string | null | undefined,
): Set<string> {
  if (!harnessId || !harnesses) return new Set();
  const h = harnesses.find((x) => x.name === harnessId);
  return new Set(h?.capabilities ?? []);
}

/**
 * The models offered for a harness's picker: every enabled model when no harness (or a
 * harness that declares no `supportedProviders`) is given, else only the models whose
 * `provider` the harness supports — mirroring bridge-ui's `harnessModels` filter. Returns
 * an empty array until the model registry loads.
 */
export function modelsForHarness(
  models: ModelOption[] | null,
  harnesses: HarnessMeta[] | null,
  harnessId?: string | null,
): ModelOption[] {
  const list = models ?? [];
  if (!harnessId) return list;
  const providers = harnesses?.find((x) => x.name === harnessId)?.supportedProviders;
  if (!providers || providers.length === 0) return list;
  return list.filter((m) => providers.includes(m.provider));
}
