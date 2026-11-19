import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import { visibleSessions } from '../src/store/selectors.js';
import type { SearchHit, SessionSummary } from '../src/net/types.js';

// `ApiClient.search` sorts the backend's hits by descending `match_count` and the
// store used to convert them to a bare `Set<string>` one call later, so the whole
// ranking was gone before anything could read it. The sidebar then fell back to
// `byUpdatedDesc` and a session with one incidental mention outranked the session
// the query was about, purely for having been touched more recently. Each case
// below pins one tier of the ordering that replaced it.

function summary(
  over: Partial<SessionSummary> & Pick<SessionSummary, 'sessionId'>,
): SessionSummary {
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

function hit(sessionId: string, matchCount: number): SearchHit {
  return { sessionId, matchCount };
}

/** Rendered order, flattened across folder groups — what the sidebar paints. */
function order(store: ReturnType<typeof createChatStore>): string[] {
  return visibleSessions(store.getState()).flatMap((g) => g.sessions.map((s) => s.sessionId));
}

describe('search ranking — the match count must survive the store', () => {
  it('orders content-only hits by match count, not by recency', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      // The freshest session, and the one that barely mentions the query.
      summary({
        sessionId: 'incidental',
        displayName: 'Incidental',
        updatedAt: '2026-07-27T18:00:00-07:00',
      }),
      // Older, and the one the query is actually about.
      summary({
        sessionId: 'about-it',
        displayName: 'About it',
        updatedAt: '2026-07-27T09:00:00-07:00',
      }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', [hit('about-it', 84), hit('incidental', 1)]);
    expect(order(store)).toEqual(['about-it', 'incidental']);
  });

  it('puts an exact session-id paste at the very top, above every transcript that mentions it', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({
        sessionId: 'br_target',
        displayName: 'The session itself',
        updatedAt: '2026-07-01T00:00:00-07:00',
      }),
      summary({
        sessionId: 'br_chatter',
        displayName: 'Talks about it a lot',
        updatedAt: '2026-07-27T18:00:00-07:00',
      }),
    ]);
    store.getState().actions.setFilter({ search: 'br_target' });
    // The chatter session matches on content 200 times; the target matches on id.
    store
      .getState()
      .actions.setContentHits('br_target', [hit('br_chatter', 200), hit('br_target', 3)]);
    expect(order(store)).toEqual(['br_target', 'br_chatter']);
  });

  it('ranks a name match above a content-only match however small the name hit is', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({
        sessionId: 'named',
        displayName: 'kubernetes upgrade',
        updatedAt: '2026-07-01T00:00:00-07:00',
      }),
      summary({
        sessionId: 'content',
        displayName: 'Something else',
        updatedAt: '2026-07-27T18:00:00-07:00',
      }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', [hit('content', 99), hit('named', 1)]);
    expect(order(store)).toEqual(['named', 'content']);
  });

  it('falls back to recency when two hits matched the same number of times', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'older', displayName: 'Older', updatedAt: '2026-07-01T00:00:00-07:00' }),
      summary({ sessionId: 'newer', displayName: 'Newer', updatedAt: '2026-07-27T18:00:00-07:00' }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store.getState().actions.setContentHits('kubernetes', [hit('older', 7), hit('newer', 7)]);
    expect(order(store)).toEqual(['newer', 'older']);
  });

  // No query, no ranking: the sidebar's normal order is recency and must not pick
  // up whatever hit set happens to still be in the store.
  it('leaves the unsearched list newest-first', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      summary({ sessionId: 'older', updatedAt: '2026-07-01T00:00:00-07:00' }),
      summary({ sessionId: 'newer', updatedAt: '2026-07-27T18:00:00-07:00' }),
    ]);
    expect(order(store)).toEqual(['newer', 'older']);
  });

  // The ranking is applied inside each folder group, and the group order is the
  // server's. This is the chat page's deliberate difference from bridge-ui, which flattens
  // grouping entirely while a query is active — recorded here so a future flatten is
  // a red test rather than a silent behavior change.
  it('ranks within a folder group and leaves the group order to the server', () => {
    const store = createChatStore();
    store.getState().actions.setFolders(['zeta', 'alpha']);
    store.getState().actions.setSessions([
      summary({ sessionId: 'z-weak', folderName: 'zeta', displayName: 'Z weak' }),
      summary({ sessionId: 'z-strong', folderName: 'zeta', displayName: 'Z strong' }),
      summary({ sessionId: 'a-strongest', folderName: 'alpha', displayName: 'A strongest' }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store
      .getState()
      .actions.setContentHits('kubernetes', [
        hit('a-strongest', 500),
        hit('z-strong', 50),
        hit('z-weak', 1),
      ]);
    // `zeta` first because the SERVER's folder order says so, even though the single
    // best hit in the whole list is in `alpha`.
    expect(order(store)).toEqual(['z-strong', 'z-weak', 'a-strongest']);
  });

  // The folder groups a loaded session invents (rule 3 in `visibleSessions`) are
  // ordered by their newest session. That was read off `sessions[0]`, which is only
  // the newest while the group is sorted newest-first — a search-ranked group puts
  // the best hit there instead, so the group order silently followed the ranking.
  it('orders unknown folders by their newest session even when the rows are search-ranked', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([
      // `busy` is the newer folder, but its top-RANKED row is its oldest session —
      // the case that separates "newest in the group" from "whatever is at index 0".
      summary({ sessionId: 'busy-old-strong', folderName: 'busy', updatedAt: '2026-07-01T00:00:00-07:00' }),
      summary({ sessionId: 'busy-new-weak', folderName: 'busy', updatedAt: '2026-07-30T00:00:00-07:00' }),
      summary({ sessionId: 'quiet-mid', folderName: 'quiet', updatedAt: '2026-07-20T00:00:00-07:00' }),
    ]);
    store.getState().actions.setFilter({ search: 'kubernetes' });
    store
      .getState()
      .actions.setContentHits('kubernetes', [
        hit('busy-old-strong', 90),
        hit('quiet-mid', 50),
        hit('busy-new-weak', 1),
      ]);
    // `busy` holds the newest session on the board (07-30), so it leads — reading
    // the group's date off `sessions[0]` would take 07-01 instead and swap them.
    const folders = visibleSessions(store.getState()).map((g) => g.folder);
    expect(folders).toEqual(['busy', 'quiet']);
    expect(order(store)).toEqual(['busy-old-strong', 'busy-new-weak', 'quiet-mid']);
  });
});

describe('a pasted session id must match a NAMED session', () => {
  // The filter read `s.displayName || s.sessionId`, so the id was only searchable
  // on a session that had no display name — i.e. never, for the sessions anyone
  // pastes an id for. bridge-ui matches the raw id unconditionally.
  it('matches on session id even when the session has a display name', () => {
    const store = createChatStore();
    store
      .getState()
      .actions.setSessions([summary({ sessionId: 'br_9f2c', displayName: 'nightly worker' })]);
    store.getState().actions.setFilter({ search: 'br_9f2c' });
    expect(order(store)).toEqual(['br_9f2c']);
  });

  it('still matches on display name', () => {
    const store = createChatStore();
    store
      .getState()
      .actions.setSessions([summary({ sessionId: 'br_9f2c', displayName: 'nightly worker' })]);
    store.getState().actions.setFilter({ search: 'nightly' });
    expect(order(store)).toEqual(['br_9f2c']);
  });
});
