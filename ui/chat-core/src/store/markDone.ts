import type { ApiClient } from '../net/ApiClient.js';
import type { ChatStoreApi } from './ChatStore.js';

/**
 * The canonical folder a session moves into when marked done.
 *
 * Mirrors `store.ArchiveFolder` in llm-bridge-server (`internal/store/store.go`) and
 * `ARCHIVE_FOLDER` in bridge-ui's `useBridgeFolders`. The casing matters: chat-core
 * used to write lowercase `'archive'`, which never matched anything the server wrote,
 * so the chat's session row had to test both spellings to decide whether a session was
 * archived. One spelling, from one constant.
 */
export const ARCHIVE_FOLDER = 'Archive';

/**
 * Optimistically mark a session done (or undo it) and reconcile with the server.
 *
 * Patches the cached row immediately, POSTs `/sessions/{id}/mark-done`, and — on a
 * non-2xx — REVERTS the row to its prior value and rethrows. A refused change must
 * surface, never be silently kept and never be silently dropped.
 *
 * Shared by `useSessionActions` so the orchestration is exercised directly by tests
 * rather than re-derived in a React-lifecycle harness (single source of truth), the
 * same arrangement as `changeSessionPermissionState`.
 *
 * BOTH halves of the optimistic row matter. `handleMarkSessionDone` broadcasts a
 * state transition AND moves the folder, as one atomic action. The previous code
 * moved the folder alone, so even against a working route the row would have shown a
 * state the server never agreed with until the next SSE upsert corrected it.
 *
 * The folder LIST is a third thing the same action changes. `handleMarkSessionDone`
 * finishes through `Store.SetSessionFolder`, which INSERTs the folder when it is not
 * there yet — so marking the first session done CREATES the `Archive` folder. Without
 * mirroring that, the row moves into a folder the sidebar's group order does not know,
 * and `visibleSessions` draws it as a trailing session-derived group instead of in the
 * server's own order. Un-marking does NOT delete the folder server-side, so the list
 * keeps it.
 */
export async function setSessionDone(
  deps: { store: ChatStoreApi; api: ApiClient },
  sessionId: string,
  done: boolean,
): Promise<void> {
  const { store, api } = deps;
  const prior = store.getState().sessions.get(sessionId);
  if (!prior) return;
  const priorFolders = store.getState().folders;

  const { actions } = store.getState();
  if (done && !priorFolders.includes(ARCHIVE_FOLDER)) {
    actions.setFolders([...priorFolders, ARCHIVE_FOLDER]);
  }
  actions.upsertSession({
    ...prior,
    state: done ? 'completed' : 'idle',
    folderName: done ? ARCHIVE_FOLDER : '',
  });

  try {
    await api.markSessionDone(sessionId, done);
  } catch (e) {
    store.getState().actions.setFolders(priorFolders);
    store.getState().actions.upsertSession(prior);
    throw e;
  }
}
