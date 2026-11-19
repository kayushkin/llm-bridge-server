import type { Entry, SessionAction } from '../net/types.js';

/**
 * The newest record of every session action among `entries`, by `action_id`.
 *
 * Each change to an action arrives as its own `session_action` entry carrying the whole
 * record, so a button drawn at its offer reads its current state from here rather than
 * from the offer entry, which stays `offered` for good. Newest is the highest event id,
 * which is the order the server wrote them in.
 */
export function newestSessionActions(entries: Iterable<Entry>): Map<string, SessionAction> {
  const newest = new Map<string, { eventId: number; action: SessionAction }>();
  for (const entry of entries) {
    const action = entry.sessionAction;
    if (entry.kind !== 'action' || !action) continue;
    const seen = newest.get(action.action_id);
    if (!seen || entry.eventId > seen.eventId) newest.set(action.action_id, { eventId: entry.eventId, action });
  }
  return new Map([...newest].map(([actionId, { action }]) => [actionId, action]));
}

const SESSION_ACTION_ID_IN_TEXT = /\bsession_action_\d{6,}\b/gi;

/**
 * Every session action id the agent wrote in its own prose among `entries`. The chat
 * draws such an action as a button where its id is written, so the card at its offer
 * shrinks to a line rather than showing the same button twice. Only the agent's
 * reply text counts: an id in a tool's output or the user's message places nothing.
 */
export function sessionActionIdsPlacedInProse(entries: Iterable<Entry>): Set<string> {
  const placed = new Set<string>();
  for (const entry of entries) {
    if (entry.role !== 'assistant' || (entry.kind !== 'text' && entry.kind !== 'result') || !entry.text) continue;
    for (const match of entry.text.matchAll(SESSION_ACTION_ID_IN_TEXT)) placed.add(match[0].toLowerCase());
  }
  return placed;
}
