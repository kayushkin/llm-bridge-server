import type { ApiClient } from '../net/ApiClient.js';
import type { SessionSummary } from '../net/types.js';
import type { ChatStoreApi } from './ChatStore.js';

/**
 * Folder mutations: create, delete, rename, and file a session into one.
 *
 * Every function here is optimistic-then-reconciled in the shape `setSessionDone`
 * established — patch the store, call the route, and on a refusal put back exactly
 * what was there and rethrow. A revert without a rethrow is what made the chat's
 * archive button look like it worked for a year, so nothing here swallows.
 *
 * ## Why these mirror SQL rather than re-reading `/folders`
 *
 * Three of the four folder routes change TWO tables in one transaction, and only
 * `PUT /sessions/{id}/folder` notifies the session-list stream. So after a delete or
 * a rename the server tells the client nothing at all: the sidebar's own state is the
 * only thing that can move those rows. Each function below therefore mirrors its
 * route's transaction exactly, and the comment on each says which statement it is
 * mirroring (`llm-bridge-server/internal/store/store.go`). The SyncEngine's reconnect
 * re-read of `GET /folders` remains the reconciliation point for anything a second
 * client did; it is not needed after a local mutation, because the local mirror
 * already lands where the server's `position` ordering puts it.
 *
 * The alternative — one extra `GET /folders` per mutation — buys nothing and races
 * the optimistic patch it is supposed to confirm.
 */
export interface FolderMutationDeps {
  store: ChatStoreApi;
  api: ApiClient;
}

/** Every loaded session currently filed under `folder`, captured before the patch so
 *  a refusal can restore each row byte-for-byte. */
function sessionsInFolder(store: ChatStoreApi, folder: string): SessionSummary[] {
  const out: SessionSummary[] = [];
  for (const s of store.getState().sessions.values()) {
    if (s.folderName === folder) out.push(s);
  }
  return out;
}

/** Put back both halves of a folder mutation after the server refused it. */
function revert(store: ChatStoreApi, folders: string[], sessions: SessionSummary[]): void {
  const { actions } = store.getState();
  actions.setFolders(folders);
  for (const s of sessions) actions.upsertSession(s);
}

/**
 * Create an empty folder.
 *
 * Mirrors `Store.CreateFolder`: `INSERT INTO folders (name, position) VALUES (?,
 * MAX(position)+1) ON CONFLICT(name) DO NOTHING`. New folders go to the END of the
 * order, which is why the optimistic entry is appended rather than sorted in.
 *
 * A blank name is refused here rather than sent: the handler answers 400 for it, so
 * the round trip only exists to produce an error the user did not ask for. A name the
 * list already holds is still sent — the list can be stale, and the INSERT is a no-op
 * server-side — but is not appended twice.
 */
export async function createFolder(deps: FolderMutationDeps, rawName: string): Promise<void> {
  const name = rawName.trim();
  if (!name) return;
  const { store, api } = deps;
  const priorFolders = store.getState().folders;
  if (!priorFolders.includes(name)) {
    store.getState().actions.setFolders([...priorFolders, name]);
  }
  try {
    await api.createFolder(name);
  } catch (e) {
    revert(store, priorFolders, []);
    throw e;
  }
}

/**
 * Delete a folder and un-file everything in it.
 *
 * Mirrors `Store.DeleteFolder`, which is `UPDATE sessions SET folder_name='' WHERE
 * folder_name=?` FOLLOWED BY `DELETE FROM folders WHERE name=?`, one transaction.
 * Dropping the header alone would leave every session in it claiming a folder the
 * server no longer has, and `visibleSessions` would draw that claim as a group of its
 * own at the bottom of the sidebar — a folder that survives its own deletion.
 *
 * Nothing is deleted but the folder. The sessions come back un-filed.
 */
export async function deleteFolder(deps: FolderMutationDeps, name: string): Promise<void> {
  if (!name) return;
  const { store, api } = deps;
  const priorFolders = store.getState().folders;
  const priorSessions = sessionsInFolder(store, name);

  const { actions } = store.getState();
  actions.setFolders(priorFolders.filter((f) => f !== name));
  for (const s of priorSessions) actions.upsertSession({ ...s, folderName: '' });

  try {
    await api.deleteFolder(name);
  } catch (e) {
    revert(store, priorFolders, priorSessions);
    throw e;
  }
}

/**
 * Rename a folder, or merge it into an existing one.
 *
 * Mirrors `Store.RenameFolder`, whose second half has two shapes:
 *
 *   - the new name is FREE → `UPDATE folders SET name=?`, so the row keeps its
 *     `position` and the folder stays where it was in the order;
 *   - the new name EXISTS → the old row is `DELETE`d and its sessions join the
 *     existing folder, at THAT folder's position. This is a merge, and it is the
 *     reason the optimistic list drops a name instead of rewriting one.
 *
 * Either way every session moves first (`UPDATE sessions SET folder_name=new WHERE
 * folder_name=old`), so both halves are patched here too.
 *
 * `RenameFolder` returns nil without touching anything when either name is empty or
 * they are equal, so those cases are not sent at all.
 */
export async function renameFolder(
  deps: FolderMutationDeps,
  oldName: string,
  rawNewName: string,
): Promise<void> {
  const newName = rawNewName.trim();
  if (!oldName || !newName || oldName === newName) return;
  const { store, api } = deps;
  const priorFolders = store.getState().folders;
  const priorSessions = sessionsInFolder(store, oldName);

  const { actions } = store.getState();
  actions.setFolders(
    priorFolders.includes(newName)
      ? priorFolders.filter((f) => f !== oldName)
      : priorFolders.map((f) => (f === oldName ? newName : f)),
  );
  for (const s of priorSessions) actions.upsertSession({ ...s, folderName: newName });

  try {
    await api.renameFolder(oldName, newName);
  } catch (e) {
    revert(store, priorFolders, priorSessions);
    throw e;
  }
}

/**
 * File a session into a folder, or un-file it with an empty `folder`.
 *
 * Mirrors `Store.SetSessionFolder`, which INSERTs the folder when it does not exist
 * before updating the session row. That is why this is the ONLY call a "new folder,
 * and move this session into it" menu item needs: creating and filing are one
 * transaction on the server, and splitting them client-side would open a window in
 * which a create succeeded and the move did not.
 *
 * A session the store does not hold is a no-op — same as `setSessionDone`. There is
 * no row to patch, so an optimistic move would have nothing to revert and the request
 * would be aimed at an id this client cannot show the result of.
 */
export async function moveSessionToFolder(
  deps: FolderMutationDeps,
  sessionId: string,
  folder: string,
): Promise<void> {
  const { store, api } = deps;
  const priorSession = store.getState().sessions.get(sessionId);
  if (!priorSession) return;
  const priorFolders = store.getState().folders;

  const { actions } = store.getState();
  if (folder && !priorFolders.includes(folder)) actions.setFolders([...priorFolders, folder]);
  actions.upsertSession({ ...priorSession, folderName: folder });

  try {
    await api.setSessionFolder(sessionId, folder);
  } catch (e) {
    revert(store, priorFolders, [priorSession]);
    throw e;
  }
}
