import { describe, expect, it } from 'vitest';
import { createChatStore, EMPTY_FILTER, type ContentHits } from '../src/store/ChatStore.js';
import { matchesFilter, selectFacets, visibleSessions } from '../src/store/selectors.js';
import type { SessionSummary } from '../src/net/types.js';

function summary(over: Partial<SessionSummary> & Pick<SessionSummary, 'sessionId'>): SessionSummary {
  return {
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'inst1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: over.sessionId,
    agentId: '',
    updatedAt: '2026-07-27T10:00:00-07:00',
    createdAt: '2026-07-27T10:00:00-07:00',
    ...over,
  };
}

const list = [
  summary({ sessionId: 'a', harness: 'claudecode', state: 'idle', instanceId: 'inst1', purpose: 'chat' }),
  summary({ sessionId: 'b', harness: 'codex', state: 'running', instanceId: 'inst2', purpose: 'autoworker' }),
  summary({ sessionId: 'c', harness: 'gemini', state: 'idle', instanceId: 'inst1', purpose: 'chat' }),
  summary({ sessionId: 'd', harness: 'codex', state: 'completed', instanceId: 'inst3', purpose: 'chat' }),
];

function seed(filter?: Partial<typeof EMPTY_FILTER>) {
  const store = createChatStore();
  store.getState().actions.setSessions(list);
  if (filter) store.getState().actions.setFilter(filter);
  return store;
}

function ids(store: ReturnType<typeof createChatStore>): string[] {
  return visibleSessions(store.getState())
    .flatMap((g) => g.sessions.map((s) => s.sessionId))
    .sort();
}

describe('matchesFilter — multi-select axes', () => {
  it('empty array on an axis matches everything', () => {
    for (const s of list) {
      expect(matchesFilter(s, { ...EMPTY_FILTER, harness: [] })).toBe(true);
    }
  });

  it('OR within an axis (any selected value matches)', () => {
    const f = { ...EMPTY_FILTER, harness: ['claudecode', 'gemini'] };
    expect(matchesFilter(list[0]!, f)).toBe(true); // claudecode
    expect(matchesFilter(list[2]!, f)).toBe(true); // gemini
    expect(matchesFilter(list[1]!, f)).toBe(false); // codex — not selected
  });

  it('AND across axes (every non-empty axis must match)', () => {
    // harness ∈ {codex} AND status ∈ {running}
    const f = { ...EMPTY_FILTER, harness: ['codex'], status: ['running'] };
    expect(matchesFilter(list[1]!, f)).toBe(true); // b: codex + running
    expect(matchesFilter(list[3]!, f)).toBe(false); // d: codex but completed
  });

  it('machine axis matches instanceId (the summary has no machine field)', () => {
    const f = { ...EMPTY_FILTER, machine: ['inst1'] };
    expect(matchesFilter(list[0]!, f)).toBe(true); // inst1
    expect(matchesFilter(list[2]!, f)).toBe(true); // inst1
    expect(matchesFilter(list[1]!, f)).toBe(false); // inst2
  });

  it('drives visibleSessions end-to-end (OR within, AND across)', () => {
    // harness ∈ {claudecode, gemini} AND status ∈ {idle} → a and c.
    const store = seed({ harness: ['claudecode', 'gemini'], status: ['idle'] });
    expect(ids(store)).toEqual(['a', 'c']);

    // machine ∈ {inst1, inst3} → a, c, d.
    const byMachine = seed({ machine: ['inst1', 'inst3'] });
    expect(ids(byMachine)).toEqual(['a', 'c', 'd']);

    // no filter → all four.
    const all = seed();
    expect(ids(all)).toEqual(['a', 'b', 'c', 'd']);
  });
});

describe('selectFacets — counts over the FULL loaded set', () => {
  it('tallies every axis independent of the active filter', () => {
    // Even with a filter that hides most rows, facets count the full set.
    const store = seed({ harness: ['codex'] });
    const facets = selectFacets(store.getState());
    expect(facets.harness).toEqual({ claudecode: 1, codex: 2, gemini: 1 });
    expect(facets.status).toEqual({ idle: 2, running: 1, completed: 1 });
    expect(facets.type).toEqual({ interactive: 4 });
    expect(facets.purpose).toEqual({ chat: 3, autoworker: 1 });
    expect(facets.mode).toEqual({ events: 4 });
    expect(facets.machine).toEqual({ inst1: 2, inst2: 1, inst3: 1 });
  });

  it('skips empty-string axis values (unfiled/unknown is not a facet)', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'x', harness: '', instanceId: '' }),
      summary({ sessionId: 'y', harness: 'codex', instanceId: 'inst9' }),
    ]);
    const facets = selectFacets(store.getState());
    expect(facets.harness).toEqual({ codex: 1 });
    expect(facets.machine).toEqual({ inst9: 1 });
  });

  it('memoizes on the sessions Map identity', () => {
    const store = seed();
    const first = selectFacets(store.getState());
    const second = selectFacets(store.getState());
    expect(second).toBe(first);
  });
});

// `matchesFilter` produces `false` from nine separate places, and a test that only asserts
// `toBe(false)` cannot say which one answered. Measured by deleting each producer in turn:
// five of the nine could be removed with the whole suite still green.
//
// Four of those five are the facet axes below. `filterPersistence.test.ts` does name
// type/purpose/mode — but it round-trips them through storage and never asks whether
// selecting one filters anything, so all three could stop filtering and only the
// persistence assertions would still hold. A facet that is stored and never applied is
// exactly the shape this block exists to pin.
//
// Each test asserts both directions on purpose. A one-sided `toBe(false)` here would be
// satisfied by any of the other eight producers and would pin nothing.

describe('matchesFilter — every facet axis actually filters', () => {
  it('purpose admits the selected value and rejects the rest', () => {
    const f = { ...EMPTY_FILTER, purpose: ['autoworker'] };
    expect(matchesFilter(summary({ sessionId: 'p', purpose: 'autoworker' }), f)).toBe(true);
    expect(matchesFilter(summary({ sessionId: 'q', purpose: 'chat' }), f)).toBe(false);
  });

  it('mode admits the selected value and rejects the rest', () => {
    const f = { ...EMPTY_FILTER, mode: ['pty'] };
    expect(matchesFilter(summary({ sessionId: 'p', mode: 'pty' }), f)).toBe(true);
    expect(matchesFilter(summary({ sessionId: 'q', mode: 'events' }), f)).toBe(false);
  });

  it('type admits the selected value and rejects the rest', () => {
    const f = { ...EMPTY_FILTER, type: ['interactive'] };
    expect(matchesFilter(summary({ sessionId: 'p', type: 'interactive' }), f)).toBe(true);
    expect(matchesFilter(summary({ sessionId: 'q', type: 'batch' }), f)).toBe(false);
  });
});

describe('matchesFilter — the default-hidden types are a rule, not a default', () => {
  // selectors.ts states this in prose — "the types in DEFAULT_HIDDEN_SESSION_TYPES are
  // still dropped. Select any type and the array rules alone" — and filterStorage.ts
  // repeats it. Both halves of the rule were unpinned: either could be deleted with the
  // suite green, so the prose was the only thing holding it.
  const external = summary({ sessionId: 'ext', type: 'external' });
  const interactive = summary({ sessionId: 'int', type: 'interactive' });

  it('hides an external session while no type is selected', () => {
    expect(matchesFilter(external, { ...EMPTY_FILTER, type: [] })).toBe(false);
    // ...and it is the type being hidden, not sessions in general.
    expect(matchesFilter(interactive, { ...EMPTY_FILTER, type: [] })).toBe(true);
  });

  it('shows it the moment that type is explicitly selected', () => {
    expect(matchesFilter(external, { ...EMPTY_FILTER, type: ['external'] })).toBe(true);
  });

  it('and the selection still rules out everything else', () => {
    expect(matchesFilter(interactive, { ...EMPTY_FILTER, type: ['external'] })).toBe(false);
  });
});

describe('matchesFilter — a spawned session is listed under its parent, not beside it', () => {
  const child = summary({ sessionId: 'child', purpose: 'subagent', managerSessionId: 'parent' });
  const parent = summary({ sessionId: 'parent', purpose: 'chat' });

  it('hides a session that names a manager while no purpose is selected', () => {
    expect(matchesFilter(child, EMPTY_FILTER)).toBe(false);
    expect(matchesFilter(parent, EMPTY_FILTER)).toBe(true);
  });

  it('lists it again once its purpose is selected', () => {
    expect(matchesFilter(child, { ...EMPTY_FILTER, purpose: ['subagent'] })).toBe(true);
  });

  it('keys on the manager, not on the purpose string', () => {
    // A subagent row whose parent link was never written is not reachable under
    // any parent, so hiding it would hide it everywhere.
    const unlinked = summary({ sessionId: 'orphan', purpose: 'subagent' });
    expect(matchesFilter(unlinked, EMPTY_FILTER)).toBe(true);
  });
});

describe('matchesFilter — a content hit belongs to the query that produced it', () => {
  const hitsFor = (query: string, sessionIds: string[]): ContentHits => ({
    query,
    matchCountBySessionId: new Map(sessionIds.map((id) => [id, 1])),
    hitCount: sessionIds.length,
    truncated: false,
  });
  // Neither the id nor the name contains the query, so the content-hit set is the only
  // thing that can make this session match.
  const contentOnly = summary({ sessionId: 'br_zzz', displayName: 'nothing alike' });

  it('counts a hit answering the query being asked', () => {
    const f = { ...EMPTY_FILTER, search: 'ceiling' };
    expect(matchesFilter(contentOnly, f, hitsFor('ceiling', ['br_zzz']))).toBe(true);
  });

  it('ignores a hit set left over from a different query', () => {
    // The stale-hit case, which is the ordinary one while someone is typing: the previous
    // query's results have arrived and the new query's have not. Without the equality
    // check the old hits answer the new question and a session matches a string its
    // transcript never contained.
    const f = { ...EMPTY_FILTER, search: 'ceiling' };
    expect(matchesFilter(contentOnly, f, hitsFor('budget', ['br_zzz']))).toBe(false);
  });
});

describe('the principal axis — who a session was started as', () => {
  const started = [
    summary({ sessionId: 'p1', principalId: 'principal_000001' }),
    summary({ sessionId: 'p2', principalId: 'principal_000002' }),
    summary({ sessionId: 'p3', principalId: '' }),
  ];
  it("matches the selected principals, and '' matches sessions started as no principal", () => {
    const f = { ...EMPTY_FILTER, principal: ['principal_000002', ''] };
    expect(started.map((s) => matchesFilter(s, f))).toEqual([false, true, true]);
  });
  it("counts every principal, '' included", () => {
    const store = createChatStore();
    store.getState().actions.setSessions(started);
    expect(selectFacets(store.getState()).principal).toEqual({ principal_000001: 1, principal_000002: 1, '': 1 });
  });
});
