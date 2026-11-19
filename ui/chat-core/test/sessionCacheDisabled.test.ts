import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it } from 'vitest';
import { IDBFactory } from 'fake-indexeddb';
import { SessionCache } from '../src/cache/SessionCache.js';
import type { SessionSummary, TurnModel, Validator } from '../src/net/types.js';

// A disabled SessionCache is the whole L2 layer switched off, and every one of its
// fifteen methods carries its own guard producing its own empty answer. Those guards
// are not decoration: `db()` REJECTS when the cache is disabled, so a method that
// forgets its guard does not degrade to "no cache" — it hands the caller a rejected
// promise, and the boot path awaits several of them.
//
// SessionCache.test.ts has one test for this, named "a disabled cache is a no-op that
// never touches IndexedDB". Measured with scripts/failure-value-plans/session-cache-
// disabled.json: it exercises three of the fifteen (putTurns, getTurns, hydrate), and
// it checks neither half of its own name — ten guards could be deleted with it still
// green, and nothing in it looks at IndexedDB at all. A reader grepping for whether
// the disabled path is covered finds that test and stops, which is exactly why the
// gap survived. This file covers the surface and asserts both halves.

function summary(id: string): SessionSummary {
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
    updatedAt: '2026-07-27T10:00:00-07:00',
    createdAt: '2026-07-27T10:00:00-07:00',
  };
}

function validator(): Validator {
  return { maxEventId: 7, eventCount: 1, updatedAt: '2026-07-27T10:00:00-07:00' };
}

function model(id: string): TurnModel {
  return {
    sessionId: id,
    turns: [{ id: 't1', role: 'user', ts: '2026-07-27T10:00:00-07:00', entryIds: ['e1'] }],
    entries: {
      e1: {
        id: 'e1',
        turnId: 't1',
        role: 'user',
        kind: 'text',
        source: 'harness',
        eventId: 7,
        ts: '2026-07-27T10:00:00-07:00',
        text: 'hi',
        duplicate: false,
        primary: true,
      },
    },
    validator: validator(),
    more: false,
  };
}

/** How many times anything asked IndexedDB to open a database since the last reset.
 *  This is what makes "never touches IndexedDB" an assertion rather than a title. */
let databaseOpens = 0;

beforeEach(() => {
  const factory = new IDBFactory();
  const open = factory.open.bind(factory);
  factory.open = ((...args: Parameters<IDBFactory['open']>) => {
    databaseOpens += 1;
    return open(...args);
  }) as IDBFactory['open'];
  globalThis.indexedDB = factory;
  databaseOpens = 0;
});

describe('a disabled SessionCache — every method, not three of them', () => {
  // One call per method, each naming the empty value that method promises. Deleting
  // any single guard leaves that method reaching `db()`, which rejects, so each row
  // here fails on its OWN method rather than on a shared setup step.
  it('answers every read with its documented empty value', async () => {
    const cache = new SessionCache(false);

    expect(cache.isEnabled).toBe(false);
    expect(await cache.getList()).toEqual([]);
    expect(await cache.getTurns('br_a')).toBeUndefined();
    expect(await cache.getValidator('br_a')).toBeUndefined();
    expect(await cache.listKeysOldestFirst()).toEqual([]);
    expect(await cache.turnKeys()).toEqual([]);

    const hydrated = await cache.hydrate();
    expect(hydrated.list).toEqual([]);
    expect(hydrated.turns.size).toBe(0);
    expect(hydrated.validators.size).toBe(0);
  });

  it('accepts every write and does nothing with it', async () => {
    const cache = new SessionCache(false);

    // Each of these RESOLVES. A write whose guard is gone rejects instead, and the
    // callers of these are not written to expect a rejection.
    await expect(cache.putList([summary('br_a')])).resolves.toBeUndefined();
    await expect(cache.putSummary(summary('br_b'))).resolves.toBeUndefined();
    await expect(cache.putTurns(model('br_a'))).resolves.toBeUndefined();
    await expect(cache.putValidator('br_a', validator())).resolves.toBeUndefined();
    await expect(cache.deleteSession('br_a')).resolves.toBeUndefined();
    await expect(cache.evictListRows(['br_a'])).resolves.toBeUndefined();
    await expect(cache.evictTurns('br_a')).resolves.toBeUndefined();
    await expect(cache.close()).resolves.toBeUndefined();
  });

  it('never opens a database, which is the half of the claim nothing checked', async () => {
    const cache = new SessionCache(false);

    await cache.putList([summary('br_a')]);
    await cache.putSummary(summary('br_b'));
    await cache.putTurns(model('br_a'));
    await cache.putValidator('br_a', validator());
    await cache.getList();
    await cache.getTurns('br_a');
    await cache.getValidator('br_a');
    await cache.hydrate();
    await cache.listKeysOldestFirst();
    await cache.turnKeys();
    await cache.evictListRows(['br_a']);
    await cache.evictTurns('br_a');
    await cache.deleteSession('br_a');
    await cache.close();

    expect(databaseOpens).toBe(0);
  });
});

describe('an ENABLED SessionCache — the guards that are not about being disabled', () => {
  it('opens no database to evict nothing', async () => {
    // The second half of `if (!this.enabled || sessionIds.length === 0) return`. It is
    // the only guard here whose whole purpose is to NOT do work, so the return value
    // says nothing about it and the open count is the only thing that can: without it,
    // an empty eviction opens the database and starts a read-write transaction. The
    // list evictor calls this on every sweep, and most sweeps evict nothing.
    const cache = new SessionCache(true);

    await cache.evictListRows([]);

    expect(databaseOpens).toBe(0);
  });

  it('closes a cache that never opened anything, without throwing', async () => {
    // `if (!this.dbPromise) return`. Drop it and `await null` yields null, and the
    // `.close()` on the next line throws a TypeError — on the teardown path, where
    // nothing is expecting to have to catch.
    const cache = new SessionCache(true);

    await expect(cache.close()).resolves.toBeUndefined();
    expect(databaseOpens).toBe(0);
  });

  it('closes twice without throwing, because close() clears what it closed', async () => {
    const cache = new SessionCache(true);
    await cache.putSummary(summary('br_a'));
    expect(databaseOpens).toBe(1);

    await cache.close();
    await expect(cache.close()).resolves.toBeUndefined();

    // ...and the cache still works afterwards: close() drops the handle rather than
    // poisoning the instance, so the next read reopens.
    expect(await cache.getList()).toHaveLength(1);
    expect(databaseOpens).toBe(2);
  });
});
