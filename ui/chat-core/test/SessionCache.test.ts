import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it } from 'vitest';
import { IDBFactory } from 'fake-indexeddb';
import { SessionCache } from '../src/cache/SessionCache.js';
import { selectEvictions } from '../src/cache/evict.js';
import type { SessionSummary, TurnModel } from '../src/net/types.js';

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

describe('SessionCache — IndexedDB round-trip', () => {
  beforeEach(() => {
    // Fresh DB per test.
    globalThis.indexedDB = new IDBFactory();
  });

  it('round-trips list put/get', async () => {
    const cache = new SessionCache(true);
    const rows = [summary('br_a', '2026-07-27T10:00:00-07:00'), summary('br_b', '2026-07-27T12:00:00-07:00')];
    await cache.putList(rows);
    const got = await cache.getList();
    // Newest-first.
    expect(got.map((s) => s.sessionId)).toEqual(['br_b', 'br_a']);
    await cache.close();
  });

  it('round-trips turns put/get + validator', async () => {
    const cache = new SessionCache(true);
    const m = model('br_a', '2026-07-27T10:00:00-07:00');
    await cache.putTurns(m);
    const got = await cache.getTurns('br_a');
    expect(got).toEqual(m);
    const v = await cache.getValidator('br_a');
    expect(v).toEqual(m.validator);
    await cache.close();
  });

  it('hydrate returns list + turns + validators together', async () => {
    const cache = new SessionCache(true);
    await cache.putList([summary('br_a', '2026-07-27T10:00:00-07:00')]);
    await cache.putTurns(model('br_a', '2026-07-27T10:00:00-07:00'));
    const hydrated = await cache.hydrate();
    expect(hydrated.list.map((s) => s.sessionId)).toEqual(['br_a']);
    expect(hydrated.turns.get('br_a')?.sessionId).toBe('br_a');
    expect(hydrated.validators.get('br_a')?.maxEventId).toBe(7);
    await cache.close();
  });

  it('deleteSession removes list + turns + validator', async () => {
    const cache = new SessionCache(true);
    await cache.putList([summary('br_a', '2026-07-27T10:00:00-07:00')]);
    await cache.putTurns(model('br_a', '2026-07-27T10:00:00-07:00'));
    await cache.deleteSession('br_a');
    expect(await cache.getList()).toHaveLength(0);
    expect(await cache.getTurns('br_a')).toBeUndefined();
    expect(await cache.getValidator('br_a')).toBeUndefined();
    await cache.close();
  });

  it('a disabled cache is a no-op that never touches IndexedDB', async () => {
    const cache = new SessionCache(false);
    await cache.putTurns(model('br_a', '2026-07-27T10:00:00-07:00'));
    expect(await cache.getTurns('br_a')).toBeUndefined();
    const hydrated = await cache.hydrate();
    expect(hydrated.list).toHaveLength(0);
  });

  it('LRU eviction picks the oldest beyond the bound', () => {
    const keys = [
      { sessionId: 'a', updatedAt: '2026-07-20T00:00:00-07:00' },
      { sessionId: 'b', updatedAt: '2026-07-25T00:00:00-07:00' },
      { sessionId: 'c', updatedAt: '2026-07-27T00:00:00-07:00' },
    ];
    expect(selectEvictions(keys, 2)).toEqual(['a']); // oldest evicted
    expect(selectEvictions(keys, 3)).toEqual([]); // within bound
  });
});
