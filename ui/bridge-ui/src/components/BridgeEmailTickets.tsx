import { useCallback, useEffect, useState } from 'react'
import type { TicketLogEntry } from '@kayushkin/kanban-store-types'
import { useBridgeConfig } from '../context'
import { listTickets } from '../kanbanStoreClient'
import { ticketArrival, ticketPlacementSummary } from '../ticketArrival'
import { CardRefChip } from './CardRefChip'
import { EmailRefChip } from './chat/EmailRefChip'

const PAGE_SIZE = 50

/**
 * Every email ticket, newest first: the email it came from, the card it
 * became, and whether the email made a new card or turned an existing one
 * into a ticket.
 *
 * Read from kanban-store's `GET /api/tickets?channel=email`. The email is the
 * ticket's `source_entity_ref`, which the classifier writes once when it makes
 * the ticket — not the card's email links, which merges and refiles move. A
 * ticket written before the source existed and not backfilled shows no email
 * rather than a guessed one.
 */
export function BridgeEmailTickets() {
  const { fetch: fetchFn, kanbanStoreBasePath } = useBridgeConfig()
  const [entries, setEntries] = useState<TicketLogEntry[]>([])
  const [nextBefore, setNextBefore] = useState<string | undefined>(undefined)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async (before?: string) => {
    setLoading(true)
    setError(null)
    const result = await listTickets(fetchFn, kanbanStoreBasePath, { channel: 'email', before, limit: PAGE_SIZE })
    setLoading(false)
    if (!result.ok) {
      setError(result.error)
      return
    }
    setEntries(previous => (before ? [...previous, ...result.value.tickets] : result.value.tickets))
    setNextBefore(result.value.next_before)
  }, [fetchFn, kanbanStoreBasePath])

  useEffect(() => { void load() }, [load])

  return (
    <div className="bet-container">
      <header>
        <h2 className="bet-title">Email tickets</h2>
        <p className="bet-subtitle">
          Each ticket the email classifier made, beside the email that made it. <em>New card</em> means the email
          created the card; <em>existing card</em> means it landed on a card that was already on the board and made
          that card a ticket.
        </p>
      </header>
      {error && <div className="bridge-error">{error}</div>}
      {!error && !loading && entries.length === 0 && <div className="bi-empty">No email tickets yet.</div>}
      {entries.length > 0 && (
        <table className="bet-table">
          <thead>
            <tr>
              <th>When</th>
              <th>Email</th>
              <th aria-label="became" />
              <th>Ticket</th>
              <th>How</th>
              <th>Requester</th>
              <th>Where</th>
            </tr>
          </thead>
          <tbody>
            {entries.map(entry => <TicketRow key={entry.ticket?.card_id} entry={entry} />)}
          </tbody>
        </table>
      )}
      {loading && <div className="bi-loading">Loading…</div>}
      {nextBefore && !loading && (
        <button type="button" className="bet-more" onClick={() => void load(nextBefore)}>Load older tickets</button>
      )}
    </div>
  )
}

function TicketRow({ entry }: { entry: TicketLogEntry }) {
  const ticket = entry.ticket
  if (!ticket) return null
  const arrival = ticketArrival(entry)
  return (
    <tr data-card-id={ticket.card_id}>
      <td className="bet-when" title={ticket.created_at}>{new Date(ticket.created_at).toLocaleString()}</td>
      <td>
        {ticket.source_entity_type === 'email' && ticket.source_entity_ref
          ? <EmailRefChip locator={ticket.source_entity_ref} />
          : <span className="bet-unknown" title="This ticket was written before tickets recorded their email, and was not backfilled">not recorded</span>}
      </td>
      <td className="bet-arrow" aria-hidden>→</td>
      <td><CardRefChip cardID={ticket.card_id} /></td>
      <td><span className={`bet-arrival bet-arrival-${arrival === 'new card' ? 'new' : 'existing'}`}>{arrival}</span></td>
      <td title={ticket.requester_principal_id}>{entry.requester_display_name || ticket.requester_principal_id}</td>
      <td className="bet-where">{ticketPlacementSummary(entry)}</td>
    </tr>
  )
}
