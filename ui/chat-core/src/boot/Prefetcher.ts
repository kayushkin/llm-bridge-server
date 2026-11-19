import type { ApiClient } from '../net/ApiClient.js';
import type { SessionCache } from '../cache/SessionCache.js';
import { enforceCacheBound } from '../cache/evict.js';
import type { ChatStoreApi, FilterState } from '../store/ChatStore.js';
import type { SessionSummaryFilterAxes } from '../net/types.js';

/** Narrow a `FilterState` to the six axes the summary endpoint understands.
 *
 *  Explicit rather than handing the whole `FilterState` over: `folder` and `search`
 *  are not server-side filters, and picking the six states that at the boundary
 *  instead of relying on the request builder to quietly ignore the other two. */
function summaryFilterAxesOf(filter: FilterState): SessionSummaryFilterAxes {
  return {
    harness: filter.harness,
    status: filter.status,
    type: filter.type,
    purpose: filter.purpose,
    mode: filter.mode,
    machine: filter.machine,
  };
}

/** A stable key for a filter's six axes, to tell whether a chip change moved them. */
function summaryFilterKey(filter: SessionSummaryFilterAxes): string {
  return JSON.stringify([
    filter.harness ?? [],
    filter.status ?? [],
    filter.type ?? [],
    filter.purpose ?? [],
    filter.mode ?? [],
    filter.machine ?? [],
  ]);
}

// Boot sequence (§6) + hover/idle prefetch. The boot order is the whole latency
// story: hydrate from cache and PAINT first (0 network), THEN fan out the three
// parallel reads. Hover/idle prefetch warms a cold session before the click so a
// select() is a Map read.
//
// HOW THE SIDEBAR'S WINDOW FILLS.
//
// 1. The boot page is FILTERED by whatever the sidebar's chips are set to, which
//    `createChatStore()` has already restored from localStorage before the first
//    paint. The page is a strict newest-first prefix, so on this box an unfiltered
//    one is spent on machine traffic; filtering server-side is what makes the first
//    page relevant rather than merely recent.
//
// 2. Changing a chip fetches the first page of the NEW filter (`refilter`). The
//    local sieve still applies at once over the rows already held; the page makes
//    sure the newest rows that match are among them.
//
// 3. Older pages come on request — scrolling to the end or the "Load older sessions"
//    button — always for the filter currently set.
//
// 4. A content-search hit whose session is not loaded is fetched by id
//    (`loadSessionsByIds`), so a hit is shown rather than counted as hidden.
//
// This replaced a background loop that paged 20 times at every boot, 2,000 sessions,
// so that chip clicks and search could sieve locally. It cost 20 requests and a full
// sidebar recompute per page on every page load, and still missed older matches.

export interface PrefetcherConfig {
  store: ChatStoreApi;
  api: ApiClient;
  cache: SessionCache;
  recentN?: number;
  turnsPerBundle?: number;
  cacheLimit?: number;
  /** Sessions per sidebar page, for the boot page and every older page after it. */
  sessionsPerPage?: number;
}

/** How long the cold cache paint may take before boot gives up on it and goes to the
 *  network.
 *
 *  The cache is an optimization and boot is what the whole UI waits on, so this step is
 *  bounded rather than trusted. `SessionCache` already bounds its own CONNECTION, but a
 *  bound there only covers the failure that was diagnosed (an upgrade blocked by another
 *  tab); this one covers the class — any read that does not come back, for any reason —
 *  at the layer that actually holds up `prime()`. A late paint is discarded rather than
 *  applied, so it can never land on top of fresher network rows. */
const CACHE_PAINT_BUDGET_MS = 4000;

export class Prefetcher {
  /** Default sessions per sidebar page. Mirrors the summary endpoint's own default
   *  so a boot that names no page size asks for exactly what the server would give. */
  static readonly DEFAULT_SESSIONS_PER_PAGE = 100;

  private readonly store: ChatStoreApi;
  private readonly api: ApiClient;
  private readonly cache: SessionCache;
  private readonly recentN: number;
  private readonly turnsPerBundle: number;
  private readonly cacheLimit: number;
  private readonly sessionsPerPage: number;
  /** Hover prefetches in flight, by session id — the PROMISE, not just the id.
   *
   *  A bare Set could say "someone is fetching this" but not "join that fetch", and
   *  the click path needs the second: hovering a sidebar row and then clicking it is
   *  how every session is opened, and the two used to fetch the same page twice,
   *  concurrently, because each guarded on a register the other could not see.
   *  Measured on the live dashboard — two `GET /messages` for one open, 2-19ms
   *  apart, each parsing a page up to a megabyte. */
  private readonly inFlight = new Map<string, Promise<void>>();

  /** The filter the current paging cursor belongs to, and its key. Pages are only
   *  ever fetched for this filter; a chip change replaces both (`refilter`). */
  private pageFilter: SessionSummaryFilterAxes = {};
  private pageFilterKey = '';
  /** Bumped by each `refilter`, so a first page that lands after a newer chip
   *  change is dropped instead of installing a cursor for a filter no longer set. */
  private refilterGeneration = 0;

  constructor(config: PrefetcherConfig) {
    this.store = config.store;
    this.api = config.api;
    this.cache = config.cache;
    this.recentN = config.recentN ?? 20;
    this.turnsPerBundle = config.turnsPerBundle ?? 30;
    this.cacheLimit = config.cacheLimit ?? 50;
    this.sessionsPerPage = config.sessionsPerPage ?? Prefetcher.DEFAULT_SESSIONS_PER_PAGE;
  }

  /** Step 1: hydrate the store from IndexedDB and paint. 0 network. Resolves
   *  once the cached list + turns are in the store (or immediately if disabled). */
  async hydrateFromCache(): Promise<void> {
    if (!this.cache.isEnabled) return;
    const actions = this.store.getState().actions;
    try {
      // Raced, not awaited: see CACHE_PAINT_BUDGET_MS. `null` means the budget won.
      const hydrated = await Promise.race([
        this.cache.hydrate(),
        new Promise<null>((resolve) => setTimeout(() => resolve(null), CACHE_PAINT_BUDGET_MS)),
      ]);
      if (hydrated === null) {
        console.warn(
          `chat-core: the cache did not answer within ${CACHE_PAINT_BUDGET_MS}ms — painting from the network instead. If this repeats, another tab is holding an older version of the cache open.`,
        );
        return;
      }
      // Bounded to one page, so the paint cannot show hundreds of rows and then
      // SHRINK back the moment the network's first page replaces them. The cached
      // list is sorted newest-first, so the head of it is the same window page one
      // is about to confirm.
      //
      // The slice is the ONLY thing bounding this paint: `enforceListBound` bounds the
      // IndexedDB list store, not `state.sessions`.
      const painted = hydrated.list.slice(0, this.sessionsPerPage);
      if (painted.length > 0) actions.setSessions(painted);
      for (const [id, model] of hydrated.turns) actions.setTurns(id, model);
    } catch (err) {
      // Cold cache, disabled, or unavailable (an upgrade another tab is blocking —
      // SessionCache.db bounds that wait rather than hanging on it). The network paint
      // below covers all three; it is reported because a silent miss here reads as an
      // empty account.
      console.warn('chat-core: cache paint skipped', err);
    }
  }

  /** Step 2: the parallel reads. Summary + recent-bundle + folders land and fill
   *  the hot store. Runs after (or racing) the cache paint.
   *
   *  A validators request for every cached session used to go out here too, and its
   *  answer was never read. The active session is revalidated when it is opened
   *  (`SyncEngine.revalidateActive`), which is the only place a stale cached model
   *  matters. */
  async prime(): Promise<void> {
    const actions = this.store.getState().actions;
    actions.setListLoading(this.store.getState().sessions.size === 0);

    // The restored chip selection, read straight off the store rather than passed
    // in: `createChatStore()` rehydrates it synchronously, so it is already correct
    // here and a second copy threaded through config would be a second source of
    // truth for the same six arrays.
    this.pageFilter = summaryFilterAxesOf(this.store.getState().filter);
    this.pageFilterKey = summaryFilterKey(this.pageFilter);

    const summaryP = this.api
      .getSummary({ limit: this.sessionsPerPage, filter: this.pageFilter })
      .then((resp) => {
        // `resp.next` is the only truncation signal the endpoint offers (it carries no
        // total), so carrying it into the store is what lets the sidebar say "there is
        // more" and page down. Dropping it was the whole 100-session cap.
        actions.setSessions(resp.sessions, resp.next ?? null);
        void this.cache.putList(resp.sessions);
        return resp;
      })
      .catch(() => null);

    const bundleP = this.api
      .getRecentBundle({ n: this.recentN, turns: this.turnsPerBundle })
      .then((bundle) => {
        for (const id of Object.keys(bundle)) {
          const entry = bundle[id];
          if (!entry) continue;
          actions.upsertSession(entry.summary);
          actions.setTurns(id, entry.model);
          void this.cache.putSummary(entry.summary);
          void this.cache.putTurns(entry.model);
        }
        return bundle;
      })
      .catch(() => null);

    const foldersP = this.api
      .listFolders()
      .then((folders) => {
        actions.setFolders(folders);
        return folders;
      })
      .catch(() => null);

    await Promise.all([summaryP, bundleP, foldersP]);

    // The list bound is the page size, not a constant: the cold paint reads one
    // page, so one page is exactly what is worth keeping.
    if (this.cache.isEnabled) {
      // Reported rather than left to become an unhandled rejection: an eviction pass that
      // cannot run means the cache grows without bound, which is worth knowing about even
      // though it costs the user nothing right now.
      void enforceCacheBound(this.cache, this.cacheLimit, this.sessionsPerPage).catch(
        (err: unknown) => console.warn('chat-core: cache eviction pass failed', err),
      );
    }
  }

  /** Full boot: cache paint → parallel prime. */
  async boot(): Promise<void> {
    await this.hydrateFromCache();
    await this.prime();
  }

  /** Pull the next page of OLDER sessions, for the filter currently paged, and merge
   *  it into the sidebar's window. Scrolling to the end and the "Load older sessions"
   *  button both go through here.
   *
   *  No-op when a page is already in flight — the in-flight flag is set synchronously
   *  before the fetch, so a scroll event and a button click firing in the same tick
   *  cannot both start a request — or when there is no cursor, which means there is
   *  genuinely nothing older for this filter.
   *
   *  These pages are NOT written to the IndexedDB cache. The cache exists for the cold
   *  first paint, which is bounded to one page; persisting every page the user scrolled
   *  through would grow the cached list without bound and slow every subsequent boot.
   *
   *  A failure leaves the cursor in place so the affordance stays and the user can
   *  retry, and is logged rather than swallowed — an older page that silently never
   *  arrives reads as "that is all the sessions there are". */
  async loadOlderSessions(): Promise<void> {
    const state = this.store.getState();
    if (state.olderSessionsLoading) return;
    const cursor = state.olderSessionsCursor;
    if (!cursor) return;
    const actions = state.actions;
    const generation = this.refilterGeneration;
    actions.setOlderSessionsLoading(true);
    try {
      const resp = await this.api.getSummary({
        limit: this.sessionsPerPage,
        before: cursor,
        filter: this.pageFilter,
      });
      if (generation !== this.refilterGeneration) {
        // The chips changed while this page was in flight: its rows are still real
        // sessions, but its cursor belongs to the old filter.
        actions.mergeSessions(resp.sessions);
        actions.setOlderSessionsLoading(false);
        return;
      }
      actions.appendOlderSessions(resp.sessions, resp.next ?? null);
    } catch (err) {
      actions.setOlderSessionsLoading(false);
      console.error('chat-core: older sessions page failed', err);
    }
  }

  /** Re-aim paging at the filter now set on the store, and fetch its first page.
   *
   *  Called on every filter change and a no-op unless the six server-side axes moved
   *  (`folder` and `search` are local). The local sieve has already applied by the
   *  time this runs; this makes sure the newest sessions that match are loaded, and
   *  that "load older" pages the filter the user is looking at. Rows already held are
   *  kept — they still match or are sieved out — so nothing on screen flickers away. */
  async refilter(): Promise<void> {
    const filter = summaryFilterAxesOf(this.store.getState().filter);
    const key = summaryFilterKey(filter);
    if (key === this.pageFilterKey) return;
    this.pageFilter = filter;
    this.pageFilterKey = key;
    const generation = ++this.refilterGeneration;
    const actions = this.store.getState().actions;
    try {
      const resp = await this.api.getSummary({ limit: this.sessionsPerPage, filter });
      if (generation !== this.refilterGeneration) return;
      actions.mergeSessions(resp.sessions);
      actions.setOlderSessionsCursor(resp.next ?? null);
    } catch (err) {
      console.error('chat-core: filtered sessions page failed', err);
    }
  }

  /** Load the summaries of these sessions that are not already held — the sessions
   *  a content search matched outside the loaded pages. Measured 2026-08-02, 55–92%
   *  of a 100-hit search fell outside the window; without this they were counted as
   *  hidden and never shown. One POST lookup by id, merged without touching the
   *  paging cursor. */
  async loadSessionsByIds(sessionIds: readonly string[]): Promise<void> {
    const held = this.store.getState().sessions;
    const missing = sessionIds.filter((id) => !held.has(id));
    if (missing.length === 0) return;
    try {
      const resp = await this.api.getSummary({ sessionIds: missing, limit: missing.length });
      this.store.getState().actions.mergeSessions(resp.sessions);
    } catch (err) {
      console.error('chat-core: loading searched sessions failed', err);
    }
  }

  /** Hover/idle hint: warm a cold session so the next click is a Map read.
   *  No-op if already warm or a fetch is already in flight. */
  prefetch(sessionId: string): void {
    if (!sessionId) return;
    const state = this.store.getState();
    if (state.turnsBySession.has(sessionId)) return;
    if (state.turnsLoading.has(sessionId)) return;
    if (this.inFlight.has(sessionId)) return;
    // Announce the fetch in the SHARED store, not just in `inFlight`. Two other parts of
    // the system need to know a page is coming and neither can see a private field:
    // `select()` joins this fetch instead of starting a second one, and the SyncEngine
    // waits for the resume point it will carry instead of connecting the stream cold and
    // having the whole current turn replayed at it. Reading `turnsLoading` without ever
    // setting it — which is what this did — left both of them blind on exactly the path
    // a session is normally opened by: hover, then click.
    state.actions.setTurnsLoading(sessionId, true);
    const warming = this.warm(sessionId);
    this.inFlight.set(sessionId, warming);
    // A hover that fails is not an error: nothing is on screen waiting for it and the
    // click path fetches on demand. The handler is attached HERE, on the hover's own
    // reference, because `warming()` hands the same promise to `select()` and a
    // rejection nobody ever claimed — the user hovered and moved on — would otherwise
    // surface as an unhandled rejection in the console.
    void warming.catch(() => {});
  }

  /**
   * The hover prefetch in flight for this session, or undefined.
   *
   * `select()` joins it instead of starting its own fetch — see `inFlight`. It is
   * exposed as the PROMISE so the caller can also handle the prefetch FAILING: a
   * click that merely skipped its own fetch on the strength of a prefetch that then
   * errored would leave the pane empty with nothing left to retry it.
   */
  warming(sessionId: string): Promise<void> | undefined {
    return this.inFlight.get(sessionId);
  }

  private async warm(sessionId: string): Promise<void> {
    try {
      const resp = await this.api.getMessages(sessionId, { limit: this.turnsPerBundle });
      // Always merge, even when the store already holds something for this session.
      // A click joins this fetch, and the session stream it opens replays the LAST
      // turn at once; that replay usually lands before this page. Skipping the page
      // whenever the store held anything left the replay alone on screen — one turn of
      // a long session — until the validator sweep repaired it (reported 2026-10-20 on
      // br_1792098153821205901). `setTurns` merges, so a page landing over a live tail
      // or over itself loses nothing.
      this.store.getState().actions.setTurns(sessionId, resp.model);
      // Coalesced: this is a cache fill, and nothing is waiting on it reaching disk.
      this.cache.scheduleTurnsWrite(resp.model);
    } finally {
      this.inFlight.delete(sessionId);
      // `setTurns` already cleared this on the success path; clearing it again is a
      // no-op. On the failure path it is the only thing that does, and leaving it set
      // would tell `select()` a page is still coming when nothing is fetching.
      this.store.getState().actions.setTurnsLoading(sessionId, false);
    }
  }
}
