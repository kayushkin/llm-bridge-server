import { describe, expect, it } from 'vitest';
import {
  createChatStore,
  DEFAULT_TURN_RETENTION_BYTES,
  DEFAULT_TURN_RETENTION_MIN_SESSIONS,
} from '../src/store/ChatStore.js';
import type { SessionSummary, TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

// The in-memory transcript store used to be unbounded. `turnsBySession` and `tails`
// were only ever removed from by `removeSession` — a SERVER-SIDE delete — so every
// session the user opened stayed in memory in full for the life of the tab.
//
// That is backwards from the layer below it: the IndexedDB cache has been bounded at
// `DEFAULT_CACHE_LIMIT` (50) since it was written, so L2 was capped and L1 was not.
//
// The cost is not theoretical. Measured against the live box on 2026-08-25, over the
// 40 most recent sessions, one `GET /sessions/{id}/messages?limit=30` page is 1.06 MB
// of JSON at the median and 10.1 MB at the worst, and the cold-boot
// `recent-bundle?n=20&turns=30` is 29.1 MB in a single response. A parsed object graph
// runs several times its wire size once string headers, the `turnIndex` Map and the
// per-entry `Set<number>` in `TailState` are counted, so a few dozen opens is a heap
// in the hundreds of megabytes — the reported growing lag (major GC over a heap that
// only grows) and then the tab crash.
//
// `limit` is a bound on MESSAGES, not on bytes: 30 messages carrying big tool results
// is 10 MB. So the page bound cannot stand in for a retention bound, and these cases
// pin the retention bound directly.
//
// Written against the OBSERVABLE store, the same way `listCacheBound.test.ts` is: the
// bug was never in a pure helper, it was that nothing evicted anything.

function summary(id: string): SessionSummary {
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
    managerSessionId: '',
    updatedAt: '2026-08-25T00:00:00Z',
    createdAt: '2026-08-25T00:00:00Z',
  };
}

/** A transcript whose payload is `chars` characters of text, so a case can state the
 *  size it means instead of hoping a fixture happens to weigh the right amount. */
function model(id: string, chars = 1000): TurnModel {
  return {
    sessionId: id,
    turns: [{ id: `${id}-t1`, role: 'user', ts: '2026-08-25T00:00:00Z', entryIds: [`${id}-e1`] }],
    entries: {
      [`${id}-e1`]: {
        id: `${id}-e1`,
        turnId: `${id}-t1`,
        role: 'user',
        kind: 'text',
        source: 'harness',
        eventId: 7,
        ts: '2026-08-25T00:00:00Z',
        text: 'x'.repeat(chars),
        duplicate: false,
        primary: true,
      },
    },
    validator: { maxEventId: 7, eventCount: 1, updatedAt: '2026-08-25T00:00:00Z' },
    more: false,
  };
}

/** Roughly what one `model(id, chars)` costs the budget: its text plus the flat
 *  per-entry overhead the estimator charges. */
function costOf(chars: number): number {
  return chars + 200;
}

function sessionId(n: number): string {
  return `br_${String(n).padStart(4, '0')}`;
}

/** One live assistant frame, with a fresh stream id so nothing rides the reducer's
 *  dedup and every case here is about retention rather than folding. */
function streamedText(n: number): WireEvent {
  const eventId = n + 1;
  return {
    id: String(eventId),
    type: 'stream',
    data: {
      type: 'stream',
      event_id: eventId,
      stream: { delta: { type: 'text_delta', text: 'streamed' } },
    },
  };
}

/** Open `count` sessions in order, exactly the way the UI does: select it, then let
 *  the fetched page land. `useTurns` fetches on a cold session and `select()` sets the
 *  active id, so both halves matter — a bound that only watched one of them would pass
 *  here and leak in the app. */
function openSessions(
  store: ReturnType<typeof createChatStore>,
  count: number,
  chars = 1000,
): void {
  const { actions } = store.getState();
  for (let n = 0; n < count; n++) {
    const id = sessionId(n);
    actions.upsertSession(summary(id));
    actions.setActive(id);
    actions.setTurns(id, model(id, chars));
  }
}

/** A store that holds exactly `sessions` transcripts of `chars` characters, with the
 *  session floor out of the way so the BUDGET is what the case is measuring. */
function storeHolding(sessions: number, chars: number) {
  return createChatStore({
    turnRetentionBytes: costOf(chars) * sessions,
    turnRetentionMinSessions: 0,
  });
}

describe('the in-memory transcript store is bounded', () => {
  it('holds no more than the byte budget however many sessions are opened', () => {
    const store = storeHolding(5, 1000);

    openSessions(store, 40, 1000);

    expect(store.getState().turnsBySession.size).toBeLessThanOrEqual(5);
  });

  it('measures the budget in BYTES, so big sessions crowd out more than small ones', () => {
    // The whole reason this is not a session count. A budget that fits five 1 KB
    // transcripts must fit only one at 5 KB — a count would hold five of each and the
    // heap would be five times bigger for the same "limit".
    const small = storeHolding(5, 1000);
    openSessions(small, 20, 1000);

    const large = createChatStore({
      turnRetentionBytes: costOf(1000) * 5,
      turnRetentionMinSessions: 0,
    });
    openSessions(large, 20, 5000);

    expect(small.getState().turnsBySession.size).toBe(5);
    expect(large.getState().turnsBySession.size).toBe(1);
  });

  it('keeps MORE ordinary sessions warm than a session count would', () => {
    // The point of the byte budget for the user rather than for the heap: someone working
    // across a dozen ordinary sessions gets all twelve instant, where a fixed count of a
    // handful would have evicted most of them.
    const store = createChatStore({
      turnRetentionBytes: costOf(1000) * 20,
      turnRetentionMinSessions: 0,
    });

    openSessions(store, 12, 1000);

    expect(store.getState().turnsBySession.size).toBe(12);
  });

  it('evicts the tail alongside the model, never one without the other', () => {
    // `TailState` holds the model AND `turnIndex` / `entryEventIds` / `seenEventIds`.
    // Dropping `turnsBySession` alone would free none of it — the tail still points at
    // the same model — so an orphaned tail is the whole leak wearing a different name.
    const store = storeHolding(5, 1000);

    openSessions(store, 40, 1000);

    const state = store.getState();
    expect(state.tails.size).toBe(state.turnsBySession.size);
    expect([...state.tails.keys()].sort()).toEqual([...state.turnsBySession.keys()].sort());
  });

  it('keeps the sessions most recently made active, not the ones opened first', () => {
    // `turnsBySession` is a Map, so its own key order is INSERTION order — the
    // longest-ago opens. An evictor that trusted that would drop exactly the sessions
    // the user is flipping between and keep the ones they have finished with.
    const store = storeHolding(3, 1000);

    openSessions(store, 10, 1000);

    const kept = [...store.getState().turnsBySession.keys()].sort();
    expect(kept).toEqual([sessionId(7), sessionId(8), sessionId(9)]);
  });

  it('refreshes recency when a WARM session is re-selected, with no refetch', () => {
    // Switching back to a session already in memory fetches nothing, so `setTurns` never
    // fires and `setActive` is the only evidence of use there is. Without it the session
    // the user is flipping to would age as though they had abandoned it, and would be
    // evicted out from under them mid-flip.
    const store = storeHolding(3, 1000);
    const { actions } = store.getState();

    openSessions(store, 3, 1000);
    const oldest = sessionId(0);
    actions.setActive(oldest); // warm — no setTurns

    actions.upsertSession(summary(sessionId(9)));
    actions.setActive(sessionId(9));
    actions.setTurns(sessionId(9), model(sessionId(9), 1000));

    // `oldest` was used most recently of the three, so the one that goes is `br_0001`.
    expect(store.getState().turnsBySession.has(oldest)).toBe(true);
    expect(store.getState().turnsBySession.has(sessionId(1))).toBe(false);
  });

  it('never evicts the active session, however far over budget it puts us', () => {
    // The active session is the one on screen. Evicting it blanks the transcript the
    // user is reading and costs a refetch to put back, which is the opposite of the
    // point.
    const store = createChatStore({ turnRetentionBytes: 1, turnRetentionMinSessions: 0 });
    const { actions } = store.getState();

    const active = sessionId(0);
    actions.upsertSession(summary(active));
    actions.setActive(active);
    actions.setTurns(active, model(active, 100_000));

    for (let n = 100; n < 110; n++) {
      const id = sessionId(n);
      actions.upsertSession(summary(id));
      actions.setTurns(id, model(id, 100_000));
    }

    const state = store.getState();
    expect(state.activeId).toBe(active);
    expect(state.turnsBySession.has(active)).toBe(true);
    expect(state.tails.has(active)).toBe(true);
  });

  it('keeps the session floor warm even when every transcript is enormous', () => {
    // The failure mode a byte budget has on its own: three 10 MB sessions overshoot any
    // sane budget, so flipping between the two the user is working across would evict and
    // refetch on every single switch — the slowest possible behavior arriving exactly
    // when transcripts are biggest and refetching hurts most.
    const store = createChatStore({ turnRetentionBytes: 1, turnRetentionMinSessions: 4 });

    openSessions(store, 10, 500_000);

    expect(store.getState().turnsBySession.size).toBe(4);
    expect([...store.getState().turnsBySession.keys()].sort()).toEqual([
      sessionId(6),
      sessionId(7),
      sessionId(8),
      sessionId(9),
    ]);
  });

  it('re-opening an evicted session brings it back and evicts something else', () => {
    // Eviction is not deletion: the session is still real, still in the sidebar, and
    // re-opening it must work. This is what makes the bound safe to set low — the cost
    // of being wrong is one refetch, not a broken page.
    const store = storeHolding(3, 1000);
    const { actions } = store.getState();

    openSessions(store, 10, 1000);
    const evicted = sessionId(0);
    expect(store.getState().turnsBySession.has(evicted)).toBe(false);

    actions.setActive(evicted);
    actions.setTurns(evicted, model(evicted, 1000));

    const state = store.getState();
    expect(state.turnsBySession.has(evicted)).toBe(true);
    expect(state.turnsBySession.size).toBeLessThanOrEqual(3);
  });

  it('drops only the transcript — the sidebar row and the draft survive', () => {
    // The heavy thing is the transcript. A summary is a few hundred bytes and the
    // sidebar needs every one it has loaded; a draft is unsent user text and losing it
    // is data loss. Evicting those to save memory would be trading the wrong thing.
    const store = storeHolding(3, 1000);
    const { actions } = store.getState();

    const withDraft = sessionId(0);
    actions.upsertSession(summary(withDraft));
    actions.setActive(withDraft);
    actions.setTurns(withDraft, model(withDraft, 1000));
    actions.setDraft(withDraft, 'half-typed message');

    openSessions(store, 10, 1000);

    const state = store.getState();
    expect(state.turnsBySession.has(withDraft)).toBe(false);
    expect(state.sessions.has(withDraft)).toBe(true);
    expect(state.drafts.get(withDraft)).toBe('half-typed message');
  });

  it('defaults to a budget that holds many ordinary sessions and few enormous ones', () => {
    // Measured on the live box 2026-08-25: 1.06 MB median per session page, 10.1 MB
    // worst. The default has to hold a comfortable number of the first without admitting
    // an unbounded number of the second.
    const medianSession = 1_060_000;
    const worstSession = 10_100_000;
    expect(DEFAULT_TURN_RETENTION_BYTES / medianSession).toBeGreaterThan(10);
    expect(DEFAULT_TURN_RETENTION_BYTES / worstSession).toBeLessThan(5);
    expect(DEFAULT_TURN_RETENTION_MIN_SESSIONS).toBeGreaterThanOrEqual(2);
  });

  it('applies the bound to live-streamed sessions too, not just fetched pages', () => {
    // A session can enter `turnsBySession` by three doors: `setTurns` (a fetched page),
    // `applyTailEvent` (a live frame) and `prependOlder` (backwards paging). A bound on
    // one door is not a bound.
    const store = createChatStore({ turnRetentionBytes: 1, turnRetentionMinSessions: 2 });
    const { actions } = store.getState();

    for (let n = 0; n < 20; n++) {
      const id = sessionId(n);
      actions.upsertSession(summary(id));
      actions.setActive(id);
      actions.applyTailEvent(id, streamedText(n));
    }

    expect(store.getState().turnsBySession.size).toBeLessThanOrEqual(2);
    expect(store.getState().tails.size).toBeLessThanOrEqual(2);
  });

  it('applies the bound to backwards paging too', () => {
    const store = storeHolding(3, 1000);
    const { actions } = store.getState();

    openSessions(store, 3, 1000);
    for (let n = 50; n < 60; n++) {
      const id = sessionId(n);
      actions.upsertSession(summary(id));
      actions.prependOlder(id, model(id, 1000));
    }

    // Three within the budget, plus the ACTIVE session — which stays pinned on top of the
    // budget rather than inside it, because the user is looking at it. Ten pages of
    // backwards paging arrive and the working set does not move off four.
    expect(store.getState().turnsBySession.size).toBe(4);
  });
});
