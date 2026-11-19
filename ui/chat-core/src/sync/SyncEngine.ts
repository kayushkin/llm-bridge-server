import type { ApiClient } from '../net/ApiClient.js';
import type { SessionCache } from '../cache/SessionCache.js';
import { DEFAULT_LIST_CACHE_LIMIT, enforceCacheBound } from '../cache/evict.js';
import type { ChatStoreApi } from '../store/ChatStore.js';
import type { Validator } from '../net/types.js';
import type { WireEvent } from '../net/wireEvents.js';
import { connectListSSE, connectSessionSSE } from './sse.js';
import { clearOpenSignalsCache } from '../react/signals.js';
import { announceSignalsChanged } from '../store/signalResolve.js';
import { announceSessionBriefsChanged } from '../store/sessionBriefChanges.js';

// L2 sync (decision D6). Owns exactly:
//   - ONE global list SSE (list deltas → session upsert/delete)
//   - ONE active-session SSE (only for the current activeId), with Last-Event-ID
//     resume + per-eventId dedup (the reducer handles the latter, idempotently)
//   - a validator sweep on an interval + on visibilitychange
// Reconcile is a SILENT repair: a mismatch refetches the tail and repairs the
// cache; it changes VISIBLE store state only when the moved session is the open
// one. Warm-but-inactive sessions never get a live stream — they are refreshed
// by the sweep, never by 20 open sockets.

export interface SyncEngineConfig {
  store: ChatStoreApi;
  api: ApiClient;
  cache: SessionCache;
  sweepIntervalMs?: number;
  sweepLimit?: number;
  /** Sidebar sessions per page. Bounds the cache's list store, whose only reader
   *  is the cold-boot paint — one page wide. Must match the Prefetcher's, or the
   *  sweep trims rows the next boot wanted to paint. */
  sessionsPerPage?: number;
}

function validatorsEqual(a: Validator | undefined, b: Validator | undefined): boolean {
  if (!a || !b) return false;
  return a.maxEventId === b.maxEventId && a.eventCount === b.eventCount;
}

export class SyncEngine {
  private readonly store: ChatStoreApi;
  private readonly api: ApiClient;
  private readonly cache: SessionCache;
  private readonly sweepIntervalMs: number;
  private readonly sweepLimit: number;
  private readonly sessionsPerPage: number;

  private listAbort: AbortController | null = null;
  private activeAbort: AbortController | null = null;
  private activeStreamId: string | null = null;
  private sweepTimer: ReturnType<typeof setInterval> | null = null;
  /** Whether the list stream has ever said hello. Distinguishes the first connect
   *  (boot already read the folders) from a reconnect (they may have changed under
   *  us while the stream was down). */
  private listHelloSeen = false;
  /** The last frame id received on the list stream, sent back as `Last-Event-ID` on
   *  a reconnect so the server replays what was missed. The server has numbered its
   *  frames and kept the last 512 since the hub was written; nothing sent the id. */
  private listStreamCursor: string | undefined;
  private unsubStore: (() => void) | null = null;
  private onVisible: (() => void) | null = null;
  private running = false;

  constructor(config: SyncEngineConfig) {
    this.store = config.store;
    this.api = config.api;
    this.cache = config.cache;
    this.sweepIntervalMs = config.sweepIntervalMs ?? 15000;
    this.sweepLimit = config.sweepLimit ?? 50;
    this.sessionsPerPage = config.sessionsPerPage ?? DEFAULT_LIST_CACHE_LIMIT;
  }

  start(): void {
    if (this.running) return;
    this.running = true;
    this.store.getState().actions.setConn('connecting');

    void this.runListStream();

    // React to activeId changes: attach/detach the single active-session SSE,
    // and re-check that what we are about to render is still current.
    let lastActive = this.store.getState().activeId;
    this.attachActive(lastActive);
    void this.revalidateActive(lastActive);
    this.unsubStore = this.store.subscribe((state) => {
      if (state.activeId !== lastActive) {
        lastActive = state.activeId;
        this.attachActive(lastActive);
        void this.revalidateActive(lastActive);
      }
    });

    this.sweepTimer = setInterval(() => void this.sweepValidators(), this.sweepIntervalMs);

    if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
      this.onVisible = () => {
        if (document.visibilityState === 'visible') void this.sweepValidators();
      };
      document.addEventListener('visibilitychange', this.onVisible);
    }
  }

  stop(): void {
    this.running = false;
    this.listAbort?.abort();
    this.activeAbort?.abort();
    this.listAbort = null;
    this.activeAbort = null;
    this.activeStreamId = null;
    this.flushTailEvents();
    // Anything still queued belongs in the cache: the next boot paints from it.
    void this.cache.flushTurnsWrites();
    if (this.sweepTimer) clearInterval(this.sweepTimer);
    this.sweepTimer = null;
    this.unsubStore?.();
    this.unsubStore = null;
    if (this.onVisible && typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.onVisible);
    }
    this.onVisible = null;
    this.store.getState().actions.setConn('closed');
  }

  // --- global list stream, with backoff reconnect ---

  private async runListStream(): Promise<void> {
    let delay = 1000;
    while (this.running) {
      const abort = new AbortController();
      this.listAbort = abort;
      try {
        for await (const frame of connectListSSE(
          this.api.fetchFor(),
          this.api.basePath,
          abort.signal,
          this.listStreamCursor,
        )) {
          if (!this.running) return;
          const actions = this.store.getState().actions;
          if ('id' in frame && frame.id) this.listStreamCursor = frame.id;
          if (frame.type === 'hello') {
            delay = 1000;
            actions.setConn('open');
            // Folders are not on this stream — it carries session upserts and deletes
            // only — so a folder created, renamed or deleted while the client was
            // disconnected is invisible until something re-reads the list. A
            // reconnect is exactly the moment the sidebar's picture of the server is
            // in doubt, so it is re-read here. NOT on the first hello: `boot()` has
            // just fetched it, and sync starts after boot resolves.
            if (this.listHelloSeen) void this.refreshFolders();
            // `replayed`: the frames missed while disconnected follow this hello, so
            // every session row — and the status on it — catches up by itself.
            // Anything else on a RE-connect means frames were lost (`gap`: more than
            // the server keeps; `none`: it restarted and our cursor meant nothing to
            // it), and a lost upsert is a session whose status stays wrong until its
            // next change. Re-read the rows we hold.
            if (this.listHelloSeen && frame.resume !== 'replayed') void this.refreshHeldSessions();
            this.listHelloSeen = true;
          } else if (frame.type === 'upsert' && frame.summary) {
            actions.upsertSession(frame.summary);
            void this.cache.putSummary(frame.summary);
          } else if (frame.type === 'delete') {
            actions.removeSession(frame.sessionId);
            void this.cache.deleteSession(frame.sessionId);
          } else if (frame.type === 'signal') {
            // The server says this session's open questions moved. Drop the
            // cached open set and let the surfaces showing it re-read.
            //
            // This is what the 30-second TTL was standing in for. A question
            // answered anywhere else — another tab, the CLI, an orchestrator —
            // used to sit on screen until that lapsed, and now that answering
            // is a real round trip through the server, someone would watch it
            // happen.
            clearOpenSignalsCache();
            announceSignalsChanged();
          } else if (frame.type === 'session_brief') {
            // A chat's brief or loose ends moved; the surfaces showing briefs
            // re-read them.
            announceSessionBriefsChanged();
          }
        }
      } catch {
        if (abort.signal.aborted || !this.running) return;
      }
      if (!this.running) return;
      this.store.getState().actions.setConn('connecting');
      await sleep(delay);
      delay = Math.min(delay * 2, 30000);
    }
  }

  /** Re-read every session row the store holds, after a reconnect that lost frames.
   *  Rows go in through `upsertSession`, not `mergeSessions`: a merge keeps the held
   *  row's fields on top because a live row is normally newer than a page, and here
   *  the held rows are exactly the ones known to have missed updates. The status on
   *  each is still decided by `as_of`, as everywhere. */
  private async refreshHeldSessions(): Promise<void> {
    const sessionIds = [...this.store.getState().sessions.keys()];
    if (sessionIds.length === 0) return;
    try {
      const resp = await this.api.getSummary({ sessionIds, limit: sessionIds.length });
      if (!this.running) return;
      const actions = this.store.getState().actions;
      for (const summary of resp.sessions) actions.upsertSession(summary);
    } catch {
      // Non-fatal: the rows stay as they are and the next reconnect tries again.
    }
  }

  /** Re-read `GET /folders` after a reconnect. A failure keeps the folder list the
   *  client already has rather than clearing it — a stale order is closer to the
   *  truth than no order, and the next reconnect tries again. */
  private async refreshFolders(): Promise<void> {
    try {
      const folders = await this.api.listFolders();
      if (this.running) this.store.getState().actions.setFolders(folders);
    } catch {
      // Non-fatal: the sidebar keeps grouping with the list it has.
    }
  }

  // --- active session stream ---

  private attachActive(sessionId: string | null): void {
    if (this.activeStreamId === sessionId) return;
    this.activeAbort?.abort();
    this.activeAbort = null;
    this.activeStreamId = sessionId;
    if (!sessionId) return;
    void this.runActiveStream(sessionId);
  }

  private async runActiveStream(sessionId: string): Promise<void> {
    let delay = 1000;
    const abort = new AbortController();
    this.activeAbort = abort;

    while (this.running && this.activeStreamId === sessionId && !abort.signal.aborted) {
      try {
        const lastEventId = this.lastEventIdFor(sessionId);
        const stream = connectSessionSSE(
          this.api.fetchFor(),
          this.api.basePath,
          sessionId,
          lastEventId,
          abort.signal,
        );
        for await (const ev of stream) {
          if (!this.running || this.activeStreamId !== sessionId) return;
          delay = 1000;
          // The resume cursor is the STREAM's own id line (llm-bridge-server's
          // row ids) — the only space the server's Last-Event-ID understands.
          if (ev.id) this.streamCursors.set(sessionId, ev.id);
          this.queueTailEvent(sessionId, ev);
        }
        this.flushTailEvents();
        // The stream ended — the turn is over, so stop waiting out a coalescing timer
        // and put the final state of it in the cache now.
        void this.cache.flushTurnsWrites();
      } catch {
        if (abort.signal.aborted || this.activeStreamId !== sessionId) return;
      }
      if (!this.running || this.activeStreamId !== sessionId) return;
      await sleep(delay);
      delay = Math.min(delay * 2, 30000);
    }
  }

  /** Per-session SSE resume cursor: the last frame id RECEIVED on the stream.
   *
   *  ⛔ RESUMING FROM THE PAGE INSTEAD (the `stream.head` every messages page carries)
   *  was tried and withdrawn on 2026-08-27, the day it shipped: it cut the current-turn
   *  replay from 5,569 KB to 0 across eight opens and DOUBLED NARRATION. The server
   *  reads the head before flushing log-store writes, so a page can hold events above
   *  it that the stream re-delivers, and page entries (`e_<log-store id>`) and live
   *  ones (`evt_<bridge id>`) never collide by id — overlap is reconciled by content
   *  only when a page lands over a live tail, not the other way round. The client
   *  plumbing that stored those heads, in memory and in IndexedDB, was deleted on
   *  2026-09-16 because nothing read it. A second attempt needs the reducer to
   *  reconcile live-over-page — skipping atomic kinds (`system`, `tool_call`,
   *  `tool_result`, `result`, `user_message`) on a content match, never the appending
   *  ones (`stream`, `block`) — or one id space for events. */
  private streamCursors = new Map<string, string>();

  // --- batched tail application ---
  //
  // Frames arrive one at a time off `for await`, and every iteration of that loop is a
  // separate task. React cannot batch state updates across an await boundary, so
  // applying each frame as it arrived meant ONE RE-RENDER PER FRAME.
  //
  // That is what made opening a session slow, and it took three experiments to find
  // because the obvious suspects were innocent. Opening a session replays its current
  // turn — hundreds of frames — and measured 2026-08-26 across eight cold opens on the
  // real dashboard, blocking the main thread for 12.7s. Cutting the per-session SSE off
  // entirely took the same eight opens to 261ms: 98% of the cost was here. Shrinking
  // the page did nothing for it — clipping tool payloads (6.17MB -> 4.02MB) moved the
  // total by 3%, and removing every tool ENTRY (6.17MB -> 0.54MB, 1043 entries -> 118)
  // moved it by nothing at all.
  //
  // Buffering the frames and applying them in ONE task lets React coalesce the renders,
  // without changing what the reducer does to any individual frame.

  private queuedTailEvents: WireEvent[] = [];
  private queuedTailSessionId: string | null = null;
  private tailFlushHandle: ReturnType<typeof setTimeout> | null = null;

  private queueTailEvent(sessionId: string, ev: WireEvent): void {
    // A switch mid-batch abandons what was queued for the session being left: those
    // frames belong to a tail nobody is looking at, and the stream for it has already
    // been aborted.
    if (this.queuedTailSessionId !== sessionId) {
      this.queuedTailEvents = [];
      this.queuedTailSessionId = sessionId;
    }
    this.queuedTailEvents.push(ev);
    if (this.tailFlushHandle !== null) return;
    // A frame, not a microtask. A microtask flush would run before the browser could
    // paint and would therefore still be one render per frame during a replay, which is
    // the whole thing being fixed.
    this.tailFlushHandle = setTimeout(() => this.flushTailEvents(), 0);
  }

  /** Apply every queued frame in one task, so the renders coalesce. Also called when a
   *  stream ends, so the last frames of a turn are not left sitting in the buffer. */
  private flushTailEvents(): void {
    if (this.tailFlushHandle !== null) {
      clearTimeout(this.tailFlushHandle);
      this.tailFlushHandle = null;
    }
    const sessionId = this.queuedTailSessionId;
    const batch = this.queuedTailEvents;
    this.queuedTailEvents = [];
    if (!sessionId || batch.length === 0) return;
    if (!this.running || this.activeStreamId !== sessionId) return;

    // ONE action for the batch, so subscribers are notified once. Looping
    // `applyTailEvent` here would still leave every selector in the app running once
    // per frame — React's auto-batching coalesces the RENDERS and nothing else.
    this.store.getState().actions.applyTailEvents(sessionId, batch);

    // ONE cache write for the batch rather than one per frame — and coalesced on top of
    // that, because IndexedDB structured-clones its argument on the main thread. See
    // SessionCache.scheduleTurnsWrite.
    const model = this.store.getState().turnsBySession.get(sessionId);
    if (model) this.cache.scheduleTurnsWrite(model);
  }

  private lastEventIdFor(sessionId: string): string | undefined {
    return this.streamCursors.get(sessionId);
  }




  // --- validator sweep + silent repair ---

  async sweepValidators(): Promise<void> {
    try {
      await this.repairChangedValidators();
    } finally {
      // The bound is local housekeeping, not part of the validator work, so it
      // runs on a tick that found nothing cached and on a tick whose network read
      // failed. It used to sit at the tail of the method below, past two early
      // returns — and the list store grows from the live stream's upserts, which
      // do not stop when the sweep has nothing to check. A tab left open with no
      // session ever selected took neither branch and trimmed nothing.
      if (this.cache.isEnabled) {
        void enforceCacheBound(this.cache, this.sweepLimit, this.sessionsPerPage);
      }
    }
  }

  /** Compare each cached session's validator against the server's and repair the
   *  ones that both changed and are on screen. */
  private async repairChangedValidators(): Promise<void> {
    const state = this.store.getState();
    // ⚠️ The ACTIVE session is always checked, whatever its position in the map.
    //
    // `turnsBySession` is a Map, so `keys()` is INSERTION order — the sessions
    // opened longest ago. Slicing it alone meant the sweep fetched validators for
    // the 50 oldest-opened sessions and then repaired only the active one
    // (`isDisplayed` below), so once a window had opened more than `sweepLimit`
    // sessions the one actually on screen fell outside the slice and was never
    // checked at all. Exactly backwards: it polled 50 tails nobody was looking at
    // and skipped the only one that renders.
    //
    // That is the "leave a tab open for a while and it goes stale until you
    // refresh" report, and its severity grows with how much you use the page.
    const active = state.activeId;
    const swept = [...state.turnsBySession.keys()].slice(0, this.sweepLimit);
    const cachedIds =
      active && state.turnsBySession.has(active) && !swept.includes(active)
        ? [active, ...swept]
        : swept;
    if (cachedIds.length === 0) return;

    let serverValidators;
    try {
      serverValidators = await this.api.getValidators(cachedIds);
    } catch {
      return; // network hiccup — try again next tick.
    }

    for (const id of cachedIds) {
      const server = serverValidators[id];
      const local = this.store.getState().turnsBySession.get(id)?.validator;
      if (validatorsEqual(local, server)) continue;
      // Idle over-poll fix: a changed validator only justifies pulling the heavy
      // message tail when this session's tail is actually on screen — i.e. it is
      // the active/open one. For an UNSELECTED cached session (even a running one)
      // we stop here: the cheap validator check has already run, and the full
      // ~500 KB tail is NOT fetched. It self-heals when the session next becomes
      // active — the active-session SSE resumes from Last-Event-ID and replays the
      // missed events, and this sweep repairs it once activeId points at it.
      if (this.isDisplayed(id)) {
        await this.repairSession(id);
      }
    }
  }

  /** True iff this session's tail is currently being displayed. In this client the
   *  only displayed tail is the active (open) session; a warm-but-inactive session
   *  is never on screen, so its tail must not be refetched by the sweep. */
  private isDisplayed(sessionId: string): boolean {
    return this.store.getState().activeId === sessionId;
  }

  /**
   * Re-check the session that just became active, without waiting for the sweep.
   *
   * ⚠️ A CACHED session was previously not re-read at all on open. `select()`
   * fetches the tail only when the store has none (`hooks.ts`), which is what
   * makes a warm switch land in single-digit milliseconds — and it means a
   * session whose cached tail has since moved on renders stale and stays stale
   * until the 15s sweep happens to cover it. The original chat has no such gap:
   * `useBridgeSession` calls `loadHistory(id)` on EVERY open, cached or not.
   *
   * This keeps the fast paint and closes the gap behind it: the cache is drawn
   * immediately, one cheap validator read follows, and the tail is refetched only
   * if the server's validator disagrees. A session the store has never seen is
   * skipped — `select()` is already fetching it, and asking twice would race.
   *
   * Placed on the activeId SUBSCRIPTION rather than inside `select()` so it holds
   * for every way a session becomes active: a sidebar row, a `?session=`
   * deeplink, a `[session:…]` ref chip, the signals inbox, and the subagent tree.
   */
  private async revalidateActive(sessionId: string | null): Promise<void> {
    if (!sessionId) return;
    const local = this.store.getState().turnsBySession.get(sessionId)?.validator;
    if (!local) return;
    let serverValidators;
    try {
      serverValidators = await this.api.getValidators([sessionId]);
    } catch {
      return; // The sweep will try again; a stale tail is not worth a thrown error.
    }
    if (validatorsEqual(local, serverValidators[sessionId])) return;
    // Still the active one? The user may have moved on during the round trip, and
    // repairing a session nobody is looking at is the over-poll this file already
    // guards against everywhere else.
    if (this.store.getState().activeId !== sessionId) return;
    await this.repairSession(sessionId);
  }

  /** Silent repair: refetch the tail, repair the cache, and update VISIBLE store
   *  state only when this session is the active (open) one. */
  private async repairSession(sessionId: string): Promise<void> {
    let resp;
    try {
      resp = await this.api.getMessages(sessionId, { limit: 30 });
    } catch {
      return;
    }
    const model = resp.model;
    void this.cache.putTurns(model);
    if (this.store.getState().activeId === sessionId) {
      this.store.getState().actions.setTurns(sessionId, model);
    }
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
