import { useEffect, useRef, type JSX, type ReactNode } from 'react';
import { ApiClient } from '../net/ApiClient.js';
import { NoteboardClient } from '../net/NoteboardClient.js';
import { ReferenceStoreClient } from '../net/ReferenceStoreClient.js';
import { SessionCache } from '../cache/SessionCache.js';
import { SyncEngine } from '../sync/SyncEngine.js';
import { Prefetcher } from '../boot/Prefetcher.js';
import { createChatStore } from '../store/ChatStore.js';
import { ChatContext, type ChatContextValue } from './context.js';

/** Props for the ChatProvider (see docs/PUBLIC-API.md). */
export interface ChatProviderProps {
  fetch: typeof fetch; // dash passes its cookie-credentialed apiFetch
  basePath: string; // '/api/bridge'
  /** Noteboard root, e.g. dash's '/api/noteboard'. Omit it and note/todo ref
   *  chips render but say lookup is not configured here — they never guess a
   *  path, because a wrong one would 404 as if the item did not exist. */
  noteboardBasePath?: string;
  /** Where reference-store is reached through the bridge, e.g. dash's
   *  '/api/bridge/reference-store'. It owns the list of reference types and
   *  looks ids up for the signed-in person. */
  referenceStoreBasePath?: string;
  recentN?: number; // warm-cache size, default 20
  turnsPerBundle?: number; // last-N turns per bundled session, default 30
  sessionsPerPage?: number; // sidebar sessions per page, default 100
  cache?: boolean; // enable IndexedDB persistence, default true
  /** Characters of transcript payload to keep warm in memory. Default
   *  `DEFAULT_TURN_RETENTION_BYTES`. This is the L1 working set, NOT a cache — the
   *  IndexedDB layer below holds more, and an evicted session repaints from there
   *  rather than from the network. Measured in bytes rather than sessions because
   *  transcript size varies by more than 10× between sessions; see the constant. */
  turnRetentionBytes?: number;
  /** Sessions kept warm regardless of the byte budget. Default
   *  `DEFAULT_TURN_RETENTION_MIN_SESSIONS` — what guarantees that flipping between the
   *  few sessions someone is working across stays instant even when each is enormous. */
  turnRetentionMinSessions?: number;
  /** Keep composer drafts and the sidebar's filters in localStorage. Default true.
   *  Both live under one fixed key each, so a second provider on the same page — a
   *  store for another server's sessions — passes false, or it would overwrite the
   *  first provider's filters and share its drafts. Pass `cache={false}` beside it
   *  to keep its sessions out of IndexedDB too. */
  keepDraftsAndFiltersInBrowser?: boolean;
  children: ReactNode;
}

/**
 * Wires ApiClient(fetch, basePath) + ChatStore + SessionCache, mounts the
 * SyncEngine, and runs the boot sequence (hydrate from cache → paint → parallel
 * prime). Everything is created exactly once for the provider's lifetime.
 */
export function ChatProvider(props: ChatProviderProps): JSX.Element {
  const {
    fetch: fetchFn,
    basePath,
    noteboardBasePath,
    referenceStoreBasePath,
    recentN = 20,
    turnsPerBundle = 30,
    sessionsPerPage,
    cache = true,
    turnRetentionBytes,
    turnRetentionMinSessions,
    keepDraftsAndFiltersInBrowser = true,
    children,
  } = props;

  const ctxRef = useRef<ChatContextValue | null>(null);
  if (ctxRef.current === null) {
    const store = createChatStore({
      turnRetentionBytes,
      turnRetentionMinSessions,
      // undefined keeps createChatStore's default, the browser's localStorage.
      ...(keepDraftsAndFiltersInBrowser ? {} : { draftStorage: null, filterStorage: null }),
    });
    const api = new ApiClient({ fetch: fetchFn, basePath });
    const sessionCache = new SessionCache(cache);
    const prefetcher = new Prefetcher({
      store,
      api,
      cache: sessionCache,
      recentN,
      turnsPerBundle,
      sessionsPerPage,
    });
    // The page size goes to BOTH: the Prefetcher paints one page from the cache
    // and the SyncEngine's sweep trims the cache to it. Configure one and not the
    // other and the sweep drops rows the next boot paint wanted.
    const sync = new SyncEngine({ store, api, cache: sessionCache, sessionsPerPage });
    // Built only when a path was given. An unconfigured noteboard is null all
    // the way to the chip, which then says so — rather than a client pointed at
    // the bridge root, whose 404 would read as "that item does not exist".
    const noteboard = noteboardBasePath
      ? new NoteboardClient({ fetch: fetchFn, basePath: noteboardBasePath })
      : null;
    const references = referenceStoreBasePath
      ? new ReferenceStoreClient({ fetch: fetchFn, basePath: referenceStoreBasePath })
      : null;
    ctxRef.current = { store, api, cache: sessionCache, sync, prefetcher, noteboard, references };
  }

  const ctx = ctxRef.current;

  useEffect(() => {
    let canceled = false;
    // The live stream starts FIRST, and never waits on boot.
    //
    // It used to be `boot().then(() => sync.start())`, which made the session-list SSE —
    // and with it `connState`, the sidebar and every composer's Send button — a hostage
    // of the boot sequence. Boot's first act is to read the IndexedDB cache, and an
    // upgrade blocked by another open tab never settles, so `start()` was simply never
    // reached: `connState` stayed 'idle', the sidebar showed "Connecting…" over an empty
    // list, and Send was disabled in every session with nothing in the console. There was
    // no `.catch()` either, so a boot that REJECTED disabled the UI just as permanently.
    //
    // The ordering was never load-bearing: `start()` reads `activeId` off the store and
    // subscribes to it, so a cache paint or a network page landing afterwards is picked
    // up exactly as any later change is. Nothing in boot has to precede the stream.
    ctx.sync.start();
    // LOUD: a boot failure costs the cache paint and the first list page, which the
    // stream then has to fill in. That is a real degradation and it gets reported.
    void ctx.prefetcher.boot().catch((err: unknown) => {
      if (!canceled) console.error('chat-core: boot failed; running on the stream alone', err);
    });
    return () => {
      canceled = true;
      ctx.sync.stop();
      void ctx.cache.close();
    };
    // ctx is stable for the provider's lifetime.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return <ChatContext.Provider value={ctx}>{children}</ChatContext.Provider>;
}
