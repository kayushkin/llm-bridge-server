import type { ApiClient } from '../net/ApiClient.js';
import type { HarnessConfig, SessionPermissionState } from '../net/types.js';
import type { ChatStoreApi } from './ChatStore.js';

/**
 * Optimistically set a session's permission state and reconcile with the server.
 *
 * Patches the cached `ManagedSessionDetail`'s `harnessConfig` immediately (so the
 * controls reflect the choice at once), PUTs `/sessions/{id}/permission-mode` with
 * every axis in ONE body, and — on a non-2xx — REVERTS the cached detail to its prior
 * value and rethrows. A failed change must surface, never be silently kept.
 *
 * One PUT, not three: the mode, the sandbox network gate and the custom knobs are
 * separate axes of one persisted state, and writing them separately would let a partial
 * failure leave the UI describing a state the server is not in. The revert is whole for
 * the same reason — it restores the prior detail, so no axis is left half-applied.
 *
 * An axis the caller omits is left alone on both sides: absent from the request body
 * (the server keeps its stored value) and absent from the optimistic patch (the cache
 * keeps its own). A UI that renders no network checkbox therefore cannot switch the
 * sandbox back on merely by changing the mode.
 *
 * Shared by `useManagedSession` so the orchestration is exercised directly by tests
 * rather than re-derived in a React-lifecycle harness (single source of truth).
 */
export async function changeSessionPermissionState(
  deps: { store: ChatStoreApi; api: ApiClient },
  sessionId: string,
  state: SessionPermissionState,
): Promise<void> {
  const { store, api } = deps;
  // Snapshot the prior detail so a failed PUT can revert every axis cleanly.
  const prior = store.getState().sessionDetail.get(sessionId) ?? null;
  const patch: Partial<HarnessConfig> = { permissionMode: state.mode };
  if (state.disableNetwork !== undefined) patch.disableNetwork = state.disableNetwork;
  if (state.permissionModeCustom !== undefined) {
    patch.permissionModeCustom = state.permissionModeCustom;
  }
  store.getState().actions.patchHarnessConfig(sessionId, patch);
  try {
    await api.setSessionPermissionState(sessionId, state);
  } catch (e) {
    if (prior) store.getState().actions.setSessionDetail(sessionId, prior);
    throw e;
  }
}
