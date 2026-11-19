import type { Entry } from '@kayushkin/chat-core'

/** System subtypes of a message the server held while it waited to restart
 *  (llm-bridge `SystemSubtypeMessageHeldForRestart` and the two that close it). */
export const MESSAGE_HELD_FOR_RESTART = 'message_held_for_restart'
export const HELD_MESSAGE_DELIVERED = 'held_message_delivered'
export const HELD_MESSAGE_DELIVERY_FAILED = 'held_message_delivery_failed'

/** How a held message ended: `{}` when it was sent, `{ failedBecause }` when
 *  the server could not send it. A held message with no outcome still waits. */
export type HeldMessageOutcome = { failedBecause?: string }

/** Every held message's outcome in a session's entries, by held message id. */
export function heldMessageOutcomes(entries: Record<string, Entry>): Map<string, HeldMessageOutcome> {
  const outcomes = new Map<string, HeldMessageOutcome>()
  for (const entry of Object.values(entries)) {
    if (entry.kind !== 'system' || !entry.heldMessageId) continue
    if (entry.subtype === HELD_MESSAGE_DELIVERED) outcomes.set(entry.heldMessageId, {})
    if (entry.subtype === HELD_MESSAGE_DELIVERY_FAILED) {
      outcomes.set(entry.heldMessageId, { failedBecause: entry.text || 'the server refused it' })
    }
  }
  return outcomes
}

/** What the conversation shows for one entry that may be a held message:
 *  `null` when the entry is not one; `'hidden'` once it was sent, since its own
 *  user_message is then the row; otherwise the row's held state — still
 *  waiting, or not sent and why. `outcomes` is asked for only when the entry is
 *  a held message, so a turn without one never scans the session. */
export function heldMessageRow(
  entry: Entry,
  outcomes: () => Map<string, HeldMessageOutcome>,
): null | 'hidden' | { failedBecause?: string } {
  if (entry.kind !== 'system' || entry.subtype !== MESSAGE_HELD_FOR_RESTART || !entry.heldMessageId) return null
  const outcome = outcomes().get(entry.heldMessageId)
  if (outcome && outcome.failedBecause === undefined) return 'hidden'
  return outcome ?? {}
}
