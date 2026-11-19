import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { Prefetcher } from '../src/boot/Prefetcher.js';
import { SessionCache } from '../src/cache/SessionCache.js';
import { createChatStore, type ChatStoreApi } from '../src/store/ChatStore.js';
import type { SessionSummary, TurnModel } from '../src/net/types.js';

// Boot fans out over four independent reads — summary, recent-bundle, validators,
// folders — and each one ends in `.catch(() => null)`. Those four swallows are a
// deliberate degradation policy: one failed read must not stop the sidebar
// appearing. Nothing pinned any of it. Measured 2026-08-21: all four `.catch`
// clauses could be deleted, and two of them could be changed to paint an EMPTY
// result instead of no result, with the whole 523-test suite staying green.
//
// ⚠️ The two halves are separate claims and both are tested below, because a swallow
// has two ways to be wrong and they fail in opposite directions:
//
//   drop the catch     -> the rejection reaches Promise.all, boot() rejects, and the
//                         SSE handshake that boot() gates never starts. One flaky read
//                         takes the whole client down.
//   swallow too much   -> the failure is painted as an EMPTY SUCCESS. A sidebar that
//                         could not load is then indistinguishable from an account
//                         with no sessions, which is the `d80db401` defect shape.
//
// The empty-success half is why every test here SEEDS the store first and asserts the
// seeded rows SURVIVE. Asserting "the store is empty after a failed read" would pass
// for both the correct behavior and the bug.

function summary(id: string): SessionSummary {
  return {
    sessionId: id,
    state: 'idle',
    harness: 'claude_code',
    instanceId: 'inst-1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: id,
    agentId: '',
    updatedAt: '2026-08-01 12:00:00',
    createdAt: '2026-08-01 12:00:00',
  };
}

/** A turn model, seeded into the store so `prime()` has cached ids to revalidate.
 *  `cachedIds` is read off `turnsBySession`, NOT off IndexedDB (Prefetcher.ts:169),
 *  so the validator branch is reachable without standing up a fake database. */
function turnModel(sessionId: string): TurnModel {
  return {
    sessionId,
    turns: [],
    entries: {},
    validator: { turnCount: 0, lastEntryId: '' },
    more: false,
  };
}

/** A fetch that fails whichever boot reads are named and answers the rest.
 *
 *  `failing` matches on the URL, so a test names the read it is knocking out in the
 *  same vocabulary the route uses. Every url is recorded: one case below asserts a
 *  request does NOT go out, which no store assertion can see. */
function bootFetch(failing: string[], seen: string[]): typeof fetch {
  return (async (url: string) => {
    const href = String(url);
    seen.push(href);
    if (failing.some((f) => href.includes(f))) {
      throw new Error(`network down: ${href}`);
    }
    let body: unknown = {};
    if (href.includes('/sessions/summary')) body = { sessions: [], next: null, revision: 'r' };
    if (href.includes('/folders')) body = { folders: [] };
    return {
      ok: true,
      status: 200,
      statusText: 'OK',
      json: async () => body,
      text: async () => JSON.stringify(body),
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

function wire(failing: string[]): {
  store: ChatStoreApi;
  prefetcher: Prefetcher;
  seen: string[];
} {
  const seen: string[] = [];
  const store = createChatStore();
  const api = new ApiClient({ fetch: bootFetch(failing, seen), basePath: '/api/bridge' });
  const prefetcher = new Prefetcher({
    store,
    api,
    cache: new SessionCache(false),
    sessionsPerPage: 3,
  });
  return { store, prefetcher, seen };
}

describe('boot fan-out — one failed read degrades, and degrading is not the same as an empty answer', () => {
  it('resolves when EVERY boot read fails, so the SSE handshake still starts', () => {
    // boot() resolving is what starts the SSE handshake (see its docstring). If any
    // of the four swallows goes, Promise.all rejects and a client that could have
    // run on cached data never connects at all.
    const { prefetcher } = wire(['/sessions/summary', '/recent', '/validators', '/folders']);
    return expect(prefetcher.boot()).resolves.toBeUndefined();
  });

  it('resolves when the summary read alone fails', () => {
    const { prefetcher } = wire(['/sessions/summary']);
    return expect(prefetcher.boot()).resolves.toBeUndefined();
  });

  it('resolves when the recent-bundle read alone fails', () => {
    const { prefetcher } = wire(['/recent']);
    return expect(prefetcher.boot()).resolves.toBeUndefined();
  });

  it('resolves when the folder read alone fails', () => {
    const { prefetcher } = wire(['/folders']);
    return expect(prefetcher.boot()).resolves.toBeUndefined();
  });

  it('leaves already-painted sessions alone when the summary read fails, rather than blanking the sidebar', async () => {
    // Stands in for a cache paint: `hydrateFromCache` runs before `prime` and can
    // already have put rows on screen. A failed network read must not overwrite them
    // with an empty list -- that turns a recoverable outage into a sidebar that says,
    // with no hedge, that the user has nothing.
    const { store, prefetcher } = wire(['/sessions/summary']);
    store.getState().actions.setSessions([summary('br_cached')], 'cursor-from-cache');

    await prefetcher.boot();

    expect([...store.getState().sessions.keys()]).toEqual(['br_cached']);
    expect(store.getState().olderSessionsCursor).toBe('cursor-from-cache');
  });

  it('leaves an already-loaded folder list alone when the folder read fails', async () => {
    // The comment over this read states the intended degradation outright: "A failure
    // leaves folders empty, which grouping already treats as not loaded and degrades
    // to the loaded sessions' own folder names". That contract is only worth the words
    // if a failure cannot instead write [] over a list that HAD loaded -- grouping
    // cannot tell that apart from an account with no folders.
    const { store, prefetcher } = wire(['/folders']);
    store.getState().actions.setFolders(['work', 'personal']);

    await prefetcher.boot();

    expect(store.getState().folders).toEqual(['work', 'personal']);
  });

  it('asks for no validators at boot, warm or cold: nothing read the answer', async () => {
    // Boot used to send the cached session ids to /validators and drop the reply.
    // The active session is revalidated when it is opened, which is the only place a
    // stale cached model is ever shown.
    const { store, prefetcher, seen } = wire([]);
    store.getState().actions.setTurns('br_warm', turnModel('br_warm'));
    await prefetcher.boot();
    expect(seen.filter((u) => u.includes('/validators'))).toEqual([]);
  });
});
