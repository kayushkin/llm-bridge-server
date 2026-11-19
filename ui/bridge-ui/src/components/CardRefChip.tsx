import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import { RefChip } from './chat/RefChip'

/**
 * A kanban card as a reference chip, with a link to its page.
 *
 * A card id is its noteboard item id, so the chip is the noteboard chip: it
 * shows the card's title and opens its details. The ↗ beside it opens the
 * card page, drawn only when the host routes one. Must be mounted inside
 * `ChatProvider`, as {@link RefChip} is; `<Bridge>` provides it.
 */
export function CardRefChip({ cardID }: { cardID: string }) {
  const { routes } = useBridgeConfig()
  return (
    <span className="bet-card-ref">
      <RefChip kind="todo" refId={cardID} />
      {routes.card && (
        <Link className="bet-card-open" to={`${routes.card}/${encodeURIComponent(cardID)}`} title="Open this card as a page">↗</Link>
      )}
    </span>
  )
}
