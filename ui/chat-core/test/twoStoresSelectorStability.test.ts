import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import { selectContentSearchReach, selectFacets, visibleSessions } from '../src/store/selectors.js';
import type { SessionSummary } from '../src/net/types.js';

// Regression for the blank chat page: bridge-ui mounts a second chat-core store for
// each connected server, beside this server's own, and the sidebar reads both in one
// commit. `visibleSessions`, `selectContentSearchReach` and `selectFacets` memoized in
// one module-level slot, so the two stores' reads evicted each other, every read
// allocated, and `useSyncExternalStore` saw a new snapshot on every check — React's
// "Maximum update depth exceeded" on every load of the chat.

function summary(sessionId: string): SessionSummary {
  return {
    sessionId,
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'inst1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: sessionId,
    agentId: '',
    updatedAt: '2026-07-27T10:00:00-07:00',
    createdAt: '2026-07-27T10:00:00-07:00',
  };
}

function twoStores() {
  const thisServer = createChatStore();
  thisServer.getState().actions.setSessions([summary('a'), summary('b')]);
  const connectedServer = createChatStore();
  connectedServer.getState().actions.setSessions([summary('remote')]);
  return [thisServer, connectedServer] as const;
}

describe('selectors stay stable when two stores are read in turn', () => {
  it('visibleSessions returns the same groups for each unchanged store', () => {
    const [thisServer, connectedServer] = twoStores();
    const first = visibleSessions(thisServer.getState());
    const remote = visibleSessions(connectedServer.getState());
    expect(visibleSessions(thisServer.getState())).toBe(first);
    expect(visibleSessions(connectedServer.getState())).toBe(remote);
  });

  it('selectFacets returns the same facets for each unchanged store', () => {
    const [thisServer, connectedServer] = twoStores();
    const first = selectFacets(thisServer.getState());
    const remote = selectFacets(connectedServer.getState());
    expect(selectFacets(thisServer.getState())).toBe(first);
    expect(selectFacets(connectedServer.getState())).toBe(remote);
  });

  it('selectContentSearchReach returns the same answer for each unchanged store', () => {
    const [thisServer, connectedServer] = twoStores();
    thisServer.getState().actions.setFilter({ search: 'a' });
    thisServer.getState().actions.setContentHits('a', [{ sessionId: 'a', matchCount: 1 }]);
    connectedServer.getState().actions.setFilter({ search: 'r' });
    connectedServer.getState().actions.setContentHits('r', [{ sessionId: 'remote', matchCount: 1 }]);
    const first = selectContentSearchReach(thisServer.getState());
    const remote = selectContentSearchReach(connectedServer.getState());
    expect(first).not.toBeNull();
    expect(selectContentSearchReach(thisServer.getState())).toBe(first);
    expect(selectContentSearchReach(connectedServer.getState())).toBe(remote);
  });
});
