// log-store's sourceGroups is the only record of a pairing whose other half the
// projected page does not carry. It has to survive every way a model is built.
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { Entry, TurnModel } from '../src/net/types.js';

const SID = 'br_sg';
const TS = '2026-09-02T22:00:00Z';
function entry(id: string, eventId: number, turnId: string, groupId?: string): Entry {
  return { id, role: 'user', kind: 'text', source: 'harness', eventId, ts: TS, turnId, text: `prompt ${id}`,
    duplicate: false, primary: true, ...(groupId ? { groupId } : {}) } as Entry;
}
function page(turnId: string, e: Entry, sourceGroups?: Record<string, string[]>, more = false): TurnModel {
  return { sessionId: SID, turns: [{ id: turnId, role: 'user', ts: TS, entryIds: [e.id] }], entries: { [e.id]: e },
    validator: { maxEventId: e.eventId, eventCount: 1, updatedAt: TS }, more, ...(sourceGroups ? { sourceGroups } : {}) };
}

describe('sourceGroups survives', () => {
  it('a cold page', () => {
    const s = createChatStore();
    s.getState().actions.setTurns(SID, page('t1', entry('e_1', 1, 't1', 'g_e_1'), { g_e_1: ['otel', 'harness'] }));
    expect(s.getState().turnsBySession.get(SID)!.sourceGroups).toEqual({ g_e_1: ['otel', 'harness'] });
  });
  it('a repair whose window moved on — the prior groups are kept for the history turns', () => {
    const s = createChatStore(); const a = s.getState().actions;
    a.setTurns(SID, page('t1', entry('e_1', 1, 't1', 'g_e_1'), { g_e_1: ['otel', 'harness'] }));
    a.setTurns(SID, page('t2', entry('e_2', 2, 't2', 'g_e_2'), { g_e_2: ['harness', 'otel'] }));
    const m = s.getState().turnsBySession.get(SID)!;
    expect(m.sourceGroups).toEqual({ g_e_1: ['otel', 'harness'], g_e_2: ['harness', 'otel'] });
    expect(m.turns.map((t) => t.id)).toEqual(['t1', 't2']);
  });
  it('an older page prepended below', () => {
    const s = createChatStore(); const a = s.getState().actions;
    a.setTurns(SID, page('t2', entry('e_2', 2, 't2', 'g_e_2'), { g_e_2: ['harness', 'otel'] }, true));
    a.prependOlder(SID, page('t1', entry('e_1', 1, 't1', 'g_e_1'), { g_e_1: ['otel', 'harness'] }));
    expect(s.getState().turnsBySession.get(SID)!.sourceGroups).toEqual({ g_e_1: ['otel', 'harness'], g_e_2: ['harness', 'otel'] });
  });
  it('a page with none leaves the field absent, not empty', () => {
    const s = createChatStore();
    s.getState().actions.setTurns(SID, page('t1', entry('e_1', 1, 't1')));
    expect(s.getState().turnsBySession.get(SID)!.sourceGroups).toBeUndefined();
  });
});
