/** The session states that mean "a turn is in flight".
 *
 *  One list, two readers. `effectiveState` (store/selectors.ts) uses it to decide
 *  whether a summary row's state is worth reconciling against the tail; the activity
 *  fold (store/activity.ts) uses it to decide whether a `session_state` event has
 *  just ended the turn. Both answers have to agree or a session can be settled for
 *  one of them and running for the other, so the list lives here rather than in
 *  either — this module imports nothing, so both can read it without a cycle.
 *
 *  ⚠️ Membership is NOT a claim that the server emits all of these. Per the deferred
 *  `SessionState` enum decision, `starting`, `model_generating` and `compacting` are
 *  emitted zero times today. They stay listed because the vocabulary is the server's
 *  and this file must not silently narrow it — but do not build a feature whose only
 *  live signal is one of them.
 */
export const RUNNING_STATES: ReadonlySet<string> = new Set([
  'starting',
  'model_generating',
  'tool_running',
  'compacting',
  'rate_limited',
  'running',
  'holding',
  'waiting_on_approval',
]);

/** True when `state` says a turn is in flight. An empty or unknown state reads as
 *  NOT running: the caller is asking "may I assume work is happening", and a state
 *  this client does not recognize is not evidence that it is. */
export function isRunningState(state: string | undefined | null): boolean {
  return !!state && RUNNING_STATES.has(state);
}
