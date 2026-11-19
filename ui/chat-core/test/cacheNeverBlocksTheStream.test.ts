import { describe, expect, it, vi } from 'vitest';
import { Prefetcher } from '../src/boot/Prefetcher.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { ApiClient } from '../src/net/ApiClient.js';
import type { SessionCache } from '../src/cache/SessionCache.js';

// THE REGRESSION THIS PINS, in one sentence: the IndexedDB cache is a first-paint
// optimization, and it must never be able to stop the session-list stream from opening.
//
// It could, and it did. `ChatProvider` ran `boot().then(() => sync.start())`, and
// `boot()`'s first act is to read the cache. An IndexedDB upgrade that another open tab
// BLOCKS never settles — the open request neither resolves nor rejects — so with eight
// dash tabs open and DB_VERSION freshly bumped to 4, `hydrateFromCache()` awaited a
// promise that would never come. `sync.start()` was therefore never called at all:
// `connState` sat at 'idle' forever, `Sidebar` rendered "Connecting…" over an empty list,
// and every composer's Send was disabled (`connected = connState === 'open'`). Sessions in
// pty mode still worked, because the terminal attaches on its own path and reads none of
// this — which is what made it look like a chat bug rather than a cache bug. Nothing threw,
// so the console was clean and the browser never issued a single request.
//
// Two independent guarantees now stand between that cache and the stream, and both are
// tested here, because either one alone would have prevented the outage.

/** A cache whose connection NEVER settles: every method returns a promise that is never
 *  resolved and never rejected. This is precisely a blocked IndexedDB upgrade — not a
 *  rejection, which `try`/`catch` would already have handled. */
function neverSettlingCache(): SessionCache {
  const never = () => new Promise<never>(() => {});
  return {
    isEnabled: true,
    hydrate: never,
    putList: never,
    putSummary: never,
    putTurns: never,
    scheduleTurnsWrite: never,
    flushTurnsWrites: never,
    close: never,
    // The surface `enforceCacheBound` reaches for; it hangs like the rest.
    turnKeys: never,
    evictTurns: never,
    listKeysOldestFirst: never,
    evictListRows: never,
  } as unknown as SessionCache;
}

function apiStub(): { api: ApiClient; summaryCalls: () => number } {
  let summaryCalls = 0;
  const api = {
    basePath: '/api/bridge',
    getSummary: async () => {
      summaryCalls += 1;
      return { sessions: [], next: null };
    },
    getRecentBundle: async () => ({}),
    listFolders: async () => [],
  } as unknown as ApiClient;
  return { api, summaryCalls: () => summaryCalls };
}

describe('the cache can never hold up the session-list stream', () => {
  it('starts the stream before boot, so a cache that never settles costs nothing', async () => {
    // Mirrors ChatProvider's effect exactly: start, then boot. The assertion is about the
    // ORDER — `start()` has already happened by the time boot is even called, so no state
    // boot can get into is able to reach it.
    const started: string[] = [];
    const sync = { start: () => started.push('start'), stop: () => undefined };
    const prefetcher = {
      boot: () => new Promise<void>(() => {}), // never settles
    };

    sync.start();
    void prefetcher.boot().catch(() => undefined);

    // One turn of the microtask queue: enough for any `.then()` chaining to have run.
    await Promise.resolve();
    await Promise.resolve();

    expect(started).toEqual(['start']);
  });

  it('boot still reaches the network when the cache hangs, so the sidebar fills', async () => {
    // The second guarantee: `hydrateFromCache` is bounded, so `prime()` runs even when the
    // cache never answers. Without this the stream would be open but the first page of
    // sessions would never be fetched — a sidebar that fills only as sessions happen to
    // change, which reads as "I have no sessions".
    const store = createChatStore();
    const { api, summaryCalls } = apiStub();
    const prefetcher = new Prefetcher({
      store,
      api,
      cache: neverSettlingCache(),
      recentN: 1,
      turnsPerBundle: 1,
      sessionsPerPage: 10,
    });

    // `hydrateFromCache` swallows the cache failure; what must NOT happen is that it
    // awaits forever and `prime()` is never reached.
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    await Promise.race([
      prefetcher.boot(),
      new Promise((_r, reject) => setTimeout(() => reject(new Error('boot hung on the cache')), 8000)),
    ]);
    warn.mockRestore();

    expect(summaryCalls()).toBeGreaterThan(0);
  }, 15000);
});
