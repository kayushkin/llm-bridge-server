import type { TicketLogEntry } from '@kayushkin/kanban-store-types'

// How a ticket reached the board: the email classifier either creates a card
// for a message and makes it a ticket in the same step, or files a message
// onto a card that already existed and makes that card a ticket. kanban-store
// records the card's creation and the ticket's, and this compares them.

/** A card created this close before its ticket was created for it. The
 *  classifier writes both in one pass, measured 0.01s apart. */
export const NEW_CARD_WINDOW_MS = 60_000

export type TicketArrival = 'new card' | 'existing card'

/** 'new card' when the card was created with its ticket; 'existing card' when
 *  it was older. A card with no card_created event predates that event
 *  (2026-08-21) and so predates every ticket. */
export function ticketArrival(entry: TicketLogEntry): TicketArrival {
  if (!entry.card_created_at || !entry.ticket) return 'existing card'
  const cardCreated = Date.parse(entry.card_created_at)
  const ticketCreated = Date.parse(entry.ticket.created_at)
  if (Number.isNaN(cardCreated) || Number.isNaN(ticketCreated)) return 'existing card'
  return ticketCreated - cardCreated <= NEW_CARD_WINDOW_MS ? 'new card' : 'existing card'
}

/** The columns the ticket sits in, one per board the caller can view, with
 *  the lifecycle each column means where it has one: "Action needed (open)". */
export function ticketPlacementSummary(entry: TicketLogEntry): string {
  return entry.states
    .map(state => (state.lifecycle_state ? `${state.column_name} (${state.lifecycle_state})` : state.column_name))
    .join(', ')
}
