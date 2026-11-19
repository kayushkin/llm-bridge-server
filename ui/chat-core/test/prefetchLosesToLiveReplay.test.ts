import { describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { Prefetcher } from '../src/boot/Prefetcher.js';
import { SessionCache } from '../src/cache/SessionCache.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { Entry, TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

// Reproduction: hover a session (prefetch starts), click it (select joins the
// prefetch), and the session stream's replay of the LAST turn lands before the page.
// Reported 2026-10-20 on br_1792098153821205901: eight replies stored, one shown.

const SID = 'br_race';
const TS = '2026-10-15T21:00:00Z';

function entry(id: string, turnId: string, role: 'user' | 'assistant', eventId: number, text: string): Entry {
  return { id, turnId, role, kind: 'text', eventId, text, source: 'harness', ts: TS, duplicate: false, primary: true } as Entry;
}

function fullPage(): TurnModel {
  const entries = [
    entry('e_1', 't_old', 'user', 1, 'old prompt'),
    entry('e_2', 't_old', 'assistant', 2, 'old reply'),
    entry('e_3', 't_last', 'user', 3, 'last prompt'),
    entry('e_4', 't_last', 'assistant', 4, 'last reply'),
  ];
  return {
    sessionId: SID,
    turns: ['t_old', 't_last'].map((id) => ({
      id,
      role: 'user',
      ts: TS,
      entryIds: entries.filter((e) => e.turnId === id).map((e) => e.id),
    })),
    entries: Object.fromEntries(entries.map((e) => [e.id, e])),
    validator: { maxEventId: 4, eventCount: 4, updatedAt: TS },
    more: false,
  };
}

const lastTurnReplay: WireEvent = {
  id: '900',
  type: 'block',
  data: {
    event_id: 900,
    type: 'block',
    turn_id: 't_last',
    timestamp: TS,
    message_id: 'msg_last',
    block: { block: { type: 'text', text_block: { text: 'last reply' } } },
  },
};

describe('a hover prefetch that lands after the live replay', () => {
  it('still puts the older turns in the store', async () => {
    let release: (() => void) | null = null;
    const api = new ApiClient({
      basePath: '/api/bridge',
      fetch: (async () => {
        await new Promise<void>((r) => (release = r));
        return { ok: true, status: 200, json: async () => ({ model: fullPage() }) };
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

    prefetcher.prefetch(SID);
    store.getState().actions.applyTailEvents(SID, [lastTurnReplay]);
    release!();
    await prefetcher.warming(SID);

    const texts = Object.values(store.getState().turnsBySession.get(SID)!.entries).map((e) => e.text);
    expect(texts).toContain('old reply');
  });
});
