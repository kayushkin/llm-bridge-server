import { useCallback, useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { CardDetail } from './BridgeKanban'
import { useBridgeConfig } from '../context'
import { useKanban, type AssignmentOutcome } from '../useKanban'
import type {
  CardAssignment,
  CardDetail as KanbanCardDetail,
  CardLink,
  CardView,
  Placement,
} from '@kayushkin/kanban-store-types'
import type { Item as NoteboardItem } from '@kayushkin/noteboard-types'

/**
 * One card, as a page of its own.
 *
 * The board already opens a card in a drawer, and `?card=<id>` links to it
 * there. This is the same view without the board behind it — a card is a unit of
 * work in its own right, and a link to one should not require loading a board to
 * read it.
 *
 * ⚠️ It does NOT call `useKanban(boardId)` to find the card. That would fetch a
 * whole board's columns, cards and links to display one of them, and it cannot
 * even start until the card's board is known. kanban-store has no single-card
 * read — `cardByID` answers PATCH and DELETE and 405s everything else — but the
 * card's four parts are each separately addressable, so the page assembles it:
 *
 *   item        noteboard `/api/items/{id}`      — a card id IS a noteboard item id
 *   placements  kanban    `/api/cards/{id}/placements`
 *   links       kanban    `/api/cards/{id}/links`
 *   assignments kanban    `/api/cards/{id}/assignments`
 *
 * ⚠️ The assignments route EXISTS (kanban-store `internal/api/assignments.go`,
 * `cardAssignments`; measured live 2026-09-09). An earlier version of this
 * page said it did not, left `assignments` undefined, and CardDetail duly
 * reported them "not loaded" — for every card, forever.
 *
 * `useKanban(null)` is mounted only for its verbs. Its board-scoped calls
 * (`createCard`, `createColumn`, `moveCard`) refuse without a board, and this
 * page uses none of them; `patchCard`, `deleteCard` and the link verbs take a
 * card id and are board-independent.
 */
export function BridgeCardPage() {
  const { cardId = '' } = useParams()
  const navigate = useNavigate()
  const { fetch: fetchFn, kanbanStoreBasePath, noteboardBasePath, mailBasePath, mailPagePath, routes } =
    useBridgeConfig()

  // No board: this page never renders a board view, and asking for one would be
  // a request whose response it discards.
  const k = useKanban(null)

  const [item, setItem] = useState<NoteboardItem | null>(null)
  const [placement, setPlacement] = useState<Placement | null>(null)
  const [links, setLinks] = useState<CardLink[]>([])
  const [assignments, setAssignments] = useState<CardAssignment[]>([])
  // The card's shared work state across every board it sits on, as kanban-store
  // reports it on GET /api/cards/{id}.
  const [workState, setWorkState] = useState<Pick<CardView, 'work_state' | 'work_state_source'>>({ work_state: null })
  const [state, setState] = useState<'loading' | 'ready' | 'missing' | 'error'>('loading')

  // ⚠️ Depend on `listCardLinks`, never on the whole `k` object. useKanban
  // returns a fresh object on every one of its renders, and it re-renders on a
  // 15-second poll — so a dependency on `k` re-created `load` every 15 seconds,
  // re-ran the effect, and reloaded a card nobody had asked to reload.
  const listCardLinks = k.listCardLinks

  // Who is on the card. Re-read after every assign/unassign, because the
  // "Assigned" section shows what the server holds, not what was just clicked.
  //
  // ⚠️ A card with nobody on it comes back as a JSON `null`, not `[]` — the Go
  // handler encodes a slice it never allocated — so `ok` is not evidence of a
  // list, the same contract `/placements` has. A non-2xx is thrown, not
  // treated as "nobody": an assignment list the store would not give is
  // unknown, and unknown must not render as empty.
  const loadAssignments = useCallback(async (): Promise<CardAssignment[]> => {
    const res = await fetchFn(`${kanbanStoreBasePath}/api/cards/${encodeURIComponent(cardId)}/assignments`)
    if (!res.ok) throw new Error(`assignments HTTP ${res.status}`)
    const raw: unknown = await res.json()
    return Array.isArray(raw) ? (raw as CardAssignment[]) : []
  }, [fetchFn, kanbanStoreBasePath, cardId])

  const load = useCallback(async (reason: 'first' | 'refresh' = 'first') => {
    if (!cardId || !kanbanStoreBasePath || !noteboardBasePath) return
    // Only the first load blanks the page. A refresh that swapped the card for
    // a spinner would unmount everything below it and take the user's state
    // with it: an expanded email collapses, a half-written timeline note is
    // gone. The card is already on screen and still true — leave it there and
    // swap the data underneath when it arrives.
    if (reason === 'first') {
      setState('loading')
    }
    try {
      const [itemRes, placementsRes, loadedAssignments, cardRes] = await Promise.all([
        fetchFn(`${noteboardBasePath}/api/items/${encodeURIComponent(cardId)}`),
        fetchFn(`${kanbanStoreBasePath}/api/cards/${encodeURIComponent(cardId)}/placements`),
        loadAssignments(),
        fetchFn(`${kanbanStoreBasePath}/api/cards/${encodeURIComponent(cardId)}`),
      ])
      // The ITEM decides whether this card exists. A card with no placement is
      // still a card — an unfiled todo — but a card with no item is a dead link.
      if (!itemRes.ok) {
        setState('missing')
        return
      }
      const loadedItem: NoteboardItem = await itemRes.json()

      // ⚠️ 200 with a JSON `null` body is how kanban-store answers a card id it
      // does not know, so `ok` is not evidence of a list. Same contract the
      // board's deeplink resolution has to survive.
      const rawPlacements: unknown = placementsRes.ok ? await placementsRes.json() : null
      const placements: Placement[] = Array.isArray(rawPlacements) ? rawPlacements : []

      // 404 is kanban-store's answer for a card on no board, which has no shared work
      // state; anything else that is not 2xx is a read that failed, not an absence.
      if (!cardRes.ok && cardRes.status !== 404) throw new Error(`card HTTP ${cardRes.status}`)
      const detail: KanbanCardDetail | null = cardRes.ok ? await cardRes.json() : null

      setItem(loadedItem)
      setPlacement(placements[0] ?? null)
      setWorkState({ work_state: detail?.work_state ?? null, work_state_source: detail?.work_state_source })
      setLinks(await listCardLinks(cardId))
      setAssignments(loadedAssignments)
      setState('ready')
    } catch {
      setState('error')
    }
  }, [cardId, fetchFn, kanbanStoreBasePath, noteboardBasePath, listCardLinks, loadAssignments])

  // After an assign or unassign the outcome is the server's; a refusal (a
  // disabled principal, an unknown id) carries its `{"error"}` text through
  // unchanged so CardDetail can show it beside the picker.
  const afterAssignmentWrite = useCallback(async (outcome: Promise<AssignmentOutcome>): Promise<AssignmentOutcome> => {
    const result = await outcome
    if (result.ok) setAssignments(await loadAssignments())
    return result
  }, [loadAssignments])

  useEffect(() => {
    void load('first')
  }, [load])

  // Back to the board with this card open, so closing the page lands where the
  // drawer would have been rather than on whatever was in history.
  const close = useCallback(() => {
    navigate(`${routes.kanban}?card=${encodeURIComponent(cardId)}`)
  }, [navigate, routes.kanban, cardId])

  if (state === 'loading') return <div className="bi-empty">Loading card…</div>
  if (state === 'missing') return <div className="bi-empty">No card with id {cardId}.</div>
  if (state === 'error') return <div className="bi-empty">Could not load this card.</div>
  if (!item) return <div className="bi-empty">No card with id {cardId}.</div>

  // A card that sits on no board still renders. `CardDetail` needs a placement
  // shape, and a synthesized one with empty ids is honest here: the card exists,
  // it is filed nowhere. The board-scoped verbs it would feed are unused.
  const card: CardView = {
    placement: placement ?? {
      card_id: cardId,
      board_id: '',
      column_id: '',
      position: 0,
      created_at: item.created_at ?? '',
      updated_at: item.updated_at ?? '',
    },
    item,
    links,
    assignments,
    ...workState,
  }

  return (
    // The library's own panel class, so every `bk-drawer-*` descendant rule in
    // the shared stylesheet applies exactly as it does in the drawer. The page
    // supplies the wrapper the drawer's backdrop would otherwise have.
    <div className="bk-drawer bk-card-page">
      <CardDetail
        card={card}
        boardID={card.placement.board_id}
        entityTypes={k.entityTypes}
        onClose={close}
        onPatch={async patch => {
          const ok = await k.patchCard(cardId, patch)
          if (ok) await load('refresh')
          return ok
        }}
        onDetach={async () => {
          // Only reachable when the card is actually on a board; CardDetail
          // hides the control otherwise.
          const ok = await k.detachCard(card.placement.board_id, cardId)
          if (ok) navigate(routes.kanban)
        }}
        onArchive={async () => {
          const ok = await k.archiveCard(cardId)
          if (ok) await load('refresh')
        }}
        onDelete={async hard => {
          const ok = await k.deleteCard(cardId, hard)
          // Deleting from here has nowhere to return to but the board.
          if (ok) navigate(routes.kanban)
        }}
        onAddLink={async (entityType, entityRef, label) => {
          const ok = await k.addCardLink(cardId, entityType, entityRef, label)
          if (ok) setLinks(await listCardLinks(cardId))
          return ok
        }}
        onDeleteLink={async linkID => {
          const ok = await k.deleteCardLink(linkID)
          if (ok) setLinks(await listCardLinks(cardId))
          return ok
        }}
        onAssign={principalID => afterAssignmentWrite(k.assign(cardId, principalID))}
        onUnassign={principalID => afterAssignmentWrite(k.unassign(cardId, principalID))}
        onOpenChat={link => navigate(`${routes.chat}?session=${encodeURIComponent(link.ref)}`)}
        onOpenInMail={(accountID, messageID) => {
          // The host's mail page, if it names one; the drawer hides the control otherwise.
          if (mailPagePath) navigate(`${mailPagePath}?account=${encodeURIComponent(accountID)}&message=${encodeURIComponent(messageID)}`)
        }}
        mailBasePath={mailBasePath}
        fetchFn={fetchFn}
      />
    </div>
  )
}
