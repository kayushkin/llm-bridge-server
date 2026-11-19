// Handing a card to an agent.
//
// One implementation, because there are two ways to trigger it — the button on
// the card tile and the panel inside the drawer — and a second copy of this
// sequence would drift. The ordering below is the part that matters, and it is
// not obvious enough to reproduce correctly from memory.

import type { FetchFn } from './types'
import { readErrorText } from './useKanban'

export type DispatchAgentArgs = {
  basePath: string
  fetchFn: FetchFn
  /** Shown in the session list, so it should read as the work, not as an id. */
  title: string
  /** Exactly what the agent is sent. Callers resolve stored-vs-suggested first. */
  prompt: string
  /**
   * Attaches the session to the card. Already bound to the card — useKanban's
   * addCardLink takes the card id as its first argument, so callers pass a
   * closure over it.
   */
  addLink: (entityType: string, entityRef: string, label?: string) => Promise<boolean>
  /**
   * The harness instance the agent runs on, which also fixes the environment
   * (machine). The session's harness is the instance's own: llm-bridge-server
   * refuses a session whose harness differs from its instance's, so sending
   * them separately would only invite a 400.
   */
  instance: { id: string; harness_type: string }
  /**
   * The bundle the session is composed from: a bundle-store numeric id as a
   * string (`"6"`), or undefined for none. The kanban page passes the board's
   * `default_bundle_id`. The server checks it with bundle-store at creation
   * (400 `unknown_bundle`) and provisions the bundle's tools at spawn.
   */
  bundleID?: string
}

/**
 * dispatchAgentOnCard creates an autonomous session, attaches it to the card,
 * sends the prompt, and returns the session id.
 *
 * The link is written BEFORE the prompt is sent. If the send fails, a linked
 * session is a visible loose end somebody can open from the board; an unlinked
 * one is a live agent nothing points at. Cost is the same either way, so the
 * order is chosen to make the failure findable.
 *
 * The session is `autonomous` because an unattended session that parks on a
 * permission prompt nobody is watching never finishes. That is the same type
 * autoworker and kanban-dispatcher already use, and it means tool calls are
 * auto-allowed — the reason this is worth a confirmation step in the UI.
 *
 * Throws on any failure rather than returning a status, so a caller cannot
 * quietly treat a failed dispatch as a started one.
 */
export async function dispatchAgentOnCard(args: DispatchAgentArgs): Promise<string> {
  const { basePath, fetchFn, title, prompt, addLink, instance, bundleID } = args

  const trimmed = prompt.trim()
  if (!trimmed) throw new Error('refusing to start an agent with an empty prompt')
  if (!instance?.id || !instance.harness_type) throw new Error('refusing to start an agent without choosing where it runs')

  const created = await fetchFn(`${basePath}/sessions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      harness: instance.harness_type,
      instance_id: instance.id,
      display_name: `card: ${title}`.slice(0, 80),
      type: 'autonomous',
      purpose: 'dispatcher',
      origin: 'kanban-card',
      ...(bundleID ? { bundle_id: bundleID } : {}),
    }),
  })
  // The server's refusal names what was wrong — an unknown bundle, a principal
  // the grants do not allow here — so its words are the message; only a bodiless
  // answer falls back to the status.
  if (!created.ok) throw new Error(await readErrorText(created, 'create session'))

  const session = await created.json()
  const sessionID: string = session?.session_id
  if (!sessionID) throw new Error('create session: response carried no session_id')

  // addCardLink reports failure by returning false rather than throwing, so an
  // unchecked call would sail past a failed link and send anyway — producing
  // exactly the orphan this ordering exists to prevent.
  const linked = await addLink('session', sessionID, 'kanban-card')
  if (!linked) {
    throw new Error(
      `started session ${sessionID} but could not attach it to the card; ` +
      `it is running and unlinked — open it from the sessions list`,
    )
  }

  const sent = await fetchFn(`${basePath}/sessions/${encodeURIComponent(sessionID)}/send`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ message: trimmed }),
  })
  if (!sent.ok) throw new Error(`send prompt: HTTP ${sent.status}`)

  return sessionID
}
