// The REAL wire shape, discovered 2026-08-25: the per-session SSE stream carries
// llm-bridge-server's OWN row ids (bridge.db, ~1.77M here) while the materialized
// page carries LOG-STORE row ids (~2.09M) — two id spaces for the same events.
// Every prior replay fixture injected log-store ids into the SSE frames, which is
// why none of them reproduced what the browser showed. These fixtures are the
// truth: the session's actual bridge.db rows as SSE frames (no event_id in data,
// the id only on the frame — as the server really sends) and log-store's actual
// materialized page.
//
// What the id-space split broke, all reproduced here:
//  - a numeric event-id join between the paths NEVER matches, so a merge keyed on
//    it keeps every live entry beside the page's copy — the doubled "You" row and
//    the "Step 1, Step 1, Step 2, Step 2" narration observed live
//    (session br_1787615605129568013);
//  - seen-id sets seeded from page eventIds can swallow a live frame whose bridge
//    id equals any loaded log-store id;
//  - sorting entries by eventId across the spaces puts newer live entries BEFORE
//    older materialized ones — the original "narration arrives out of order".
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import sseFixture from './fixtures/duplication-session-sse.json';
import pageFixture from './fixtures/duplication-session-page.json';
import prefixPages from './fixtures/duplication-session-prefix-pages.json';

const SID = 'br_1787615605129568013';
const USER_TEXT_PREFIX = 'Do these three steps one at a time';

const frames = sseFixture as unknown as WireEvent[];
const finalPage = (pageFixture as { model: TurnModel }).model;
const pages = prefixPages as TurnModel[];

function visibleTexts(store: ReturnType<typeof createChatStore>): string[] {
  const model = store.getState().turnsBySession.get(SID);
  if (!model) return [];
  return Object.values(model.entries)
    .filter((e) => !e.duplicate && e.text)
    .map((e) => e.text!) as string[];
}

// The optimistic row carries EXACTLY what the user typed — that equality is the
// text-match half of its reconciliation, so the fixture must honor it.
const userPrompt =
  (frames.find((f) => f.type === 'user_message')!.data as { result?: { text?: string } }).result!
    .text!;

const count = (texts: string[], prefix: string) =>
  texts.filter((t) => t.startsWith(prefix)).length;

function assertClean(store: ReturnType<typeof createChatStore>, label: string, failures: string[]) {
  const texts = visibleTexts(store);
  const users = count(texts, USER_TEXT_PREFIX);
  const s1 = count(texts, 'Step 1:');
  const s2 = count(texts, 'Step 2:');
  if (users !== 1 || s1 !== 1 || s2 !== 1) {
    failures.push(`${label}: user=${users} s1=${s1} s2=${s2}`);
  }
}

describe('two id spaces: bridge SSE ids vs log-store page ids', () => {
  it('the stream then a repair leaves each thing visible exactly once', () => {
    const failures: string[] = [];
    // Repair injected at every stream position; page is the FINAL materialization.
    for (let p = 0; p <= frames.length; p++) {
      const store = createChatStore();
      const a = store.getState().actions;
      a.appendOptimisticUser(SID, userPrompt, 'creq_test');
      for (let i = 0; i < p; i++) a.applyTailEvent(SID, frames[i]!);
      a.setTurns(SID, finalPage);
      for (let i = p; i < frames.length; i++) a.applyTailEvent(SID, frames[i]!);
      assertClean(store, `final-page p=${p}`, failures);
    }
    expect(failures.slice(0, 8), `${failures.length} failing interleavings`).toEqual([]);
  });

  it('mid-turn repairs (real prefix pages) also converge', () => {
    const failures: string[] = [];
    for (let p = 0; p <= frames.length; p++) {
      for (let j = 0; j <= Math.min(p, pages.length - 1); j++) {
        const store = createChatStore();
        const a = store.getState().actions;
        a.appendOptimisticUser(SID, userPrompt, 'creq_test');
        for (let i = 0; i < p; i++) a.applyTailEvent(SID, frames[i]!);
        a.setTurns(SID, pages[j]!);
        for (let i = p; i < frames.length; i++) a.applyTailEvent(SID, frames[i]!);
        // The state that persists when no further repair arrives.
        assertClean(store, `prefix p=${p} j=${j}`, failures);
      }
    }
    expect(failures.slice(0, 8), `${failures.length} failing interleavings`).toEqual([]);
  });

  it('a live frame whose bridge id collides with a loaded log-store id still folds', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    // Cold load: the page is on screen first (its eventIds are log-store ids).
    a.setTurns(SID, finalPage);
    // A NEW live frame whose bridge id happens to equal one of the page's
    // log-store eventIds. It is a different event in a different id space and
    // must fold, not be silently swallowed as "seen".
    const collidingId = String(finalPage.entries['e_2097209']!.eventId); // 2097209
    const frame: WireEvent = {
      id: collidingId,
      type: 'block',
      data: {
        type: 'block',
        turn_id: 'turn_next',
        message_id: 'msg_new_turn',
        timestamp: '2026-08-25T00:10:00Z',
        block: { block: { type: 'text', text_block: { text: 'a brand new narration line' } } },
      } as never,
    };
    a.applyTailEvent(SID, frame);
    expect(visibleTexts(store).some((t) => t === 'a brand new narration line')).toBe(true);
  });

  it('after the repair, live-only content orders AFTER materialized content', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.setTurns(SID, finalPage);
    // Live continuation: a new user turn arrives on the stream (bridge ids, which
    // are NUMERICALLY SMALLER than every materialized eventId on the page).
    a.applyTailEvent(SID, {
      id: '1771000',
      type: 'user_message',
      data: {
        type: 'user_message',
        turn_id: 'turn_next',
        message_id: 'msg_next_user',
        timestamp: '2026-08-25T00:11:00Z',
        result: { text: 'a follow-up question' },
      } as never,
    });
    const model = store.getState().turnsBySession.get(SID)!;
    const lastTurn = model.turns[model.turns.length - 1]!;
    const texts = lastTurn.entryIds
      .map((id) => model.entries[id]?.text)
      .filter(Boolean);
    expect(texts, 'the follow-up must be in the LAST turn, not sorted to the front').toContain(
      'a follow-up question',
    );
    expect(model.turns[0]!.entryIds.map((id) => model.entries[id]?.text)).not.toContain(
      'a follow-up question',
    );
  });
});
