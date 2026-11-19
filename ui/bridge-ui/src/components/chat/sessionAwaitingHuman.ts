// Whether a session's state puts it in the operator's queue is chat-core's rule
// (`isSessionAwaitingOperator`: an awaiting state AND an interactive or herald session),
// shared by every surface that marks or counts what needs the operator. Only the wording
// lives here.

/** What the marker is asking for, in the user's words rather than the wire's. Used for the
 *  accessible names, so a screen reader hears "approval" and not `awaiting_permission`. */
export function humanAskedFor(state: string): string {
  return state === 'awaiting_permission' ? 'approval' : 'question'
}
