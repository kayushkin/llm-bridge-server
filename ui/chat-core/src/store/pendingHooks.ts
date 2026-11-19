import type { ApiClient } from '../net/ApiClient.js';
import type { HookResolveInput, PendingHook } from '../net/types.js';
import type { HookEventWire, WireEvent } from '../net/wireEvents.js';
import type { ChatStoreApi } from './ChatStore.js';

/** The one phase that parks a tool call on a human. */
export const HOOK_PHASE_AWAITING = 'awaiting_resolution';
/** The phase that closes an awaiting_resolution hook, carrying the same request id. */
export const HOOK_PHASE_COMPLETED = 'completed';

/** Project a wire HookEvent to the camelCase `PendingHook`. Returns null when the event
 *  cannot address a decision — no `request_id` means nothing to resolve, and the resolve
 *  endpoint is keyed on it. */
export function pendingHookFromWire(w: HookEventWire | undefined): PendingHook | null {
  if (!w || !w.request_id) return null;
  const hook: PendingHook = {
    requestId: w.request_id,
    event: w.event ?? '',
    phase: w.phase ?? '',
    source: w.source ?? '',
  };
  if (w.tool_name !== undefined) hook.toolName = w.tool_name;
  if (w.matcher !== undefined) hook.matcher = w.matcher;
  if (w.hook_id !== undefined) hook.hookId = w.hook_id;
  if (w.input !== undefined) hook.input = w.input;
  return hook;
}

/**
 * Fold one live event into a session's parked-hook map, returning the NEXT map — or the
 * same reference when nothing changed, so the store can skip a needless set().
 *
 * `awaiting_resolution` inserts, `completed` deletes, every other phase is observation
 * and leaves the map alone. Both directions are idempotent, which is what lets the SSE
 * resume replay the same events without disturbing the banner.
 */
export function foldHookEvent(
  current: ReadonlyMap<string, PendingHook>,
  event: WireEvent,
): ReadonlyMap<string, PendingHook> {
  if (event.data?.type !== 'hook' && event.type !== 'hook') return current;
  const hook = pendingHookFromWire(event.data?.hook);
  if (!hook) return current;
  if (hook.phase === HOOK_PHASE_AWAITING) {
    const prior = current.get(hook.requestId);
    if (prior && prior.phase === hook.phase && prior.source === hook.source) return current;
    const next = new Map(current);
    next.set(hook.requestId, hook);
    return next;
  }
  if (hook.phase === HOOK_PHASE_COMPLETED) {
    if (!current.has(hook.requestId)) return current;
    const next = new Map(current);
    next.delete(hook.requestId);
    return next;
  }
  return current;
}

/**
 * Deliver a decision for one parked hook and reconcile with the server.
 *
 * The entry is cleared OPTIMISTICALLY — the matching `phase="completed"` event arrives
 * moments later and `foldHookEvent` is idempotent with this, so the two never fight. On
 * a non-2xx the entry is RESTORED and the error rethrown: a decision the server refused
 * must leave the card on screen, because the tool call is still parked and the only way
 * out is another click.
 *
 * Shared by `usePendingPermissions` so the orchestration is tested directly rather than
 * re-derived in a React-lifecycle harness (single source of truth).
 */
export async function resolvePendingHook(
  deps: { store: ChatStoreApi; api: ApiClient },
  sessionId: string,
  input: HookResolveInput,
): Promise<void> {
  const { store, api } = deps;
  if (!input.requestId) throw new Error('resolve hook: requestId is required');
  const prior = store.getState().pendingHooks.get(sessionId)?.get(input.requestId) ?? null;
  store.getState().actions.clearPendingHook(sessionId, input.requestId);
  try {
    await api.resolveHook(sessionId, input);
  } catch (e) {
    if (prior) store.getState().actions.upsertPendingHook(sessionId, prior);
    throw e;
  }
}
