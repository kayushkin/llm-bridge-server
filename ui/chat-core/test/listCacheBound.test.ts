import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it } from 'vitest';
import { IDBFactory } from 'fake-indexeddb';
import { SessionCache } from '../src/cache/SessionCache.js';
import {
  DEFAULT_LIST_CACHE_LIMIT,
  enforceCacheBound,
  enforceListBound,
} from '../src/cache/evict.js';
import type { SessionSummary, TurnModel } from '../src/net/types.js';

// The list store used to be append-only: `SyncEngine` wrote a row for every
// session it ever saw upserted and only `deleteSession` (a server delete) ever
// removed one, so it grew with the box's whole session history — 8,173 sessions
// on this host — while `Prefetcher.hydrateFromCache` read back one page.
//
// These cases are written against the OBSERVABLE store, not against
// `selectEvictions`. The pure helper was already green on the broken code: the
// bug was that nothing called anything with the list store's keys.

function summary(id: string, updatedAt: string): SessionSummary {
  return {
    sessionId: id,
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'inst1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: `Session ${id}`,
    agentId: '',
    updatedAt,
    createdAt: updatedAt,
  };
}

function model(id: string, updatedAt: string): TurnModel {
  return {
    sessionId: id,
    turns: [{ id: 't1', role: 'user', ts: updatedAt, entryIds: ['e1'] }],
    entries: {
      e1: {
        id: 'e1',
        turnId: 't1',
        role: 'user',
        kind: 'text',
        source: 'harness',
        eventId: 7,
        ts: updatedAt,
        text: 'hi',
        duplicate: false,
        primary: true,
      },
    },
    validator: { maxEventId: 7, eventCount: 1, updatedAt },
    more: false,
  };
}

/**
 * `n` summaries, returned oldest first.
 *
 * The ids run OPPOSITE to age on purpose — the oldest row is `br_<n-1>` and the
 * newest is `br_0000`. IndexedDB's primary-key order is the id order, so an
 * evictor that reads keys instead of the `updatedAt` index would drop the newest
 * rows, and with ids that sorted WITH age these cases would pass on it.
 */
function aged(n: number): SessionSummary[] {
  const base = Date.parse('2026-01-01T00:00:00-07:00');
  return Array.from({ length: n }, (_, i) =>
    summary(
      `br_${String(n - 1 - i).padStart(4, '0')}`,
      new Date(base + i * 60_000).toISOString(),
    ),
  );
}

/** The id `aged(n)` gives the `i`-th oldest row. */
function oldestNth(n: number, i: number): string {
  return `br_${String(n - 1 - i).padStart(4, '0')}`;
}

describe('list cache bound', () => {
  beforeEach(() => {
    globalThis.indexedDB = new IDBFactory();
  });

  it('trims the list store to the newest `limit` rows', async () => {
    const cache = new SessionCache(true);
    await cache.putList(aged(300));
    expect(await cache.getList()).toHaveLength(300); // the growth, reproduced

    const victims = await enforceListBound(cache, 100);

    expect(victims).toHaveLength(200);
    const kept = await cache.getList();
    expect(kept).toHaveLength(100);
    // Newest-first: the survivors are the 100 youngest rows.
    expect(kept[0]?.sessionId).toBe(oldestNth(300, 299));
    expect(kept[99]?.sessionId).toBe(oldestNth(300, 200));
    expect(victims).toContain(oldestNth(300, 0)); // the oldest row
    expect(victims).not.toContain(oldestNth(300, 200));
    await cache.close();
  });

  it('leaves a store already inside the bound alone', async () => {
    const cache = new SessionCache(true);
    await cache.putList(aged(40));
    expect(await enforceListBound(cache, 100)).toEqual([]);
    expect(await cache.getList()).toHaveLength(40);
    await cache.close();
  });

  it('leaves a store ONE ROW under the bound alone — the case a negative slice length destroys', async () => {
    // The case above cannot fail for the reason this one can, and the gap is
    // arithmetic rather than taste. `enforceListBound` computes
    // `over = length - limit` and returns early only because of the
    // `if (over <= 0)` guard; delete that guard and the code reaches
    // `oldestFirst.slice(0, over)` with a NEGATIVE length, which counts back
    // from the END instead of returning nothing.
    //
    // A negative `over` is only VISIBLE while `length > limit - length`, i.e.
    // while the store is more than half full. At 40 rows under a 100 cap
    // `slice(0, -60)` clamps to empty and the unguarded code is
    // indistinguishable from the guarded one — so the fixture above passes on
    // both, and the guard it appears to cover is unpinned.
    //
    // 99 under 100 is the tightest under-limit case there is, and the worst:
    // `slice(0, -1)` evicts 98 of the 99 rows the bound was asked to keep.
    const cache = new SessionCache(true);
    await cache.putList(aged(99));

    expect(await enforceListBound(cache, 100)).toEqual([]);
    expect(await cache.getList()).toHaveLength(99);
    await cache.close();
  });

  it('evicts nothing at EXACTLY the bound, and one row past it evicts exactly one', async () => {
    // The two sides of `over <= 0`. Straddling the boundary rather than sitting
    // on one side of it is what makes the comparison itself load-bearing: an
    // off-by-one to `over < 0` changes neither of the cases above.
    const atLimit = new SessionCache(true);
    await atLimit.putList(aged(100));
    expect(await enforceListBound(atLimit, 100)).toEqual([]);
    expect(await atLimit.getList()).toHaveLength(100);
    await atLimit.close();

    globalThis.indexedDB = new IDBFactory();
    const overLimit = new SessionCache(true);
    await overLimit.putList(aged(101));
    expect(await enforceListBound(overLimit, 100)).toEqual([oldestNth(101, 0)]);
    expect(await overLimit.getList()).toHaveLength(100);
    await overLimit.close();
  });

  it('keeps an old row whose turns are still cached', async () => {
    const cache = new SessionCache(true);
    const rows = aged(120);
    await cache.putList(rows);
    const oldest = oldestNth(120, 0);
    // The oldest row there is, so the bound would drop it first.
    await cache.putTurns(model(oldest, rows[0]!.updatedAt));

    const victims = await enforceListBound(cache, 100);

    expect(victims).not.toContain(oldest);
    expect(victims).toHaveLength(19); // 20 over the bound, one protected
    const kept = await cache.getList();
    // The bound yields to the invariant rather than orphaning a turn model.
    expect(kept).toHaveLength(101);
    expect(kept.map((s) => s.sessionId)).toContain(oldest);
    await cache.close();
  });

  it('drops the list row but keeps nothing else — turns and validators are the other bound', async () => {
    const cache = new SessionCache(true);
    await cache.putList(aged(102));
    const oldest = oldestNth(102, 0);
    // A validator with no turn model: not protected, and not the list evictor's
    // to delete either. Only `deleteSession` drops all three.
    await cache.putValidator(oldest, { maxEventId: 3, eventCount: 1, updatedAt: 'x' });

    await enforceListBound(cache, 100);

    expect((await cache.getList()).map((s) => s.sessionId)).not.toContain(oldest);
    expect(await cache.getValidator(oldest)).toBeDefined();
    await cache.close();
  });

  it('enforceCacheBound bounds BOTH stores in one pass', async () => {
    const cache = new SessionCache(true);
    const rows = aged(150);
    await cache.putList(rows);
    // 60 sessions with turns, all among the newest 60 rows.
    for (const s of rows.slice(90)) await cache.putTurns(model(s.sessionId, s.updatedAt));

    const turnVictims = await enforceCacheBound(cache, 50, 100);

    expect(turnVictims).toHaveLength(10); // 60 turn models, bound 50
    expect(await cache.turnKeys()).toHaveLength(50);
    expect(await cache.getList()).toHaveLength(100);
    await cache.close();
  });

  it('turns are evicted BEFORE the list pass reads the surviving set', async () => {
    // Ordering matters: a session whose turns this same pass just dropped must
    // not keep protecting its list row. Bound turns to zero and every row is
    // eligible again.
    const cache = new SessionCache(true);
    const rows = aged(120);
    await cache.putList(rows);
    for (const s of rows.slice(0, 20)) await cache.putTurns(model(s.sessionId, s.updatedAt));

    await enforceCacheBound(cache, 0, 100);

    expect(await cache.turnKeys()).toHaveLength(0);
    expect(await cache.getList()).toHaveLength(100); // no row survived on a dropped turn model
    await cache.close();
  });

  it('a disabled cache never touches IndexedDB', async () => {
    const cache = new SessionCache(false);
    expect(await cache.listKeysOldestFirst()).toEqual([]);
    expect(await enforceListBound(cache, 100)).toEqual([]);
  });

  it('the default bound is one sidebar page', () => {
    // Not a tautology: it is the assertion that the constant tracks
    // `Prefetcher.DEFAULT_SESSIONS_PER_PAGE`. A cache bound below the page size
    // would trim rows the very next boot paint wants.
    expect(DEFAULT_LIST_CACHE_LIMIT).toBe(100);
  });

  it('listKeysOldestFirst orders by updatedAt, not by insertion or by id', async () => {
    const cache = new SessionCache(true);
    // Ids sort OPPOSITE to age, so a primary-key read answers exactly backwards.
    await cache.putList([
      summary('br_a', '2026-01-03T00:00:00-07:00'),
      summary('br_z', '2026-01-01T00:00:00-07:00'),
      summary('br_m', '2026-01-02T00:00:00-07:00'),
    ]);
    expect(await cache.listKeysOldestFirst()).toEqual(['br_z', 'br_m', 'br_a']);
    await cache.close();
  });
});

describe('turnKeys', () => {
  beforeEach(() => {
    globalThis.indexedDB = new IDBFactory();
  });

  it('lists every cached transcript with its validator timestamp, and nothing else', async () => {
    const cache = new SessionCache(true);
    await cache.putTurns(model('br_a', '2026-09-01T00:00:00Z'));
    await cache.putTurns(model('br_b', '2026-09-02T00:00:00Z'));
    // A validator with no transcript is not a cached transcript.
    await cache.putValidator('br_orphan', { maxEventId: 1, eventCount: 1, updatedAt: '2026-09-03T00:00:00Z' });

    const keys = await cache.turnKeys();
    expect(keys.sort((a, b) => a.sessionId.localeCompare(b.sessionId))).toEqual([
      { sessionId: 'br_a', updatedAt: '2026-09-01T00:00:00Z' },
      { sessionId: 'br_b', updatedAt: '2026-09-02T00:00:00Z' },
    ]);
    await cache.close();
  });

  it('never reads a transcript to list them', async () => {
    const cache = new SessionCache(true);
    await cache.putTurns(model('br_a', '2026-09-01T00:00:00Z'));
    // Reading a value from the turns store would go through getAll/get on it.
    const store = IDBObjectStore.prototype;
    const getAll = store.getAll;
    const get = store.get;
    const readStores: string[] = [];
    store.getAll = function (this: IDBObjectStore, ...args: Parameters<typeof getAll>) {
      readStores.push(this.name);
      return getAll.apply(this, args);
    };
    store.get = function (this: IDBObjectStore, ...args: Parameters<typeof get>) {
      readStores.push(this.name);
      return get.apply(this, args);
    };
    try {
      await cache.turnKeys();
    } finally {
      store.getAll = getAll;
      store.get = get;
    }
    expect(readStores).not.toContain('turns');
    await cache.close();
  });
});

describe('database upgrade', () => {
  beforeEach(() => {
    globalThis.indexedDB = new IDBFactory();
  });

  it('drops the stream resume store version 2 wrote', async () => {
    const { openDB } = await import('idb');
    const v2 = await openDB('chat-core', 2, {
      upgrade(db) {
        db.createObjectStore('list', { keyPath: 'sessionId' }).createIndex('updatedAt', 'updatedAt');
        db.createObjectStore('turns', { keyPath: 'sessionId' });
        db.createObjectStore('validators', { keyPath: 'sessionId' });
        db.createObjectStore('streamResume', { keyPath: 'sessionId' });
      },
    });
    await v2.put('streamResume', { sessionId: 'br_kept', head: 42 });
    v2.close();

    const cache = new SessionCache(true);
    await cache.hydrate();
    await cache.close();

    const upgraded = await openDB('chat-core');
    expect([...upgraded.objectStoreNames]).not.toContain('streamResume');
    upgraded.close();
  });

  it('version 4 empties transcripts written before entries carried `origin`, and keeps the list', async () => {
    // A pre-stamp transcript holds live rows nothing marks as live; read back, the merge
    // takes them for page history and draws the prompt twice (liveReplayCacheRoundTrip).
    const { openDB } = await import('idb');
    const v3 = await openDB('chat-core', 3, {
      upgrade(db) {
        db.createObjectStore('list', { keyPath: 'sessionId' }).createIndex('updatedAt', 'updatedAt');
        db.createObjectStore('turns', { keyPath: 'sessionId' });
        db.createObjectStore('validators', { keyPath: 'sessionId' });
      },
    });
    const unstamped = model('br_unstamped', '2026-09-01T00:00:00Z');
    await v3.put('turns', unstamped);
    await v3.put('validators', { sessionId: 'br_unstamped', validator: unstamped.validator });
    await v3.put('list', { sessionId: 'br_unstamped', summary: { sessionId: 'br_unstamped' }, updatedAt: '2026-09-01T00:00:00Z' });
    v3.close();

    const cache = new SessionCache(true);
    const hydrated = await cache.hydrate();
    expect(hydrated.turns.has('br_unstamped')).toBe(false);
    expect(hydrated.validators.has('br_unstamped')).toBe(false);
    expect(hydrated.list.map((s) => s.sessionId)).toContain('br_unstamped');

    // And a transcript written by THIS build survives the next open.
    await cache.putTurns(model('br_stamped', '2026-09-17T00:00:00Z'));
    await cache.close();
    const reopened = new SessionCache(true);
    expect((await reopened.hydrate()).turns.has('br_stamped')).toBe(true);
    await reopened.close();
  });
});
