import { describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { Prefetcher } from '../src/boot/Prefetcher.js';
import { SessionCache } from '../src/cache/SessionCache.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { TurnModel } from '../src/net/types.js';

// Hovering a sidebar row and then clicking it is how a session is opened. It used to
// fetch the same page TWICE, concurrently.
//
// `Prefetcher.prefetch` guarded on a private `inFlight` Set; `select()` guarded on the
// store's `turnsLoading`. Neither register was visible to the other, so the hover
// started a fetch and the click started a second one a few milliseconds later.
// Measured on the live dashboard: two `GET /sessions/{id}/messages` per open, 2-19ms
// apart, each parsing a page of up to a megabyte — on the cold path that the whole
// projection exercise exists to make fast.
//
// These cases are about the NUMBER of requests. Both fetches produced a correct
// transcript, which is exactly why this survived: nothing was ever visibly wrong.

function model(sessionId: string): TurnModel {
  return {
    sessionId,
    turns: [],
    entries: {},
    validator: { maxEventId: 1, eventCount: 1, updatedAt: '2026-08-26T00:00:00Z' },
    more: false,
  };
}

function harness(opts?: { fail?: boolean }) {
  const urls: string[] = [];
  let reject: ((e: Error) => void) | null = null;
  let resolve: (() => void) | null = null;
  const api = new ApiClient({
    basePath: '/api/bridge',
    fetch: (async (url: string) => {
      urls.push(String(url));
      // Held open so the click lands while the hover's fetch is still in flight —
      // which is the whole scenario. Resolved by the test.
      await new Promise<void>((res, rej) => {
        resolve = res;
        reject = () => rej(new Error('prefetch failed'));
      });
      if (opts?.fail) throw new Error('prefetch failed');
      return { ok: true, status: 200, json: async () => ({ model: model('br_1') }) };
    }) as unknown as typeof fetch,
  });
  const store = createChatStore();
  const cache = {
    isEnabled: false,
    putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}),
  } as unknown as SessionCache;
  const prefetcher = new Prefetcher({ store, api, cache });
  return {
    api,
    store,
    prefetcher,
    urls,
    messageRequests: () => urls.filter((u) => u.includes('/messages')),
    settle: async () => {
      resolve?.();
      await new Promise((r) => setTimeout(r, 0));
    },
    fail: async () => {
      reject?.();
      await new Promise((r) => setTimeout(r, 0));
    },
  };
}

describe('a hover followed by a click fetches the page once', () => {
  it('the prefetch is joinable — `warming` hands back the fetch in flight', () => {
    const h = harness();
    h.prefetcher.prefetch('br_1');
    expect(h.prefetcher.warming('br_1')).toBeInstanceOf(Promise);
  });

  it('reports nothing in flight for a session nobody hovered', () => {
    const h = harness();
    expect(h.prefetcher.warming('br_never_hovered')).toBeUndefined();
  });

  it('a second hover does not start a second fetch', () => {
    const h = harness();
    h.prefetcher.prefetch('br_1');
    h.prefetcher.prefetch('br_1');
    expect(h.messageRequests()).toHaveLength(1);
  });

  it('stops warming once the fetch has landed, so the next hover is free to refetch', async () => {
    const h = harness();
    h.prefetcher.prefetch('br_1');
    await h.settle();
    expect(h.prefetcher.warming('br_1')).toBeUndefined();
  });

  it('skips the hover entirely when the click path is already fetching', () => {
    // The mirror of the bug: `select()` sets `turnsLoading` synchronously, and a hover
    // arriving after it must not start a second fetch either.
    const h = harness();
    h.store.getState().actions.setTurnsLoading('br_1', true);
    h.prefetcher.prefetch('br_1');
    expect(h.messageRequests()).toHaveLength(0);
  });

  it('skips the hover for a session already in the store', () => {
    const h = harness();
    h.store.getState().actions.setTurns('br_1', model('br_1'));
    h.prefetcher.prefetch('br_1');
    expect(h.messageRequests()).toHaveLength(0);
  });

  it('announces the fetch in the SHARED store, not just its own register', () => {
    // `inFlight` is private, and `select()` reads the store's `turnsLoading` to decide
    // whether to join this fetch rather than start a second one. Reading that flag
    // without ever setting it — which is what this did — left the click path blind on
    // the ONE path a session is normally opened by, so hover-then-click fetched the
    // same page twice.
    //
    // (It also blinded the stream-resume-point work, which was withdrawn the day it
    // shipped for an unrelated reason — see `SyncEngine.streamCursors`. That is why the
    // measurements in this file's header are about request COUNT and nothing else.)
    const h = harness();
    h.prefetcher.prefetch('br_1');
    expect(h.store.getState().turnsLoading.has('br_1')).toBe(true);
  });

  it('stops announcing it once the fetch lands', async () => {
    const h = harness();
    h.prefetcher.prefetch('br_1');
    await h.settle();
    expect(h.store.getState().turnsLoading.has('br_1')).toBe(false);
  });

  it('stops announcing it when the fetch FAILS', async () => {
    // The only thing that clears it on this path — `setTurns` never runs. Left set, it
    // tells `select()` a page is still coming when nothing is fetching, and the pane
    // stays empty forever.
    const h = harness({ fail: true });
    h.prefetcher.prefetch('br_1');
    await h.fail();
    expect(h.store.getState().turnsLoading.has('br_1')).toBe(false);
  });

  it('a failed hover leaves nothing in flight, so the click path can retry', async () => {
    // The hole that joining the prefetch opens: a click that skipped its own fetch on
    // the strength of a prefetch that then errored would leave the pane empty with
    // nothing left to retry it. `select()` handles the rejection; this pins that the
    // register is clean afterwards so the retry is not itself blocked.
    const h = harness({ fail: true });
    h.prefetcher.prefetch('br_1');
    await h.fail();
    expect(h.prefetcher.warming('br_1')).toBeUndefined();
    expect(h.store.getState().turnsBySession.has('br_1')).toBe(false);
  });

  it('a hover nobody claims does not raise an unhandled rejection', async () => {
    // The user hovered a row and moved on. `warming()` hands the same promise to
    // `select()`, so the rejection has to be claimed on the hover's own reference or a
    // failed prefetch shouts in the console for something nobody asked for.
    const unhandled: unknown[] = [];
    const onUnhandled = (e: PromiseRejectionEvent | unknown): void => {
      unhandled.push(e);
    };
    process.on('unhandledRejection', onUnhandled);
    try {
      const h = harness({ fail: true });
      h.prefetcher.prefetch('br_1');
      await h.fail();
      await new Promise((r) => setTimeout(r, 10));
      expect(unhandled).toHaveLength(0);
    } finally {
      process.off('unhandledRejection', onUnhandled);
    }
  });
});
