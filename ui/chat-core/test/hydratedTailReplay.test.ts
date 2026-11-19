// A reload replays the current turn onto entries the cache handed back — and one of
// those can be a LIVE-KEYED entry (`${messageId}_text`), because the page drops the
// answer's streamed block as superseded by its `result` and so never reports the
// live copy's unit; the merge keeps it, the cache writes it, the next boot hydrates
// it with an EMPTY folded-frame set. The server then replays the turn (no
// Last-Event-ID on a fresh tab — the cursor is memory-only) and the block frame lands
// on that entry as a fold, and `applyPayload` appends: one more copy of the answer per
// reload, joined with no separator. Measured 2026-09-10 on br_1788973449319671731
// (real page + real replay through this reducer: 1 copy live, 2 after one reload, 3
// after two). The fix treats a frame whose content a HYDRATED same-key entry already
// holds as the cross-space replay it is.
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import type { Entry, TurnModel } from '../src/net/types.js';

const SID = 'br_hydrated_replay';
const TS = '2026-09-10T18:05:00+00:00';
const MSG = 'msg_answer';
const ANSWER = 'Phase 3 is complete. Say the word.';

function frame(streamId: number, type: string, data: Record<string, unknown>): WireEvent {
  return { id: String(streamId), type, data: { type, turn_id: 't1', timestamp: TS, ...data } };
}
const block = (streamId: number, text: string) =>
  frame(streamId, 'block', {
    message_id: MSG,
    block: { index: 0, message_id: MSG, block: { type: 'text', text_block: { text } } },
  });
const result = (streamId: number, text: string) =>
  frame(streamId, 'result', { message_id: MSG, result: { text } });

// What log-store materializes for this turn: the prompt and the result. The streamed
// answer block is server-marked `duplicate` and is NOT on the collapsed page.
function pageWithoutTheBlock(): TurnModel {
  const entries: Record<string, Entry> = {
    e_1: {
      id: 'e_1', turnId: 't1', role: 'user', kind: 'text', source: 'harness', eventId: 1, ts: TS,
      messageId: 'msg_prompt', text: 'Go ahead', duplicate: false, primary: true,
    } as Entry,
    e_3: {
      id: 'e_3', turnId: 't1', role: 'assistant', kind: 'result', source: 'harness', eventId: 3, ts: TS,
      messageId: MSG, text: ANSWER, duplicate: false, primary: true,
    } as Entry,
  };
  return {
    sessionId: SID,
    turns: [{ id: 't1', role: 'user', ts: TS, entryIds: ['e_1', 'e_3'] }],
    entries,
    validator: { maxEventId: 3, eventCount: 3, updatedAt: TS },
    more: false,
  };
}

const copies = (s: string | undefined) => (s ?? '').split(ANSWER).length - 1;
const modelOf = (store: ReturnType<typeof createChatStore>) => store.getState().turnsBySession.get(SID)!;

/** The live tab: the turn streams in, then the page lands over it. */
function liveTab() {
  const store = createChatStore();
  const a = store.getState().actions;
  a.applyTailEvents(SID, [block(100, ANSWER), result(101, ANSWER)]);
  a.setTurns(SID, pageWithoutTheBlock());
  return modelOf(store);
}

/** A reload: the cached model hydrates as a page into an empty store, the SSE replays
 *  the current turn (no cursor), and the page is refetched. */
function reload(cached: TurnModel) {
  const store = createChatStore();
  const a = store.getState().actions;
  a.setTurns(SID, cached);
  a.applyTailEvents(SID, [block(100, ANSWER), result(101, ANSWER)]);
  a.setTurns(SID, pageWithoutTheBlock());
  return modelOf(store);
}

describe('a replayed frame onto a hydrated live-keyed entry', () => {
  it('precondition: the live answer entry survives the page merge and so reaches the cache', () => {
    const model = liveTab();
    const live = model.entries[`${MSG}_text`];
    expect(live, 'live text entry kept — the page never reports its unit').toBeDefined();
    expect(copies(live!.text)).toBe(1);
  });

  it('holds the answer ONCE after one reload, and after two', () => {
    const afterOne = reload(liveTab());
    expect(copies(afterOne.entries[`${MSG}_text`]?.text)).toBe(1);
    const afterTwo = reload(afterOne);
    expect(copies(afterTwo.entries[`${MSG}_text`]?.text)).toBe(1);
    // And nothing else on the collapsed view carries a second copy either.
    const visible = Object.values(afterTwo.entries).filter((e) => !e.duplicate && e.role === 'assistant');
    expect(visible.reduce((n, e) => n + copies(e.text), 0)).toBe(2); // the live text + the page's result
  });

  it('still appends a block that is genuinely NEW text for a hydrated message', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.setTurns(SID, liveTab());
    a.applyTailEvents(SID, [block(102, ' More.')]);
    expect(modelOf(store).entries[`${MSG}_text`]?.text).toBe(ANSWER + ' More.');
  });

  it('a live entry this tail built itself still takes a second block of the same text', () => {
    // Not hydrated: the entry has folded frame ids, so a repeated block is real
    // content (the model said the same thing twice), not a replay.
    const store = createChatStore();
    const a = store.getState().actions;
    a.applyTailEvents(SID, [block(100, 'Done.'), block(103, 'Done.')]);
    expect(modelOf(store).entries[`${MSG}_text`]?.text).toBe('Done.Done.');
  });
});
