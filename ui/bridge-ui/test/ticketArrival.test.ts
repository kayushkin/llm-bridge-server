import { describe, expect, it } from 'vitest'
import type { TicketLogEntry } from '@kayushkin/kanban-store-types'
import { ticketArrival, ticketPlacementSummary } from '../src/ticketArrival'
import { getCardTicket, listCardsForEntity, listTickets } from '../src/kanbanStoreClient'
import type { FetchFn } from '../src/types'

function entry(ticketCreatedAt: string, cardCreatedAt?: string): TicketLogEntry {
  return {
    ticket: {
      card_id: 'c1', requester_principal_id: 'principal_000010', channel: 'email',
      source_entity_type: 'email', source_entity_ref: 'demo-work:lm1',
      created_at: ticketCreatedAt, updated_at: ticketCreatedAt,
    },
    states: [],
    card_created_at: cardCreatedAt,
  }
}

describe('ticketArrival', () => {
  it('calls a card created in the same pass as its ticket a new card', () => {
    // Measured live: the classifier wrote card and ticket 0.01s apart.
    expect(ticketArrival(entry('2026-09-21T17:01:07.196Z', '2026-09-21T17:01:07.183Z'))).toBe('new card')
  })

  it('calls a card that was on the board before the email an existing card', () => {
    expect(ticketArrival(entry('2026-09-21T18:00:45Z', '2026-08-29T05:23:52Z'))).toBe('existing card')
  })

  it('calls a card with no creation event an existing card, since it predates every ticket', () => {
    expect(ticketArrival(entry('2026-09-21T18:00:45Z'))).toBe('existing card')
  })
})

describe('ticketPlacementSummary', () => {
  it('names each column, with its lifecycle where the column has one', () => {
    const summary = ticketPlacementSummary({
      ...entry('2026-09-21T18:00:45Z'),
      states: [
        { board_id: 'b1', column_id: 'k1', column_name: 'Action needed', lifecycle_state: 'open' },
        { board_id: 'b2', column_id: 'k2', column_name: 'Triage' },
      ],
    })
    expect(summary).toBe('Action needed (open), Triage')
  })
})

function fetchAnswering(status: number, body: unknown) {
  const urls: string[] = []
  const fetchFn = (async (url: string) => {
    urls.push(url)
    return new Response(JSON.stringify(body), { status })
  }) as unknown as FetchFn
  return { urls, fetchFn }
}

describe('ticket reads', () => {
  it('lists email tickets from a cursor', async () => {
    const { urls, fetchFn } = fetchAnswering(200, { tickets: [] })
    await listTickets(fetchFn, '/api/kanban', { channel: 'email', before: '2026-09-21T17:01:07.188278837Z', limit: 50 })
    expect(urls).toEqual(['/api/kanban/api/tickets?channel=email&before=2026-09-21T17%3A01%3A07.188278837Z&limit=50'])
  })

  it('keeps an email locator one path segment when asking for its cards', async () => {
    const { urls, fetchFn } = fetchAnswering(200, [])
    await listCardsForEntity(fetchFn, '/api/kanban', 'email', 'gmail-personal:1a0b/73')
    expect(urls).toEqual(['/api/kanban/api/entities/email/gmail-personal%3A1a0b%2F73/cards'])
  })

  it('reads a card that is not a ticket as null, not as a failure', async () => {
    const { fetchFn } = fetchAnswering(404, { error: 'card c1 is not a ticket' })
    expect(await getCardTicket(fetchFn, '/api/kanban', 'c1')).toEqual({ ok: true, value: null })
  })

  it('reports any other refusal verbatim', async () => {
    const { fetchFn } = fetchAnswering(502, { error: 'principal-store check failed' })
    const result = await getCardTicket(fetchFn, '/api/kanban', 'c1')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toContain('principal-store check failed')
  })
})
