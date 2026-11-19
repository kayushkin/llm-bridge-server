// Replay of the REAL session whose "You" row was reported once before and once
// after the answer (br_1788370653337509270, turn ...VWQ1V9ZZ, observed live
// 2026-09-02). Fixtures are the bridge's own SSE replay for that turn and
// log-store's projected page for it — nothing synthesized.
//
// The page in this capture holds the prompt's OTel echo as its only user entry:
// the window floor landed between the harness copy and the echo, so the pair was
// split and the echo went out unpaired and primary. This test asks what the client
// makes of that, for every interleaving of one repair into the stream.
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import eventsFixture from './fixtures/echo-boundary-session-events.json';
import pageFixture from './fixtures/echo-boundary-session-page.json';

const SID = 'br_1788370653337509270';
const USER_PREFIX = 'Ignore llmux, not going to worry';
const ANSWER_PREFIX = 'Done. Two files in the whole fleet';

const events: WireEvent[] = (eventsFixture as Array<Record<string, unknown>>).map((data) => ({
  id: String(data.event_id),
  type: String(data.type),
  data: data as never,
}));
const page = (pageFixture as { model: TurnModel }).model;

/** Visible entries in TRANSCRIPT ORDER: turns in order, entryIds in order, !duplicate. */
function visibleInOrder(store: ReturnType<typeof createChatStore>) {
  const model = store.getState().turnsBySession.get(SID);
  if (!model) return [];
  const out: { id: string; role?: string; kind?: string; source?: string; messageId?: string; text: string }[] = [];
  for (const t of model.turns) {
    for (const id of t.entryIds) {
      const e = model.entries[id];
      if (e && !e.duplicate && e.text) out.push({ id, role: e.role, kind: e.kind, source: e.source, messageId: e.messageId, text: e.text });
    }
  }
  return out;
}

function report(store: ReturnType<typeof createChatStore>) {
  const vis = visibleInOrder(store);
  const userIdx = vis.map((e, i) => (e.role === 'user' && e.text.startsWith(USER_PREFIX) ? i : -1)).filter((i) => i >= 0);
  // The answer is one MESSAGE that travels as a text block and a result with the same
  // messageId; the render edge folds those by messageId, so count messages, not rows.
  const answerIdx = vis.map((e, i) => (e.role === 'assistant' && e.text.startsWith(ANSWER_PREFIX) ? i : -1)).filter((i) => i >= 0);
  const answerMessages = new Set(answerIdx.map((i) => vis[i]!.messageId ?? vis[i]!.id));
  const userAfterAnswer = answerIdx.length > 0 && userIdx.some((u) => u > answerIdx[0]!);
  return { users: userIdx.length, answers: answerMessages.size, userAfterAnswer,
           userRows: userIdx.map((i) => `${vis[i]!.id}(${vis[i]!.source})@${i}`), answerAt: answerIdx };
}

describe('replay: the prompt pair split by a page window', () => {
  it('no interleaving of one repair shows the prompt twice, or after the answer', () => {
    const failures: string[] = [];
    for (let k = 0; k <= events.length; k++) {
      const store = createChatStore();
      const a = store.getState().actions;
      a.appendOptimisticUser(SID, events[0]!.data.result?.text ?? '', 'creq_test');
      for (let i = 0; i < k; i++) a.applyTailEvent(SID, events[i]!);
      a.setTurns(SID, page);
      for (let i = k; i < events.length; i++) a.applyTailEvent(SID, events[i]!);
      a.setTurns(SID, page);
      const r = report(store);
      if (r.users !== 1 || r.answers !== 1 || r.userAfterAnswer) failures.push(`k=${k}: ${JSON.stringify(r)}`);
    }
    // Print the distinct shapes rather than 250 lines.
    const distinct = [...new Set(failures.map((f) => f.replace(/^k=\d+: /, '')))];
    expect(failures.length, `${failures.length}/${events.length + 1} interleavings failed; shapes: ${distinct.slice(0, 4).join(' | ')}`).toBe(0);
  });

  it('the browser order: page first, then the whole stream as ONE batch, then a repair', () => {
    // SyncEngine folds a flush of frames through applyTailEvents (annotate once at the
    // end), not frame by frame; a cold select fetches the page before the stream opens.
    const store = createChatStore();
    const a = store.getState().actions;
    a.setTurns(SID, page);
    a.applyTailEvents(SID, events);
    a.setTurns(SID, page);
    expect(report(store)).toMatchObject({ users: 1, answers: 1, userAfterAnswer: false });
  });
  it('the browser order, stream in two batches around a repair', () => {
    const failures: string[] = [];
    for (let k = 0; k <= events.length; k += 7) {
      const store = createChatStore();
      const a = store.getState().actions;
      a.setTurns(SID, page);
      a.applyTailEvents(SID, events.slice(0, k));
      a.setTurns(SID, page);
      a.applyTailEvents(SID, events.slice(k));
      a.setTurns(SID, page);
      const r = report(store);
      if (r.users !== 1 || r.answers !== 1 || r.userAfterAnswer) failures.push(`k=${k}: ${JSON.stringify(r)}`);
    }
    expect(failures, failures.slice(0, 3).join(' | ')).toEqual([]);
  });
  it('the stream alone (no repair) is clean', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.appendOptimisticUser(SID, events[0]!.data.result?.text ?? '', 'creq_test');
    for (const ev of events) a.applyTailEvent(SID, ev);
    expect(report(store)).toMatchObject({ users: 1, answers: 1, userAfterAnswer: false });
  });
  it('the page alone (cold load) is clean', () => {
    const store = createChatStore();
    store.getState().actions.setTurns(SID, page);
    expect(report(store)).toMatchObject({ users: 1, answers: 1, userAfterAnswer: false });
  });
});
