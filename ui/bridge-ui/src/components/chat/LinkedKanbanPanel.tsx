import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../../context'
import { RefChip } from './RefChip'
import { preserveUnchangedKanbanPayload, useKanban } from '../../useKanban'
import type { EntityCardView, EntityTag } from '@kayushkin/kanban-store-types'

const DO_NOT_TRACK_TAGS = ['kanban-do-not-track', 'kanban:do-not-track'] as const

// How often an open pane re-reads its session's cards. kanban-store has no SSE
// and no notifier of any kind, so a poll is the only way this pane can learn
// that something moved — and things move on their own: the kanban-curator and
// the classifier both run on a scheduler tick and rewrite cards under it.
// 15 seconds is the cadence every other chat-pane timer already uses
// (Workspace's reachability check, OrchestratorPanel, useKanban's board view).
const LINKED_KANBAN_REFRESH_MS = 15000

export interface LinkedKanbanPanelProps {
  sessionId: string
  onToggleCollapse: () => void
  style?: React.CSSProperties
  paneKey?: string
}

export function LinkedKanbanPanel({ sessionId, onToggleCollapse, style, paneKey }: LinkedKanbanPanelProps) {
  const { routes, kanbanStoreBasePath } = useBridgeConfig()
  const kanban = useKanban(null, { loadBoards: false, loadEntityTypes: false })
  // Depend on the two calls, not on the hook's whole return value. That value
  // is a useMemo over the hook's state as well as its callbacks, so it takes a
  // new identity when the hook's own mount fetch flips `loading` — which would
  // re-run the refresh effect milliseconds after the first one, issuing a
  // second pair of requests and re-arming the interval. These two are
  // useCallbacks over fetchFn, kanbanStoreBasePath and enabled, all of which
  // come from a memoized provider config, so they hold still.
  const { listCardsForEntity, listEntityTags } = kanban
  const [cards, setCards] = useState<EntityCardView[]>([])
  const [tags, setTags] = useState<EntityTag[]>([])
  const [loading, setLoading] = useState(false)
  const [toggling, setToggling] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const requestInFlight = useRef(false)

  // background: this refresh came from the timer rather than from the user
  // opening the pane or toggling tracking. It stays silent — no "Loading…" in
  // place of cards that are already on screen — and it yields to a request that
  // is still running instead of stacking a second one behind it.
  const refresh = useCallback(async ({ background = false } = {}) => {
    if (!kanbanStoreBasePath || !sessionId) {
      setCards([])
      setTags([])
      setError(null)
      return
    }
    if (background && requestInFlight.current) return
    requestInFlight.current = true
    if (!background) setLoading(true)
    try {
      const [nextCards, nextTags] = await Promise.all([
        listCardsForEntity('session', sessionId),
        listEntityTags('session', sessionId),
      ])
      setCards(prev => preserveUnchangedKanbanPayload(prev, nextCards))
      setTags(prev => preserveUnchangedKanbanPayload(prev, nextTags))
      setError(null)
    } catch (err) {
      // Keep whatever is on screen. A failed read tells us nothing about the
      // session's cards, and blanking the list would claim it has none.
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      requestInFlight.current = false
      if (!background) setLoading(false)
    }
  }, [listCardsForEntity, listEntityTags, kanbanStoreBasePath, sessionId])

  useEffect(() => {
    refresh()
    if (!kanbanStoreBasePath || !sessionId) return
    const timer = setInterval(() => { refresh({ background: true }) }, LINKED_KANBAN_REFRESH_MS)
    return () => clearInterval(timer)
  }, [refresh, kanbanStoreBasePath, sessionId])

  const doNotTrackTags = useMemo(
    () => tags.filter(tag => DO_NOT_TRACK_TAGS.includes(tag.tag as typeof DO_NOT_TRACK_TAGS[number])),
    [tags]
  )
  const trackingDisabled = doNotTrackTags.length > 0

  const handleToggleTracking = useCallback(async () => {
    if (!sessionId) return
    setToggling(true)
    setError(null)
    try {
      if (trackingDisabled) {
        await Promise.all(doNotTrackTags.map(tag => kanban.deleteEntityTag('session', sessionId, tag.tag)))
      } else {
        const ok = await kanban.addEntityTag('session', sessionId, DO_NOT_TRACK_TAGS[0])
        if (!ok) throw new Error('failed to add do-not-track tag')
      }
      await refresh()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setToggling(false)
    }
  }, [doNotTrackTags, kanban, refresh, sessionId, trackingDisabled])

  const stop = (e: React.MouseEvent | React.KeyboardEvent) => e.stopPropagation()

  const summary = loading
    ? 'Loading linked cards…'
    : cards.length > 0
      ? `${cards.length} linked ${cards.length === 1 ? 'card' : 'cards'}`
      : 'No linked cards yet'

  const interactive = !!kanbanStoreBasePath && !!sessionId

  return (
    <div className="bc-split-pane bc-split-pane-kanban" style={style} data-pane={paneKey}>
      <div
        className="bc-split-pane-header bc-header-clickable"
        onClick={onToggleCollapse}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onToggleCollapse() } }}
        role="button"
        tabIndex={0}
        title="Hide kanban"
        aria-label="Hide kanban"
      >
        <span className="bc-split-pane-title">Linked Work</span>
        <span className="bc-spacer" />
        {interactive && (
          <>
            <button
              type="button"
              className={`bc-linked-kanban-btn ${trackingDisabled ? 'bc-linked-kanban-btn-active' : ''}`}
              onClick={e => { stop(e); handleToggleTracking() }}
              onKeyDown={stop}
              disabled={toggling}
              title={trackingDisabled ? 'Allow classifier tracking again for this session' : 'Mark this session as do-not-track for kanban classifier'}
            >
              {trackingDisabled ? 'Resume tracking' : 'Do not track'}
            </button>
            <Link
              className="bc-linked-kanban-link"
              to={routes.kanban}
              onClick={stop}
            >Open Kanban</Link>
          </>
        )}
        <span className="bc-split-collapse-btn" aria-hidden="true">×</span>
      </div>
      <div className="bc-linked-kanban-body">
        {!interactive ? (
          <div className="bc-linked-kanban-summary">No active session.</div>
        ) : (
          <>
            <div className="bc-linked-kanban-summary">{summary}</div>
            {error && <div className="bridge-error">{error}</div>}
            {trackingDisabled && (
              <div className="bc-linked-kanban-note">
                Classifier auto-tracking is disabled for this session.
              </div>
            )}
            {cards.length > 0 && (
              <div className="bc-linked-kanban-list">
                {cards.map(card => {
                  const item = card.item
                  const cardSummary = summarizeBody(item?.body)
                  return (
                    <article key={card.card_id} className="bc-linked-kanban-card">
                      <div className="bc-linked-kanban-card-head">
                        {/* The chat's own reference chip, not a bare anchor.
                            A card id IS a noteboard todo id, so a linked card is
                            the same thing a `[todo:…]` reference in the transcript
                            points at, and it should read and behave the same way
                            in both places — glyph, title, and a panel carrying
                            status, priority, tags and due date, with "Open on the
                            board" as its destination.

                            ⚠️ The chip resolves its own title from noteboard, and
                            this pane already holds the item, so a card costs one
                            request it did not before. Accepted here and worth
                            knowing: the pane shows a handful of cards, keys are
                            stable across its 15s poll so nothing remounts or
                            refetches, and the alternative was a second rendering
                            of a reference this fleet already has one of. It would
                            NOT be acceptable per row in a virtualized list. */}
                        <div className="bc-linked-kanban-card-title">
                          <RefChip kind="todo" refId={card.card_id} />
                        </div>
                        {item?.status && <span className="bc-linked-kanban-chip">{item.status}</span>}
                      </div>
                      {cardSummary && <div className="bc-linked-kanban-card-summary">{cardSummary}</div>}
                      <div className="bc-linked-kanban-card-meta">
                        <span className="bc-linked-kanban-card-id">{card.card_id.slice(0, 8)}</span>
                        {item?.priority ? <span className="bc-linked-kanban-chip">P{item.priority}</span> : null}
                        {item?.tags?.slice(0, 3).map(tag => <span key={tag} className="bc-linked-kanban-chip">#{tag}</span>)}
                      </div>
                    </article>
                  )
                })}
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}

function summarizeBody(body?: string | null): string {
  if (!body) return ''
  const cleaned = body
    .replace(/[*_`>#-]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
  if (!cleaned) return ''
  return cleaned.length > 180 ? `${cleaned.slice(0, 177)}...` : cleaned
}
