// Replay of the REAL session that rendered its user row and narration twice
// (br_1787615605129568013, observed live 2026-08-24). The fixtures are the
// session's actual stored events (SSE-shaped) and log-store's actual materialized
// page — nothing synthesized. The test brute-forces every interleaving of one
// repair (setTurns) into the live stream, plus a trailing repair after the turn
// completes, and requires that NO interleaving leaves duplicated visible content.
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import eventsFixture from './fixtures/duplication-session-events.json';
import pageFixture from './fixtures/duplication-session-page.json';

const SID = 'br_1787615605129568013';
const USER_TEXT_PREFIX = 'Do these three steps one at a time';

const events: WireEvent[] = (eventsFixture as Array<Record<string, unknown>>).map((data) => ({
  id: String(data.event_id),
  type: String(data.type),
  data: data as never,
}));
const page = (pageFixture as { model: TurnModel }).model;

function visibleTexts(store: ReturnType<typeof createChatStore>): string[] {
  const model = store.getState().turnsBySession.get(SID);
  if (!model) return [];
  return Object.values(model.entries)
    .filter((e) => !e.duplicate && e.text)
    .map((e) => e.text!) as string[];
}

function countStartingWith(texts: string[], prefix: string): number {
  return texts.filter((t) => t.startsWith(prefix)).length;
}

describe('live replay of the duplication session', () => {
  it('no interleaving of a repair into the stream duplicates the user row or narration', () => {
    const failures: string[] = [];
    for (let k = 0; k <= events.length; k++) {
      const store = createChatStore();
      const a = store.getState().actions;
      a.appendOptimisticUser(SID, events[0]!.data.result?.text ?? '', 'creq_test');
      for (let i = 0; i < k; i++) a.applyTailEvent(SID, events[i]!);
      a.setTurns(SID, page);
      for (let i = k; i < events.length; i++) a.applyTailEvent(SID, events[i]!);
      // The trailing repair — the state that persists after the turn settles.
      a.setTurns(SID, page);

      const texts = visibleTexts(store);
      const users = countStartingWith(texts, USER_TEXT_PREFIX);
      const step1 = countStartingWith(texts, 'Step 1:');
      const step2 = countStartingWith(texts, 'Step 2:');
      if (users !== 1 || step1 !== 1 || step2 !== 1) {
        failures.push(`k=${k}: user=${users} step1=${step1} step2=${step2}`);
      }
    }
    expect(failures, failures.join('; ')).toEqual([]);
  });

  it('the stream alone (no repair) shows the user row and each step exactly once', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.appendOptimisticUser(SID, events[0]!.data.result?.text ?? '', 'creq_test');
    for (const ev of events) a.applyTailEvent(SID, ev);

    const texts = visibleTexts(store);
    expect(countStartingWith(texts, USER_TEXT_PREFIX)).toBe(1);
    expect(countStartingWith(texts, 'Step 1:')).toBe(1);
    expect(countStartingWith(texts, 'Step 2:')).toBe(1);
  });
});
