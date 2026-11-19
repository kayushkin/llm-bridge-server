import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { BridgeContext, useBridgeConfig } from '../context'
import {
  KANBAN_SERVER_PARAM, kanbanLastBoardStorageKey, kanbanServerNamed, kanbanServerParamValue, kanbanSessionHref,
  serverBoardsHeading, type KanbanServer, type ServerBoards,
} from '../kanbanServers'
import { serverBoardsKey, useBoardsOnServers, useKanbanServers } from '../useKanbanServers'
import { peerConnectionBridgeConfig } from '../peers'
import type { FetchFn } from '../types'
import { cleanEmailBodyForPreview } from '../emailText'
import { useKanban, type AssignmentOutcome } from '../useKanban'
import {
  pickablePrincipals, principalInitials, principalIsDisabled, usePrincipals,
  type PrincipalKindFilter, type PrincipalsDirectory,
} from '../usePrincipals'
import type { CardAssignment, CardLink, CardView, ColumnView, DefaultSource, Placement } from '@kayushkin/kanban-store-types'
import type { Item as NoteboardItem } from '@kayushkin/noteboard-types'
import type { MailMessage } from '../types-mailstack'
import { formatAgeCompact } from '../utils'
import { entityTarget, isLocalPathRef } from '../entityLinks'
import { CardBudgetBadge, CardTimelinePanel, hasClockData } from './CardTime'
import { readAgentPrompt, stripAgentPrompt, writeAgentPrompt, suggestAgentPrompt } from '../agentPrompt'
import { dispatchAgentOnCard } from '../agentDispatch'
import { useBridgeInstances } from '../useBridgeInstances'
import { useBridgeMachines } from '../useBridgeMachines'
import { fetchSessionRunsOn, useSessionRunsOn } from '../sessionRunsOn'
import { dispatchInstanceChoices, machineLabel } from '../grantResources'
import { useInstancesListedForPrincipals } from '../useResourceCatalogs'
import { useBundles } from '../useBundles'
import {
  CARD_BUNDLE_HELP_TEXT, bundleLabelForID, defaultInstanceLabel, defaultSourceLabel, describeInstanceLocation,
  dispatchTargetForCard, dispatchTargetNote,
} from '../kanbanBoardSettings'
import { getCardEffectiveDefaults } from '../kanbanStoreClient'
import { useCardEffectiveDefaults } from '../useCardEffectiveDefaults'
import { SIGNAL_KIND_QUESTION, groupSignalsByRequest, type Signal } from '@kayushkin/chat-core'
import { SignalRequestCard } from './chat/SignalCard'
import { useOpenSignalsByTodo, useOpenSignalsForTodo } from '../kanbanSignals'
import { fetchNoteboardItemRef } from './noteboardItemRef'
import {
  CARD_AXES, allCardsOf, axisUsage, filterIsActive, matchesFilter,
  parseEmailLocator, sortCards, withAxisValue,
  type AxisFilter, type SortKey,
} from '../kanbanAxes'

// A card's link to the agent session working it. Post session-consolidation
// (2026-05-09) every card_link with entity_type='session' carries the
// canonical llm-bridge-server session_id, which is a direct deeplink target.
type SessionLinkRef = {
  ref: string
  // When the card was handed to this agent. Read straight off the link row, so
  // it costs nothing — the board view already carries every link.
  dispatchedAt: string
}
type OpenChatFn = (link: SessionLinkRef) => void

// latestSessionLink returns the most recently attached session, which is the one
// that describes what is happening to the card now.
//
// Cards carry a single session link today, so newest and first are the same row
// and the old "return the first match" behavior was never visibly wrong. It was
// only ever right by accident: nothing stops a second dispatch adding a second
// link, and on the day that happens, first-match silently reports the oldest
// agent as the current one.
function latestSessionLink(card: CardView): SessionLinkRef | null {
  let newest: { ref: string; dispatchedAt: string; at: number } | null = null
  for (const l of card.links ?? []) {
    if (l.entity_type !== 'session' || !l.entity_ref) continue
    // An unparseable timestamp sorts oldest rather than winning by accident.
    const at = new Date(l.created_at).getTime()
    const rank = Number.isFinite(at) ? at : -Infinity
    if (!newest || rank > newest.at) {
      newest = { ref: l.entity_ref, dispatchedAt: l.created_at, at: rank }
    }
  }
  return newest ? { ref: newest.ref, dispatchedAt: newest.dispatchedAt } : null
}

const LAYOUT_KEY = 'bk:layout'

/** A board on another server, as the board picker's option value. It cannot
 *  collide with a board id on the shown server, which never holds a newline. */
function otherServerBoardValue(server: KanbanServer, boardID: string): string {
  return `${serverBoardsKey(server)}\n${boardID}`
}
const COLLAPSED_COLUMNS_KEY = 'bk:collapsedColumns'
const DEFAULT_BOARD_NAME = 'Agent runs'

/** How many cards a column loads at a time, and how many each "show more" adds. */
const CARDS_PER_COLUMN = 25

// How many linked emails a card drawer shows before collapsing the rest behind a
// button. Bucket cards on the Email board accumulate every message from a sender,
// so this list grows without bound while the card itself stays one thing.
const EMAIL_LINKS_SHOWN = 25

type Layout = 'horizontal' | 'vertical'

function loadCollapsedColumns(): Set<string> {
  if (typeof localStorage === 'undefined') return new Set()
  try {
    const raw = localStorage.getItem(COLLAPSED_COLUMNS_KEY)
    if (!raw) return new Set()
    const arr = JSON.parse(raw)
    return Array.isArray(arr) ? new Set(arr.filter((x): x is string => typeof x === 'string')) : new Set()
  } catch {
    return new Set()
  }
}

/**
 * Kanban page. Boards stay on the server that holds them: `?server=` names a
 * connected server by its own id, and the board below then runs under that
 * server's config (`peerConnectionBridgeConfig`), reading its kanban-store
 * through the connection's proxy. No `?server=` is this server. The board
 * picker lists every server's boards, each server's under its own heading.
 */
export function BridgeKanban() {
  const config = useBridgeConfig()
  const [searchParams, setSearchParams] = useSearchParams()
  const { servers, loaded, error } = useKanbanServers(config)
  const serverParam = searchParams.get(KANBAN_SERVER_PARAM)
  const server = kanbanServerNamed(servers, serverParam)
  const connectionId = server?.connectionId ?? null
  const serverConfig = useMemo(
    () => (connectionId ? peerConnectionBridgeConfig(config, connectionId) : config),
    [config, connectionId],
  )
  const boardsByServer = useBoardsOnServers(servers, config)
  const pickBoardOnServer = useCallback((target: KanbanServer, boardID: string) => {
    localStorage.setItem(kanbanLastBoardStorageKey(target), boardID)
    setSearchParams(prev => {
      const next = new URLSearchParams(prev)
      const value = kanbanServerParamValue(target)
      if (value) next.set(KANBAN_SERVER_PARAM, value)
      else next.delete(KANBAN_SERVER_PARAM)
      // A card id belongs to the server it is on.
      next.delete('card')
      return next
    })
  }, [setSearchParams])

  if (!server) {
    if (!loaded) return <div className="bi-loading">Loading servers…</div>
    return (
      <div className="bridge-error">
        No server this one is connected to has the id {serverParam}.{error ? ` ${error}` : ''}{' '}
        <Link to="?">Show this server's boards</Link>
      </div>
    )
  }
  if (server.blockedReason) {
    return (
      <div className="bridge-error">
        {server.label}'s boards cannot be read now: {server.blockedReason}.{' '}
        <Link to="?">Show this server's boards</Link>
      </div>
    )
  }
  return (
    <BridgeContext.Provider value={serverConfig}>
      <KanbanBoardOnServer
        // Board, drawer and filters belong to one server; a new server starts fresh.
        key={serverBoardsKey(server)}
        server={server}
        servers={servers}
        boardsByServer={boardsByServer}
        onPickBoardOnServer={pickBoardOnServer}
      />
    </BridgeContext.Provider>
  )
}

interface KanbanBoardOnServerProps {
  /** The server whose board is shown; the config above is that server's. */
  server: KanbanServer
  /** Every listed server, this one first, for the board picker. */
  servers: readonly KanbanServer[]
  /** Each server's boards by `serverBoardsKey`, for the other servers' groups. */
  boardsByServer: ReadonlyMap<string, ServerBoards>
  onPickBoardOnServer: (server: KanbanServer, boardID: string) => void
}

/** One server's board: the header, columns and card drawer, all read through
 *  the config its parent provides. */
function KanbanBoardOnServer({ server, servers, boardsByServer, onPickBoardOnServer }: KanbanBoardOnServerProps) {
  const lastBoardKey = kanbanLastBoardStorageKey(server)
  const [selectedBoardID, setSelectedBoardID] = useState<string | null>(null)
  // ⚠️ The open card lives in the URL and NOWHERE else — there is deliberately no
  // `useState` mirroring it.
  //
  // A card is shareable ("look at this one"), so `?card=<id>` had to exist. The
  // obvious shape is local state plus two effects syncing it to the query string,
  // and that is the shape that loops: each effect observes the other's write and
  // writes back. `sessionDeeplink.ts` carries a small state machine precisely to
  // survive that, because the chat's `activeId` is genuine state owned by a store.
  // The drawer's is not — nothing but this component ever decides which card is
  // open — so making the query string the only copy removes the synchronisation
  // problem rather than managing it.
  //
  // The id IS a noteboard item id (see the signals comment below), so the link is
  // also a stable reference to the todo the card is made of.
  const [searchParams, setSearchParams] = useSearchParams()
  const drawerCardID = searchParams.get('card')
  const setDrawerCardID = useCallback(
    (cardID: string | null) => {
      setSearchParams(
        prev => {
          const next = new URLSearchParams(prev)
          if (cardID) next.set('card', cardID)
          else next.delete('card')
          return next
        },
        // Replace rather than push: opening and closing a drawer half a dozen
        // times while reading a board would otherwise bury the page the user
        // arrived from under a stack of its own states.
        { replace: true },
      )
    },
    [setSearchParams],
  )
  const [showNewBoard, setShowNewBoard] = useState(false)
  const [showNewColumn, setShowNewColumn] = useState(false)
  const [composeColumn, setComposeColumn] = useState<string | null>(null)
  const [layout, setLayout] = useState<Layout>(() => {
    if (typeof localStorage === 'undefined') return 'horizontal'
    return localStorage.getItem(LAYOUT_KEY) === 'vertical' ? 'vertical' : 'horizontal'
  })
  const [collapsedColumns, setCollapsedColumns] = useState<Set<string>>(loadCollapsedColumns)
  // Axis filter and sort are view state only: they never write to the board, so
  // two people can look at the same board through different lenses.
  const [axisFilter, setAxisFilter] = useState<AxisFilter>({})
  // Priority-then-due-date is the default because the stored order is arbitrary:
  // every writer on this host creates cards at position 0, so "board order" is
  // really "reverse order of creation" and says nothing about what to do first.
  const [sortKey, setSortKey] = useState<SortKey>('priority')

  const {
    routes, mailBasePath, mailPagePath, basePath: bridgeBasePath, fetch: fetchFn,
    // Read directly as well as through useKanban: resolving a deeplinked card's
    // board is a one-off request that hook has no verb for.
    kanbanStoreBasePath,
  } = useBridgeConfig()
  // Who a dispatched session is started as: the Settings page's default.
  const navigate = useNavigate()
  const openSessionLink = (link: SessionLinkRef) => {
    navigate(kanbanSessionHref(routes.chat, server, link.ref))
  }

  // A screenful a column. The largest board here holds 6,466 cards and answered
  // 12 MB per read before this cap; the rest arrive when asked for.
  const k = useKanban(selectedBoardID, { columnPageSize: CARDS_PER_COLUMN })
  // One directory for every tile's assignee chips. Resolved here, not per tile:
  // the hook shares one request either way, but hundreds of subscriptions for
  // one answer is still hundreds of subscriptions.
  const principals = usePrincipals()
  // The tile's 🤖 names where the agent will run in its confirmation, so the
  // board reads the bridge's instance and machine lists (one shared poll each).
  const { instances: bridgeInstances } = useBridgeInstances()
  const { machines: bridgeMachines } = useBridgeMachines()
  // ...and the bundle list, so the confirmation names the board's bundle.
  const { bundles: bundlesOnBoard } = useBundles()
  // A card id IS a noteboard todo id here, so the signals a session raised
  // against its todo land on the card that todo already has. The map covers
  // every todo, so switching boards needs no refetch.
  const signalsByTodo = useOpenSignalsByTodo()

  useEffect(() => {
    localStorage.setItem(LAYOUT_KEY, layout)
  }, [layout])

  useEffect(() => {
    localStorage.setItem(COLLAPSED_COLUMNS_KEY, JSON.stringify([...collapsedColumns]))
  }, [collapsedColumns])

  const toggleColumnCollapsed = (columnID: string) => {
    setCollapsedColumns(prev => {
      const next = new Set(prev)
      if (next.has(columnID)) next.delete(columnID); else next.add(columnID)
      return next
    })
  }

  useEffect(() => {
    if (selectedBoardID) localStorage.setItem(lastBoardKey, selectedBoardID)
  }, [selectedBoardID, lastBoardKey])

  // Pick the initial board: last-opened (if it still exists) → Agent Runs → first.
  useEffect(() => {
    if (selectedBoardID) return
    if (k.boards.length === 0) return
    const last = localStorage.getItem(lastBoardKey)
    if (last && k.boards.some(b => b.id === last)) {
      setSelectedBoardID(last)
      return
    }
    const named = k.boards.find(b => b.name === DEFAULT_BOARD_NAME)
    if (named) {
      setSelectedBoardID(named.id)
      return
    }
    setSelectedBoardID(k.boards[0].id)
  }, [k.boards, selectedBoardID, lastBoardKey])

  // Deep link into the host's mail page. The account is carried alongside the
  // message id because mailstack requires it on every read — a message id alone
  // is not addressable there.
  const openEmailInMail = (accountID: string, messageID: string) => {
    if (!mailPagePath) return
    navigate(`${mailPagePath}?account=${encodeURIComponent(accountID)}&message=${encodeURIComponent(messageID)}`)
  }

  const drawerCard: CardView | null = useMemo(() => {
    if (!drawerCardID || !k.view) return null
    for (const col of k.view.columns) {
      for (const c of col.cards ?? []) {
        if (c.placement.card_id === drawerCardID) return c
      }
    }
    return null
  }, [drawerCardID, k.view])

  // A deeplinked card is usually on some OTHER board than the one that opened.
  //
  // `drawerCard` above searches the loaded board's view, so without this a
  // `?card=` link silently opened nothing whenever the recipient's last-opened
  // board was not the card's — which is the common case, and indistinguishable
  // from a dead link. kanban-store answers "which boards is this card on?"
  // directly, so the fix is one request rather than loading every board.
  //
  // Guarded by a ref keyed on the card id, not a boolean: the effect re-runs when
  // the view arrives, and an unguarded version would re-request on every render
  // for a card that genuinely is on no board. One attempt per id, and a card that
  // resolves to nothing leaves the board selection alone rather than clearing it.
  const boardResolveAttempted = useRef<string | null>(null)
  useEffect(() => {
    if (!drawerCardID || drawerCard || !kanbanStoreBasePath) return
    // Wait for the view: mid-load it is null and the card may well be in it.
    if (!k.view) return
    if (boardResolveAttempted.current === drawerCardID) return
    boardResolveAttempted.current = drawerCardID

    let canceled = false
    void (async () => {
      try {
        const res = await fetchFn(
          `${kanbanStoreBasePath}/api/cards/${encodeURIComponent(drawerCardID)}/placements`,
        )
        if (!res.ok || canceled) return
        // ⚠️ kanban-store answers a card id it does not know with 200 and a JSON
        // `null` body, NOT a 404 — measured against the live service. So `res.ok`
        // is not evidence that a card exists, and annotating this `Placement[]`
        // was a claim the wire does not honor.
        //
        // ⚪ This check changes NO observable answer, and that was measured
        // rather than assumed. Removing it and running the browser spec leaves
        // all nine cases green: `.find` throws on the null, the catch below
        // swallows it, and the outcome — no board change, no drawer, one
        // request — is identical. A `pageerror` assertion cannot see it either,
        // because the throw is handled by design.
        //
        // It is kept for the TYPE, not the control flow: the response was
        // annotated `Placement[]`, which is a claim the wire does not honor, so
        // `unknown` plus a checked narrowing is the honest shape. Anyone
        // tempted to restore the annotation should know it was measured false.
        //
        // ⚠️ Do NOT cite this as precedent for keeping a defensive guard.
        // `dash/src/pages/chat/panePersistence.ts` DELETED an `Array.isArray`
        // guard for exactly this property and recorded the measurement in its
        // place. The only thing earning this one its keep is the type.
        const placements: unknown = await res.json()
        if (!Array.isArray(placements) || canceled) return
        const target = (placements as Placement[]).find(
          p => p.board_id && p.board_id !== selectedBoardID,
        )
        if (target?.board_id && !canceled) setSelectedBoardID(target.board_id)
      } catch {
        // A link to a card that no longer exists is a dead link, not an error
        // state for the whole board — the drawer simply stays shut.
      }
    })()
    return () => {
      canceled = true
    }
  }, [drawerCardID, drawerCard, k.view, kanbanStoreBasePath, fetchFn, selectedBoardID])

  // Axis controls render only for boards whose cards actually carry these tags.
  // Boards that predate the classifier report no axes and are left exactly as
  // they were.
  const axes = useMemo(() => (k.view ? axisUsage(allCardsOf(k.view.columns)) : []), [k.view])

  // Filtering and sorting are applied to a copy. The board view itself stays
  // untouched so the drawer, the delete-column count and the orphan list keep
  // reporting what is really on the board rather than what survived the filter.
  const visibleColumns: ColumnView[] = useMemo(() => {
    if (!k.view) return []
    return k.view.columns.map(cv => ({
      ...cv,
      cards: sortCards((cv.cards ?? []).filter(c => matchesFilter(c, axisFilter)), sortKey),
    }))
  }, [k.view, axisFilter, sortKey])

  const hiddenCardCount = k.view
    ? allCardsOf(k.view.columns).length - allCardsOf(visibleColumns).length
    : 0

  // This server's boards come from useKanban, so a board made or deleted here
  // shows at once; another server's come from the page's one read of each.
  const thisServersOptions = k.boards.map(b => (
    <option key={b.id} value={b.id}>
      {b.name}{b.archived ? ' (archived)' : ''}
    </option>
  ))
  const otherServerBoardChoices = new Map<string, { server: KanbanServer; boardID: string }>()
  for (const listed of servers) {
    if (serverBoardsKey(listed) === serverBoardsKey(server)) continue
    const boards = boardsByServer.get(serverBoardsKey(listed))
    if (boards?.state !== 'loaded') continue
    for (const b of boards.boards) otherServerBoardChoices.set(otherServerBoardValue(listed, b.id), { server: listed, boardID: b.id })
  }

  return (
    <div className="bk-container">
      <main className="bk-main">
        {k.error && <div className="bridge-error">{k.error}</div>}

        <div className="bk-board-header">
          <div className="bk-board-header-main">
            <select
              className="bk-board-select"
              value={selectedBoardID ?? ''}
              onChange={e => {
                const other = otherServerBoardChoices.get(e.target.value)
                if (other) onPickBoardOnServer(other.server, other.boardID)
                else setSelectedBoardID(e.target.value || null)
              }}
            >
              {!selectedBoardID && <option value="">— select board —</option>}
              {servers.length === 1 ? thisServersOptions : servers.map(listed => {
                if (serverBoardsKey(listed) === serverBoardsKey(server)) {
                  return <optgroup key={serverBoardsKey(listed)} label={listed.label}>{thisServersOptions}</optgroup>
                }
                const boards = boardsByServer.get(serverBoardsKey(listed)) ?? { state: 'loading' as const }
                return (
                  <optgroup key={serverBoardsKey(listed)} label={serverBoardsHeading(listed, boards)}>
                    {boards.state === 'loaded'
                      ? boards.boards.map(b => (
                        <option key={b.id} value={otherServerBoardValue(listed, b.id)}>
                          {b.name}{b.archived ? ' (archived)' : ''}
                        </option>
                      ))
                      : <option disabled value="">{boards.state === 'failed' ? boards.error : boards.state === 'blocked' ? boards.reason : 'Reading…'}</option>}
                  </optgroup>
                )
              })}
            </select>
            <button className="bi-add-btn" onClick={() => setShowNewBoard(s => !s)}>+ New Board</button>
            {selectedBoardID && routes.kanbanSettings && (
              <Link
                className="bi-add-btn bk-board-settings-link"
                to={`${routes.kanbanSettings}?board=${encodeURIComponent(selectedBoardID)}`}
                title="This board's defaults, classifier, priority ladder and business hours"
              >⚙ Board settings</Link>
            )}
            {k.view?.board.description && (
              <p className="bk-board-desc">{k.view.board.description}</p>
            )}
          </div>
          <div className="bk-board-actions">
            <button
              className="bi-add-btn"
              onClick={() => setLayout(l => l === 'horizontal' ? 'vertical' : 'horizontal')}
              title={layout === 'horizontal' ? 'Switch to vertical (stacked) columns' : 'Switch to horizontal (side-by-side) columns'}
            >
              {layout === 'horizontal' ? 'Vertical layout' : 'Horizontal layout'}
            </button>
            {selectedBoardID && k.view && (
              <>
                <button className="bi-add-btn" onClick={() => setShowNewColumn(s => !s)}>+ Column</button>
                <button
                  className="bi-add-btn"
                  onClick={async () => {
                    if (!confirm(`Delete board "${k.view!.board.name}"? Cards remain in noteboard.`)) return
                    const ok = await k.deleteBoard(k.view!.board.id)
                    if (ok) setSelectedBoardID(null)
                  }}
                >Delete board</button>
              </>
            )}
          </div>
        </div>

        {showNewBoard && (
          <NewBoardForm
            onCreate={async (args) => {
              const b = await k.createBoard(args)
              if (b) { setSelectedBoardID(b.id); setShowNewBoard(false) }
            }}
            onCancel={() => setShowNewBoard(false)}
          />
        )}

        {!selectedBoardID ? (
          k.boards.length === 0 && !k.loading
            ? <div className="bi-empty">No boards. Create one to start.</div>
            : <div className="bi-empty">Select a board.</div>
        ) : !k.view ? (
          <div className="bi-loading">Loading…</div>
        ) : (
          <>
            {showNewColumn && (
              <NewColumnForm
                onCreate={async (args) => {
                  const ok = await k.createColumn(args)
                  if (ok) setShowNewColumn(false)
                }}
                onCancel={() => setShowNewColumn(false)}
              />
            )}

            {axes.length > 0 && (
              <CardAxisToolbar
                axes={axes}
                filter={axisFilter}
                onFilterChange={setAxisFilter}
                sortKey={sortKey}
                onSortChange={setSortKey}
                hiddenCardCount={hiddenCardCount}
              />
            )}

            <div className={`bk-columns bk-columns-${layout}`}>
              {visibleColumns.map(cv => (
                <ColumnPane
                  key={cv.column.id}
                  cv={cv}
                  onLoadMore={() => k.loadMoreCards(cv.column.id, CARDS_PER_COLUMN)}
                  signalsByTodo={signalsByTodo}
                  principals={principals}
                  boardColumns={k.view!.columns.map(c => c.column)}
                  collapsed={collapsedColumns.has(cv.column.id)}
                  onToggleCollapse={() => toggleColumnCollapsed(cv.column.id)}
                  onCompose={() => setComposeColumn(cv.column.id)}
                  composeOpen={composeColumn === cv.column.id}
                  onCancelCompose={() => setComposeColumn(null)}
                  onCreateCard={async (args) => {
                    const ok = await k.createCard({ ...args, column_id: cv.column.id })
                    if (ok) setComposeColumn(null)
                  }}
                  onMoveCard={(cardID, columnID) => k.moveCard(cardID, columnID)}
                  onOpenCard={(cardID) => setDrawerCardID(cardID)}
                  onOpenChat={openSessionLink}
                  onStopCard={async (cardID) => {
                    // Parking work nobody is doing yet is cheap and reversible, so
                    // it just happens. Interrupting an agent mid-turn is not the
                    // same act, and the card's own session link is what tells the
                    // two apart — so only that case asks.
                    const card = cv.cards?.find(c => c.placement.card_id === cardID)
                    if (card && latestSessionLink(card)) {
                      if (!confirm('This card has a running session. Stop will pause the agent mid-turn (resumable) and park the work. Continue?')) return false
                    }
                    return k.stopCard(cardID)
                  }}
                  onPlayCard={(cardID) => k.playCard(cardID)}
                  onRunAgent={async (card) => {
                    const item = card.item
                    const title = (item?.title as string) ?? card.placement.card_id
                    // The tile sends exactly what the drawer would: the saved
                    // prompt, or the same suggestion the drawer would show. A
                    // shortcut that dispatched something different from what
                    // the card displays would be a trap.
                    const stored = readAgentPrompt(item?.body as string | undefined)
                    const prompt = stored ?? suggestAgentPrompt({
                      cardID: card.placement.card_id,
                      title,
                      body: stripAgentPrompt(item?.body as string | undefined),
                      linkedEmailCount: (card.links ?? []).filter(l => l.entity_type === 'email').length,
                    })

                    // An autonomous session auto-allows tool calls and spends
                    // money, and this button sits on a tile next to two others.
                    // One misclick should not silently start an agent, so the
                    // confirmation names the card, says whether the prompt was
                    // written or merely suggested, and warns about a second
                    // agent when one is already attached.
                    //
                    // Where it runs is a choice, and a tile has no room to offer
                    // one. A card whose last session ran somewhere reuses that
                    // instance and names it in the confirmation. A card with no
                    // session yet opens instead, so the choice is made in the
                    // drawer's "Runs on" select rather than guessed here.
                    const existing = latestSessionLink(card)
                    const openToChoose = (why?: string) => {
                      if (why) window.alert(`${why} Open the card to choose where the agent runs.`)
                      setDrawerCardID(card.placement.card_id)
                      return false
                    }
                    // A card with no session yet runs on its effective default
                    // instance — a matching tag rule's, else the board's — when
                    // that is one the bridge lists enabled; only a card with
                    // neither opens to choose. The defaults are kanban-store's
                    // resolution for this card's tags, read fresh here so the
                    // confirmation names what the dispatch will really use; a
                    // failed read starts nothing.
                    const defaultsResult = await getCardEffectiveDefaults(fetchFn, kanbanStoreBasePath, card.placement.board_id, card.placement.card_id)
                    if (!defaultsResult.ok) {
                      window.alert(`Could not read this card's defaults, so no agent was started: ${defaultsResult.error}`)
                      return false
                    }
                    const cardDefaults = defaultsResult.value.defaults
                    let lastInstanceID: string | null = null
                    if (existing) {
                      try {
                        lastInstanceID = (await fetchSessionRunsOn(fetchFn, bridgeBasePath, existing.ref)).instanceID
                      } catch (e) {
                        return openToChoose(`Could not tell where this card's last session ran (${e instanceof Error ? e.message : String(e)}).`)
                      }
                    }
                    const target = dispatchTargetForCard({
                      lastSessionInstanceID: lastInstanceID,
                      defaultInstance: cardDefaults.default_instance_id ?? null,
                      instances: bridgeInstances,
                    })
                    if (target.kind === 'none') return openToChoose(target.why ? `${target.why[0].toUpperCase()}${target.why.slice(1)}.` : undefined)
                    const instance = target.instance
                    const bundleDefault = cardDefaults.default_bundle_id
                    const bundleID = bundleDefault?.value
                    const lines = [
                      `Start an agent on "${title}"?`,
                      '',
                      `Runs on ${describeInstanceLocation(instance, bridgeMachines)}, ${dispatchTargetNote(target)}.`,
                      ...(bundleDefault
                        ? [`Provisioned with bundle ${bundleLabelForID(bundleDefault.value, bundlesOnBoard)} (${defaultSourceLabel(bundleDefault.source)}).`]
                        : []),
                      stored ? 'Using the prompt saved on this card.' : 'Using the suggested prompt (nothing saved on this card).',
                      ...(existing ? ['', 'This card already has a session. This adds a second one.'] : []),
                    ]
                    lines.push('', '--- prompt ---', prompt.length > 600 ? prompt.slice(0, 600) + '…' : prompt)
                    if (!window.confirm(lines.join('\n'))) return false

                    try {
                      const sessionID = await dispatchAgentOnCard({
                        basePath: bridgeBasePath,
                        fetchFn: fetchFn,
                        title,
                        prompt,
                        addLink: (et, er, label) => k.addCardLink(card.placement.card_id, et, er, label),
                        instance: { id: instance.id, harness_type: instance.harness_type },
                        bundleID,
                      })
                      navigate(kanbanSessionHref(routes.chat, server, sessionID))
                      return true
                    } catch (e) {
                      // No toast surface on this page, and a dispatch that
                      // failed must not look like one that worked.
                      window.alert(`Could not start an agent: ${e instanceof Error ? e.message : String(e)}`)
                      return false
                    }
                  }}
                  onDeleteColumn={async () => {
                    // Count from the real board, not the filtered copy: deleting
                    // a column detaches every card in it, including the ones the
                    // active filter is hiding, and a count that only reflects
                    // what is on screen would understate what is about to happen.
                    const actual = k.view!.columns.find(c => c.column.id === cv.column.id)?.cards?.length ?? 0
                    if (actual > 0) {
                      if (!confirm(`Column "${cv.column.name}" has ${actual} cards. Delete column AND detach those cards?`)) return
                    }
                    await k.deleteColumn(cv.column.id)
                  }}
                />
              ))}
            </div>

            {k.view.orphans && k.view.orphans.length > 0 && (
              <div className="bk-orphans">
                <h3>Orphaned placements ({k.view.orphans.length})</h3>
                <p className="bk-orphan-note">
                  These placements are still on the board but their noteboard items were
                  deleted, so there is nothing left to show. A reversible delete leaves
                  the placement behind on purpose — restoring the item has to be able to
                  put it back here — which is why they accumulate.
                </p>
                {k.view.orphans.map(o => (
                  <div key={o.placement.card_id} className="bk-orphan-row">
                    <code>{o.placement.card_id}</code>
                    <button
                      className="bi-add-btn"
                      title="Remove this empty placement from the board"
                      onClick={() => k.detachCard(k.view!.board.id, o.placement.card_id)}
                    >Detach</button>
                  </div>
                ))}
              </div>
            )}
          </>
        )}
      </main>

      {drawerCard && k.view && (
        <CardDrawer
          // This card came off the board read, which carries assignments and
          // omits the key when there are none. Absent here means nobody, and
          // CardDetail reads `undefined` as "not loaded" — the card page's
          // case — so the board says what it knows before handing it over.
          card={drawerCard.assignments ? drawerCard : { ...drawerCard, assignments: [] }}
          boardID={k.view.board.id}
          entityTypes={k.entityTypes}
          onClose={() => setDrawerCardID(null)}
          onPatch={(patch) => k.patchCard(drawerCard.placement.card_id, patch)}
          onDetach={async () => {
            const ok = await k.detachCard(k.view!.board.id, drawerCard.placement.card_id)
            if (ok) setDrawerCardID(null)
          }}
          onArchive={async () => {
            const ok = await k.archiveCard(drawerCard.placement.card_id)
            if (ok) setDrawerCardID(null)
          }}
          onDelete={async (hard) => {
            const ok = await k.deleteCard(drawerCard.placement.card_id, hard)
            if (ok) setDrawerCardID(null)
          }}
          onAddLink={(et, er, label) => k.addCardLink(drawerCard.placement.card_id, et, er, label)}
          onDeleteLink={(linkID) => k.deleteCardLink(linkID)}
          onAssign={(principalID) => k.assign(drawerCard.placement.card_id, principalID)}
          onUnassign={(principalID) => k.unassign(drawerCard.placement.card_id, principalID)}
          onOpenChat={openSessionLink}
          onOpenInMail={openEmailInMail}
          mailBasePath={mailBasePath}
          fetchFn={fetchFn}
        />
      )}
    </div>
  )
}

// ============================ Sub-components ============================

/**
 * Filter and sort controls for the card axes a board actually uses.
 *
 * Both are view state — neither writes to the board — so this is safe to leave
 * on while the classifier keeps filing in the background.
 */
function CardAxisToolbar({
  axes,
  filter,
  onFilterChange,
  sortKey,
  onSortChange,
  hiddenCardCount,
}: {
  axes: ReturnType<typeof axisUsage>
  filter: AxisFilter
  onFilterChange: (f: AxisFilter) => void
  sortKey: SortKey
  onSortChange: (k: SortKey) => void
  hiddenCardCount: number
}) {
  const toggle = (prefix: string, value: string) => {
    const current = filter[prefix] ?? []
    const next = current.includes(value)
      ? current.filter(v => v !== value)
      : [...current, value]
    onFilterChange({ ...filter, [prefix]: next })
  }

  return (
    <div className="bk-axis-toolbar">
      {axes.map(({ axis, values }) => (
        <div key={axis.prefix} className="bk-axis-group">
          <span className="bk-axis-label">{axis.label}</span>
          {values.map(({ value, count }) => {
            const on = (filter[axis.prefix] ?? []).includes(value)
            return (
              <button
                key={value}
                type="button"
                className={`bk-axis-chip${on ? ' bk-axis-chip-on' : ''}`}
                onClick={() => toggle(axis.prefix, value)}
                title={`${count} card${count === 1 ? '' : 's'}`}
              >
                {value} <span className="bk-axis-count">{count}</span>
              </button>
            )
          })}
        </div>
      ))}

      <div className="bk-axis-group">
        <span className="bk-axis-label">Sort</span>
        <select value={sortKey} onChange={e => onSortChange(e.target.value as SortKey)}>
          {/* 'Board order' is kept because every writer on this host passes
              position 0, so the stored order is arbitrary — but it is still the
              order the board itself reports, and hiding it would be a lie. */}
          <option value="priority">Priority, then due date</option>
          <option value="urgency">Urgency</option>
          <option value="newest">Recently updated</option>
          <option value="title">Title</option>
          <option value="stored">Board order</option>
        </select>
      </div>

      {filterIsActive(filter) && (
        <div className="bk-axis-group">
          <button type="button" className="bi-add-btn" onClick={() => onFilterChange({})}>
            Clear filter{hiddenCardCount > 0 ? ` (${hiddenCardCount} hidden)` : ''}
          </button>
        </div>
      )}
    </div>
  )
}

/**
 * Structured editors for a card's axis tags.
 *
 * The drawer already has a free-text tag box, and it stays: it is the only way
 * to touch tags that are not axes. These selects exist because reclassifying by
 * retyping "cat:commerce, action:decide, urgency:high" invites typos that
 * silently drop a card out of every filter.
 */
function CardAxisEditor({
  tags,
  onChange,
}: {
  tags: string[]
  onChange: (tags: string[]) => void
}) {
  return (
    <div className="bk-axis-editor">
      {CARD_AXES.map(axis => {
        const current = tags.find(t => t.startsWith(axis.prefix))?.slice(axis.prefix.length) ?? ''
        // A value the classifier wrote outside the vocabulary is offered as an
        // extra option rather than silently reset to blank by the select.
        const options = current && !axis.values.includes(current)
          ? [...axis.values, current]
          : axis.values
        return (
          <div key={axis.prefix}>
            <label className="bk-drawer-label">{axis.label}</label>
            <select
              value={current}
              onChange={e => onChange(withAxisValue(tags, axis.prefix, e.target.value))}
            >
              <option value="">— unset —</option>
              {options.map(v => <option key={v} value={v}>{v}</option>)}
            </select>
          </div>
        )
      })}
    </div>
  )
}

function NewBoardForm({
  onCreate,
  onCancel,
}: {
  onCreate: (args: { name: string; description?: string }) => void | Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  return (
    <form
      className="bk-new-form"
      onSubmit={e => { e.preventDefault(); if (name.trim()) onCreate({ name: name.trim(), description: description.trim() || undefined }) }}
    >
      <input autoFocus placeholder="Board name" value={name} onChange={e => setName(e.target.value)} />
      <input placeholder="Description (optional)" value={description} onChange={e => setDescription(e.target.value)} />
      <div className="bk-form-actions">
        <button type="submit" className="bi-save-btn">Create</button>
        <button type="button" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function NewColumnForm({
  onCreate,
  onCancel,
}: {
  onCreate: (args: { name: string; wip_limit?: number; auto_status?: string }) => void | Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [wip, setWip] = useState('')
  const [autoStatus, setAutoStatus] = useState('')
  return (
    <form
      className="bk-new-form"
      onSubmit={e => {
        e.preventDefault()
        if (!name.trim()) return
        onCreate({
          name: name.trim(),
          wip_limit: wip ? Number(wip) : undefined,
          auto_status: autoStatus || undefined,
        })
      }}
    >
      <input autoFocus placeholder="Column name" value={name} onChange={e => setName(e.target.value)} />
      <input placeholder="WIP limit (optional)" type="number" min={1} value={wip} onChange={e => setWip(e.target.value)} />
      <select value={autoStatus} onChange={e => setAutoStatus(e.target.value)}>
        <option value="">— no auto-status —</option>
        <option value="open">open</option>
        <option value="done">done</option>
        <option value="archived">archived</option>
      </select>
      <div className="bk-form-actions">
        <button type="submit" className="bi-save-btn">Add column</button>
        <button type="button" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function ColumnPane({
  cv,
  signalsByTodo,
  principals,
  boardColumns,
  collapsed,
  onToggleCollapse,
  onLoadMore,
  onCompose,
  composeOpen,
  onCancelCompose,
  onCreateCard,
  onMoveCard,
  onOpenCard,
  onOpenChat,
  onStopCard,
  onPlayCard,
  onRunAgent,
  onDeleteColumn,
}: {
  cv: ColumnView
  onLoadMore: () => void
  signalsByTodo: Map<string, Signal[]>
  principals: PrincipalsDirectory
  boardColumns: { id: string; name: string }[]
  collapsed: boolean
  onToggleCollapse: () => void
  onCompose: () => void
  composeOpen: boolean
  onCancelCompose: () => void
  onCreateCard: (args: NewCardArgs) => void | Promise<void>
  onMoveCard: (cardID: string, columnID: string) => Promise<boolean>
  onOpenCard: (cardID: string) => void
  onOpenChat: OpenChatFn
  onStopCard: (cardID: string) => Promise<boolean>
  onPlayCard: (cardID: string) => Promise<boolean>
  onRunAgent: (card: CardView) => Promise<boolean>
  onDeleteColumn: () => void
}) {
  const cards = cv.cards ?? []
  const wip = cv.column.wip_limit
  // Against the column's real size, not the page's: a column of 200 with 25
  // loaded is over a WIP limit of 50, and saying otherwise would be the page
  // reporting on itself.
  const overWIP = wip != null && cv.total > wip
  const hidden = Math.max(0, cv.total - cards.length)
  const className = [
    'bk-column',
    overWIP ? 'bk-column-over-wip' : '',
    collapsed ? 'bk-column-collapsed' : '',
  ].filter(Boolean).join(' ')
  return (
    <section className={className}>
      <header className="bk-column-head" style={cv.column.color ? { borderTopColor: cv.column.color } : undefined}>
        <div className="bk-column-title">
          <button
            className="bk-column-collapse-btn"
            onClick={onToggleCollapse}
            title={collapsed ? 'Expand column' : 'Collapse column'}
            aria-label={collapsed ? 'Expand column' : 'Collapse column'}
          >
            {collapsed ? '▸' : '▾'}
          </button>
          <strong>{cv.column.name}</strong>
          <span className="bk-column-count" title={hidden > 0 ? `${hidden} more not loaded` : undefined}>
            {hidden > 0 ? `${cards.length} of ${cv.total}` : cards.length}{wip != null ? ` / ${wip}` : ''}
          </span>
        </div>
        {!collapsed && (
          <div className="bk-column-actions">
            <button className="bi-add-btn" onClick={onCompose}>+</button>
            <button className="bi-add-btn" onClick={onDeleteColumn} title="Delete column">×</button>
          </div>
        )}
        {cv.column.auto_status && !collapsed && (
          <div className="bk-column-meta">auto-status: {cv.column.auto_status}</div>
        )}
      </header>

      {!collapsed && composeOpen && (
        <NewCardForm onCreate={onCreateCard} onCancel={onCancelCompose} />
      )}

      {!collapsed && (
        <div className="bk-card-list">
          {cards.map(c => (
            <CardTile
              key={c.placement.card_id}
              card={c}
              signals={signalsByTodo.get(c.placement.card_id) ?? []}
              principals={principals}
              currentColumn={cv.column.id}
              boardColumns={boardColumns}
              onMove={onMoveCard}
              onOpen={() => onOpenCard(c.placement.card_id)}
              onOpenChat={onOpenChat}
              onStop={onStopCard}
              onPlay={onPlayCard}
              onRunAgent={onRunAgent}
            />
          ))}
          {cards.length === 0 && (
            <div className="bk-card-empty">no cards</div>
          )}
          {hidden > 0 && (
            <div className="bk-column-more">
              <button type="button" className="bi-add-btn" onClick={onLoadMore}>
                Show {Math.min(hidden, 25)} more
              </button>
              {/* The sort control orders what is loaded, and cannot order what is
                  not: the fields it sorts by live in noteboard, so kanban-store
                  pages in stored order. Saying so is the difference between a
                  page and a wrong answer. */}
              <span className="bk-column-more-note">{hidden} more — sorting applies to the {cards.length} loaded</span>
            </div>
          )}
        </div>
      )}
    </section>
  )
}

/** What the new-card form collects. Mirrors the subset of CreateCardArgs the
 *  form exposes; column_id is supplied by the column that owns the form. */
interface NewCardArgs {
  title: string
  body?: string
  tags?: string[]
  hold?: boolean
  hold_reason?: string
  auto_hold_at_usd?: number
}

function NewCardForm({
  onCreate,
  onCancel,
}: {
  onCreate: (args: NewCardArgs) => void | Promise<void>
  onCancel: () => void
}) {
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [tags, setTags] = useState('')
  const [hold, setHold] = useState(false)
  const [ceiling, setCeiling] = useState('')
  return (
    <form
      className="bk-new-form bk-new-card"
      onSubmit={e => {
        e.preventDefault()
        if (!title.trim()) return
        // An empty box is NO ceiling, not a ceiling of zero — and a ceiling of
        // zero is a real thing ("stop before spending a cent"). parseFloat('')
        // is NaN, so the empty case is filtered out explicitly rather than
        // being allowed to fall through as 0.
        const parsed = parseFloat(ceiling)
        const auto_hold_at_usd = ceiling.trim() === '' || Number.isNaN(parsed) ? undefined : parsed
        onCreate({
          title: title.trim(),
          body: body.trim() || undefined,
          tags: tags.split(',').map(s => s.trim()).filter(Boolean),
          hold: hold || undefined,
          hold_reason: hold ? 'created held' : undefined,
          auto_hold_at_usd,
        })
      }}
    >
      <input autoFocus placeholder="Card title" value={title} onChange={e => setTitle(e.target.value)} />
      <textarea placeholder="Body (markdown, optional)" rows={3} value={body} onChange={e => setBody(e.target.value)} />
      <input placeholder="tags (comma-separated)" value={tags} onChange={e => setTags(e.target.value)} />
      <label className="bk-form-check" title="Create this card parked: no agent will pick it up until you press play.">
        <input type="checkbox" checked={hold} onChange={e => setHold(e.target.checked)} />
        Start held (agents can't pick this up)
      </label>
      <input
        type="number" min="0" step="0.50"
        placeholder="Auto-hold at $ (optional — blank = no limit)"
        value={ceiling}
        onChange={e => setCeiling(e.target.value)}
        title="Once this card's agent sessions have cost this much in total, it is held automatically. Each session is also capped at whatever is left."
      />
      <div className="bk-form-actions">
        <button type="submit" className="bi-save-btn">Add card</button>
        <button type="button" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

/** What a card says when a session working this todo has raised something.
 *
 * A question and a notification are different demands and get different words:
 * a question is blocking a session that cannot proceed without an answer, a
 * notification is an FYI nobody has read yet. When both are open the question
 * is what the card leads with — it is the one costing time right now.
 *
 * The named one is the newest, matching the order every other signal surface
 * reads in. The count is what says there are more.
 *
 * The badge states the problem and does not offer to solve it. Answering
 * happens where the signal has a resolve verb: the chat, the inbox, or the
 * RefChip panel. Putting an inert answer box on a board card would promise a
 * resolution the board cannot deliver. */
function SignalBadge({ signals }: { signals: Signal[] }) {
  if (signals.length === 0) return null
  const questions = signals.filter(s => s.kind === SIGNAL_KIND_QUESTION)
  const leading = questions[0] ?? signals[0]
  const label = questions.length > 0
    ? (questions.length > 1 ? `❓ ${questions.length} open questions` : '❓ open question')
    : (signals.length > 1 ? `📣 ${signals.length} unread notifications` : '📣 unread notification')
  return (
    <div className={`bk-card-signal bk-card-signal-${leading.kind}`} title={leading.title}>
      <span className="bk-card-signal-label">{label}</span>
      <span className="bk-card-signal-title">{leading.title}</span>
    </div>
  )
}

// LinkedActivity is the last thing that actually happened to a card: mail
// arriving on it, or an agent being handed it.
type LinkedActivity = { at: string; kind: 'email' | 'session' }

// latestLinkedActivity finds the most recent of those, from links the board
// view already carries.
//
// Only 'email' and 'session' count. 'email_msgid' is the same arrival recorded
// under its RFC identity and would double-count it, and 'email_sender' is a
// learned affinity between a sender and a card rather than an event.
function latestLinkedActivity(card: CardView): LinkedActivity | null {
  let newest: (LinkedActivity & { rank: number }) | null = null
  for (const l of card.links ?? []) {
    if (l.entity_type !== 'email' && l.entity_type !== 'session') continue
    const rank = new Date(l.created_at).getTime()
    if (!Number.isFinite(rank)) continue
    if (!newest || rank > newest.rank) {
      newest = { at: l.created_at, kind: l.entity_type as 'email' | 'session', rank }
    }
  }
  return newest ? { at: newest.at, kind: newest.kind } : null
}

// CardAgeBadge answers "when did anything last happen here?".
//
// It reports time since the last linked email or dispatch, not time since the
// card was created. Creation age says how long ago a bucket was opened, which
// stops being interesting immediately — a card opened in May that took mail an
// hour ago is live, and one opened yesterday that has been silent since is not.
// Creation time stays in the tooltip, where it is context rather than the
// headline.
//
// It deliberately does NOT show time since the agent last did something inside
// its session. That lives on the session, not the link, and reading it means
// asking llm-bridge-server per session. The Agent runs board holds 6,466 cards
// across 5,628 distinct sessions, so on a 15-second poll that is thousands of
// requests a minute to render a caption. The drawer shows it for one card.
function CardAgeBadge({
  card,
  placement,
}: {
  card: CardView
  placement: Placement
}) {
  const activity = latestLinkedActivity(card)
  // With no links at all there is no activity to report, so the card falls back
  // to saying how long it has been sitting there — which is the honest answer.
  const shown = activity
    ? formatAgeCompact(activity.at)
    : formatAgeCompact(placement.created_at)
  if (!shown) return null

  const title = [
    activity
      ? `Last ${activity.kind === 'email' ? 'email attached' : 'handed to an agent'}: ${new Date(activity.at).toLocaleString()}`
      : 'Nothing has happened on this card yet',
    `On this board since ${new Date(placement.created_at).toLocaleString()}`,
  ].join('\n')

  return (
    <span className="bk-card-age" title={title}>
      {activity ? (activity.kind === 'email' ? '✉' : '▶') : '🕒'} {shown}
    </span>
  )
}

function CardTile({
  card,
  signals,
  principals,
  currentColumn,
  boardColumns,
  onMove,
  onOpen,
  onOpenChat,
  onStop,
  onPlay,
  onRunAgent,
}: {
  card: CardView
  signals: Signal[]
  principals: PrincipalsDirectory
  currentColumn: string
  boardColumns: { id: string; name: string }[]
  onMove: (cardID: string, columnID: string) => Promise<boolean>
  onOpen: () => void
  onOpenChat: OpenChatFn
  onStop: (cardID: string) => Promise<boolean>
  onPlay: (cardID: string) => Promise<boolean>
  onRunAgent: (card: CardView) => Promise<boolean>
}) {
  // Guards the button between click and session id, so an impatient second
  // click cannot start a second agent on the same card.
  const [running, setRunning] = useState(false)
  const item = card.item
  if (!item) {
    return (
      <div className="bk-card bk-card-orphan" onClick={onOpen}>
        <em>missing noteboard item</em>
        <small>{card.placement.card_id}</small>
      </div>
    )
  }
  const tags: string[] = Array.isArray(item.tags) ? item.tags : []
  const status = item.status as string
  const session = latestSessionLink(card)
  // The gate is a property of the work, not of the column it sits in — so the
  // button renders on every card in every column, not just in a gate column.
  const held = !!item.held_at
  const ceiling = typeof item.auto_hold_at_usd === 'number' ? item.auto_hold_at_usd : null
  return (
    <div className={`bk-card${held ? ' bk-card-held' : ''}`} onClick={onOpen}>
      <div className="bk-card-title">{item.title}</div>
      {ceiling !== null && (
        <div
          className="bk-card-ceiling"
          title={`Auto-holds once this card's sessions have cost $${ceiling.toFixed(2)} in total. Each session is capped at whatever is left of that.`}
        >
          ⛽ auto-hold at ${ceiling.toFixed(2)}
        </div>
      )}
      {held && (
        <div className="bk-card-hold" title={item.hold_reason || 'No reason given'}>
          ⏸ held — no agent will pick this up
          {item.hold_reason ? `: ${item.hold_reason}` : ''}
        </div>
      )}
      <SignalBadge signals={signals} />
      {tags.length > 0 && (
        <div className="bk-card-tags">
          {tags.map(t => <span key={t} className="bk-tag">{t}</span>)}
        </div>
      )}
      {principals.enabled && (card.assignments?.length ?? 0) > 0 && (
        <div className="bk-card-assignees">
          {card.assignments!.map(a => (
            <AssigneeChip key={a.principal_id} assignment={a} directory={principals} compact />
          ))}
        </div>
      )}
      <div className="bk-card-foot">
        <span className={`bk-status bk-status-${status}`}>{status}</span>
        {/* Both, when both have something to say. The clock badge carries the
            rung and the limit; the activity badge carries when anything last
            happened. A card with recorded actions needs only the first, and a
            card without them — every card older than the event log — is best
            served by its limit next to the last thing that touched it. */}
        {hasClockData(card.time) && <CardBudgetBadge time={card.time} />}
        {(card.time?.event_count ?? 0) === 0 && (
          <CardAgeBadge card={card} placement={card.placement} />
        )}
        <button
          type="button"
          className={held ? 'bk-card-play' : 'bk-card-stop'}
          title={held
            ? 'Play — clear the hold so agents may work this, and resume its session if it was paused'
            : 'Stop — park this work so no agent picks it up, and pause any session already running it'}
          onClick={e => {
            e.stopPropagation()
            held ? onPlay(card.placement.card_id) : onStop(card.placement.card_id)
          }}
        >{held ? '▶' : '⏸'}</button>
        {/* Not ▶ — that glyph is already the unhold button two positions left,
            and two play triangles meaning different things on one row is how a
            misclick starts a paid agent. */}
        <button
          type="button"
          className="bk-card-run"
          disabled={held || running}
          title={held
            ? 'Held — clear the hold before starting an agent'
            : session
              ? 'Start another agent on this card, on the instance its last session ran on. It already has one.'
              : 'Open this card to choose where an agent runs, and start it there'}
          onClick={async e => {
            e.stopPropagation()
            setRunning(true)
            try { await onRunAgent(card) } finally { setRunning(false) }
          }}
        >{running ? '…' : '🤖'}</button>
        {session && (
          <button
            type="button"
            className="bk-card-chat"
            title={`Open chat session ${session.ref}`}
            onClick={e => { e.stopPropagation(); onOpenChat(session) }}
          >chat ↗</button>
        )}
        <select
          value={currentColumn}
          onClick={e => e.stopPropagation()}
          onChange={e => onMove(card.placement.card_id, e.target.value)}
          title="Move to column"
        >
          {boardColumns.map(c => (
            <option key={c.id} value={c.id}>{c.name}</option>
          ))}
        </select>
      </div>
    </div>
  )
}

// CardTiming is the drawer's answer to "how long has this been going?".
//
// Unlike the badge on the tile, this one may ask llm-bridge-server for the
// session's real last activity, because the drawer shows one card at a time.
// The same question asked from the board would be thousands of requests per
// poll; asked here it is one, on open.
function CardTiming({
  card,
  fetchFn,
}: {
  card: CardView
  fetchFn: FetchFn
}) {
  const placement = card.placement
  const session = latestSessionLink(card)
  const { basePath } = useBridgeConfig()
  // Where a dispatch would run and what it would be provisioned with. Not the
  // board's record: a tag rule matching this card's tags can override either,
  // so kanban-store resolves it per card and says where each value came from.
  const cardDefaults = useCardEffectiveDefaults(
    placement.board_id || null,
    placement.card_id,
    (card.item?.tags ?? []).join(' '),
  )
  const defaults = cardDefaults.effective?.defaults ?? null
  const { bundles } = useBundles()
  const [lastActivity, setLastActivity] = useState<string | null>(null)
  const [state, setState] = useState<string | null>(null)
  // Distinguishes "we have not asked yet" from "we asked and the bridge could
  // not say". Without it a failed lookup renders identically to a pending one
  // and the row just never fills in.
  const [unavailable, setUnavailable] = useState(false)

  useEffect(() => {
    if (!session) return
    let canceled = false
    setUnavailable(false)
    setLastActivity(null)
    setState(null)

    fetchFn(`${basePath}/sessions/${encodeURIComponent(session.ref)}`)
      .then(r => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then(s => {
        if (canceled) return
        setLastActivity(typeof s?.updated_at === 'string' ? s.updated_at : null)
        setState(typeof s?.state === 'string' ? s.state : null)
      })
      .catch(() => { if (!canceled) setUnavailable(true) })

    return () => { canceled = true }
  }, [session?.ref, basePath, fetchFn])

  const boardAge = formatAgeCompact(placement.created_at)
  const dispatchAge = session ? formatAgeCompact(session.dispatchedAt) : null
  const activityAge = lastActivity ? formatAgeCompact(lastActivity) : null

  const activity = latestLinkedActivity(card)
  const activityAgeFromLink = activity ? formatAgeCompact(activity.at) : null

  return (
    <dl className="bk-drawer-timing">
      <div>
        <dt>{activity?.kind === 'email' ? 'Last email' : activity ? 'Last dispatch' : 'Last activity'}</dt>
        <dd title={activity ? new Date(activity.at).toLocaleString() : undefined}>
          {activityAgeFromLink ?? 'never'}
        </dd>
      </div>
      <div>
        <dt>On this board</dt>
        <dd title={new Date(placement.created_at).toLocaleString()}>{boardAge ?? '—'}</dd>
      </div>
      {defaults?.default_bundle_id && (
        <div>
          <dt>Bundle</dt>
          <dd>
            <span
              className="bk-runs-on bk-board-bundle"
              data-bundle-id={defaults.default_bundle_id.value}
              title={`${defaultSourceLabel(defaults.default_bundle_id.source)} — ${CARD_BUNDLE_HELP_TEXT}`}
            >
              {bundleLabelForID(defaults.default_bundle_id.value, bundles)}
            </span>
          </dd>
        </div>
      )}
      {!session && defaults?.default_instance_id && (
        <div>
          <dt>Would run on</dt>
          <dd>
            <DefaultInstanceLabel
              instanceID={defaults.default_instance_id.value}
              source={defaults.default_instance_id.source}
            />
          </dd>
        </div>
      )}
      {session && (
        <>
          <div>
            <dt>Given to an agent</dt>
            <dd title={new Date(session.dispatchedAt).toLocaleString()}>{dispatchAge ?? '—'}</dd>
          </div>
          <div>
            <dt>Runs on</dt>
            <dd><SessionRunsOnLabel sessionID={session.ref} /></dd>
          </div>
          <div>
            <dt>Last agent activity</dt>
            <dd title={lastActivity ? new Date(lastActivity).toLocaleString() : undefined}>
              {activityAge
                ? `${activityAge}${state ? ` · ${state}` : ''}`
                : unavailable
                  ? 'bridge did not answer'
                  : '…'}
            </dd>
          </div>
        </>
      )}
    </dl>
  )
}

/**
 * Where one session runs: its harness instance, the harness, and the
 * environment (machine) the instance lives in. Every session record carries the
 * instance the server bound it to at creation, so a card already knows this for
 * each agent it has had; this puts it on screen.
 *
 * An instance or machine the bridge's lists do not name shows as its raw id,
 * never as nothing — the session record is true even when a name is missing.
 */
function SessionRunsOnLabel({ sessionID }: { sessionID: string }) {
  const { runsOn, error } = useSessionRunsOn(sessionID)
  const { instances } = useBridgeInstances()
  const { machines } = useBridgeMachines()
  if (error) {
    return (
      <span className="bk-runs-on bk-runs-on-unknown" title={`The bridge could not say where ${sessionID} runs: ${error}`}>
        unknown ({error})
      </span>
    )
  }
  if (!runsOn) return <span className="bk-runs-on">…</span>
  const instance = instances.find(i => i.id === runsOn.instanceID)
  const machine = instance ? machines.find(m => m.id === instance.machine_id) : undefined
  return (
    <span
      className="bk-runs-on"
      data-instance-id={runsOn.instanceID}
      title={`instance ${runsOn.instanceID}${instance ? `, machine ${instance.machine_id}` : ''}`}
    >
      <span className="bk-runs-on-instance">{instance ? instance.name : runsOn.instanceID}</span>
      {runsOn.harness && <span className="bk-runs-on-harness">{runsOn.harness}</span>}
      {instance && <span className="bk-runs-on-machine">{machine ? machineLabel(machine) : instance.machine_id}</span>}
    </span>
  )
}

/**
 * The board's default instance, drawn as the same chip "Runs on" uses, for a
 * card that has had no session yet: where a dispatch from this board would run.
 * An instance the bridge does not list, or has disabled, says so — a dispatch
 * would not pick it, so the chip must not read as though it would.
 */
function DefaultInstanceLabel({ instanceID, source }: { instanceID: string; source: DefaultSource }) {
  const { instances } = useBridgeInstances()
  const { machines } = useBridgeMachines()
  const instance = instances.find(i => i.id === instanceID)
  const machine = instance ? machines.find(m => m.id === instance.machine_id) : undefined
  const unusable = !instance ? 'the bridge does not list it' : !instance.enabled ? 'disabled' : null
  return (
    <span
      className={`bk-runs-on${unusable ? ' bk-runs-on-unknown' : ''}`}
      data-instance-id={instanceID}
      title={`${defaultInstanceLabel(source)} ${instanceID}${unusable ? ` — ${unusable}, so a dispatch will ask where to run` : ''}`}
    >
      <span className="bk-runs-on-instance">{instance ? instance.name : instanceID}</span>
      {instance && <span className="bk-runs-on-harness">{instance.harness_type}</span>}
      {instance && <span className="bk-runs-on-machine">{machine ? machineLabel(machine) : instance.machine_id}</span>}
      {unusable && <span className="bk-runs-on-harness">{unusable}</span>}
    </span>
  )
}

// AgentPromptPanel is the card's "hand this to an agent" control: the prompt it
// will be given, editable in place, where it runs, and the button that starts it.
//
// The prompt shown when a card carries none is a suggestion, not a saved value —
// it renders in the box but is not written to the card until the drawer is
// saved. Writing it on open would put an agent prompt on every card anyone
// merely looked at.
function AgentPromptPanel({
  cardID,
  title,
  body,
  linkedEmailCount,
  prompt,
  onPromptChange,
  existingSession,
  boardID,
  savedTags,
  assignments,
  principals,
  onAddLink,
  onOpenChat,
  fetchFn,
}: {
  cardID: string
  title: string
  body: string
  linkedEmailCount: number
  prompt: string
  onPromptChange: (next: string) => void
  existingSession: SessionLinkRef | null
  /** The board the card sits on, against whose tag rules and defaults the
   *  card's effective instance and bundle resolve. Null for a card on no board. */
  boardID: string | null
  /** The card's tags as kanban-store has them — not the drawer's unsaved edit
   *  buffer. Which rules match is decided by the stored tags, so previewing
   *  against untyped-but-unsaved ones would name a bundle the dispatch will not
   *  use. Only its identity matters here; the store reads the tags itself. */
  savedTags: readonly string[]
  /** Who is on the card. The instances on their lists are offered first.
   *  Undefined on a view that did not load assignments. */
  assignments: CardAssignment[] | undefined
  principals: PrincipalsDirectory
  onAddLink: (entity_type: string, entity_ref: string, label?: string) => Promise<boolean>
  onOpenChat: OpenChatFn
  fetchFn: FetchFn
}) {
  const { basePath } = useBridgeConfig()
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Where the agent runs. The select starts on the instance this card's last
  // session ran on, read off that session's own record; for a card that has had
  // none, on the board's default instance, which is the board's stated choice
  // rather than a guess; and on nothing when neither is usable.
  const { instances, error: instancesError } = useBridgeInstances()
  const { machines } = useBridgeMachines()
  const assigneeIDs = useMemo(() => (assignments ?? []).map(a => a.principal_id), [assignments])
  const { listedBy, error: listsError } = useInstancesListedForPrincipals(assigneeIDs)
  const choices = useMemo(() => dispatchInstanceChoices(instances, machines, listedBy), [instances, machines, listedBy])
  const { runsOn: lastRunsOn, error: lastRunsOnError } = useSessionRunsOn(existingSession?.ref ?? null)
  // What this card would run with: kanban-store resolves the board's tag rules
  // against the card's stored tags and answers with each default and where it
  // came from. The board's own record is deliberately not read here — a tag
  // rule can override it, and the precedence is the store's to apply.
  const cardDefaults = useCardEffectiveDefaults(boardID, cardID, savedTags.join(' '))
  const defaults = cardDefaults.effective?.defaults ?? null
  const { bundles } = useBundles()
  const [chosenInstanceID, setChosenInstanceID] = useState<string | null>(null)
  useEffect(() => { setChosenInstanceID(null) }, [cardID])
  // Nothing is preselected until both reads that decide it have answered: the
  // card's last session and its effective defaults. Preselecting a default
  // while the last session is still loading would let a quick click start paid
  // work somewhere this card never ran.
  const lastSessionPending = !!existingSession && !lastRunsOn && !lastRunsOnError
  const target = cardDefaults.pending || lastSessionPending || lastRunsOnError
    ? null
    : dispatchTargetForCard({
      lastSessionInstanceID: lastRunsOn?.instanceID ?? null,
      defaultInstance: defaults?.default_instance_id ?? null,
      instances,
    })
  const selectedInstanceID = chosenInstanceID ?? (target && target.kind !== 'none' ? target.instance.id : '')
  const selectedInstance = instances.find(i => i.id === selectedInstanceID && i.enabled)
  const targetNote = chosenInstanceID !== null
    ? ''
    : lastRunsOnError
      ? `could not tell where this card’s last session ran (${lastRunsOnError})`
      : !target
        ? ''
        : dispatchTargetNote(target) ?? 'this card has had no session yet'
  const principalName = (id: string) => principals.byId.get(id)?.display_name ?? id

  const suggestion = useMemo(
    () => suggestAgentPrompt({ cardID, title, body, linkedEmailCount }),
    [cardID, title, body, linkedEmailCount],
  )
  // What the box shows, and — importantly — what a dispatch would actually send.
  // These must be the same string, or the agent gets something the human never
  // read.
  const effective = prompt.trim() ? prompt : suggestion
  const usingSuggestion = !prompt.trim()

  const start = async () => {
    if (!selectedInstance) {
      setError('Choose where the agent runs first.')
      return
    }
    // The card's effective defaults say which bundle a dispatch is provisioned
    // with. Starting before that read has answered would start without the
    // bundle, which is not what the board — or a tag rule — asks for.
    if (cardDefaults.pending) {
      setError('Still reading this card’s defaults.')
      return
    }
    if (cardDefaults.error) {
      setError(`Could not read this card's defaults, so its bundle is unknown: ${cardDefaults.error}`)
      return
    }
    setStarting(true)
    setError(null)
    try {
      const sessionID = await dispatchAgentOnCard({
        basePath, fetchFn, title, prompt: effective, addLink: onAddLink,
        instance: { id: selectedInstance.id, harness_type: selectedInstance.harness_type },
        bundleID: defaults?.default_bundle_id?.value,
      })
      onOpenChat({ ref: sessionID, dispatchedAt: new Date().toISOString() })
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setStarting(false)
    }
  }

  return (
    <section className="bk-agent-prompt">
      <div className="bk-agent-prompt-head">
        <label className="bk-drawer-label">Agent prompt</label>
        {usingSuggestion && <span className="bk-agent-prompt-note">suggested — edit and save to keep</span>}
      </div>
      <textarea
        className="bk-agent-prompt-text"
        rows={10}
        value={effective}
        onChange={e => onPromptChange(e.target.value)}
      />
      <div className="bk-agent-target">
        <label className="bk-drawer-label" htmlFor={`bk-agent-target-${cardID}`}>Runs on</label>
        <select
          id={`bk-agent-target-${cardID}`}
          className="bk-agent-target-select"
          value={selectedInstanceID}
          onChange={e => setChosenInstanceID(e.target.value)}
        >
          <option value="" disabled>Choose a harness instance…</option>
          {choices.listed.length > 0 && (
            <optgroup label="On an assignee’s list">
              {choices.listed.map(choice => (
                <option key={choice.instance.id} value={choice.instance.id}>
                  {`${choice.instance.name} — ${choice.instance.harness_type} · ${choice.machineLabel} · ${choice.listedBy.map(principalName).join(', ')}`}
                </option>
              ))}
            </optgroup>
          )}
          {choices.byMachine.map(group => (
            <optgroup key={group.machineID} label={group.machineLabel}>
              {group.choices.map(choice => (
                <option key={choice.instance.id} value={choice.instance.id}>
                  {`${choice.instance.name} — ${choice.instance.harness_type}`}
                </option>
              ))}
            </optgroup>
          ))}
        </select>
        {targetNote && <span className="bk-agent-target-note">{targetNote}</span>}
      </div>
      {defaults?.default_bundle_id && (
        <div className="bk-agent-target">
          <span className="bk-drawer-label">Bundle</span>
          <span
            className="bk-runs-on bk-board-bundle"
            data-bundle-id={defaults.default_bundle_id.value}
            title={CARD_BUNDLE_HELP_TEXT}
          >
            {bundleLabelForID(defaults.default_bundle_id.value, bundles)}
          </span>
          <span className="bk-agent-target-note">
            {defaultSourceLabel(defaults.default_bundle_id.source)} — {CARD_BUNDLE_HELP_TEXT}
          </span>
        </div>
      )}
      {cardDefaults.error && <div className="bridge-error">Could not read this card’s defaults: {cardDefaults.error}</div>}
      {instancesError && <div className="bridge-error">Could not list harness instances: {instancesError}</div>}
      {listsError && <div className="bridge-error">Could not read an assignee’s list: {listsError}</div>}
      <div className="bk-agent-prompt-actions">
        <button
          type="button"
          className="bi-add-btn"
          disabled={starting || !effective.trim() || !selectedInstance || cardDefaults.pending}
          title={selectedInstance ? undefined : 'Choose where the agent runs first'}
          onClick={start}
        >{starting ? 'Starting…' : '▶ Start an agent on this'}</button>
        {prompt.trim() && (
          <button
            type="button"
            className="bk-agent-prompt-reset"
            disabled={starting}
            onClick={() => onPromptChange('')}
            title="Drop the saved prompt and go back to the suggested one. Takes effect on save."
          >reset to suggested</button>
        )}
        {existingSession && (
          <button
            type="button"
            className="bk-agent-prompt-reset"
            onClick={() => onOpenChat(existingSession)}
            title={`This card already has session ${existingSession.ref}. Starting another adds a second one.`}
          >open current session ↗</button>
        )}
      </div>
      {existingSession && (
        <p className="bk-agent-prompt-warn">
          An agent already has this card. Starting another gives it a second session.
        </p>
      )}
      {error && <div className="bridge-error">{error}</div>}
    </section>
  )
}

/** Every open signal raised against this card's todo, read and closed in the
 * drawer.
 *
 * This is the fourth surface `SignalCard` mounts on, and the one the `kanban`
 * surface was minted for: an autonomous worker's signal has carried
 * `surface:"kanban"` since the record existed, and until now nothing rendered
 * it anywhere it could be answered.
 *
 * A card id IS a noteboard item id, so the drawer knows its todo without a
 * lookup. It reads its own rows rather than indexing the board-wide map behind
 * `SignalBadge` — the badge and this are different things at different depths,
 * and the badge stays exactly as it is.
 *
 * Renders nothing at all when there is nothing open, including against a
 * bridge-server with no signals route: a drawer is a card editor first, and
 * every board would otherwise open onto an error until the gateway carries the
 * route. */
function CardSignals({ todoID }: { todoID: string }) {
  const signals = useOpenSignalsForTodo(todoID)
  if (signals.length === 0) return null
  return (
    <section className="bk-drawer-signals">
      <h4>Needs you</h4>
      {groupSignalsByRequest(signals).map(request => (
        <SignalRequestCard
          key={request.requestId || request.signals[0].id}
          request={request}
          // A worker's blocker can be answered here, or closed unanswered —
          // but never acknowledged. SignalCard offers Acknowledge for
          // notifications only, and the server refuses it for a question, so
          // an unanswered blocker can never read as handled.
          //
          // "Closed unanswered" was false until this flag started reaching
          // TOOL-raised blockers too. The button was gated on the signal
          // having no request id, so a worker that asked through
          // AskUserQuestion and then stopped left a card whose only control
          // was Decline — a deny addressed to a hook nobody was holding any
          // more. Nothing on the board could close it.
          allowDismissWithoutAnswer
        />
      ))}
    </section>
  )
}

/** What a card view needs to render and mutate one card, wherever it is mounted.
 *
 *  Named and exported because there are two mounts now — the board's drawer and
 *  the standalone card page — and an inline literal cannot be spread from one to
 *  the other without being written twice.
 *
 *  ⚠️ `boardID` is required but deliberately unused by the body (`_boardID`). It
 *  is the board a mutation applies to, and the drawer has always taken it; the
 *  page passes the placement it resolved. Dropping it would be a wider change
 *  than this split. */
export interface CardDetailProps {
  card: CardView
  boardID: string
  entityTypes: { type: string; service?: string; search?: string }[]
  /** Dismiss. The drawer closes; the page navigates back. */
  onClose: () => void
  onPatch: (patch: Record<string, unknown>) => Promise<boolean>
  /** Take the card off this board, leaving the noteboard item alone. */
  onDetach: () => void | Promise<void>
  /** Mark the work archived. The item stays live and restorable. */
  onArchive: () => void | Promise<void>
  onDelete: (hard: boolean) => void | Promise<void>
  onAddLink: (entity_type: string, entity_ref: string, label?: string) => Promise<boolean>
  onDeleteLink: (linkID: string) => Promise<boolean>
  /** Put a principal on the card. Optional because a host may mount this view
   *  without the verb; the picker is then not offered. The outcome carries the
   *  server's refusal, which is shown beside the picker. */
  onAssign?: (principalID: string) => Promise<AssignmentOutcome>
  /** Take a principal off the card. Optional for the same reason; the chips
   *  then have no remove button. */
  onUnassign?: (principalID: string) => Promise<AssignmentOutcome>
  onOpenChat: OpenChatFn
  onOpenInMail: (accountID: string, messageID: string) => void
  mailBasePath: string
  fetchFn: FetchFn
  /** Rendered in the header beside the close button. The drawer puts a link to
   *  the standalone card page here; the page itself passes nothing, because a
   *  link to where you already are is noise. */
  headerAction?: React.ReactNode
}

export function CardDetail({
  card,
  boardID: _boardID,
  entityTypes,
  onClose,
  onPatch,
  onDetach,
  onArchive,
  onDelete,
  onAddLink,
  onDeleteLink,
  onAssign,
  onUnassign,
  onOpenChat,
  onOpenInMail,
  mailBasePath,
  fetchFn,
  headerAction,
}: CardDetailProps) {
  const item = card.item as NoteboardItem | null
  const principals = usePrincipals()
  const [title, setTitle] = useState(item?.title ?? '')
  // The prompt block is split out of the body here and recombined on save, so
  // the body box shows what the card says and the prompt box shows what the
  // agent is told. Left merged, every card body would open with a wall of
  // instructions aimed at an agent rather than at the reader.
  const [body, setBody] = useState(stripAgentPrompt(item?.body))
  const [agentPrompt, setAgentPrompt] = useState(readAgentPrompt(item?.body) ?? '')
  const [tags, setTags] = useState((item?.tags ?? []).join(', '))
  const [status, setStatus] = useState(item?.status ?? 'open')
  const [dirty, setDirty] = useState(false)
  const [showAllEmails, setShowAllEmails] = useState(false)

  // Escape closes the drawer. Until the stacking fix above it was the only way
  // out that could not misfire, and it stays because a modal that traps you
  // until you find the backdrop is a modal people stop opening.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  // Re-seed when the underlying card changes (e.g. after a refresh)
  useEffect(() => {
    setTitle(item?.title ?? '')
    setBody(stripAgentPrompt(item?.body))
    setAgentPrompt(readAgentPrompt(item?.body) ?? '')
    setTags((item?.tags ?? []).join(', '))
    setStatus(item?.status ?? 'open')
    setDirty(false)
    setShowAllEmails(false)
  }, [item?.id, item?.updated_at])

  const links: CardLink[] = card.links ?? []
  const tagList = tags.split(',').map(s => s.trim()).filter(Boolean)
  // email_msgid and email_sender links are bookkeeping the classifier reads, not
  // something to show a human, so only the addressable email links are listed.
  const emailLinks = links.filter(l => l.entity_type === 'email')
  const otherLinks = links.filter(l => l.entity_type !== 'email')
  const shownEmailLinks = showAllEmails ? emailLinks : emailLinks.slice(0, EMAIL_LINKS_SHOWN)

  const save = async () => {
    const patch: Record<string, unknown> = {
      title,
      body: writeAgentPrompt(body, agentPrompt),
      status,
      tags: tags.split(',').map(s => s.trim()).filter(Boolean),
    }
    const ok = await onPatch(patch)
    if (ok) setDirty(false)
  }

  return (
    <>
        <header className="bk-drawer-head">
          <h3>Card</h3>
          {headerAction}
          <button onClick={onClose} className="bi-add-btn">×</button>
        </header>

        <CardTiming card={card} fetchFn={fetchFn} />

        {item && (
          <AgentPromptPanel
            cardID={card.placement.card_id}
            title={title}
            body={body}
            linkedEmailCount={emailLinks.length}
            prompt={agentPrompt}
            onPromptChange={next => { setAgentPrompt(next); setDirty(true) }}
            existingSession={latestSessionLink(card)}
            boardID={card.placement.board_id || null}
            savedTags={item.tags ?? []}
            assignments={card.assignments}
            principals={principals}
            onAddLink={onAddLink}
            onOpenChat={onOpenChat}
            fetchFn={fetchFn}
          />
        )}

        {!item ? (
          <div className="bridge-error">noteboard item is missing for placement {card.placement.card_id}</div>
        ) : (
          <>
            <CardSignals todoID={card.placement.card_id} />

            <label className="bk-drawer-label">Title</label>
            <input value={title} onChange={e => { setTitle(e.target.value); setDirty(true) }} />

            <label className="bk-drawer-label">Body (markdown)</label>
            <textarea rows={8} value={body} onChange={e => { setBody(e.target.value); setDirty(true) }} />

            <div className="bk-drawer-row">
              <div>
                <label className="bk-drawer-label">Status</label>
                <select value={status} onChange={e => { setStatus(e.target.value); setDirty(true) }}>
                  <option value="open">open</option>
                  <option value="done">done</option>
                  <option value="archived">archived</option>
                </select>
              </div>
              <div className="bk-drawer-grow">
                <label className="bk-drawer-label">Tags</label>
                <input value={tags} onChange={e => { setTags(e.target.value); setDirty(true) }} placeholder="comma-separated" />
              </div>
            </div>

            {/* Shown only for cards that already carry a classification, so
                boards that never had these tags see no extra controls. */}
            {tagList.some(t => CARD_AXES.some(a => t.startsWith(a.prefix))) && (
              <CardAxisEditor
                tags={tagList}
                onChange={next => { setTags(next.join(', ')); setDirty(true) }}
              />
            )}

            {/* Four actions that used to be two, because the two lied. The old
                "Archive" called delete and never touched status, and neither
                button could take a card off a board without destroying the work
                behind it — which is the thing people actually want most often. */}
            <div className="bk-form-actions">
              <button className="bi-save-btn" disabled={!dirty} onClick={save}>Save</button>
              {/* A card can be viewed while sitting on no board at all — the
                  standalone card page synthesizes a placement with an empty
                  board_id for exactly that case. There is nothing to remove it
                  from, so the verb is not offered. */}
              {card.placement.board_id && (
                <button onClick={onDetach} title="Take this card off the board. The todo stays in noteboard.">
                  Remove from board
                </button>
              )}
              <button onClick={onArchive} title="Mark the work archived. It stays in noteboard and can be restored.">
                Archive
              </button>
              <button
                onClick={() => {
                  if (confirm('Delete the todo from noteboard?\n\nThe work itself goes away, not just this card. It can be restored from noteboard, and until then the card stays on this board as an orphan.\n\nTo take the card off the board without deleting the todo, use "Remove from board".')) {
                    onDelete(false)
                  }
                }}
              >Delete todo</button>
              <button onClick={() => { if (confirm('Hard delete card from noteboard? Cannot be undone.')) onDelete(true) }}>Hard delete</button>
            </div>

            <hr />

            {/* The card's history, and the totals computed from it. Both mounts
                of CardDetail get it: the drawer on the board and the standalone
                card page. */}
            <h4>History</h4>
            <CardTimelinePanel
              cardID={card.placement.card_id}
              boardID={card.placement.board_id || undefined}
            />

            <hr />

            {principals.enabled && (
              <>
                <h4>Assigned</h4>
                <CardAssignees
                  assignments={card.assignments}
                  directory={principals}
                  onAssign={onAssign}
                  onUnassign={onUnassign}
                />
                <hr />
              </>
            )}

            {/* Email links are split out and capped. A bucket card gathers every
                message from a sender, so "Medium Daily Digest" reaches hundreds
                of links within months — rendering them all would bury the
                handful of links that describe the card itself. */}
            {emailLinks.length > 0 && (
              <>
                <h4>Linked emails ({emailLinks.length})</h4>
                <ul className="bk-link-list">
                  {shownEmailLinks.map(l => (
                    <LinkedEmailRow
                      key={l.id}
                      link={l}
                      mailBasePath={mailBasePath}
                      fetchFn={fetchFn}
                      onOpenInMail={onOpenInMail}
                      onDeleteLink={onDeleteLink}
                    />
                  ))}
                  {emailLinks.length > shownEmailLinks.length && (
                    <li>
                      <button type="button" className="bi-add-btn" onClick={() => setShowAllEmails(true)}>
                        Show {emailLinks.length - shownEmailLinks.length} more
                      </button>
                    </li>
                  )}
                </ul>
              </>
            )}

            <h4>Entity links</h4>
            <ul className="bk-link-list">
              {otherLinks.map(l => (
                <EntityLinkRow
                  key={l.id}
                  link={l}
                  onOpenChat={onOpenChat}
                  onDeleteLink={onDeleteLink}
                />
              ))}
              {otherLinks.length === 0 && <li className="bi-empty">No links yet.</li>}
            </ul>
            <AddLinkForm entityTypes={entityTypes} onAdd={onAddLink} />
          </>
        )}
    </>
  )
}

/**
 * One linked email: its label, a deep link into the Mail page, and an expandable
 * preview of the message itself.
 *
 * The preview renders `body_text` as PLAIN TEXT, never `body_html`. Mail bodies
 * are attacker-controlled — anyone who can email this user can put markup in
 * them — and dash's Mail page only renders them safely because it uses a
 * sandboxed iframe with remote images stripped. Rebuilding that here would mean
 * maintaining a second sandbox; the deep link hands the job to the one that
 * already exists.
 *
 * The fetch is deliberate, not eager: mailstack caches nothing, so reading one
 * message is a live provider round trip. A bucket card can carry hundreds of
 * links, and expanding them all on open would be hundreds of Gmail calls.
 */
/**
 * One row in a card's "Entity links" list.
 *
 * Every link used to render as `<type> <raw uuid>`, with a click-through on
 * `session` alone. A card is linked to the sessions, subagent runs and todos
 * that did its work — all three ARE linkable today, via `session` and via
 * `note`, which resolves noteboard's `/api/items` and therefore covers todos —
 * but a todo showed as 36 characters of hex naming nothing. Measured on this
 * host: zero `note` links existed, which is what an unusable affordance looks
 * like from the data side.
 *
 * A `note` ref now resolves to its title and links to the host's notes page.
 * The resolution is `fetchNoteboardItemRef`, the same call the chat's reference chips
 * make — reused rather than re-derived, so a todo reads identically wherever it
 * is referenced.
 *
 * ⚠️ Resolution is per row and only for `note`. That is affordable HERE and
 * would not be in a virtualized list: a drawer shows one card with a handful of
 * links, where a transcript pane would issue one request per rendered row on
 * every scroll. Do not lift this into a list without changing how it fetches.
 *
 * A ref that will not resolve — a deleted todo, a hand-typed id — keeps its raw
 * value rather than disappearing or reading as an error. The link is still a
 * true record that someone attached that id; only the title is unknown.
 */
/**
 * The card drawer: `CardDetail` plus the overlay chrome that makes it a drawer.
 *
 * The split exists because the same content is now reachable two ways — as this
 * overlay on the board, and as a page of its own at the host's card route. Only
 * the chrome differs, so only the chrome is duplicated: a page rendering this
 * component would have drawn a modal floating over an empty document, and a
 * `variant` prop toggling the backdrop would have put two layouts inside one
 * component to avoid moving three lines.
 *
 * `CardDetail` keeps `.bk-drawer-head` and every other `bk-drawer-*` class, so
 * the stylesheet's descendant rules apply in both mounts and neither surface
 * ships CSS. The page supplies its own `.bk-drawer` wrapper for the same reason.
 */
function CardDrawer(props: CardDetailProps) {
  return (
    <div className="bk-drawer-backdrop" onClick={props.onClose}>
      {/* Stops a click inside the panel from reaching the backdrop, which
          closes. Without it, editing a field shuts the drawer. */}
      <aside className="bk-drawer" onClick={e => e.stopPropagation()}>
        <CardDetail {...props} headerAction={<OpenCardPageLink cardID={props.card.placement.card_id} />} />
      </aside>
    </div>
  )
}

/** Link out to the host's standalone card page, when it mounts one. Absent
 *  rather than disabled on a host that does not — an empty route is how
 *  a host says it has no such page. */
function OpenCardPageLink({ cardID }: { cardID: string }) {
  const { routes } = useBridgeConfig()
  if (!routes.card) return null
  return (
    <Link className="bk-link-ref-action" to={`${routes.card}/${encodeURIComponent(cardID)}`} title="Open this card as a page">
      open ↗
    </Link>
  )
}

function EntityLinkRow({
  link,
  onOpenChat,
  onDeleteLink,
}: {
  link: CardLink
  onOpenChat: OpenChatFn
  onDeleteLink: (linkID: string) => void
}) {
  const { routes, noteboardBasePath, fetch: fetchFn } = useBridgeConfig()
  const [title, setTitle] = useState<string | null>(null)

  const isSessionLink = link.entity_type === 'session' && !!link.entity_ref
  // A pull request, a commit or a remote repo is a real address. Resolving it
  // here rather than in kanban-store is deliberate: the store publishes an
  // entity-type registry and leaves resolution to whoever knows the service.
  const target = entityTarget(link.entity_type, link.entity_ref)
  // `note` is noteboard's whole item space, todos included — the registry entry
  // points at `/api/items`, not at a notes-only route.
  const isNoteLink = link.entity_type === 'note' && !!link.entity_ref

  useEffect(() => {
    if (!isNoteLink || !noteboardBasePath) return
    let canceled = false
    fetchNoteboardItemRef(fetchFn, noteboardBasePath, link.entity_ref)
      .then(ref => {
        if (!canceled && ref?.title) setTitle(ref.title)
      })
      .catch(() => {
        // Unresolvable is a legible outcome — the raw ref stays on screen.
      })
    return () => {
      canceled = true
    }
  }, [isNoteLink, noteboardBasePath, fetchFn, link.entity_ref])

  return (
    <li>
      <span className="bk-link-type">{link.entity_type}</span>
      {isSessionLink ? (
        <>
          <button
            type="button"
            className="bk-link-ref bk-link-ref-action"
            title={`Open chat session ${link.entity_ref}`}
            onClick={() => onOpenChat({ ref: link.entity_ref, dispatchedAt: link.created_at })}
          >{link.entity_ref} ↗</button>
          <SessionRunsOnLabel sessionID={link.entity_ref} />
        </>
      ) : isNoteLink && routes.notes ? (
        <Link
          className="bk-link-ref bk-link-ref-action"
          to={`${routes.notes}?item=${encodeURIComponent(link.entity_ref)}`}
          // The id stays reachable on hover: the title is the readable name, the
          // uuid is what the row actually records.
          title={`Open ${link.entity_ref} in notes`}
        >{title ?? link.entity_ref} ↗</Link>
      ) : target ? (
        <a
          className="bk-link-ref bk-link-ref-action"
          href={target.href}
          target="_blank"
          // noreferrer as well as noopener: this opens a repository URL, and the
          // referrer would say which board the reader came from.
          rel="noopener noreferrer"
          // The full ref stays on hover. The label is the readable part — a
          // shortened sha or a PR number — and the ref is what the row records.
          title={link.entity_ref}
        >{target.label} ↗</a>
      ) : isLocalPathRef(link.entity_type) ? (
        // A path on this machine, shown as one. Not underlined and not
        // clickable, because there is nothing for a browser to open.
        <code className="bk-link-ref bk-link-path">{link.entity_ref}</code>
      ) : (
        <span className="bk-link-ref">{title ?? link.entity_ref}</span>
      )}
      {link.label && <span className="bk-link-label">{link.label}</span>}
      <button className="bk-link-del" onClick={() => onDeleteLink(link.id)}>×</button>
    </li>
  )
}

function LinkedEmailRow({
  link,
  mailBasePath,
  fetchFn,
  onOpenInMail,
  onDeleteLink,
}: {
  link: CardLink
  mailBasePath: string
  fetchFn: FetchFn
  onOpenInMail: (accountID: string, messageID: string) => void
  onDeleteLink: (linkID: string) => Promise<boolean>
}) {
  const parsed = parseEmailLocator(link.entity_ref)
  const [expanded, setExpanded] = useState(false)
  const [message, setMessage] = useState<MailMessage | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const toggle = async () => {
    if (expanded) { setExpanded(false); return }
    setExpanded(true)
    if (message || loading || !parsed || !mailBasePath) return
    setLoading(true)
    setError(null)
    try {
      const res = await fetchFn(
        `${mailBasePath}/messages/${encodeURIComponent(parsed.messageID)}?account=${encodeURIComponent(parsed.accountID)}`,
      )
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json()
      if (data.error) throw new Error(String(data.error))
      setMessage(data as MailMessage)
    } catch (e) {
      // Reported in place rather than swallowed: a message can genuinely be
      // gone (deleted upstream), and a silently empty preview reads as "this
      // email had no content", which is a different and wrong claim.
      setError(String(e))
    } finally {
      setLoading(false)
    }
  }

  const canOpen = !!parsed && !!mailBasePath
  return (
    <li className="bk-email-row">
      <div className="bk-email-head">
        <button type="button" className="bk-email-toggle" onClick={toggle} disabled={!canOpen}
          title={canOpen ? 'Show this email' : 'Mail service is not configured for this host'}>
          {expanded ? '▾' : '▸'}
        </button>
        <span className="bk-link-label">{link.label || '(no label)'}</span>
        {canOpen && (
          <button
            type="button"
            className="bk-link-ref bk-link-ref-action"
            title={`Open in Mail — account ${parsed!.accountID}`}
            onClick={() => onOpenInMail(parsed!.accountID, parsed!.messageID)}
          >open ↗</button>
        )}
        <button className="bk-link-del" onClick={() => onDeleteLink(link.id)} title="Unlink this email">×</button>
      </div>
      {expanded && (
        <div className="bk-email-body">
          {loading && <span className="bi-empty">Loading…</span>}
          {error && <span className="bridge-error">{error}</span>}
          {message && (
            <>
              <div className="bk-email-meta">
                <strong>{message.meta?.subject || '(no subject)'}</strong>
                <span>{message.meta?.from?.name || message.meta?.from?.email}</span>
                <span>{message.meta?.date ? new Date(message.meta.date).toLocaleString() : ''}</span>
              </div>
              {/* Plain text only — see the note on this component. */}
              <pre className="bk-email-text">
                {cleanEmailBodyForPreview(message.body_text)
                  || message.meta?.snippet
                  || '(this message has only an HTML body — use “open ↗” to read it in Mail)'}
              </pre>
            </>
          )}
        </div>
      )}
    </li>
  )
}

/**
 * One assignee as a chip. A human is an initials avatar, a group is the group
 * glyph and its name; hover gives the full name and the kind.
 *
 * An id the directory cannot resolve — principal-store down, or an id it has
 * never heard of — renders as the raw `principal_…` in a dimmed chip, never as
 * nothing. The assignment is a true record that someone is on this card; only
 * the name is unknown, and a tile with no chip would read as unassigned.
 *
 * A disabled principal resolves and renders struck through. The card was
 * assigned to them and still is; that they have since left is information, not
 * a reason to hide who had the work.
 */
function AssigneeChip({
  assignment,
  directory,
  compact = false,
  onRemove,
}: {
  assignment: CardAssignment
  directory: PrincipalsDirectory
  /** The tile's variant: humans show initials only, and group names truncate. */
  compact?: boolean
  onRemove?: () => void
}) {
  const id = assignment.principal_id
  const principal = directory.byId.get(id)
  if (!principal) {
    const why = directory.loading
      ? 'principals are still loading'
      : directory.error
        ? `principal-store did not answer: ${directory.error}`
        : 'principal-store does not know this id'
    return (
      <span className="bk-assignee bk-assignee-unresolved" data-principal-id={id} title={`${id} — ${why}`}>
        <span className="bk-assignee-name">{id}</span>
        {onRemove && <button type="button" className="bk-assignee-remove" title={`Unassign ${id}`} onClick={onRemove}>×</button>}
      </span>
    )
  }
  const disabled = principalIsDisabled(principal)
  const title = `${principal.display_name} (${principal.kind}${disabled ? ', disabled' : ''})`
  return (
    <span
      className={`bk-assignee bk-assignee-${principal.kind}${disabled ? ' bk-assignee-disabled' : ''}`}
      data-principal-id={id}
      title={title}
    >
      {principal.kind === 'group'
        ? <span className="bk-assignee-glyph" aria-hidden="true">👥</span>
        : <span className="bk-assignee-avatar" aria-hidden="true">{principalInitials(principal.display_name)}</span>}
      {(principal.kind === 'group' || !compact) && (
        <span className="bk-assignee-name">{principal.display_name}</span>
      )}
      {onRemove && (
        <button type="button" className="bk-assignee-remove" title={`Unassign ${principal.display_name}`} onClick={onRemove}>×</button>
      )}
    </span>
  )
}

/**
 * The drawer's "Assigned" section: the chips, and the picker that adds one.
 *
 * `assignments` undefined means the host assembled this card from the per-card
 * routes, which do not carry assignments — dash's card page does exactly that.
 * That is said plainly rather than shown as an empty list, because "nobody is
 * assigned" and "this view was not told who is assigned" are different facts
 * and only one of them is true here. The picker is withheld in that case too:
 * it could write, but the section could not show the result.
 */
function CardAssignees({
  assignments,
  directory,
  onAssign,
  onUnassign,
}: {
  assignments: CardAssignment[] | undefined
  directory: PrincipalsDirectory
  onAssign?: (principalID: string) => Promise<AssignmentOutcome>
  onUnassign?: (principalID: string) => Promise<AssignmentOutcome>
}) {
  const [error, setError] = useState<string | null>(null)
  if (assignments === undefined) {
    return <p className="bk-assignee-note">Assignees are not loaded on this view — open the card on its board.</p>
  }
  const assignedIDs = assignments.map(a => a.principal_id)
  const run = async (outcome: Promise<AssignmentOutcome>) => {
    const result = await outcome
    setError(result.ok ? null : result.error)
    return result.ok
  }
  return (
    <>
      <div className="bk-assignee-list">
        {assignments.map(a => (
          <AssigneeChip
            key={a.principal_id}
            assignment={a}
            directory={directory}
            onRemove={onUnassign ? () => { void run(onUnassign(a.principal_id)) } : undefined}
          />
        ))}
        {assignments.length === 0 && <span className="bk-assignee-empty">Nobody yet.</span>}
      </div>
      {error && <div className="bridge-error bk-assignee-error">{error}</div>}
      {onAssign && (
        <AssigneePicker
          directory={directory}
          assignedIDs={assignedIDs}
          onPick={principalID => run(onAssign(principalID))}
        />
      )}
    </>
  )
}

/** How many matches the picker lists before asking for a narrower search. The
 *  directory is capped at 500 and a list that long is not a choice. */
const ASSIGNEE_MATCHES_SHOWN = 30

/**
 * The assignee picker: a text filter over display names, a kind toggle, and
 * the matches as buttons. Already-assigned and disabled principals are not
 * offered (`pickablePrincipals`).
 *
 * A directory that failed to load renders the failure, not an empty list. An
 * empty list here would say "there is nobody to assign", which is a claim
 * about the organization, when the truth is that principal-store did not
 * answer.
 */
function AssigneePicker({
  directory,
  assignedIDs,
  onPick,
}: {
  directory: PrincipalsDirectory
  assignedIDs: string[]
  onPick: (principalID: string) => Promise<boolean>
}) {
  const [query, setQuery] = useState('')
  const [kind, setKind] = useState<PrincipalKindFilter>('all')
  const [busyID, setBusyID] = useState<string | null>(null)

  const matches = useMemo(
    () => pickablePrincipals(directory.list, { query, kind, excludeIDs: assignedIDs }),
    [directory.list, query, kind, assignedIDs],
  )
  const shown = matches.slice(0, ASSIGNEE_MATCHES_SHOWN)

  const kinds: { value: PrincipalKindFilter; label: string }[] = [
    { value: 'all', label: 'All' },
    { value: 'human', label: 'People' },
    { value: 'group', label: 'Groups' },
  ]

  return (
    <div className="bk-assignee-picker">
      <div className="bk-assignee-picker-row">
        <input
          type="search"
          className="bk-assignee-query"
          placeholder="Assign someone…"
          value={query}
          onChange={e => setQuery(e.target.value)}
          aria-label="Filter principals by name"
        />
        <div className="bk-assignee-kinds" role="group" aria-label="Principal kind">
          {kinds.map(k => (
            <button
              key={k.value}
              type="button"
              className="bk-assignee-kind"
              aria-pressed={kind === k.value}
              onClick={() => setKind(k.value)}
            >{k.label}</button>
          ))}
        </div>
      </div>
      {directory.error && (
        <div className="bridge-error bk-assignee-error">
          Could not load principals: {directory.error}
          {' '}
          <button type="button" className="bk-link-ref-action" onClick={() => { void directory.refresh() }}>retry</button>
        </div>
      )}
      {directory.loading && !directory.error && (
        <div className="bk-assignee-empty">Loading principals…</div>
      )}
      {/* Matches are listed only when there is a directory to list from. A
          failed load with nothing cached must not present "no matches". */}
      {!directory.loading && (directory.list.length > 0 || !directory.error) && (
        <ul className="bk-assignee-matches">
          {shown.map(p => (
            <li key={p.id}>
              <button
                type="button"
                className="bk-assignee-match"
                data-principal-id={p.id}
                disabled={busyID !== null}
                title={`Assign ${p.display_name} (${p.kind})`}
                onClick={async () => {
                  setBusyID(p.id)
                  try { await onPick(p.id) } finally { setBusyID(null) }
                }}
              >
                {p.kind === 'group'
                  ? <span className="bk-assignee-glyph" aria-hidden="true">👥</span>
                  : <span className="bk-assignee-avatar" aria-hidden="true">{principalInitials(p.display_name)}</span>}
                <span className="bk-assignee-name">{p.display_name}</span>
                {p.email && <span className="bk-assignee-email">{p.email}</span>}
                {busyID === p.id && <span className="bk-assignee-busy">…</span>}
              </button>
            </li>
          ))}
          {shown.length === 0 && (
            <li className="bk-assignee-empty">
              {directory.list.length === 0 ? 'principal-store has no principals.' : 'No matching principals.'}
            </li>
          )}
          {matches.length > shown.length && (
            <li className="bk-assignee-empty">{matches.length - shown.length} more — narrow the search.</li>
          )}
        </ul>
      )}
    </div>
  )
}

function AddLinkForm({
  entityTypes,
  onAdd,
}: {
  entityTypes: { type: string }[]
  onAdd: (entity_type: string, entity_ref: string, label?: string) => Promise<boolean>
}) {
  const [type, setType] = useState(entityTypes[0]?.type ?? 'session')
  const [ref, setRef] = useState('')
  const [label, setLabel] = useState('')
  return (
    <form
      className="bk-new-form bk-link-form"
      onSubmit={async e => {
        e.preventDefault()
        if (!type || !ref.trim()) return
        const ok = await onAdd(type, ref.trim(), label.trim() || undefined)
        if (ok) { setRef(''); setLabel('') }
      }}
    >
      <select value={type} onChange={e => setType(e.target.value)}>
        {entityTypes.map(t => <option key={t.type} value={t.type}>{t.type}</option>)}
      </select>
      <input placeholder="entity ref (id/url/path)" value={ref} onChange={e => setRef(e.target.value)} />
      <input placeholder="label (optional)" value={label} onChange={e => setLabel(e.target.value)} />
      <button type="submit" className="bi-save-btn">+ Link</button>
    </form>
  )
}
