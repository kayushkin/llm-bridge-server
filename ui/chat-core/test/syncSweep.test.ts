import { describe, expect, it, vi } from 'vitest';
import { SyncEngine } from '../src/sync/SyncEngine.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { ApiClient } from '../src/net/ApiClient.js';
import type { SessionCache } from '../src/cache/SessionCache.js';
import type { TurnModel, Validator } from '../src/net/types.js';

function modelWith(sessionId: string, validator: Validator): TurnModel {
  return { sessionId, turns: [], entries: {}, validator, more: false };
}

// ⚠️ ABSENT IS `null` HERE, NOT `undefined`, AND THAT IS THE WHOLE POINT.
// These parameters have defaults, and a default fills in for an argument that is
// `undefined` — passing `undefined` explicitly is indistinguishable from not
// passing it at all. Written the obvious way, the two cases below that mean "the
// validator is MISSING" silently ran against the default PRESENT validator: they
// passed, and they went on passing with the missing-side guard deleted from
// `validatorsEqual`. A fixture that cannot express the absence it is named for
// fails green.
function setup(
  activeId: string | null,
  // The server's answer for the cached session. Defaults to a validator AHEAD of
  // the cached one; the cases below pass an equal one, or `null` for none at
  // all, because those are the inputs that make the comparison load-bearing.
  serverValidator: Validator | null = { maxEventId: 6, eventCount: 6, updatedAt: '' },
  localValidator: Validator | null = { maxEventId: 5, eventCount: 5, updatedAt: '' },
) {
  const store = createChatStore();
  const sid = 'br_running';
  // A warm/cached tail for a running-but-UNSELECTED session, validator {5,5}.
  store.getState().actions.setTurns(
    sid,
    modelWith(sid, (localValidator ?? undefined) as Validator),
  );
  if (activeId) store.getState().actions.setActive(activeId);

  const getValidators = vi.fn(async () => (
    serverValidator === null ? {} : { [sid]: serverValidator }
  ));
  const getMessages = vi.fn(async () => ({
    model: modelWith(sid, { maxEventId: 6, eventCount: 6, updatedAt: '' }),
  }));
  const api = { getValidators, getMessages } as unknown as ApiClient;
  const cache = { isEnabled: false, putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}) } as unknown as SessionCache;
  const engine = new SyncEngine({ store, api, cache });
  return { store, engine, sid, getValidators, getMessages };
}

describe('SyncEngine.sweepValidators — idle over-poll fix', () => {
  it('an UNSELECTED running session gets validators only, NO tail fetch', async () => {
    const { engine, getValidators, getMessages } = setup(null);
    await engine.sweepValidators();
    expect(getValidators).toHaveBeenCalledTimes(1); // cheap check ran
    expect(getMessages).not.toHaveBeenCalled(); // no ~500 KB tail pull
  });

  it('pulls the tail only once the session is the active/open one', async () => {
    const { store, engine, sid, getMessages } = setup(null);
    await engine.sweepValidators();
    expect(getMessages).not.toHaveBeenCalled();

    // Select it — now its tail is displayed and must be repaired.
    store.getState().actions.setActive(sid);
    await engine.sweepValidators();
    expect(getMessages).toHaveBeenCalledTimes(1);
  });

  // Everything above uses local {5,5} against server {6,6}: the two validators
  // ALWAYS differ, so the comparison never has to answer "the same". Forcing
  // `validatorsEqual` to return false unconditionally left this whole file green
  // — the control mutation for the file was not caught, which means nothing here
  // exercised the equality at all. The cases below are that missing half.

  it('an ACTIVE session whose validator is UNCHANGED is not refetched', () => {
    // The sweep's reason to exist. Every tick compares a cheap validator so the
    // ~500 KB tail is pulled only when something moved; if "equal" stopped being
    // recognized, every idle tick would refetch the open session's whole tail.
    // The session is ACTIVE here on purpose — with it unselected, `isDisplayed`
    // would suppress the fetch on its own and the case would pass without the
    // comparison ever being consulted.
    const same: Validator = { maxEventId: 5, eventCount: 5, updatedAt: '' };
    const { engine, getValidators, getMessages } = setup('br_running', same, same);
    return engine.sweepValidators().then(() => {
      expect(getValidators).toHaveBeenCalledTimes(1); // the cheap check still runs
      expect(getMessages).not.toHaveBeenCalled(); // and nothing was refetched
    });
  });

  it('the same maxEventId with a DIFFERENT count still counts as changed', async () => {
    // Both fields, not just the head. A tail can be edited in place — an entry
    // annotated, a compaction rewriting rows — leaving the highest event id put
    // while the count moves. Comparing `maxEventId` alone would call that equal
    // and never repair it.
    const { engine, getMessages } = setup(
      'br_running',
      { maxEventId: 5, eventCount: 9, updatedAt: '' },
      { maxEventId: 5, eventCount: 5, updatedAt: '' },
    );
    await engine.sweepValidators();
    expect(getMessages).toHaveBeenCalledTimes(1);
  });

  it('a session the server reports NO validator for is not treated as unchanged', async () => {
    // `getValidators` answers with an object keyed by session id, so a session
    // the server omits reads back as `undefined`. That is not evidence the tail
    // is current — it is the absence of evidence, and the missing-side guard is
    // what keeps the two apart. Called equal instead, a session the server has
    // stopped reporting would never be repaired again.
    const { engine, getMessages } = setup(
      'br_running',
      null, // the server omits this session entirely
      { maxEventId: 5, eventCount: 5, updatedAt: '' },
    );
    await engine.sweepValidators();
    expect(getMessages).toHaveBeenCalledTimes(1);
  });

  it('a cached tail with NO local validator is repaired, not skipped', async () => {
    // The other side of the same guard: a cached model carrying no validator
    // cannot be shown to match anything, so it must be repaired rather than
    // assumed current.
    const { engine, getMessages } = setup(
      'br_running',
      { maxEventId: 6, eventCount: 6, updatedAt: '' },
      null, // nothing cached locally
    );
    await engine.sweepValidators();
    expect(getMessages).toHaveBeenCalledTimes(1);
  });

  it('BOTH sides absent is still not equal — two unknowns do not agree', async () => {
    // The case each single-sided test above leaves open. `!a || !b` is one
    // expression covering both sides, so a change that added `if (!a && !b)
    // return true` above it would keep every case above green while turning the
    // both-absent tick into "already reconciled". A session with nothing cached
    // and nothing reported would then never be repaired — and it is the session
    // in the worst state that reaches this branch.
    const { engine, getMessages } = setup('br_running', null, null);
    await engine.sweepValidators();
    expect(getMessages).toHaveBeenCalledTimes(1);
  });
});

// ── Two ways a cached tail went stale and stayed stale ────────────────────────
//
// Reported symptom: "sometimes I need to refresh because a window has been open,
// and the chat page is not actively checking for updates on session change like the
// original did." Both halves are real and independent.

describe('the sweep always checks the session that is on screen', () => {
  /** `sweepLimit` cached sessions, then one more that is the ACTIVE one — so the
   *  active session is last in insertion order and outside any slice from the
   *  front. */
  function setupBeyondTheLimit(sweepLimit: number) {
    const store = createChatStore();
    const validator = { maxEventId: 5, eventCount: 5, updatedAt: '' };
    for (let i = 0; i < sweepLimit; i++) {
      const id = `br_old_${i}`;
      store.getState().actions.setTurns(id, modelWith(id, validator));
    }
    const active = 'br_active';
    store.getState().actions.setTurns(active, modelWith(active, validator));
    store.getState().actions.setActive(active);

    const getValidators = vi.fn(async (ids: string[]) =>
      Object.fromEntries(ids.map((id) => [id, { maxEventId: 6, eventCount: 6, updatedAt: '' }])),
    );
    const getMessages = vi.fn(async (id: string) => ({
      model: modelWith(id, { maxEventId: 6, eventCount: 6, updatedAt: '' }),
    }));
    const api = { getValidators, getMessages } as unknown as ApiClient;
    const cache = { isEnabled: false, putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}) } as unknown as SessionCache;
    const engine = new SyncEngine({ store, api, cache, sweepLimit });
    return { engine, active, getValidators, getMessages };
  }

  it('repairs the active session even when it is past sweepLimit in the cache', async () => {
    // ⚠️ The bug. `turnsBySession` is a Map, so `keys()` is INSERTION order —
    // oldest-opened first. Slicing from the front meant the sweep fetched
    // validators for the `sweepLimit` sessions opened longest ago and then
    // repaired only the active one, so a window that had opened more than
    // `sweepLimit` sessions never checked the tail it was actually rendering.
    // It polled 50 tails nobody was looking at and skipped the only one visible.
    const { engine, active, getValidators, getMessages } = setupBeyondTheLimit(3);
    await engine.sweepValidators();

    expect(getValidators.mock.calls[0]![0]).toContain(active);
    expect(getMessages).toHaveBeenCalledWith(active, expect.anything());
  });

  it('does not ask about the active session twice when it is already in the slice', async () => {
    // A small cache: the active session is inside the slice, so prepending it
    // would send a duplicate id.
    const store = createChatStore();
    const sid = 'br_only';
    store.getState().actions.setTurns(sid, modelWith(sid, { maxEventId: 5, eventCount: 5, updatedAt: '' }));
    store.getState().actions.setActive(sid);
    const getValidators = vi.fn(async () => ({ [sid]: { maxEventId: 5, eventCount: 5, updatedAt: '' } }));
    const api = { getValidators, getMessages: vi.fn() } as unknown as ApiClient;
    const cache = { isEnabled: false, putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}) } as unknown as SessionCache;
    const engine = new SyncEngine({ store, api, cache });

    await engine.sweepValidators();
    const asked = getValidators.mock.calls[0]![0] as string[];
    expect(asked.filter((id) => id === sid)).toHaveLength(1);
  });
});

describe('opening a session re-checks it, without waiting for the sweep', () => {
  /** A cached session whose server-side validator has moved on, plus a store
   *  with nothing selected yet. */
  function setupOpenable() {
    const store = createChatStore();
    const sid = 'br_cached';
    store.getState().actions.setTurns(sid, modelWith(sid, { maxEventId: 5, eventCount: 5, updatedAt: '' }));
    const getValidators = vi.fn(async () => ({ [sid]: { maxEventId: 9, eventCount: 9, updatedAt: '' } }));
    const getMessages = vi.fn(async () => ({
      model: modelWith(sid, { maxEventId: 9, eventCount: 9, updatedAt: '' }),
    }));
    const api = {
      getValidators,
      getMessages,
      // `start()` opens the list stream; a rejection here is caught by the engine.
      fetchFor: () => async () => {
        throw new Error('no stream in this test');
      },
      basePath: '',
    } as unknown as ApiClient;
    const cache = { isEnabled: false, putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}) } as unknown as SessionCache;
    const engine = new SyncEngine({ store, api, cache });
    return { store, engine, sid, getValidators, getMessages };
  }

  it('refetches a CACHED session whose tail has moved on', async () => {
    // ⚠️ The other half. `select()` fetches only when the store has NO tail — that
    // is what makes a warm switch land in single-digit milliseconds — so a cached
    // session rendered from a stale cache and stayed stale. The original chat has
    // no such gap: it calls `loadHistory(id)` on every open, cached or not.
    const { store, engine, sid, getMessages } = setupOpenable();
    engine.start();
    store.getState().actions.setActive(sid);
    await vi.waitFor(() => expect(getMessages).toHaveBeenCalledTimes(1));
    engine.stop();
  });

  it('leaves an UNCHANGED cached session alone — the cheap check is the whole cost', async () => {
    // The fast warm switch has to survive the fix. A session whose validator still
    // matches must cost one validator read and no tail.
    const store = createChatStore();
    const sid = 'br_fresh';
    const same = { maxEventId: 5, eventCount: 5, updatedAt: '' };
    store.getState().actions.setTurns(sid, modelWith(sid, same));
    const getValidators = vi.fn(async () => ({ [sid]: same }));
    const getMessages = vi.fn();
    const api = {
      getValidators,
      getMessages,
      fetchFor: () => async () => {
        throw new Error('no stream in this test');
      },
      basePath: '',
    } as unknown as ApiClient;
    const cache = { isEnabled: false, putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}) } as unknown as SessionCache;
    const engine = new SyncEngine({ store, api, cache });

    engine.start();
    store.getState().actions.setActive(sid);
    await vi.waitFor(() => expect(getValidators).toHaveBeenCalled());
    expect(getMessages).not.toHaveBeenCalled();
    engine.stop();
  });

  it('does not fetch for a session the store has never cached', async () => {
    // `select()` is already fetching that one; asking here too would race it and
    // double the cost of every cold open.
    const store = createChatStore();
    const getValidators = vi.fn();
    const api = {
      getValidators,
      getMessages: vi.fn(),
      fetchFor: () => async () => {
        throw new Error('no stream in this test');
      },
      basePath: '',
    } as unknown as ApiClient;
    const cache = { isEnabled: false, putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}) } as unknown as SessionCache;
    const engine = new SyncEngine({ store, api, cache });

    engine.start();
    store.getState().actions.setActive('br_never_seen');
    await new Promise((r) => setTimeout(r, 20));
    expect(getValidators).not.toHaveBeenCalled();
    engine.stop();
  });
});
