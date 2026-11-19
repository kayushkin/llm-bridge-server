import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import { selectContentSearchReach, visibleSessions, visibleCount } from '../src/store/selectors.js';
import type { SearchHit, SessionSummary } from '../src/net/types.js';

function summary(over: Partial<SessionSummary> & Pick<SessionSummary, 'sessionId'>): SessionSummary {
  return {
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'i',
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

/** A `SearchHit[]` from ids alone, for the cases below where only membership
 *  matters. Counts descend so the list arrives in the order `ApiClient.search`
 *  sorts it into; ranking itself is pinned in test/searchRanking.test.ts. */
function hitsOf(...sessionIds: string[]): SearchHit[] {
  return sessionIds.map((sessionId, i) => ({ sessionId, matchCount: sessionIds.length - i }));
}

function ids(store: ReturnType<typeof createChatStore>): string[] {
  return visibleSessions(store.getState())
    .flatMap((g) => g.sessions.map((s) => s.sessionId))
    .sort();
}

describe('content-search folding (C6)', () => {
  it('name match is instant and needs no content hits', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'a', displayName: 'Alpha deploy' }),
      summary({ sessionId: 'b', displayName: 'Beta' }),
    ]);
    store.getState().actions.setFilter({ search: 'alpha' });
    expect(ids(store)).toEqual(['a']);
  });

  it('folds content-hit ids in when the async search returns', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'a', displayName: 'Alpha' }),
      summary({ sessionId: 'b', displayName: 'Beta' }),
      summary({ sessionId: 'c', displayName: 'Gamma' }),
    ]);
    // A query that matches NO display name.
    store.getState().actions.setFilter({ search: 'kubernetes' });
    expect(visibleCount(store.getState())).toBe(0);

    // The async content search says b + c mention it in their transcripts.
    store.getState().actions.setContentHits('kubernetes', hitsOf('b', 'c'));
    expect(ids(store)).toEqual(['b', 'c']);
  });

  it('ignores a stale content-hit response for a different query', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a', displayName: 'Alpha' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('docker', hitsOf('a')); // query mismatch → ignored
    expect(visibleCount(store.getState())).toBe(0);
  });

  it('invalidates prior hits when the search query changes', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'b', displayName: 'Beta' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', hitsOf('b'));
    expect(ids(store)).toEqual(['b']);
    // Change the query — the stale 'kubernetes' hits must not leak into 'docker'.
    store.getState().actions.setFilter({ search: 'docker' });
    expect(visibleCount(store.getState())).toBe(0);
  });

  // The comparison between `filter.search` (raw, as typed) and `contentHits.query`
  // (trimmed by the caller) used to be made without trimming, so every hit for a
  // query with a stray space was discarded on arrival and search silently matched
  // nothing. A trailing space is one keystroke away in normal typing.
  it('keeps hits for a query the user typed with surrounding whitespace', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'a', displayName: 'Alpha' }),
      summary({ sessionId: 'b', displayName: 'Beta' }),
    ]);
    store.getState().actions.setFilter({ search: '  kubernetes  ' });
    store.getState().actions.setContentHits('kubernetes', hitsOf('b'));
    expect(ids(store)).toEqual(['b']);
  });

  it('matches a display name for a query typed with surrounding whitespace', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a', displayName: 'Alpha' })]);
    store.getState().actions.setFilter({ search: ' alpha ' });
    expect(ids(store)).toEqual(['a']);
  });
});

describe('content-search reach — the shortfall must be reportable', () => {
  it('is null when no content search is active', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a' })]);
    expect(selectContentSearchReach(store.getState())).toBeNull();
  });

  it('is null while the hits for the live query have not landed', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    expect(selectContentSearchReach(store.getState())).toBeNull();
  });

  it('counts hits the loaded window cannot paint as hidden', () => {
    const store = createChatStore();
    // Only `b` is loaded; the backend matched three sessions.
    store.getState().actions.setSessions([summary({ sessionId: 'b', displayName: 'Beta' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', hitsOf('b', 'offscreen1', 'offscreen2'));
    const reach = selectContentSearchReach(store.getState());
    expect(reach).not.toBeNull();
    expect(reach!.hitCount).toBe(3);
    expect(reach!.shownHitCount).toBe(1);
    expect(reach!.hiddenHitCount).toBe(2);
    expect(reach!.truncated).toBe(false);
  });

  it('reports nothing hidden when every hit is loaded', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'a', displayName: 'Alpha' }),
      summary({ sessionId: 'b', displayName: 'Beta' }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', hitsOf('a', 'b'));
    const reach = selectContentSearchReach(store.getState());
    expect(reach!.hitCount).toBe(2);
    expect(reach!.shownHitCount).toBe(2);
    expect(reach!.hiddenHitCount).toBe(0);
  });

  // A hit that IS loaded but is filtered out by another axis is honestly hidden:
  // counting against `state.sessions` instead of the rendered rows would claim it
  // was on screen when the user cannot see it.
  it('counts a loaded hit removed by another filter axis as hidden', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'a', displayName: 'Alpha', harness: 'claudecode' }),
      summary({ sessionId: 'b', displayName: 'Beta', harness: 'codex' }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes', harness: ['claudecode'] });
    store.getState().actions.setContentHits('kubernetes', hitsOf('a', 'b'));
    const reach = selectContentSearchReach(store.getState());
    expect(reach!.hitCount).toBe(2);
    expect(reach!.shownHitCount).toBe(1);
    expect(reach!.hiddenHitCount).toBe(1);
  });

  it('carries the backend truncation flag through', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a', displayName: 'Alpha' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', hitsOf('a'), true);
    expect(selectContentSearchReach(store.getState())!.truncated).toBe(true);
  });
});

// The in-flight marker is deliberately its own state and not derived from
// `contentHits === null`. Hits are null in three different situations — before
// anyone typed, while a search is out, and after one failed — and only one of them
// means "searching". Every case below pins one of the three.
describe('content-search in-flight state', () => {
  it('is idle before anything is typed', () => {
    const store = createChatStore();
    expect(store.getState().contentSearchInFlight).toBeNull();
    expect(store.getState().contentSearchError).toBeNull();
  });

  it('reports the query it is running for, and stops when its hits land', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.startContentSearch('kubernetes');
    expect(store.getState().contentSearchInFlight).toBe('kubernetes');

    store.getState().actions.setContentHits('kubernetes', hitsOf('a'));
    expect(store.getState().contentSearchInFlight).toBeNull();
  });

  it('keeps the newer search running when an older one lands late', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a' })]);
    // 'kube' goes out, then the user finishes typing and 'kubernetes' goes out.
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.startContentSearch('kube');
    store.getState().actions.startContentSearch('kubernetes');
    // Now 'kube' answers. Its hits are dropped (query mismatch) and — the point of
    // this case — it must not report the live search as finished either.
    store.getState().actions.setContentHits('kube', hitsOf('a'));
    expect(store.getState().contentSearchInFlight).toBe('kubernetes');
  });

  it('keeps the newer search running when an older one FAILS late', () => {
    const store = createChatStore();
    store.getState().actions.startContentSearch('kube');
    store.getState().actions.startContentSearch('kubernetes');
    store.getState().actions.endContentSearch('kube', 'search failed: 502');
    expect(store.getState().contentSearchInFlight).toBe('kubernetes');
    expect(store.getState().contentSearchError).toBeNull();
  });

  it('stops searching and records why when the search fails', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a', displayName: 'Alpha' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.startContentSearch('kubernetes');
    store.getState().actions.endContentSearch('kubernetes', 'search failed: 502');

    expect(store.getState().contentSearchInFlight).toBeNull();
    expect(store.getState().contentSearchError).toBe('search failed: 502');
    // A failed transcript search must NOT masquerade as an empty one: hits stay
    // null rather than becoming an empty set, so nothing claims the server looked
    // and found nothing.
    expect(store.getState().contentHits).toBeNull();
    expect(selectContentSearchReach(store.getState())).toBeNull();
  });

  it('cancels without an error when a scheduled search never fires', () => {
    const store = createChatStore();
    store.getState().actions.startContentSearch('kubernetes');
    store.getState().actions.endContentSearch('kubernetes', null);
    expect(store.getState().contentSearchInFlight).toBeNull();
    expect(store.getState().contentSearchError).toBeNull();
  });

  it('clears a previous failure when the next search starts', () => {
    const store = createChatStore();
    store.getState().actions.startContentSearch('kube');
    store.getState().actions.endContentSearch('kube', 'search failed: 502');
    expect(store.getState().contentSearchError).toBe('search failed: 502');

    store.getState().actions.startContentSearch('kubernetes');
    expect(store.getState().contentSearchError).toBeNull();
  });

  // The narrow race `startContentSearch` does NOT cover, found by a sabotage round
  // that came back green: one query can have two requests out at once, because a
  // trailing space re-fires it under the same trimmed query. If the first answer is
  // a failure and the second is hits, only `setContentHits` can take the failure
  // notice back down — otherwise the sidebar shows results and "search failed" at
  // the same time.
  it('takes the failure notice down when a second request for the SAME query succeeds', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    // Two requests for 'kubernetes' are out; only one startContentSearch precedes
    // both answers, so nothing re-clears the error between them.
    store.getState().actions.startContentSearch('kubernetes');
    store.getState().actions.endContentSearch('kubernetes', 'search failed: 502');
    store.getState().actions.setContentHits('kubernetes', hitsOf('a'));
    expect(store.getState().contentSearchError).toBeNull();
    expect(ids(store)).toEqual(['a']);
  });

  it('clears a previous failure when a later search succeeds', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 'a' })]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.startContentSearch('kubernetes');
    store.getState().actions.endContentSearch('kubernetes', 'search failed: 502');
    // A retry for the same query answers.
    store.getState().actions.startContentSearch('kubernetes');
    store.getState().actions.setContentHits('kubernetes', hitsOf('a'));
    expect(store.getState().contentSearchError).toBeNull();
    expect(store.getState().contentSearchInFlight).toBeNull();
  });
});
