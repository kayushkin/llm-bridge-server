import { useEffect, useState } from 'react'
import type { EntityCardView } from '@kayushkin/kanban-store-types'
import { useBridgeConfig } from '../context'
import { getCardTicket, listCardsForEntity } from '../kanbanStoreClient'
import { CardRefChip } from './CardRefChip'

/**
 * The kanban cards one email was filed onto, as chips, for a mail reader.
 * A card that became a ticket because of this email says so.
 *
 * `locator` is the mailstack locator `<account_id>:<message id>`, the ref the
 * classifier's `email` card links carry. Renders nothing on a host that
 * proxies no kanban-store, and nothing for an email on no card.
 */
export function CardsForEmail({ locator }: { locator: string }) {
  const { fetch: fetchFn, kanbanStoreBasePath } = useBridgeConfig()
  const [cards, setCards] = useState<EntityCardView[]>([])
  const [ticketFromThisEmail, setTicketFromThisEmail] = useState<Set<string>>(new Set())
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!kanbanStoreBasePath || !locator) return
    let canceled = false
    setCards([])
    setTicketFromThisEmail(new Set())
    setError(null)
    void (async () => {
      const result = await listCardsForEntity(fetchFn, kanbanStoreBasePath, 'email', locator)
      if (canceled) return
      if (!result.ok) {
        setError(result.error)
        return
      }
      setCards(result.value)
      // An email lands on a handful of cards at most, so one ticket read each.
      const tickets = await Promise.all(result.value.map(card => getCardTicket(fetchFn, kanbanStoreBasePath, card.card_id)))
      if (canceled) return
      const fromThisEmail = new Set<string>()
      tickets.forEach((ticket, index) => {
        if (!ticket.ok) {
          setError(ticket.error)
          return
        }
        const source = ticket.value?.ticket
        if (source?.source_entity_type === 'email' && source.source_entity_ref === locator) {
          fromThisEmail.add(result.value[index].card_id)
        }
      })
      setTicketFromThisEmail(fromThisEmail)
    })()
    return () => { canceled = true }
  }, [fetchFn, kanbanStoreBasePath, locator])

  if (!kanbanStoreBasePath || (cards.length === 0 && !error)) return null
  return (
    <div className="bet-email-cards">
      <span className="bet-email-cards-label">On {cards.length === 1 ? 'card' : 'cards'}:</span>
      {cards.map(card => (
        <span key={card.card_id} className="bet-email-card">
          <CardRefChip cardID={card.card_id} />
          {ticketFromThisEmail.has(card.card_id) && (
            <span className="bet-arrival bet-arrival-new" title="This email made the card a ticket">ticket from this email</span>
          )}
        </span>
      ))}
      {error && <span className="bridge-error">{error}</span>}
    </div>
  )
}
