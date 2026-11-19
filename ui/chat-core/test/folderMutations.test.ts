import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore } from '../src/store/ChatStore.js';
import {
  createFolder,
  deleteFolder,
  moveSessionToFolder,
  renameFolder,
} from '../src/store/folders.js';
import { setSessionDone } from '../src/store/markDone.js';
import { visibleSessions } from '../src/store/selectors.js';
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

const OK = { ok: true, status: 204 };
const REFUSED = { ok: false, status: 500, statusText: 'Internal Server Error' };

describe('ApiClient — the four folder routes the gateway registers', () => {
  it('POSTs /folders with {name} to create', async () => {
    const seen: Recorded[] = [];
    await fakeApi(OK, seen).createFolder('Work');
    expect(seen[0].url).toBe('/api/bridge/folders');
    expect(seen[0].method).toBe('POST');
    expect(JSON.parse(seen[0].body ?? '{}')).toEqual({ name: 'Work' });
  });

  it('DELETEs /folders/{name}, url-encoding a name with a slash in it', async () => {
    const seen: Recorded[] = [];
    await fakeApi(OK, seen).deleteFolder('a/b');
    expect(seen[0].url).toBe('/api/bridge/folders/a%2Fb');
    expect(seen[0].method).toBe('DELETE');
    // The route is `DELETE /folders/{name}`; an unencoded slash would address
    // `/folders/a/b`, which the mux does not register at all.
    expect(seen[0].body).toBeUndefined();
  });

  it('PUTs /folders/{name} with the snake_case {new_name} the Go struct declares', async () => {
    const seen: Recorded[] = [];
    await fakeApi(OK, seen).renameFolder('Work', 'Archive');
    expect(seen[0].url).toBe('/api/bridge/folders/Work');
    expect(seen[0].method).toBe('PUT');
    // msg.RenameFolderRequest is `NewName string \`json:"new_name"\`` — a camelCase
    // key decodes to "" and the handler answers 400.
    expect(JSON.parse(seen[0].body ?? '{}')).toEqual({ new_name: 'Archive' });
  });

  it('PUTs /sessions/{id}/folder with {folder} to file a session', async () => {
    const seen: Recorded[] = [];
    await fakeApi(OK, seen).setSessionFolder('br_1', 'Work');
    expect(seen[0].url).toBe('/api/bridge/sessions/br_1/folder');
    expect(seen[0].method).toBe('PUT');
    expect(JSON.parse(seen[0].body ?? '{}')).toEqual({ folder: 'Work' });
  });

  it('rejects loudly on a non-2xx — including DELETE, which returns no body', async () => {
    const api = fakeApi({ ok: false, status: 404, statusText: 'Not Found' });
    await expect(api.createFolder('Work')).rejects.toThrow(/404/);
    await expect(api.deleteFolder('Work')).rejects.toThrow(/DELETE .*404/);
    await expect(api.renameFolder('Work', 'Other')).rejects.toThrow(/404/);
    await expect(api.setSessionFolder('br_1', 'Work')).rejects.toThrow(/404/);
  });
});

describe('createFolder', () => {
  it('appends — a new folder goes to the END, where MAX(position)+1 puts it', async () => {
    const store = createChatStore();
    store.getState().actions.setFolders(['Work', 'Reading']);
    await createFolder({ store, api: fakeApi(OK) }, 'Ops');
    expect(store.getState().folders).toEqual(['Work', 'Reading', 'Ops']);
  });

  it('trims, and refuses a blank name without a request', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    const api = fakeApi(OK, seen);
    await createFolder({ store, api }, '   ');
    expect(seen).toHaveLength(0);
    await createFolder({ store, api }, '  Ops  ');
    expect(store.getState().folders).toEqual(['Ops']);
    expect(JSON.parse(seen[0].body ?? '{}')).toEqual({ name: 'Ops' });
  });

  it('still sends a name the list already holds, but does not list it twice', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    store.getState().actions.setFolders(['Work']);
    await createFolder({ store, api: fakeApi(OK, seen) }, 'Work');
    // The local list can be stale; the server INSERT is ON CONFLICT DO NOTHING.
    expect(seen).toHaveLength(1);
    expect(store.getState().folders).toEqual(['Work']);
  });

  it('reverts the list and rethrows when the server refuses', async () => {
    const store = createChatStore();
    store.getState().actions.setFolders(['Work']);
    await expect(createFolder({ store, api: fakeApi(REFUSED) }, 'Ops')).rejects.toThrow(/500/);
    expect(store.getState().folders).toEqual(['Work']);
  });
});

describe('deleteFolder — the second half of the transaction is the one that matters', () => {
  it('un-files every session in the folder, exactly as the UPDATE before the DELETE does', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work', 'Reading']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: 'Work' }));
    actions.upsertSession(row({ sessionId: 'br_2', folderName: 'Work' }));
    actions.upsertSession(row({ sessionId: 'br_3', folderName: 'Reading' }));

    await deleteFolder({ store, api: fakeApi(OK) }, 'Work');

    expect(store.getState().folders).toEqual(['Reading']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('');
    expect(store.getState().sessions.get('br_2')?.folderName).toBe('');
    // A session in a different folder is untouched.
    expect(store.getState().sessions.get('br_3')?.folderName).toBe('Reading');
  });

  it('leaves no group behind for the deleted folder', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: 'Work' }));

    await deleteFolder({ store, api: fakeApi(OK) }, 'Work');

    // Dropping the header alone would leave br_1 still claiming "Work", and
    // visibleSessions draws a session-claimed folder as a trailing group of its own —
    // a folder that outlives its own deletion.
    const groups = visibleSessions(store.getState());
    expect(groups.map((g) => g.folder)).toEqual(['']);
    expect(groups[0].sessions.map((s) => s.sessionId)).toEqual(['br_1']);
  });

  it('refuses a blank name without a request, and still sends a real one', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: '' }));
    const api = fakeApi(OK, seen);

    // Without the guard this is `DELETE /folders/` — a request aimed at no folder —
    // and sessionsInFolder('') then matches every UN-FILED row, so the revert path
    // would be carrying the whole no-folder bucket. Neither shows up in the store,
    // because un-filing an un-filed session changes nothing: the REQUEST is the only
    // observable, which is why this asserts on `seen` and not on the state.
    await deleteFolder({ store, api }, '');
    expect(seen).toHaveLength(0);

    // A usable call AFTER the refused one, so an api that records nothing at all
    // cannot pass the assertion above.
    await deleteFolder({ store, api }, 'Work');
    expect(seen).toHaveLength(1);
    expect(seen[0].method).toBe('DELETE');
  });

  it('restores both the list and every moved row when the server refuses', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work', 'Reading']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: 'Work' }));

    await expect(deleteFolder({ store, api: fakeApi(REFUSED) }, 'Work')).rejects.toThrow(/500/);

    expect(store.getState().folders).toEqual(['Work', 'Reading']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Work');
  });
});

describe('renameFolder', () => {
  it('keeps the folder in place when the new name is free — the row is UPDATEd, not moved', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work', 'Reading', 'Ops']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: 'Reading' }));

    await renameFolder({ store, api: fakeApi(OK) }, 'Reading', 'Papers');

    expect(store.getState().folders).toEqual(['Work', 'Papers', 'Ops']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Papers');
  });

  it('MERGES when the new name already exists: the old row goes, its sessions join', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work', 'Reading', 'Ops']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: 'Reading' }));
    actions.upsertSession(row({ sessionId: 'br_2', folderName: 'Ops' }));

    await renameFolder({ store, api: fakeApi(OK) }, 'Reading', 'Ops');

    // Store.RenameFolder DELETEs the old row when the target exists, so "Ops" keeps
    // its own position rather than inheriting "Reading"'s.
    expect(store.getState().folders).toEqual(['Work', 'Ops']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Ops');
    expect(store.getState().sessions.get('br_2')?.folderName).toBe('Ops');
  });

  it('sends nothing for the three cases RenameFolder itself treats as a no-op', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    const api = fakeApi(OK, seen);
    await renameFolder({ store, api }, '', 'Ops');
    await renameFolder({ store, api }, 'Work', '   ');
    await renameFolder({ store, api }, 'Work', 'Work');
    expect(seen).toHaveLength(0);
  });

  it('restores both halves when the server refuses', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work', 'Reading']);
    actions.upsertSession(row({ sessionId: 'br_1', folderName: 'Reading' }));

    await expect(
      renameFolder({ store, api: fakeApi(REFUSED) }, 'Reading', 'Papers'),
    ).rejects.toThrow(/500/);

    expect(store.getState().folders).toEqual(['Work', 'Reading']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Reading');
  });
});

describe('moveSessionToFolder', () => {
  it('files the session and registers a folder the list did not have', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row());

    await moveSessionToFolder({ store, api: fakeApi(OK, seen) }, 'br_1', 'Ops');

    // ONE call does both: Store.SetSessionFolder INSERTs the folder before the UPDATE,
    // so the menu never needs a create followed by a move.
    expect(seen).toHaveLength(1);
    expect(store.getState().folders).toEqual(['Work', 'Ops']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Ops');
  });

  it('un-files on an empty folder, and does not add "" to the folder list', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row({ folderName: 'Work' }));

    await moveSessionToFolder({ store, api: fakeApi(OK) }, 'br_1', '');

    expect(store.getState().sessions.get('br_1')?.folderName).toBe('');
    // The empty bucket is the ABSENCE of a folder, never a row.
    expect(store.getState().folders).toEqual(['Work']);
  });

  it('is a no-op for a session the store does not hold', async () => {
    const seen: Recorded[] = [];
    const store = createChatStore();
    await moveSessionToFolder({ store, api: fakeApi(OK, seen) }, 'nope', 'Work');
    expect(seen).toHaveLength(0);
    expect(store.getState().folders).toEqual([]);
  });

  it('does not re-list a folder the list already holds', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work', 'Ops']);
    actions.upsertSession(row({ folderName: 'Work' }));

    await moveSessionToFolder({ store, api: fakeApi(OK) }, 'br_1', 'Ops');

    // SetSessionFolder's INSERT is ON CONFLICT DO NOTHING, so the mirror must be too.
    // Appending unconditionally gives the sidebar two headers reading "Ops", and
    // visibleSessions groups by name — so the second one is always empty.
    expect(store.getState().folders).toEqual(['Work', 'Ops']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Ops');
  });

  it('restores the row AND the folder it had auto-created when the server refuses', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row({ folderName: 'Work' }));

    await expect(
      moveSessionToFolder({ store, api: fakeApi(REFUSED) }, 'br_1', 'Ops'),
    ).rejects.toThrow(/500/);

    // A refused move must not leave the invented folder behind as an empty group.
    expect(store.getState().folders).toEqual(['Work']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('Work');
  });
});

describe('setSessionDone registers Archive too — same INSERT, same mirror', () => {
  it('adds Archive to the folder list the first time a session is marked done', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row());

    await setSessionDone({ store, api: fakeApi(OK) }, 'br_1', true);

    // handleMarkSessionDone finishes through Store.SetSessionFolder, which INSERTs the
    // folder. Without the mirror the row lands in a folder the group order does not
    // know, and it paints as a trailing session-derived group instead of in place.
    expect(store.getState().folders).toEqual(['Work', 'Archive']);
    const groups = visibleSessions(store.getState());
    expect(groups.map((g) => g.folder)).toEqual(['Work', 'Archive']);
  });

  it('un-marking leaves the Archive folder standing — the server never deletes it', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Archive']);
    actions.upsertSession(row({ state: 'completed', folderName: 'Archive' }));

    await setSessionDone({ store, api: fakeApi(OK) }, 'br_1', false);

    expect(store.getState().folders).toEqual(['Archive']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('');
  });

  it('un-marking does NOT invent Archive when the list does not hold it', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    // The fixture above starts with Archive already listed, which is exactly the state
    // in which the `done &&` half of the guard cannot be observed: the includes() check
    // short-circuits it either way. Archive is absent here, so only `done` decides.
    actions.setFolders(['Work']);
    actions.upsertSession(row({ state: 'completed', folderName: 'Archive' }));

    await setSessionDone({ store, api: fakeApi(OK) }, 'br_1', false);

    // Un-marking finishes through SetSessionFolder with an EMPTY folder, which inserts
    // nothing. Mirroring an insert here would put a header on the sidebar for a folder
    // the session is on its way out of.
    expect(store.getState().folders).toEqual(['Work']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('');
  });

  it('marking a SECOND session done does not list Archive twice', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row({ sessionId: 'br_1' }));
    actions.upsertSession(row({ sessionId: 'br_2' }));
    const api = fakeApi(OK);

    await setSessionDone({ store, api }, 'br_1', true);
    expect(store.getState().folders).toEqual(['Work', 'Archive']);

    // The first call is what makes this discriminate: on the second, Archive is already
    // there, so only the includes() check can keep the list at one header.
    await setSessionDone({ store, api }, 'br_2', true);
    expect(store.getState().folders).toEqual(['Work', 'Archive']);
    expect(visibleSessions(store.getState()).map((g) => g.folder)).toEqual(['Work', 'Archive']);
  });

  it('a refused mark-done takes the invented Archive folder back out', async () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setFolders(['Work']);
    actions.upsertSession(row());

    await expect(setSessionDone({ store, api: fakeApi(REFUSED) }, 'br_1', true)).rejects.toThrow(
      /500/,
    );

    expect(store.getState().folders).toEqual(['Work']);
    expect(store.getState().sessions.get('br_1')?.folderName).toBe('');
  });
});
