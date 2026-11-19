import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { Prefetcher } from '../src/boot/Prefetcher.js';
import { SessionCache } from '../src/cache/SessionCache.js';
import { createChatStore, type ChatStoreApi } from '../src/store/ChatStore.js';
import type { TurnModel } from '../src/net/types.js';

// `Prefetcher.prefetch()` is the hover/idle hint: warm a cold session so the
// next click is a Map read. It had no test of its own, so every one of its three
// refusals — no id, already warm, already in flight — was unobserved, and so was
// `hydrateFromCache`'s refusal to touch a disabled cache.
//
// Refusing costs nothing visible. That is the whole difficulty: a guard that
// wrongly stops firing spends a request and changes no state, because `warm()`
// re-checks the store before writing. The REQUEST is the observable throughout,
// which is why these fixtures count urls rather than inspect the store.

function turnModel(sessionId: string): TurnModel {
  return {
    sessionId,
    turns: [],
    entries: {},
    validator: { turnCount: 0, lastEntryId: '' },
    more: false,
  };
}

/** A fetch that answers every messages request and records the urls it was asked
 *  for. Deliberately records ONLY messages urls: `prefetch` is the only caller
 *  under test and any other traffic would make the counts meaningless. */
function messagesFetch(seen: string[]): typeof fetch {
  return (async (url: string) => {
    const href = String(url);
    if (href.includes('/messages')) seen.push(href);
    const id = href.split('/sessions/')[1]?.split('/')[0] ?? '';
    return {
      ok: true,
      status: 200,
      statusText: 'OK',
      json: async () => ({ model: turnModel(id) }),
      text: async () => '',
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

function wire(): { store: ChatStoreApi; prefetcher: Prefetcher; seen: string[] } {
  const seen: string[] = [];
  const store = createChatStore();
  const api = new ApiClient({ fetch: messagesFetch(seen), basePath: '/api/bridge' });
  const prefetcher = new Prefetcher({ store, api, cache: new SessionCache(false) });
  return { store, prefetcher, seen };
}

/** Let the floating `void this.warm(...)` chain settle. `prefetch` is sync and
 *  returns before the request goes out, so asserting immediately would pass for
 *  the wrong reason. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

describe('Prefetcher.prefetch — the hover hint', () => {
  it('warms a cold session', () => {
    // The positive case, and it has to come first: every refusal below is
    // "no request went out", which a prefetch that never works at all also
    // satisfies. Without this, all of them would pass against a dead method.
    const { prefetcher, seen } = wire();
    prefetcher.prefetch('s-1');
    expect(seen).toHaveLength(1);
    expect(seen[0]).toContain('/sessions/s-1/messages');
  });

  it('refuses an empty session id rather than fetching /sessions//messages', () => {
    const { prefetcher, seen } = wire();
    prefetcher.prefetch('');
    expect(seen).toEqual([]);
    // A usable call after the refused one, so a fixture that records nothing at
    // all cannot pass this by accident.
    prefetcher.prefetch('s-1');
    expect(seen).toHaveLength(1);
  });

  it('refuses a session whose turns are already in the store', async () => {
    const { store, prefetcher, seen } = wire();
    store.getState().actions.setTurns('s-1', turnModel('s-1'));

    prefetcher.prefetch('s-1');
    await settle();

    // Nothing to assert in the store: `warm()` re-checks before writing, so the
    // turns would survive a re-fetch untouched. Only the wire tells the two apart.
    expect(seen).toEqual([]);
    prefetcher.prefetch('s-2');
    expect(seen).toHaveLength(1);
  });

  it('refuses a second hover while the first is still in flight', () => {
    // Separate from the already-warm refusal: this session is COLD, so nothing
    // is in the store and that guard cannot refuse either call.
    const { prefetcher, seen } = wire();
    prefetcher.prefetch('s-1');
    prefetcher.prefetch('s-1');
    expect(seen).toHaveLength(1);
  });

  it('releases the in-flight mark after a warm that FAILED, so a later hover retries', async () => {
    // The other half of the in-flight guard, and a failed warm is the only way to
    // ask it: a warm that succeeds writes turns, and then the already-warm guard
    // is what refuses the second hover — which would pass whether the in-flight
    // mark was released or held forever.
    //
    // Held forever, the row simply stops warming. Nothing throws, nothing is
    // logged, and every click on it pays the full fetch it was meant to avoid.
    const seen: string[] = [];
    let calls = 0;
    const store = createChatStore();
    const api = new ApiClient({
      basePath: '/api/bridge',
      fetch: (async (url: string) => {
        const href = String(url);
        if (!href.includes('/messages')) throw new Error(`unexpected request: ${href}`);
        seen.push(href);
        calls += 1;
        if (calls === 1) return { ok: false, status: 503, statusText: 'nope' } as Response;
        return {
          ok: true,
          status: 200,
          statusText: 'OK',
          json: async () => ({ model: turnModel('s-1') }),
        } as unknown as Response;
      }) as unknown as typeof fetch,
    });
    const prefetcher = new Prefetcher({ store, api, cache: new SessionCache(false) });

    prefetcher.prefetch('s-1');
    await settle();
    expect(seen).toHaveLength(1);
    expect(store.getState().turnsBySession.has('s-1')).toBe(false);

    prefetcher.prefetch('s-1');
    await settle();
    expect(seen).toHaveLength(2);
    expect(store.getState().turnsBySession.has('s-1')).toBe(true);
  });
});

describe('Prefetcher.hydrateFromCache — a disabled cache', () => {
  it('is not read at all, rather than read and swallowed', async () => {
    // `hydrate()` is inside a try/catch that swallows everything, so a disabled
    // cache reached anyway would look exactly like a cold one. Count the calls.
    let hydrateCalls = 0;
    const store = createChatStore();
    const api = new ApiClient({ fetch: messagesFetch([]), basePath: '/api/bridge' });
    const cache = {
      isEnabled: false,
      hydrate: async () => {
        hydrateCalls += 1;
        return { list: [], turns: new Map(), validators: new Map() };
      },
    } as unknown as SessionCache;

    await new Prefetcher({ store, api, cache }).hydrateFromCache();
    expect(hydrateCalls).toBe(0);

    // Same fake with the flag flipped, so "never called" cannot be an artifact
    // of a fake nobody wired up.
    const enabled = { ...cache, isEnabled: true } as unknown as SessionCache;
    await new Prefetcher({ store, api, cache: enabled }).hydrateFromCache();
    expect(hydrateCalls).toBe(1);
  });
});
