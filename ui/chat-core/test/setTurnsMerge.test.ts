// setTurns MERGES the materialized page into the live tail — never replaces it
// (dash docs/chat-turns-per-message.md §6). The join is the tail's per-entry
// EVENT-ID SETS: the two paths name the same content differently (live
// `${msgId}_${kind}` / materialized `e_<rowid>`), so an id-keyed merge would call
// everything new. These pin the rules one by one.
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { Entry, TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

const SID = 'br_merge';
const TS = '2026-08-24T05:00:00+00:00';

function ev(eventId: number, type: string, data: Record<string, unknown>): WireEvent {
  return {
    id: String(eventId),
    type,
    data: { event_id: eventId, type, turn_id: 't1', timestamp: TS, ...data },
  };
}

const liveText = (eventId: number, messageId: string, text: string) =>
  ev(eventId, 'block', {
    message_id: messageId,
    block: { block: { type: 'text', text_block: { text } } },
  });

const liveThinking = (eventId: number, messageId: string, text: string) =>
  ev(eventId, 'stream', {
    message_id: messageId,
    stream: { delta: { index: 0, type: 'thinking_delta', thinking: text } },
  });

function matEntry(over: Partial<Entry> & Pick<Entry, 'id' | 'kind' | 'eventId'>): Entry {
  return {
    turnId: 't1',
    role: 'assistant',
    source: 'harness',
    ts: TS,
    duplicate: false,
    primary: true,
    ...over,
  } as Entry;
}

function page(entries: Entry[], turnIds?: string[]): TurnModel {
  const dict: Record<string, Entry> = {};
  for (const e of entries) dict[e.id] = e;
  const ids = turnIds ?? [...new Set(entries.map((e) => e.turnId))];
  return {
    sessionId: SID,
    turns: ids.map((id) => ({
      id,
      role: 'user',
      ts: TS,
      entryIds: entries.filter((e) => e.turnId === id).map((e) => e.id),
    })),
    entries: dict,
    validator: { maxEventId: 99, eventCount: entries.length, updatedAt: TS },
    more: false,
  };
}

const modelOf = (store: ReturnType<typeof createChatStore>) =>
  store.getState().turnsBySession.get(SID)!;

describe('setTurns merges instead of replacing', () => {
  it('keeps live content the page has not materialized — the "everything resets" bug', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    // Live: narration text on event 10, which the page below does NOT carry.
    a.applyTailEvent(SID, liveText(10, 'msg_live', 'not yet materialized'));

    a.setTurns(SID, page([matEntry({ id: 'e_1', kind: 'text', eventId: 1, role: 'user', text: 'the prompt' })]));

    const m = modelOf(store);
    const kept = Object.values(m.entries).find((e) => e.text === 'not yet materialized');
    expect(kept, 'live-only entry must survive the page').toBeDefined();
    expect(m.entries['e_1']?.text).toBe('the prompt');
    // And its turn still lists it.
    const turn = m.turns.find((t) => t.entryIds.includes(kept!.id));
    expect(turn).toBeDefined();
  })

  it('supersedes a live entry whose every folded event the page reports', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.applyTailEvent(SID, liveText(10, 'msg_a', 'narration text'));

    a.setTurns(
      SID,
      page([matEntry({ id: 'e_10', kind: 'text', eventId: 10, messageId: 'msg_a', text: 'narration text' })]),
    );

    const m = modelOf(store);
    const holders = Object.values(m.entries).filter((e) => e.text === 'narration text');
    expect(holders.map((e) => e.id)).toEqual(['e_10']); // the page's version, once
  })

  it('keeps live reasoning WITH TEXT even when its event is reported (the carve-out)', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.applyTailEvent(SID, liveThinking(10, 'msg_a', 'weighing options'));

    // The page reports event 10 — as an EMPTY thinking block, which is all storage has.
    a.setTurns(
      SID,
      page([
        matEntry({ id: 'e_10', kind: 'thinking', eventId: 10, messageId: 'msg_a', text: '', duplicate: true, primary: false }),
      ]),
    );

    const m = modelOf(store);
    const withText = Object.values(m.entries).filter((e) => e.kind === 'thinking' && e.text);
    expect(withText).toHaveLength(1);
    expect(withText[0]!.text).toBe('weighing options');
    // Exactly one VISIBLE thinking entry — the page's empty copy arrives duplicate.
    const visible = Object.values(m.entries).filter((e) => e.kind === 'thinking' && !e.duplicate);
    expect(visible).toHaveLength(1);
  })

  it('reuses the held object for unchanged content, so memos keep hitting', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    const p = () => page([matEntry({ id: 'e_1', kind: 'text', eventId: 1, messageId: 'msg_a', text: 'stable' })]);
    a.setTurns(SID, p());
    const first = modelOf(store).entries['e_1'];
    a.setTurns(SID, p());
    expect(modelOf(store).entries['e_1']).toBe(first);
  })

  it('takes the incoming object when the content actually changed', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.setTurns(SID, page([matEntry({ id: 'e_1', kind: 'text', eventId: 1, text: 'draft' })]));
    a.setTurns(SID, page([matEntry({ id: 'e_1', kind: 'text', eventId: 1, text: 'edited' })]));
    expect(modelOf(store).entries['e_1']?.text).toBe('edited');
  })

  it('a replayed SSE event stays a no-op after the merge', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.applyTailEvent(SID, liveText(10, 'msg_a', 'once'));
    a.setTurns(
      SID,
      page([matEntry({ id: 'e_10', kind: 'text', eventId: 10, messageId: 'msg_a', text: 'once' })]),
    );
    // Last-Event-ID replay of the same event. The old rebuild-from-page forgot it
    // was applied, so the text folded in AGAIN next to the page's copy.
    a.applyTailEvent(SID, liveText(10, 'msg_a', 'once'));

    const holders = Object.values(modelOf(store).entries).filter((e) => e.text === 'once');
    expect(holders).toHaveLength(1);
  })

  it('supersedes the OPTIMISTIC user row when the page reports the real prompt', () => {
    // Observed live (session br_1787614088376534890): the optimistic row has no
    // folded event ids, so the event-id rule keeps it forever, and it rendered as
    // a duplicate "You" row after the first repair. The page reporting the same
    // prompt — by client request id or normalized text — is its supersession.
    const store = createChatStore();
    const a = store.getState().actions;
    a.appendOptimisticUser(SID, 'List the files', 'c1');

    a.setTurns(
      SID,
      page([
        matEntry({
          id: 'e_5', kind: 'text', eventId: 5, role: 'user', text: 'List the files',
          raw: { client_request_id: 'c1' } as unknown,
        } as never),
      ]),
    );

    const users = Object.values(modelOf(store).entries).filter(
      (e) => e.role === 'user' && e.text === 'List the files',
    );
    expect(users.map((e) => e.id)).toEqual(['e_5']);
  })

  it('an already-seen user_message event still strips the optimistic row', () => {
    // The trap: a repair page lands first, so the SSE user_message's event id is
    // already in seenEventIds and applyEvent no-ops — the strip must still commit
    // rather than being discarded with the no-op. The page here carries the event
    // under DIFFERENT text (server-side templating), so the merge-time supersession
    // above cannot fire and only the SSE strip can reconcile it.
    const store = createChatStore();
    const a = store.getState().actions;
    a.appendOptimisticUser(SID, 'hello there', 'c9');
    a.setTurns(
      SID,
      page([matEntry({ id: 'e_7', kind: 'text', eventId: 7, role: 'user', text: '<templated prompt>' })]),
    );
    // Optimistic still present (texts differ, no client id on the page raw).
    expect(
      Object.values(modelOf(store).entries).some((e) => e.text === 'hello there'),
    ).toBe(true);

    // The SSE copy of event 7 arrives, strippable by client_request_id.
    a.applyTailEvent(SID, ev(7, 'user_message', {
      client_request_id: 'c9',
      result: { text: '<templated prompt>' },
    }));

    expect(
      Object.values(modelOf(store).entries).some((e) => e.text === 'hello there'),
    ).toBe(false);
  })

  it('keeps paged-up older turns through a repair, ordered before the page', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    // The tail page, then an older page above it.
    a.setTurns(SID, page([matEntry({ id: 'e_50', kind: 'text', eventId: 50, turnId: 't5', role: 'user', text: 'newer prompt' })], ['t5']));
    store.getState().actions.prependOlder(
      SID,
      page([matEntry({ id: 'e_10', kind: 'text', eventId: 10, turnId: 't2', role: 'user', text: 'older prompt' })], ['t2']),
    );
    // A repair refetches only the tail window.
    a.setTurns(SID, page([matEntry({ id: 'e_50', kind: 'text', eventId: 50, turnId: 't5', role: 'user', text: 'newer prompt' })], ['t5']));

    const m = modelOf(store);
    expect(m.turns.map((t) => t.id)).toEqual(['t2', 't5']);
    expect(m.entries['e_10']?.text).toBe('older prompt');
  })
})
