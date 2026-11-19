import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { Prefetcher } from '../src/boot/Prefetcher.js';
import { SessionCache } from '../src/cache/SessionCache.js';
import { createChatStore, type ChatStoreApi } from '../src/store/ChatStore.js';
import type { SessionSummary, SummaryResponse } from '../src/net/types.js';

// The sidebar's window used to be exactly one page of the newest sessions, whatever
// they were. On this box that page is ~8% sessions the user opened themselves and
// ~92% machine traffic, so the list was mostly autoworkers and reaching 50 real
// sessions meant paging 677 rows deep.
//
// The boot page is FILTERED server-side by the restored chip selection; a chip change
// fetches the first page of the new filter; older pages come on request for the
// filter that is set; a content-search hit outside the window is fetched by id.
// A background loop that paged 2,000 sessions at every boot did this job before.

function summary(id: string, updatedAt: string, extra: Partial<SessionSummary> = {}): SessionSummary {
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
    updatedAt,
    createdAt: updatedAt,
    ...extra,
  };
}

/** A page of `n` uniquely-named sessions, so merging pages grows the window rather
 *  than overwriting it. */
function page(prefix: string, n: number): SessionSummary[] {
  return Array.from({ length: n }, (_, i) =>
    summary(`${prefix}-${i}`, `2026-08-01 12:00:0${i % 10}`),
  );
}

/** Answers /sessions/summary from a queue and records the summary URLs in order (a
 *  POST lookup is recorded as its URL plus body). Anything else off the same fetch
 *  (recent-bundle, folders) answers empty and is deliberately NOT recorded. */
function summaryFetch(responses: SummaryResponse[], seen: string[]): typeof fetch {
  let i = 0;
  return (async (url: string, init?: RequestInit) => {
    const href = String(url);
    let body: unknown = {};
    if (href.includes('/sessions/summary')) {
      seen.push(init?.body ? `${href} ${String(init.body)}` : href);
      body = responses[Math.min(i, responses.length - 1)];
      i += 1;
    }
    return {
      ok: true,
      status: 200,
      statusText: 'OK',
      json: async () => body,
      text: async () => JSON.stringify(body),
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

function wire(
  responses: SummaryResponse[],
  opts: { perPage?: number } = {},
): { store: ChatStoreApi; prefetcher: Prefetcher; seen: string[] } {
  const seen: string[] = [];
  const store = createChatStore();
  const api = new ApiClient({ fetch: summaryFetch(responses, seen), basePath: '/api/bridge' });
  const prefetcher = new Prefetcher({
    store,
    api,
    // Cache off: these tests are about the network window, and an enabled cache
    // would need a real IndexedDB.
    cache: new SessionCache(false),
    sessionsPerPage: opts.perPage ?? 3,
  });
  return { store, prefetcher, seen };
}

/** The axes a summary URL asked for, as `axis=value` pairs — what the server will
 *  actually filter on, rather than what the caller meant to send. */
function axesOf(url: string): string[] {
  const query = new URLSearchParams(url.split('?')[1] ?? '');
  const out: string[] = [];
  for (const [key, value] of query.entries()) {
    if (key !== 'limit' && key !== 'before') out.push(`${key}=${value}`);
  }
  return out.sort();
}

describe('the boot page is filtered server-side', () => {
  it('sends the restored chip selection, so the first page is relevant and not merely recent', async () => {
    const { store, prefetcher, seen } = wire([{ sessions: page('a', 3), next: null, revision: 'r' }]);
    store.getState().actions.setFilter({ type: ['interactive', 'herald'] });

    await prefetcher.prime();

    expect(axesOf(seen[0])).toEqual(['type=herald', 'type=interactive']);
  });

  it('repeats a parameter per value rather than joining on commas', async () => {
    // A purpose on this box reads "browser verification + A/B perf"; nothing
    // stops one holding a comma, and a joined list would be cut in half by the split.
    const { store, prefetcher, seen } = wire([{ sessions: [], next: null, revision: 'r' }]);
    store.getState().actions.setFilter({ purpose: ['a,b', 'c'] });

    await prefetcher.prime();

    const query = seen[0].split('?')[1] ?? '';
    expect(query).toContain('purpose=a%2Cb');
    expect(query).toContain('purpose=c');
  });

  it('sends no axes at all when nothing is selected, so an unfiltered boot is unchanged', async () => {
    const { prefetcher, seen } = wire([{ sessions: page('a', 3), next: null, revision: 'r' }]);
    await prefetcher.prime();
    expect(axesOf(seen[0])).toEqual([]);
  });
});

describe('paging after boot', () => {
  it('boot fetches exactly one page of sessions — nothing pages in the background', async () => {
    const { prefetcher, seen } = wire([
      { sessions: page('p1', 3), next: 'c1', revision: 'r' },
      { sessions: page('p2', 3), next: 'c2', revision: 'r' },
    ]);
    await prefetcher.boot();
    await new Promise((r) => setTimeout(r, 20));
    expect(seen).toHaveLength(1);
  });

  it('loads an older page for the filter that is set, and stops when there is no cursor', async () => {
    const { store, prefetcher, seen } = wire([
      { sessions: page('p1', 3), next: 'c1', revision: 'r' },
      { sessions: page('p2', 3), next: null, revision: 'r' },
    ]);
    store.getState().actions.setFilter({ type: ['interactive'] });
    await prefetcher.prime();
    await prefetcher.loadOlderSessions();
    expect(seen[1]).toContain('before=c1');
    expect(axesOf(seen[1])).toEqual(['type=interactive']);
    expect(store.getState().sessions.size).toBe(6);
    expect(store.getState().olderSessionsCursor).toBeNull();

    await prefetcher.loadOlderSessions();
    expect(seen).toHaveLength(2);
  });
});

describe('a chip change fetches the new filter', () => {
  it('fetches the first page of the new filter, keeps the rows already held, and pages it from there', async () => {
    const { store, prefetcher, seen } = wire([
      { sessions: page('boot', 3), next: 'boot-cursor', revision: 'r' },
      { sessions: page('herald', 2), next: 'herald-cursor', revision: 'r' },
    ]);
    await prefetcher.prime();
    store.getState().actions.setFilter({ type: ['herald'] });
    await prefetcher.refilter();

    expect(axesOf(seen[1])).toEqual(['type=herald']);
    expect(seen[1]).not.toContain('before=');
    expect(store.getState().sessions.size).toBe(5);
    expect(store.getState().olderSessionsCursor).toBe('herald-cursor');
  });

  it('asks for nothing when only a local field changed', async () => {
    const { store, prefetcher, seen } = wire([{ sessions: page('boot', 3), next: null, revision: 'r' }]);
    await prefetcher.prime();
    store.getState().actions.setFilter({ search: 'hello', folder: 'work' });
    await prefetcher.refilter();
    expect(seen).toHaveLength(1);
  });

  it('drops a first page that lands after a newer chip change', async () => {
    const seen: string[] = [];
    const store = createChatStore();
    let releaseSlow: (() => void) | null = null;
    const fetchFn = (async (url: string) => {
      const href = String(url);
      seen.push(href);
      const types = new URLSearchParams(href.split('?')[1] ?? '').getAll('type');
      if (types.includes('slow')) await new Promise<void>((r) => (releaseSlow = r));
      const body: SummaryResponse = {
        sessions: page(types[0] ?? 'none', 1),
        next: `${types[0] ?? 'none'}-cursor`,
        revision: 'r',
      };
      return { ok: true, status: 200, statusText: 'OK', json: async () => body, text: async () => '' } as unknown as Response;
    }) as unknown as typeof fetch;
    const prefetcher = new Prefetcher({
      store,
      api: new ApiClient({ fetch: fetchFn, basePath: '/api/bridge' }),
      cache: new SessionCache(false),
      sessionsPerPage: 1,
    });

    store.getState().actions.setFilter({ type: ['slow'] });
    const slow = prefetcher.refilter();
    store.getState().actions.setFilter({ type: ['fast'] });
    await prefetcher.refilter();
    (releaseSlow as unknown as () => void)();
    await slow;

    expect(store.getState().olderSessionsCursor).toBe('fast-cursor');
  });
});

describe('a content-search hit outside the window is loaded by id', () => {
  it('looks up only the sessions not already held, without touching the cursor', async () => {
    const { store, prefetcher, seen } = wire([
      { sessions: page('boot', 3), next: 'boot-cursor', revision: 'r' },
      { sessions: [summary('far-away', '2026-01-01 00:00:00')], next: null, revision: 'r' },
    ]);
    await prefetcher.prime();
    await prefetcher.loadSessionsByIds(['boot-0', 'far-away']);

    expect(seen[1]).toContain('"session_ids":["far-away"]');
    expect(store.getState().sessions.has('far-away')).toBe(true);
    expect(store.getState().olderSessionsCursor).toBe('boot-cursor');

    await prefetcher.loadSessionsByIds(['boot-0', 'far-away']);
    expect(seen).toHaveLength(2);
  });
});
