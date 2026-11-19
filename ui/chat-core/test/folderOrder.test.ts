import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore } from '../src/store/ChatStore.js';
import { visibleSessions } from '../src/store/selectors.js';
import type { SessionSummary } from '../src/net/types.js';

// The gateway serves an EXPLICIT, ORDERED folder list (`GET /folders` →
// `{folder_order: [...]}`) and folders are rows that exist with no sessions in
// them. chat-core used to throw both facts away: groups were ordered by their
// newest session, and a folder nobody had filed anything into was unrepresentable.
//
// The controls below are as load-bearing as the failing cases. Two of them pin
// behavior that must SURVIVE the fix (newest-first within a group; a folder the
// server has not listed still groups its sessions), and one pins the degraded
// path (no folder list → the old recency order, not an empty sidebar).

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
    updatedAt: '2026-08-02T10:00:00-07:00',
    createdAt: '2026-08-02T10:00:00-07:00',
    ...over,
  };
}

function seed(list: SessionSummary[], folders?: string[]) {
  const store = createChatStore();
  store.getState().actions.setSessions(list);
  if (folders) store.getState().actions.setFolders(folders);
  return store;
}

describe('folder groups follow the server order', () => {
  it('orders groups by the folder list, not by their newest session', () => {
    // Recency order here is Kanban (12:00) → work (11:00) → Archive (09:00);
    // the server's order is the reverse. The server wins.
    const store = seed(
      [
        summary({ sessionId: 'a', folderName: 'Archive', updatedAt: '2026-08-02T09:00:00-07:00' }),
        summary({ sessionId: 'b', folderName: 'work', updatedAt: '2026-08-02T11:00:00-07:00' }),
        summary({ sessionId: 'c', folderName: 'Kanban', updatedAt: '2026-08-02T12:00:00-07:00' }),
      ],
      ['Archive', 'work', 'Kanban'],
    );
    expect(visibleSessions(store.getState()).map((g) => g.folder)).toEqual([
      'Archive',
      'work',
      'Kanban',
    ]);
  });

  it('emits a group for a folder that holds no sessions at all', () => {
    const store = seed(
      [summary({ sessionId: 'a', folderName: 'Archive' })],
      ['Archive', 'Conformance', 'Subagents'],
    );
    const groups = visibleSessions(store.getState());
    expect(groups.map((g) => g.folder)).toEqual(['Archive', 'Conformance', 'Subagents']);
    expect(groups[1]!.sessions).toEqual([]);
    expect(groups[2]!.sessions).toEqual([]);
  });

  it('emits a group for a folder every session of which the filter hid', () => {
    const store = seed(
      [
        summary({ sessionId: 'a', folderName: 'Archive', harness: 'claudecode' }),
        summary({ sessionId: 'b', folderName: 'work', harness: 'codex' }),
      ],
      ['Archive', 'work'],
    );
    store.getState().actions.setFilter({ harness: ['claudecode'] });
    const groups = visibleSessions(store.getState());
    expect(groups.map((g) => g.folder)).toEqual(['Archive', 'work']);
    expect(groups[1]!.sessions).toEqual([]);
  });

  it('puts the unfoldered bucket first, ahead of every named folder', () => {
    // The unfoldered session is the OLDEST here, so recency ordering would put it
    // last. bridge-ui renders unfiled sessions above the folders and so does this.
    const store = seed(
      [
        summary({ sessionId: 'a', folderName: '', updatedAt: '2026-08-02T08:00:00-07:00' }),
        summary({ sessionId: 'b', folderName: 'Archive', updatedAt: '2026-08-02T11:00:00-07:00' }),
      ],
      ['Archive'],
    );
    expect(visibleSessions(store.getState()).map((g) => g.folder)).toEqual(['', 'Archive']);
  });

  it('omits the unfoldered bucket when nothing is unfoldered', () => {
    // CONTROL for the rule above. An empty NAMED folder is a server row and renders;
    // an empty unfoldered bucket is the absence of a folder and must not render, or
    // every sidebar grows a permanent empty "active" header.
    const store = seed([summary({ sessionId: 'a', folderName: 'Archive' })], ['Archive']);
    expect(visibleSessions(store.getState()).map((g) => g.folder)).toEqual(['Archive']);
  });

  it('CONTROL: still groups a session whose folder the server has not listed', () => {
    // The summary row's `folderName` is itself authoritative. A folder created since
    // the last `/folders` read must not swallow its sessions into the unfoldered
    // bucket — they group under their own name, after the known folders.
    const store = seed(
      [
        summary({ sessionId: 'a', folderName: 'Archive' }),
        summary({ sessionId: 'b', folderName: 'brand-new' }),
      ],
      ['Archive'],
    );
    const groups = visibleSessions(store.getState());
    expect(groups.map((g) => g.folder)).toEqual(['Archive', 'brand-new']);
    expect(groups[1]!.sessions.map((s) => s.sessionId)).toEqual(['b']);
  });

  it('CONTROL: with no folder list loaded, groups stay in recency order', () => {
    // The degraded path — `/folders` failed or has not landed. Grouping must fall
    // back to what it did before, never to an empty sidebar.
    const store = seed([
      summary({ sessionId: 'a', folderName: 'work', updatedAt: '2026-08-02T09:00:00-07:00' }),
      summary({ sessionId: 'b', folderName: 'Kanban', updatedAt: '2026-08-02T12:00:00-07:00' }),
    ]);
    expect(visibleSessions(store.getState()).map((g) => g.folder)).toEqual(['Kanban', 'work']);
  });

  it('CONTROL: sessions inside a group stay newest-first', () => {
    const store = seed(
      [
        summary({ sessionId: 'a', folderName: 'work', updatedAt: '2026-08-02T09:00:00-07:00' }),
        summary({ sessionId: 'b', folderName: 'work', updatedAt: '2026-08-02T11:00:00-07:00' }),
      ],
      ['work'],
    );
    const work = visibleSessions(store.getState())[0]!;
    expect(work.sessions.map((s) => s.sessionId)).toEqual(['b', 'a']);
  });

  it('repaints when the folder list changes', () => {
    // visibleSessions is memoized on input identity. The folder list is a new input
    // and must be part of that key, or the first `/folders` response after boot
    // paints nothing.
    const store = seed([summary({ sessionId: 'a', folderName: 'work' })], ['work']);
    const before = visibleSessions(store.getState());
    store.getState().actions.setFolders(['work', 'Archive']);
    const after = visibleSessions(store.getState());
    expect(before.map((g) => g.folder)).toEqual(['work']);
    expect(after.map((g) => g.folder)).toEqual(['work', 'Archive']);
  });
});

describe('ApiClient.listFolders', () => {
  function fakeFetch(body: unknown, record?: (url: string) => void): typeof fetch {
    return (async (url: string) => {
      record?.(String(url));
      return {
        ok: true,
        status: 200,
        statusText: 'OK',
        json: async () => body,
        text: async () => '',
      } as unknown as Response;
    }) as unknown as typeof fetch;
  }

  it('GETs /folders and returns folder_order in the order the server gave it', async () => {
    const seen: string[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ folder_order: ['Scheduled', 'Archive', 'Kanban'] }, (u) => seen.push(u)),
      basePath: '/api/bridge',
    });
    await expect(api.listFolders()).resolves.toEqual(['Scheduled', 'Archive', 'Kanban']);
    expect(seen[0]).toBe('/api/bridge/folders');
  });

  it('reads a missing folder_order as no folders, not as a crash', async () => {
    // A nil Go slice serializes to JSON `null`. The handler coerces it to `[]` today,
    // but the client must not depend on that to avoid throwing on `.map`.
    const api = new ApiClient({
      fetch: fakeFetch({ folder_order: null }),
      basePath: '/api/bridge',
    });
    await expect(api.listFolders()).resolves.toEqual([]);
  });
});
