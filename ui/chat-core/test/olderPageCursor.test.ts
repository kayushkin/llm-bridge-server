// The load-older cursor must come from the page's numbering only. Live entries carry
// llm-bridge-server's row ids (~2.85M on 2026-10-08) while page entries carry
// log-store's (~3.25M); a cursor taken over both asked for history older than the
// whole session and got none (br_1791424129618218898).
import { describe, expect, it } from 'vitest';
import { olderPageCursor } from '../src/reduce/TurnReducer.js';
import type { Entry, TurnModel } from '../src/net/types.js';

function entry(id: string, eventId: number, origin: 'live' | 'page'): Entry {
  return {
    id,
    turnId: 'turn_A',
    role: 'user',
    kind: 'text',
    source: 'harness',
    eventId,
    ts: '2026-10-08T08:41:39Z',
    origin,
    duplicate: false,
    primary: true,
  } as Entry;
}

function model(entries: Entry[]): TurnModel {
  return {
    sessionId: 'br_test',
    turns: [],
    entries: Object.fromEntries(entries.map((e) => [e.id, e])),
    validator: { maxEventId: 0, eventCount: entries.length, updatedAt: '' },
    more: true,
  };
}

describe('olderPageCursor', () => {
  it('ignores live entries, whose ids are another numbering', () => {
    const m = model([entry('e_1', 3256447, 'page'), entry('e_2', 3257955, 'page'), entry('live_1', 2856406, 'live')]);
    expect(olderPageCursor(m)).toBe(3256447);
  });

  it('has no cursor when only live entries are held', () => {
    expect(olderPageCursor(model([entry('live_1', 2856406, 'live')]))).toBeNull();
  });
});
