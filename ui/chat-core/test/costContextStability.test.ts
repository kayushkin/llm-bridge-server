import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import { contextUsage, sessionCost } from '../src/store/selectors.js';
import type { TurnAggregates, TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

// Regression suite for the chat header flicker: the context strip and the cost chip
// blinking in and out on a LIVE session, taking 14px of header height with them on every
// blink. Two independent defects produced it, and both are covered here.
//
//   1. `applyEvent` rebuilt the TurnModel from an explicit field list that never named
//      `aggregates`, so every streamed event ERASED the server's cost/context roll-up.
//      That is the flicker itself: real data going away and coming back.
//   2. `sessionCost` / `contextUsage` memoized in a SINGLE slot keyed on the TurnModel's
//      identity, so two sessions read in one commit knocked each other out of the cache
//      and handed `useSyncExternalStore` a fresh object on every snapshot read.
//
// Only (1) was reachable from today's UI (one caller, one sessionId). (2) is a latent
// trap that would have turned the next second caller into a render loop, so it is fixed
// and pinned here rather than left for whoever adds that caller to discover.

function modelWith(sessionId: string, aggregates?: TurnAggregates): TurnModel {
  return {
    sessionId,
    turns: [],
    entries: {},
    validator: { maxEventId: 0, eventCount: 0, updatedAt: '2026-08-05T10:00:00-07:00' },
    more: false,
    ...(aggregates ? { aggregates } : {}),
  };
}

/** A streamed text delta — the single most frequent event on a live session, and so the
 *  one that decided how often the header blinked. */
function streamDelta(eventId: number, turnId: string, text: string): WireEvent {
  return {
    id: String(eventId),
    type: 'stream',
    data: {
      event_id: eventId,
      message_id: `m${eventId}`,
      turn_id: turnId,
      timestamp: '2026-08-05T10:00:01-07:00',
      stream: { delta: { type: 'text_delta', text } },
    },
  };
}

const AGG: TurnAggregates = {
  totalUsd: 1.25,
  byModel: { 'claude-opus-4': 1.25 },
  byQuerySource: { harness: 1.25 },
  contextTokens: 40_000,
  contextLimit: 200_000,
};

describe('aggregates survive the live tail (the header flicker)', () => {
  it('keeps the cost/context roll-up across a streamed event', () => {
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a', AGG));

    // What the header paints the moment `GET /turns` lands.
    expect(contextUsage(store.getState(), 'a')).toEqual({
      tokens: 40_000,
      limit: 200_000,
      pct: 20,
    });
    expect(sessionCost(store.getState(), 'a').totalUsd).toBe(1.25);

    // One token of streamed output. This must not be able to unsay what the roll-up said:
    // a stream delta carries no spend and no context figure, so it has nothing to replace
    // them WITH — dropping them reports "no cost data" for a session that has cost data.
    store.getState().actions.applyTailEvent('a', streamDelta(1, 't1', 'hello'));

    expect(contextUsage(store.getState(), 'a')).toEqual({
      tokens: 40_000,
      limit: 200_000,
      pct: 20,
    });
    expect(sessionCost(store.getState(), 'a').totalUsd).toBe(1.25);
  });

  it('keeps them across many streamed events, not just the first', () => {
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a', AGG));
    for (let i = 1; i <= 20; i++) {
      store.getState().actions.applyTailEvent('a', streamDelta(i, 't1', `tok${i}`));
    }
    expect(store.getState().turnsBySession.get('a')?.aggregates).toEqual(AGG);
    expect(contextUsage(store.getState(), 'a').pct).toBe(20);
  });

  it('keeps them when an older page is prepended', () => {
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a', AGG));
    // Paging backwards loads OLDER turns. The roll-up on screen describes the session,
    // not the page that happened to arrive last, so a backwards page must not blank it.
    store.getState().actions.prependOlder('a', modelWith('a'));
    expect(contextUsage(store.getState(), 'a').pct).toBe(20);
    expect(sessionCost(store.getState(), 'a').totalUsd).toBe(1.25);
  });

  it('does not let an OLDER page walk the roll-up backwards', () => {
    // Direction, not just presence. An older page's last-value-wins figures are by
    // construction the staler answer, so a page that carries its own block still must
    // not overwrite the one already on screen.
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a', AGG));
    store.getState().actions.prependOlder('a', modelWith('a', { ...AGG, contextTokens: 9_000 }));
    expect(contextUsage(store.getState(), 'a').tokens).toBe(40_000);
  });

  it('takes an older page’s roll-up when there is no other one at all', () => {
    // Preferring the fresher block must not mean refusing the only block there is: the
    // first page loaded may be the one WITHOUT spend events on it.
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a')); // no aggregates
    store.getState().actions.prependOlder('a', modelWith('a', AGG));
    expect(contextUsage(store.getState(), 'a').pct).toBe(20);
    expect(sessionCost(store.getState(), 'a').totalUsd).toBe(1.25);
  });

  it('lets a fresher materialization replace the roll-up', () => {
    // The flip side of every rule above: preserving a known figure must never mean
    // refusing a new one, or the readout freezes at whatever the first page said. This
    // is also what makes a /compact — which SHRINKS the window — show up.
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a', AGG));
    store.getState().actions.setTurns('a', modelWith('a', { ...AGG, contextTokens: 5_000 }));
    expect(contextUsage(store.getState(), 'a').tokens).toBe(5_000);
  });
});

describe('cost/context selectors are referentially stable per session', () => {
  // The invariant `useSyncExternalStore` demands: for an UNCHANGED state, repeated calls
  // with the same sessionId return the identical object, no matter which other sessions
  // were read in between. A single-slot cache cannot promise this — the interleaved read
  // evicts the slot and the next call allocates, so React sees the snapshot change on
  // every check and re-renders forever.
  function twoSessionStore() {
    const store = createChatStore();
    store.getState().actions.setTurns('a', modelWith('a', AGG));
    store
      .getState()
      .actions.setTurns('b', modelWith('b', { ...AGG, contextTokens: 10_000, totalUsd: 0.5 }));
    return store;
  }

  it('contextUsage: a → b → a returns the same object for a', () => {
    const store = twoSessionStore();
    const state = store.getState();
    const first = contextUsage(state, 'a');
    contextUsage(state, 'b');
    const second = contextUsage(state, 'a');
    expect(second).toBe(first);
  });

  it('sessionCost: a → b → a returns the same object for a', () => {
    const store = twoSessionStore();
    const state = store.getState();
    const first = sessionCost(state, 'a');
    sessionCost(state, 'b');
    const second = sessionCost(state, 'a');
    expect(second).toBe(first);
  });

  it('stays stable when a null sessionId is interleaved', () => {
    // The header passes `id`, which is null while nothing is selected and non-null after.
    // Both `null` and an unloaded id resolve to the SAME cache key (`undefined` model),
    // so a single slot cannot tell "no session" apart from "session a" either.
    const store = twoSessionStore();
    const state = store.getState();
    const first = contextUsage(state, 'a');
    contextUsage(state, null);
    expect(contextUsage(state, 'a')).toBe(first);

    const firstCost = sessionCost(state, 'a');
    sessionCost(state, null);
    expect(sessionCost(state, 'a')).toBe(firstCost);
  });

  it('round-robins three sessions without ever allocating twice for one', () => {
    const store = twoSessionStore();
    store.getState().actions.setTurns('c', modelWith('c', { ...AGG, contextLimit: 400_000 }));
    const state = store.getState();
    const a = contextUsage(state, 'a');
    const b = contextUsage(state, 'b');
    const c = contextUsage(state, 'c');
    for (let i = 0; i < 5; i++) {
      expect(contextUsage(state, 'a')).toBe(a);
      expect(contextUsage(state, 'b')).toBe(b);
      expect(contextUsage(state, 'c')).toBe(c);
    }
    // …and the three still hold DIFFERENT answers: stability must not come from
    // collapsing them onto one shared object.
    expect(b.tokens).toBe(10_000);
    expect(c.limit).toBe(400_000);
  });

  it('returns a NEW object once the session’s model actually changes', () => {
    // Stability is not staleness. A real mutation must invalidate.
    const store = twoSessionStore();
    const before = contextUsage(store.getState(), 'a');
    store.getState().actions.setTurns('a', modelWith('a', { ...AGG, contextTokens: 80_000 }));
    const after = contextUsage(store.getState(), 'a');
    expect(after).not.toBe(before);
    expect(after.tokens).toBe(80_000);
  });
});
