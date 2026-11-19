import type { SessionAction, SessionActionType } from '@kayushkin/chat-core'

/**
 * What the chat says about a session action — a button the agent put in its chat,
 * which llm-bridge-server runs when the person confirms it. Pure, so the rules have
 * tests; `components/chat/SessionActions.tsx` only draws them.
 */

/** What each action type does, in a few words, for the line under the label. */
export const SESSION_ACTION_TYPE_LABEL: Record<SessionActionType, string> = {
  deploy: 'Deploy',
  run_scheduler_job: 'Run a scheduler job',
  run_command: 'Run a command',
  model_call: 'Ask a model',
  background_agent: 'Start a background agent',
  send_message: 'Send a message here',
  fork_and_send: 'Fork this session and send',
  new_session_and_send: 'Start a new session and send',
  send_to_session: 'Send a message to another session',
  stop_session: 'Stop another session',
}

/** The other session a button acts on, and what it does to it, for the line beside the
 *  button: "Sends to" or "Stops", then the session as a chip. Null for an action that
 *  names no target. */
export function sessionActionTarget(action: SessionAction): { verb: string; sessionId: string } | null {
  const sessionId = action.offer.target_session_id
  if (!sessionId) return null
  switch (action.offer.type) {
    case 'send_to_session':
      return { verb: 'Sends to', sessionId }
    case 'stop_session':
      return { verb: 'Stops', sessionId }
    default:
      return null
  }
}

/** The button can be pressed only while the action waits for its one run, and never
 *  when its reviewer rejected its command. */
export function sessionActionIsConfirmable(action: SessionAction): boolean {
  return action.state === 'offered' && action.review?.verdict !== 'reject'
}

/** Whether the output is drawn as markdown: a model's answer always is, a command's
 *  when the agent said so. */
export function sessionActionOutputIsMarkdown(action: SessionAction): boolean {
  if (action.offer.type === 'model_call' || action.offer.type === 'background_agent') return true
  return action.offer.result_format === 'markdown'
}

/** What the button may spend and, once it ran, what it did spend, e.g. "up to $0.20" or
 *  "spent $0.0021 of $0.20". Empty for an action that spends nothing. */
export function sessionActionCostText(action: SessionAction): string {
  const limit = action.offer.maximum_cost_usd
  if (!limit) return ''
  const limitText = `$${limit.toFixed(2)}`
  if (action.cost_usd) return `spent $${action.cost_usd.toFixed(4)} of ${limitText}`
  return `up to ${limitText}`
}

/** The reviewer's verdict in a word, for the badge beside the button. */
export const SESSION_ACTION_REVIEW_LABEL: Record<NonNullable<SessionAction['review']>['verdict'], string> = {
  approve: 'Reviewed: fine',
  caution: 'Reviewed: read first',
  reject: 'Reviewed: rejected',
}

/** Who ran it: principal-store's id of the person, or the internal service when no
 *  person was named. */
export function sessionActionRunBy(action: SessionAction): string {
  return action.run_by_principal_id || 'the internal service'
}

/** The state shown on the button once it has been pressed. */
export function sessionActionStatusText(action: SessionAction): string {
  switch (action.state) {
    case 'offered':
      return ''
    case 'running':
      return 'Running…'
    case 'succeeded':
      return 'Done'
    case 'failed':
      return 'Failed'
    case 'outcome_unknown':
      return 'Outcome unknown'
  }
}
