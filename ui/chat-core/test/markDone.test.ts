import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore } from '../src/store/ChatStore.js';
import { ARCHIVE_FOLDER, setSessionDone } from '../src/store/markDone.js';
import type { SessionSummary } from '../src/net/types.js';

interface Recorded {
  url: string;
  method?: string;
  body?: string;
}

function fakeApi(
  res: { ok: boolean; status: number; statusText?: string },
  seen?: Recorded[],
): ApiClient {
  const fetchFn = (async (url: string, init?: RequestInit) => {
    seen?.push({ url: String(url), method: init?.method, body: init?.body as string | undefined });
    return {
      ok: res.ok,
      status: res.status,
      statusText: res.statusText ?? '',
      json: async () => ({}),
      text: async () => '',
    } as unknown as Response;
  }) as unknown as typeof fetch;
  return new ApiClient({ fetch: fetchFn, basePath: '/api/bridge' });
}

function row(over: Partial<SessionSummary> = {}): SessionSummary {
  return {
    sessionId: 'br_1',
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'inst1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: 'a session',
    agentId: '',
    updatedAt: '2026-08-02T10:00:00-07:00',
    createdAt: '2026-08-02T10:00:00-07:00',
    ...over,
  };
}

/**
 * The defect this file pins.
 *
 * chat-core used to archive with `POST /sessions/{id}/archive` and unarchive with
 * `POST /sessions/{id}/unarchive`. NEITHER ROUTE HAS EVER EXISTED on the gateway —
 * `internal/server/server.go` registers `PUT /sessions/{id}/folder` and
 * `POST /sessions/{id}/mark-done` and nothing else in this area, and a grep for
 * "unarchive" across every *.go file in ~/repos returns nothing at all.
 *
 * Probed against the live gateway on 2026-08-02, same session id for all three:
 *
 *     POST /sessions/{id}/archive     -> 404
 *     POST /sessions/{id}/unarchive   -> 404
 *     POST /sessions/{id}/mark-done   -> 204
 *
 * The 404s are route-level, not session-level: mark-done answered 204 for the very
 * same id. Because the caller swallowed the rejection, the chat page's archive button
 * optimistically flipped the row, 404'd, and silently reverted.
 */
describe('ApiClient.markSessionDone — the route that actually exists', () => {
  it('POSTs /sessions/{id}/mark-done with {done:true}', async () => {
    const seen: Recorded[] = [];
    const api = fakeApi({ ok: true, status: 204 }, seen);
    await api.markSessionDone('br_1', true);
    expect(seen[0].url).toBe('/api/bridge/sessions/br_1/mark-done');
    expect(seen[0].method).toBe('POST');
    expect(JSON.parse(seen[0].body ?? '{}')).toEqual({ done: true });
  });

  it('POSTs {done:false} to the SAME route to undo — there is no separate unarchive route', async () => {
    const seen: Recorded[] = [];
    const api = fakeApi({ ok: true, status: 204 }, seen);
    await api.markSessionDone('br_1', false);
    expect(seen[0].url).toBe('/api/bridge/sessions/br_1/mark-done');
    expect(JSON.parse(seen[0].body ?? '{}')).toEqual({ done: false });
  });

  it('rejects on a non-2xx instead of resolving', async () => {
    const api = fakeApi({ ok: false, status: 404, statusText: 'Not Found' });
    await expect(api.markSessionDone('br_1', true)).rejects.toThrow(/404/);
  });
});

describe('setSessionDone — the optimistic row must match what the server does', () => {
  it('sets BOTH halves the server sets: state=completed and the canonical Archive folder', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    store.getState().actions.upsertSession(row());

    await setSessionDone({ store, api: fakeApi({ ok: true, status: 204 }, seen) }, 'br_1', true);

    const after = store.getState().sessions.get('br_1');
    // handleMarkSessionDone broadcasts SessionCompleted AND moves the folder — the two
    // halves are one atomic action server-side, so an optimistic row that moves the
    // folder alone renders a state the server will never agree with.
    expect(after?.state).toBe('completed');
    expect(after?.folderName).toBe(ARCHIVE_FOLDER);
  });

  it('uses "Archive", the constant the server writes — not lowercase "archive"', async () => {
    const store = createChatStore();
    store.getState().actions.upsertSession(row());
    await setSessionDone({ store, api: fakeApi({ ok: true, status: 204 }) }, 'br_1', true);

    // store.ArchiveFolder is "Archive" (llm-bridge-server/internal/store/store.go).
    // chat-core used to write lowercase 'archive', which is why the chat page's row had to test
    // both spellings to decide whether a session was archived.
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Archive');
    expect(ARCHIVE_FOLDER).toBe('Archive');
  });

  it('undo restores state=idle and clears the folder, mirroring the server', async () => {
    const store = createChatStore();
    store.getState().actions.upsertSession(row({ state: 'completed', folderName: 'Archive' }));

    await setSessionDone({ store, api: fakeApi({ ok: true, status: 204 }) }, 'br_1', false);

    const after = store.getState().sessions.get('br_1');
    expect(after?.state).toBe('idle');
    expect(after?.folderName).toBe('');
  });

  it('reverts the row AND rethrows when the server refuses', async () => {
    const store = createChatStore();
    store.getState().actions.upsertSession(row());

    await expect(
      setSessionDone({ store, api: fakeApi({ ok: false, status: 404, statusText: 'Not Found' }) }, 'br_1', true),
    ).rejects.toThrow(/404/);

    // The revert is not the whole fix: it is what USED to happen, silently, on every
    // single click. It must still happen, but the throw is what makes it visible.
    const after = store.getState().sessions.get('br_1');
    expect(after?.state).toBe('idle');
    expect(after?.folderName).toBe('');
  });

  it('is a no-op for a session the store does not hold', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    await setSessionDone({ store, api: fakeApi({ ok: true, status: 204 }, seen) }, 'nope', true);
    expect(seen).toHaveLength(0);
  });
});

describe('regression guard: no caller may reach for the routes that do not exist', () => {
  it('never requests /archive or /unarchive', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    const api = fakeApi({ ok: true, status: 204 }, seen);
    store.getState().actions.upsertSession(row());

    await setSessionDone({ store, api }, 'br_1', true);
    await setSessionDone({ store, api }, 'br_1', false);

    for (const r of seen) {
      expect(r.url).not.toMatch(/\/(un)?archive$/);
    }
    expect(seen).toHaveLength(2);
  });
});
