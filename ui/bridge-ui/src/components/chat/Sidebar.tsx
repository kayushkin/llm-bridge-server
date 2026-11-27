import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { VList, type VListHandle } from 'virtua'
import {
  useSessionList,
  useActiveSession,
  usePrefetch,
  useFilters,
  useSessionActions,
  useFolders,
  useConnState,
  useSubagentSessions,
  isRunningState,
  ARCHIVE_FOLDER,
  useSessionPrincipals,
} from '@kayushkin/chat-core'
import type { SessionSummary } from '@kayushkin/chat-core'
import { sessionStarterOf, starterChipLabel, type SessionStarter } from '../../sessionStarters'
import { useBridgeConfig } from '../../context'
import { useBridgeHarnesses } from '../../useBridgeHarnesses'
import { useBridgeInstances } from '../../useBridgeInstances'
import { useBridgeMachines } from '../../useBridgeMachines'
import { EditableName } from './EditableName'
import { OrchestratorRow } from './OrchestratorRow'
import { AwaitingReplySidebarSection } from './AwaitingReplySidebarSection'
import { useHomeSession } from './useHomeSession'
import { StatusDot } from './StatusDot'
import { isArchivedFolder } from './bridgeAdapters'
import {
  loadCollapsedFolders,
  loadCollapsedProjectGroups,
  loadGroupByProject,
  loadGroupByServer,
  loadHideArchived,
  loadServerTab,
  saveCollapsedFolders,
  saveCollapsedProjectGroups,
  saveGroupByProject,
  saveGroupByServer,
  saveHideArchived,
  saveServerTab,
  THIS_SERVER_TAB_KEY,
} from './sidebarPersistence'
import { useProjectStore } from '../../useProjectStore'
import { groupSessionsByProject } from '../../projects'
import { FolderQuestionRollupMarker, SessionQuestionMarker } from './QuestionMarkers'
import { isAwaitingHumanState, isOperatorSessionType, isSessionAwaitingOperator } from '@kayushkin/chat-core'
import { useSessionsWithOpenOperatorQuestion } from './sessionsWithOpenQuestion'
import NewSessionMenu from './NewSessionMenu'
import { NEW_SESSION_MENU_TOGGLE_ATTRIBUTE, type NewSessionMenuScope } from '../../newSessionMenu'
import type { NewSessionTarget } from './useNewSessionTarget'
import { useSelectSession } from './useSelectSession'
import SignalsInbox from './SignalsInbox'
import { useConnectedServers } from './ConnectedServers'
import {
  ConnectedServerSessionRow,
  ConnectedServerSidebarFeed,
  ConnectedServerTabBody,
  ServerTabStrip,
  connectedServerTab,
  type ConnectedServerListReport,
  type ServerTab,
} from './ConnectedServerSidebar'
import { connectedServerSessionKey, readableConnectionIds, thisServerLabel, type ConnectedServerSessionRef } from '../../serverListings'
import { mergeSessionsNewestFirst, sessionDayHeading, shownServerTab, type MergedSessionEntry } from '../../sidebarServerTabs'
import type { HarnessInfo } from '../../types'
import styles from './Chat.module.css'

/** The seven multi-select axes chat-core's FilterState supports. Each axis is a
 *  `string[]` (empty = no filter); `machine` matches SessionSummary.instanceId and is
 *  resolved to a machine display name/emoji for the label only (the filter value stays
 *  the instanceId). `principal` matches who the session was started as, named from the
 *  server's GET /sessions/principals; '' is a session started as no person. */
const AXES = ['harness', 'status', 'type', 'purpose', 'mode', 'machine', 'principal'] as const

/** What a chip row is called above its chips, where the axis name alone would not say. */
const AXIS_LABELS: Partial<Record<Axis, string>> = { principal: 'started by' }
type Axis = (typeof AXES)[number]

/** One selectable chip on a filter axis.
 *
 *  `value` is what the filter actually matches on and is never shown: it is the raw
 *  harness name or the instanceId. `label` is what the user reads, and for the axes
 *  that have server-registered display metadata it is the registered label, so a chip
 *  says the same thing as the session rows below it.
 *
 *  `image` is server-relative (a `HarnessInfo.image`), so rendering it needs the base
 *  path the harness registry was read from — `ChipRow` takes that separately rather
 *  than each option carrying a resolved copy of the same prefix. */
interface ChipOption {
  value: string
  count: number
  label: string
  emoji?: string
  image?: string
}

/** Toggle a value in/out of an axis's selection array. */
function toggle(arr: string[], value: string): string[] {
  return arr.includes(value) ? arr.filter((v) => v !== value) : [...arr, value]
}

/** How close to the bottom of the scroller counts as "at the end", in CSS pixels.
 *  Roughly two session rows, so the next page starts landing before the user hits
 *  the floor rather than after. */
const NEAR_END_PX = 96

/** Shortest gap between two AUTO-loaded pages, in milliseconds. Only throttles the
 *  scroll path; the button is a deliberate act and is never throttled. */
const AUTO_PAGE_COOLDOWN_MS = 1000

/** What the open context menu is pointed at, and where to draw it.
 *
 *  `type` decides the menu's whole contents, so it is part of the state rather than
 *  inferred from the id: a folder and a session can share a name, and reading the id
 *  back to work out what was right-clicked is exactly the name-join that goes wrong
 *  the first time they collide. */
type ContextMenuTarget =
  | { type: 'session'; sessionId: string; x: number; y: number }
  | { type: 'folder'; folder: string; x: number; y: number }

/** Sidebar → SessionList parity: mirrors bridge-ui's SessionList DOM (bc-session-list,
 *  bc-new-session, bc-session-search, bc-inst-filter / bc-class-filter-row chips,
 *  bc-session-item, bc-folder-header) so it inherits the shared stylesheet. A
 *  new-session split control, content search, a collapsible multi-axis chip filter
 *  with live counts, per-row status dot fed the row's own state, inline rename (bridge-ui's
 *  exported EditableName), and folder groups with collapse. Virtualized. All
 *  filtering/sorting/grouping is chat-core's — no transcript re-derivation here.
 *
 *  The loaded set is ONE page deep; older sessions are paged in on scroll-to-end or
 *  from the button at the foot of the list. */
interface SidebarProps {
  /** Where a new chat should be aimed, resolved once by the page (see
   *  `useNewSessionTarget`) and shared with the cold-load bootstrap so the button and
   *  the bootstrap can never target different instances. */
  newTarget: NewSessionTarget
  /** Fold the sidebar down to the vertical strip. The flag itself lives in `Chat` —
   *  collapsing unmounts this component, so a flag held here would go with it. */
  onToggleCollapse: () => void
  /** Called after this list has opened a session — a row, an inbox card, or the
   *  "+ New" button. The minimal (mobile) chrome renders this list inside a slide-over
   *  drawer, which has to close itself once it has been used, and the signals page has
   *  to give the thread pane back to the session the user just asked for.
   *
   *  A callback rather than watching the active id: the active id also changes for a
   *  `?session=` deeplink and for a reference chip inside a transcript, neither of
   *  which this list is involved in. */
  onAfterOpenSession: () => void
  /** Show the signals page in place of the thread (the inbox's "Open all"). */
  onOpenSignalsPage: () => void
  /** Show the work graph in place of the thread (the pinned row under the
   *  Orchestrator's). */
  onOpenWorkGraph: () => void
  /** Whether the work graph is the view on screen, so its row reads as selected. */
  workGraphOpen: boolean
  /** Show the projects view in place of the thread: the landing page for null, or
   *  one project's page. */
  onOpenProjects: (projectId: string | null) => void
  /** Whether the projects view is on screen, so its row reads as selected. */
  projectsOpen: boolean
  /** Show a multichat conversation in place of the thread (a Waiting on you row). */
  onOpenConversation: (roomID: string) => void
  /** The conversation on screen, or null, so its row reads as selected. */
  openConversationRoomID: string | null
  /** Show a connected server's session in place of the thread, or a new
   *  session there for a null id (a row or "+" in that server's folder). */
  onOpenConnectedServerSession: (ref: ConnectedServerSessionRef) => void
  /** The connected server's session on screen, or null, so its row reads as
   *  selected and none of this server's does. */
  openConnectedServerSession: ConnectedServerSessionRef | null
}

export default function Sidebar({
  newTarget,
  onToggleCollapse,
  onAfterOpenSession,
  onOpenSignalsPage,
  onOpenWorkGraph,
  workGraphOpen,
  onOpenProjects,
  projectsOpen,
  onOpenConversation,
  openConversationRoomID,
  onOpenConnectedServerSession,
  openConnectedServerSession,
}: SidebarProps) {
  const {
    groups: allGroups,
    loading,
    facets,
    moreSessions,
    loadingOlderSessions,
    loadOlderSessions,
  } = useSessionList()
  // The durable half of "this session is waiting on you". Session state is the transient
  // half and cannot answer for a session that has since finished — see the hook.
  const { sessionIds: sessionsWithOpenOperatorQuestion } = useSessionsWithOpenOperatorQuestion()
  /** Whether a row should carry the `?`. Only ever for a session that is the operator's
   *  to answer — interactive or herald, by chat-core's one rule; a background session
   *  (autoworker, delegate, scheduled task, system, external) never raises it, by state
   *  or by signal, and its signals are listed in the inbox's Background agents group.
   *
   *  A UNION, and both halves are load-bearing:
   *
   *   - an open QUESTION signal is durable and survives the session finishing, which is
   *     the case session state cannot see;
   *   - `awaiting_permission` mints no signal at all — only `AskUserQuestion` does
   *     (`recordAskUserQuestionSignals`) — so a plain tool approval is visible ONLY in the
   *     state, and dropping the state half would have traded one blind spot for another.
   *
   * `awaiting_user` overlaps both and is harmless in either. */
  const waitingOnOperator = useCallback(
    (session: SessionSummary) =>
      sessionsWithOpenOperatorQuestion.has(session.sessionId) || isSessionAwaitingOperator(session),
    [sessionsWithOpenOperatorQuestion],
  )
  const { id: activeId } = useActiveSession()
  // Not chat-core's bare `select`: opening a row also records the session as its
  // instance's last session, the same as the header's nav arrows. See `useSelectSession`.
  //
  // Wrapped ONCE here rather than at each row: every row already goes through this one
  // binding, so the drawer's close travels with the open by construction and a row added
  // later cannot forget it.
  const selectSession = useSelectSession()
  const select = useCallback((sessionId: string) => {
    selectSession(sessionId)
    onAfterOpenSession()
  }, [selectSession, onAfterOpenSession])
  // The orchestrator's home session, an ordinary chat session opened through the same
  // `select` as any row; the pinned row and the conductor dock both open it.
  const homeSession = useHomeSession(select)
  const prefetch = usePrefetch()
  const { filter, set, contentSearchReach, searching, searchError } = useFilters()
  const { newSession, archive, unarchive, rename, error: actionError } = useSessionActions()
  const {
    folders,
    createFolder,
    deleteFolder,
    renameFolder,
    moveSessionToFolder,
    error: folderError,
  } = useFolders()
  const { instanceMap } = useBridgeInstances()
  const { machineMap } = useBridgeMachines()
  const { harnessMap, basePath } = useBridgeHarnesses()
  // An empty `orchestratorBasePath` means this host proxies no orchestrator, in which
  // case its row is not rendered at all rather than pointed at a guessed path.
  const { orchestratorBasePath, gitActivityBasePath } = useBridgeConfig()
  // Liveness of the session-list stream. `listLoading` only covers the FIRST fetch, so
  // without this an empty list after a dropped stream reads as "you have no sessions".
  // 'open' is the only state in which updates are flowing — see useConnState.
  const connState = useConnState()
  const connected = connState === 'open'

  // The servers. With none connected the sidebar is this server's list, as it always
  // was. With one or more, "by server" in the filters picks between a tab per server
  // (this server's first) and one list of every server's sessions, newest first.
  // Both read back in the useState initializer, so the first paint is already right.
  const { servers: connectedServerListings, thisServerName, thisServerNameError, storeOf } = useConnectedServers()
  const showServerChoice = connectedServerListings.length > 0
  const [groupByServer, setGroupByServerState] = useState(loadGroupByServer)
  const toggleGroupByServer = useCallback(() => {
    const next = !groupByServer
    saveGroupByServer(next)
    setGroupByServerState(next)
  }, [groupByServer])
  const [storedServerTab, setStoredServerTab] = useState(loadServerTab)
  const showServerTab = useCallback((key: string) => {
    saveServerTab(key)
    setStoredServerTab(key)
  }, [])
  const shownTab = shownServerTab(
    storedServerTab,
    THIS_SERVER_TAB_KEY,
    connectedServerListings.map((server) => server.connectionId),
  )
  const serverTabsOn = showServerChoice && groupByServer
  const mergedListOn = showServerChoice && !groupByServer
  const shownConnectedServer = serverTabsOn
    ? connectedServerListings.find((server) => server.connectionId === shownTab) ?? null
    : null
  // What each readable connected server's store shows, reported by its feed.
  const [connectedReports, setConnectedReports] = useState<ReadonlyMap<string, ConnectedServerListReport>>(new Map())
  const reportConnectedList = useCallback((connectionId: string, report: ConnectedServerListReport | null) => {
    setConnectedReports((prev) => {
      if (report === null) {
        if (!prev.has(connectionId)) return prev
        const next = new Map(prev)
        next.delete(connectionId)
        return next
      }
      return new Map(prev).set(connectionId, report)
    })
  }, [])
  const readableConnections = useMemo(
    () => readableConnectionIds(connectedServerListings).filter((id) => !!storeOf(id)),
    [connectedServerListings, storeOf],
  )
  const openConnectedServerNewSession = useCallback((connectionId: string) => {
    onOpenConnectedServerSession({ connectionId, sessionId: null })
  }, [onOpenConnectedServerSession])
  /** The New session menu for `scope`. A pick on this server goes through
   *  `newTarget.optsFor`, so the picked harness's saved defaults are merged on
   *  exactly as the bare "+ New" button's are; a pick on a connected server is
   *  started by the menu in that server's own store, and its thread opened. */
  const renderNewSessionMenu = (scope: NewSessionMenuScope) => (
    <NewSessionMenu
      scope={scope}
      onPick={({ instanceId, harness }) => {
        // A deliberate pick is what the bare button reads next time.
        newTarget.remember(instanceId)
        newSession(newTarget.optsFor(instanceId, harness))
        onAfterOpenSession()
      }}
      onStartedOnConnectedServer={openConnectedServerNewSession}
      onClose={closeNewMenu}
    />
  )

  // The filters are a pop-up under the search box's funnel. Open is not kept: a
  // pop-up that opened itself on every load would sit over the list.
  const [filtersOpen, setFiltersOpen] = useState(false)
  const filtersRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!filtersOpen) return
    const closeOnOutside = (event: MouseEvent) => {
      if (filtersRef.current && !filtersRef.current.contains(event.target as Node)) setFiltersOpen(false)
    }
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setFiltersOpen(false)
    }
    document.addEventListener('mousedown', closeOnOutside)
    document.addEventListener('keydown', closeOnEscape)
    return () => {
      document.removeEventListener('mousedown', closeOnOutside)
      document.removeEventListener('keydown', closeOnEscape)
    }
  }, [filtersOpen])
  const [collapsedFolders, setCollapsedFolders] = useState<Set<string>>(loadCollapsedFolders)

  // "Hide archived" drops the Archive folder before anything reads the list, so the
  // folder view, the project groups and every count agree. Archived is a folder fact
  // (see `isArchivedFolder`), so dropping that folder drops every archived session.
  const [hideArchived, setHideArchivedState] = useState(loadHideArchived)
  const toggleHideArchived = useCallback(() => {
    const next = !hideArchived
    saveHideArchived(next)
    setHideArchivedState(next)
  }, [hideArchived])
  const groups = useMemo(
    () => (hideArchived ? allGroups.filter((g) => !isArchivedFolder(g.folder)) : allGroups),
    [hideArchived, allGroups],
  )

  // Grouping by project. The toggle and each project group's collapse are read back
  // in the initializer like the folders above. The projects and every filed session
  // come from one shared read (`useProjectStore`: `GET /projects` and
  // `GET /links?entity_type=session`), which the header's filing control refreshes,
  // so a filed session moves groups as soon as project-store has it.
  const projectStore = useProjectStore()
  const [groupByProject, setGroupByProjectState] = useState(loadGroupByProject)
  const toggleGroupByProject = useCallback(() => {
    const next = !groupByProject
    saveGroupByProject(next)
    setGroupByProjectState(next)
  }, [groupByProject])
  const [collapsedProjectGroups, setCollapsedProjectGroups] = useState<Set<string>>(loadCollapsedProjectGroups)
  useEffect(() => {
    saveCollapsedProjectGroups(collapsedProjectGroups)
  }, [collapsedProjectGroups])
  const toggleProjectGroupCollapsed = useCallback((key: string) => {
    setCollapsedProjectGroups((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }, [])
  // Null when the list is grouped by folder. Until project-store has answered — or
  // when it failed, which the sidebar says in words below the toggle — the list keeps
  // its folders rather than showing every session as "Not filed".
  const projectGroups = useMemo(() => {
    if (!groupByProject || !projectStore.enabled || !projectStore.loaded || projectStore.error) return null
    return groupSessionsByProject(
      groups.flatMap((g) => g.sessions),
      projectStore.sessionLinks,
      projectStore.projects,
    )
  }, [groupByProject, projectStore.enabled, projectStore.loaded, projectStore.error, projectStore.sessionLinks, projectStore.projects, groups])

  // Write on change rather than from inside the toggles. The collapse toggle is a
  // functional `setState` updater, and React may call one of those twice — a save in
  // there would be a side effect in a function that is meant to be pure. An effect
  // keyed on the value is idempotent and stays correct if a second control ever
  // collapses a folder.
  useEffect(() => {
    saveCollapsedFolders(collapsedFolders)
  }, [collapsedFolders])

  const toggleFolderCollapsed = useCallback((folder: string) => {
    setCollapsedFolders((prev) => {
      const next = new Set(prev)
      if (next.has(folder)) next.delete(folder)
      else next.add(folder)
      return next
    })
  }, [])
  // Expand only — what the folder header's question rollup does. It is NOT the toggle
  // above: the rollup exists because collapsing hid the rows that carry the answerable
  // markers, so a rollup that could also collapse would hide the very thing it is
  // pointing at. Returning `prev` unchanged when the folder is already open keeps the
  // state identity stable, so nothing re-renders on a no-op.
  const expandFolder = useCallback((folder: string) => {
    setCollapsedFolders((prev) => {
      if (!prev.has(folder)) return prev
      const next = new Set(prev)
      next.delete(folder)
      return next
    })
  }, [])
  // The New session menu under the New button, listing every server.
  const [newMenu, setNewMenu] = useState<{ scope: NewSessionMenuScope; at: 'new-button' } | null>(null)
  const newMenuOpen = newMenu?.at === 'new-button'
  const closeNewMenu = useCallback(() => setNewMenu(null), [])
  const listRef = useRef<VListHandle>(null)

  // Which row's question dropdown is open, by session id — null for none.
  //
  // Held HERE rather than inside each marker, and that is the whole of the "one open at a
  // time" rule: a marker cannot close a sibling it has never heard of, so the id lives
  // where every row can see it and opening one is what closes the other. It is also why
  // the rows stay cheap — see the `rows` memo below.
  const [openQuestionSessionId, setOpenQuestionSessionId] = useState<string | null>(null)
  const toggleQuestionPanel = useCallback((sessionId: string) => {
    setOpenQuestionSessionId((current) => (current === sessionId ? null : sessionId))
  }, [])
  const closeQuestionPanel = useCallback(() => setOpenQuestionSessionId(null), [])

  // The row/folder context menu. One at a time, so opening a second closes the first.
  const [ctxMenu, setCtxMenu] = useState<ContextMenuTarget | null>(null)
  // Which sub-form the open menu is showing, if any. Kept beside `ctxMenu` rather than
  // inside it so closing the menu clears both in one place.
  const [folderForm, setFolderForm] = useState<'new' | 'rename' | null>(null)
  const [folderDraft, setFolderDraft] = useState('')
  const folderInputRef = useRef<HTMLInputElement>(null)

  const closeMenu = useCallback(() => {
    setCtxMenu(null)
    setFolderForm(null)
    setFolderDraft('')
  }, [])

  // Any click outside the menu dismisses it. The menu stops propagation on its own
  // clicks, so its buttons still fire; the listener is only mounted while a menu is
  // open, so the sidebar costs nothing the rest of the time. A right-click fires
  // `contextmenu`, not `click`, which is why opening a second menu from another row
  // does not immediately close it.
  useEffect(() => {
    if (!ctxMenu) return
    window.addEventListener('click', closeMenu)
    return () => window.removeEventListener('click', closeMenu)
  }, [ctxMenu, closeMenu])

  useEffect(() => {
    if (folderForm) folderInputRef.current?.focus()
  }, [folderForm])

  const openSessionMenu = useCallback((e: React.MouseEvent, sessionId: string) => {
    e.preventDefault()
    e.stopPropagation()
    setFolderForm(null)
    setFolderDraft('')
    setCtxMenu({ type: 'session', sessionId, x: e.clientX, y: e.clientY })
  }, [])

  const openFolderMenu = useCallback((e: React.MouseEvent, folder: string) => {
    e.preventDefault()
    e.stopPropagation()
    // The unfoldered bucket is the ABSENCE of a folder, not a folder — there is
    // nothing to rename or delete, so it gets no menu at all.
    if (!folder) return
    setFolderForm(null)
    setFolderDraft('')
    setCtxMenu({ type: 'folder', folder, x: e.clientX, y: e.clientY })
  }, [])

  // Every menu action closes the menu first and then mutates. The mutations are
  // void-returning and optimistic (chat-core reverts and reports through `error`), so
  // there is nothing to await and no reason to hold the menu open over the request.
  const submitFolderForm = useCallback(() => {
    const name = folderDraft.trim()
    if (!name || !ctxMenu) return
    const target = ctxMenu
    closeMenu()
    if (folderForm === 'rename') {
      if (target.type === 'folder') renameFolder(target.folder, name)
      return
    }
    // "+ New folder" on a session row is ONE call: filing a session into a folder that
    // does not exist creates it in the same server transaction. On a folder header
    // there is no session to file, so the folder is created empty.
    if (target.type === 'session') moveSessionToFolder(target.sessionId, name)
    else createFolder(name)
  }, [folderDraft, folderForm, ctxMenu, closeMenu, renameFolder, moveSessionToFolder, createFolder])

  // Infinite scroll: reaching the end of the loaded window pulls the next page. virtua
  // 0.44's VList has no onRangeChange, so the end is measured off the handle rather
  // than off a row index — which is the right measure anyway, since the rows are a
  // flattened mix of folder headers and sessions.
  //
  // chat-core refuses a second page while one is in flight, and that is NOT enough on
  // its own: each page that lands grows the scroller under a cursor still resting at
  // the bottom, so the browser fires another scroll, which pulls another page. Measured
  // in a browser, holding at the foot chain-loaded eleven pages — 1,100 sessions — in
  // 640ms, each with a valid distinct cursor. The whole point of paging is that the
  // client never holds the whole table, so the auto-load is rate-limited to one page
  // per second. The button below has no cooldown: a click is a request, a scroll that
  // the last page caused is not.
  const lastAutoPageAtRef = useRef(0)
  const onScroll = useCallback(
    (offset: number) => {
      const handle = listRef.current
      if (!handle || !moreSessions || loadingOlderSessions) return
      if (handle.scrollSize - (offset + handle.viewportSize) >= NEAR_END_PX) return
      const now = Date.now()
      if (now - lastAutoPageAtRef.current < AUTO_PAGE_COOLDOWN_MS) return
      lastAutoPageAtRef.current = now
      loadOlderSessions()
    },
    [moreSessions, loadingOlderSessions, loadOlderSessions],
  )

  // instanceId → machine display {name, emoji}, resolved through the instance's
  // machine_id. Used for the machine chip labels and the per-row emoji. The FILTER
  // value stays the instanceId — this is presentation only.
  // Who each session was started as, named by the server. A row shows its starter only
  // when this caller can see sessions from more than one: a person who sees only their
  // own would read the same initials on every row.
  const sessionPrincipals = useSessionPrincipals()
  const starters = useMemo(() => {
    const principals = sessionPrincipals.data ?? []
    if (principals.length < 2) return null
    return new Map(principals.map((p) => [p.principalId, sessionStarterOf(p)]))
  }, [sessionPrincipals.data])

  const machineDisplay = useCallback(
    (instanceId: string | undefined): { name: string; emoji?: string } | undefined => {
      if (!instanceId) return undefined
      const machineId = instanceMap.get(instanceId)?.machine_id
      const m = machineId ? machineMap.get(machineId) : undefined
      return { name: m?.name || instanceId, emoji: m?.emoji }
    },
    [instanceMap, machineMap],
  )

  // Per-axis chip options with live counts, sourced from chat-core's cross-axis
  // `facets` (counts over the FULL loaded set, independent of the active filter —
  // fixes the Phase-1 "faceted-within-selection" limitation). The active selection is
  // always kept in the option set so it can be cleared even at count 0. The machine
  // axis's facet keys are instanceId values, resolved to a machine name/emoji label.
  const axisOptions = useMemo(() => {
    const out = {} as Record<Axis, ChipOption[]>
    for (const axis of AXES) {
      if (axis === 'principal') {
        // Every starter the server knows of, with its count over ALL the caller's
        // sessions, not just the loaded window: a person whose sessions are all older
        // than the first page is still a choice, and picking them asks the server.
        const counts = new Map((sessionPrincipals.data ?? []).map((p) => [p.principalId, p]))
        for (const sel of filter.principal) if (!counts.has(sel)) counts.set(sel, { principalId: sel, displayName: '', sessionCount: 0 })
        out.principal = [...counts.values()]
          .map((p): ChipOption => ({ value: p.principalId, count: p.sessionCount, label: starterChipLabel(p) }))
          .sort((a, b) => b.count - a.count || a.label.localeCompare(b.label))
        continue
      }
      const counts: Record<string, number> = { ...facets[axis] }
      for (const sel of filter[axis]) if (!(sel in counts)) counts[sel] = 0
      const opts = Object.entries(counts).map(([value, count]): ChipOption => {
        if (axis === 'machine') {
          const d = machineDisplay(value)
          return { value, count, label: d?.name || value, emoji: d?.emoji }
        }
        if (axis === 'harness') {
          // The facet key is the raw harness name (`claude_code`). Every other surface
          // on this page shows the server's registered label and logo for it, session
          // rows included, so the chip does too — same `harnessMap`, no icon table and
          // no second request. A harness the server has not registered keeps its raw
          // name as the label: that is what the session itself reports, and inventing a
          // prettier one here would be a second source of truth for the same string.
          const info = harnessMap.get(value)
          return {
            value,
            count,
            label: info?.label || value,
            emoji: info?.emoji || undefined,
            image: info?.image,
          }
        }
        return { value, count, label: value }
      })
      opts.sort((a, b) => a.label.localeCompare(b.label))
      out[axis] = opts
    }
    return out
    // `harnessMap` arrives from a poll, so it is empty on the first render and fills in
    // a moment later. Left out of these deps the chips keep the raw ids until something
    // unrelated re-renders them, which is indistinguishable from the bug being unfixed.
  }, [facets, filter, machineDisplay, harnessMap, sessionPrincipals.data])

  const activeFilterCount = AXES.reduce((n, a) => n + filter[a].length, 0)
  // The Filters badge also counts "hide archived", which hides rows like a chip does.
  // `activeFilterCount` stays chips only: "show all" clears exactly those.
  const filterBadgeCount = activeFilterCount + (hideArchived ? 1 : 0)
  const searchActive = filter.search.trim() !== ''
  // How many rows the list is actually showing. `groups` IS chat-core's
  // `visibleSessions`, so summing it is the same number `visibleCount` reports —
  // not a second opinion about what is on screen.
  const visibleSessionCount = useMemo(
    () => groups.reduce((n, g) => n + g.sessions.length, 0),
    [groups],
  )
  // A connected server's sessions as the sidebar lists them: its store's visible set,
  // with the archived ones left out when "hide archived" is on, as this server's are.
  const connectedSessionsShown = useCallback(
    (report: ConnectedServerListReport | undefined): SessionSummary[] => {
      if (!report) return []
      return hideArchived ? report.sessions.filter((s) => !isArchivedFolder(s.folderName)) : report.sessions
    },
    [hideArchived],
  )
  const serverTabs = useMemo<ServerTab[]>(() => [
    {
      key: THIS_SERVER_TAB_KEY,
      label: thisServerLabel(thisServerName),
      title: thisServerNameError
        ? `This server. Its name could not be read: ${thisServerNameError}`
        : thisServerName
          ? `${thisServerName}, this server`
          : 'This server. It has no name yet: set one in its server settings',
      count: visibleSessionCount,
      blockedWord: null,
    },
    ...connectedServerListings.map((server) => {
      const report = connectedReports.get(server.connectionId)
      return connectedServerTab(server, report ? connectedSessionsShown(report).length : null, !!report)
    }),
  ], [thisServerName, thisServerNameError, visibleSessionCount, connectedServerListings, connectedReports, connectedSessionsShown])
  // The one list: this server's visible sessions and every connected server's,
  // newest first. Null while the tabs, or no connected server, are what is shown.
  const mergedEntries = useMemo<MergedSessionEntry[] | null>(() => {
    if (!mergedListOn) return null
    const entries: MergedSessionEntry[] = groups.flatMap((g) => g.sessions.map((session) => ({ server: 'this' as const, session })))
    for (const server of connectedServerListings) {
      for (const session of connectedSessionsShown(connectedReports.get(server.connectionId))) {
        entries.push({ server: 'connected', connectionId: server.connectionId, serverLabel: server.label, session })
      }
    }
    return mergeSessionsNewestFirst(entries)
  }, [mergedListOn, groups, connectedServerListings, connectedReports, connectedSessionsShown])
  const shownConnectedReport = shownConnectedServer ? connectedReports.get(shownConnectedServer.connectionId) : undefined
  // How many rows the view on screen lists: this server's, a connected server's tab,
  // or the one list of every server.
  const listedSessionCount = shownConnectedServer
    ? connectedSessionsShown(shownConnectedReport).length
    : mergedEntries
      ? mergedEntries.length
      : visibleSessionCount
  // Paging. In the one list the button pages every server that has more.
  const mergedReports = mergedListOn ? [...connectedReports.values()] : []
  const listHasOlderSessions = moreSessions || mergedReports.some((r) => r.moreSessions)
  const listLoadingOlderSessions = loadingOlderSessions || mergedReports.some((r) => r.loadingOlderSessions)
  const loadOlderSessionsShown = () => {
    if (moreSessions) loadOlderSessions()
    for (const report of mergedReports) if (report.moreSessions) report.loadOlderSessions()
  }
  // An empty list means two different things and only this tells them apart: with no
  // filter on, nothing has arrived; with one on, everything is filtered out. So the
  // "Connecting…" empty state is gated on it — saying "connecting" over a filter that
  // simply matches nothing would blame the network for the user's own filter.
  const anyFilterActive = activeFilterCount > 0 || filter.search.trim() !== ''

  // What each visible session spawned.
  //
  // The parent list is every session currently in `groups` — one request for the whole
  // page, which is also what tells the sidebar WHICH rows have children. No count field
  // exists on the wire and none was added: the children come back, and their number is
  // their number.
  //
  // ⚠️ The read is deliberately OUTSIDE the filter. `groups` is chat-core's filtered
  // set and is used here only to decide which parents to ASK about; the children
  // themselves are whatever the server says that parent spawned, so a chip, a search
  // term or the hidden-type default cannot remove one from its parent's list. A list of
  // "what this session spawned" that silently drops three of eight is worse than none.
  const visibleSessionIds = useMemo(
    () => groups.flatMap((g) => g.sessions.map((s) => s.sessionId)),
    [groups],
  )
  const subagentsOf = useSubagentSessions(visibleSessionIds)

  // Which parents are showing their children. In memory, not persisted: unlike a
  // collapsed folder — which is how the user has arranged their workspace — this is
  // transient exploration, and a tree that reopened itself on every load would fight
  // the reason the rows are collapsed in the first place.
  const [expandedSubagentParents, setExpandedSubagentParents] = useState<ReadonlySet<string>>(
    () => new Set(),
  )
  const toggleSubagents = useCallback((sessionId: string) => {
    setExpandedSubagentParents((previous) => {
      const next = new Set(previous)
      if (next.has(sessionId)) next.delete(sessionId)
      else next.add(sessionId)
      return next
    })
  }, [])

  // Rows: folder headers (collapsible) + session rows. All filtering (incl. the
  // machine axis) is chat-core's now — `groups` already reflect it. Flattened into
  // one virtualized list.
  const rows = useMemo(() => {
    const out: React.ReactNode[] = []
    // A session's row, and under it the sessions it spawned when its caret is open.
    // `keyPrefix` scopes the keys: grouped by project, one session can sit under two
    // projects, and every row goes into ONE flat array of siblings.
    const pushSessionRows = (s: SessionSummary, keyPrefix: string): void => {
      out.push(
        <SessionRow
          key={`${keyPrefix}${s.sessionId}`}
          session={s}
          harnessInfo={harnessMap.get(s.harness)}
          basePath={basePath}
          starter={starters ? starters.get(s.principalId) ?? sessionStarterOf({ principalId: s.principalId, displayName: '' }) : undefined}
          displayState={s.state}
          hasOpenOperatorQuestion={sessionsWithOpenOperatorQuestion.has(s.sessionId)}
          active={!workGraphOpen && !projectsOpen && !openConnectedServerSession && s.sessionId === activeId}
          subagentCount={subagentsOf(s.sessionId).length}
          subagentsExpanded={expandedSubagentParents.has(s.sessionId)}
          onToggleSubagents={toggleSubagents}
          // Only the row whose panel is open, and the one that just closed, see this
          // prop change — every other row's props are identical and `SessionRow`'s
          // `memo` bails out. Rebuilding this array is cheap; re-rendering hundreds of
          // rows would not be.
          questionOpen={s.sessionId === openQuestionSessionId}
          onSelect={select}
          onPrefetch={prefetch}
          onArchive={archive}
          onUnarchive={unarchive}
          onRename={rename}
          onContextMenu={openSessionMenu}
          onToggleQuestion={toggleQuestionPanel}
          onDismissQuestion={closeQuestionPanel}
        />,
      )
      // The sessions this one spawned, drawn under it.
      //
      // ⚠️ These come from `subagentsOf`, NOT from `groups`, and that is the whole
      // point rather than a shortcut. `groups` is chat-core's FILTERED set, so a
      // child excluded by a chip, by the search box or by the hidden-type default
      // would silently vanish from its parent's list — and a list of "what this
      // session spawned" that quietly drops three of eight is worse than none. Being
      // a separate read is what makes them immune to all of it.
      //
      // They are full `SessionRow`s: the same component, the same click-to-open, the
      // same status dot. A child that ALSO appears in the flat list therefore appears
      // twice, which is intended — the nested one is a copy, and pulling it out of the
      // main list would mean expanding a caret changed what the list above showed.
      //
      // One level, not a recursion, and that is measured rather than assumed: on this
      // host every one of the 1,325 subagent sessions is a direct child of a top-level
      // session — 8,388 roots, 1,325 children, zero grandchildren. The store does
      // support deeper trees (`internal/store/subagent_test.go` pins an L2), so if
      // that histogram ever grows a third bucket this is the place that needs to
      // learn recursion.
      if (expandedSubagentParents.has(s.sessionId)) {
        for (const child of subagentsOf(s.sessionId)) {
          out.push(
            <SessionRow
              // Prefixed with the parent, because the same session can be BOTH a row
              // in the flat list and a row under its parent — and these go into ONE
              // flat array of siblings, so the bare session id would appear twice as a
              // key.
              //
              // ⚠️ Measured, not assumed: swapping this for the bare id does NOT drop
              // a row, and the spec beside this stays green with it. React's
              // production build renders both and only warns in development, so a
              // duplicate key here is undefined reconciliation behavior rather than a
              // visible break — the kind that surfaces later as row state attaching to
              // the wrong row. Scoped because it is right, not because a test caught
              // it; do not read the green spec as proof this line is load-bearing.
              key={`${keyPrefix}sub:${s.sessionId}:${child.sessionId}`}
              session={child}
              harnessInfo={harnessMap.get(child.harness)}
              basePath={basePath}
              // The child's own state. `useSubagents` already hands back the
              // store's copy of a child where the store holds one, so this is the
              // row the list stream keeps current, not the point-in-time fetch.
              displayState={child.state}
              hasOpenOperatorQuestion={sessionsWithOpenOperatorQuestion.has(child.sessionId)}
              active={!workGraphOpen && !projectsOpen && !openConnectedServerSession && child.sessionId === activeId}
              // A child's own children are not drawn — see the note above on depth.
              subagentCount={0}
              subagentsExpanded={false}
              onToggleSubagents={toggleSubagents}
              nested
              questionOpen={child.sessionId === openQuestionSessionId}
              onSelect={select}
              onPrefetch={prefetch}
              onArchive={archive}
              onUnarchive={unarchive}
              onRename={rename}
              onContextMenu={openSessionMenu}
              onToggleQuestion={toggleQuestionPanel}
              onDismissQuestion={closeQuestionPanel}
            />,
          )
        }
      }
        }

    // The one list of every server: a heading per local day, this server's rows as
    // they always are, and a connected server's rows tagged with its name.
    if (mergedEntries) {
      const now = new Date()
      let day: string | null = null
      for (const entry of mergedEntries) {
        const heading = sessionDayHeading(entry.session.updatedAt, now)
        if (heading !== day) {
          day = heading
          out.push(
            <div key={`d:${heading}`} className="bc-session-day-heading">
              {heading}
            </div>,
          )
        }
        if (entry.server === 'this') {
          pushSessionRows(entry.session, 'm:')
          continue
        }
        out.push(
          <ConnectedServerSessionRow
            key={connectedServerSessionKey(entry.connectionId, entry.session.sessionId)}
            connectionId={entry.connectionId}
            serverName={entry.serverLabel}
            session={entry.session}
            active={openConnectedServerSession?.connectionId === entry.connectionId && openConnectedServerSession.sessionId === entry.session.sessionId}
            onOpen={onOpenConnectedServerSession}
            serverLabel={entry.serverLabel}
          />,
        )
      }
      return out
    }

    if (projectGroups) {
      for (const pg of projectGroups) {
        const collapsed = collapsedProjectGroups.has(pg.key)
        const waitingCount = pg.sessions.reduce(
          (n, s) => (!isArchivedFolder(s.folderName) && waitingOnOperator(s) ? n + 1 : n),
          0,
        )
        out.push(
          <div key={`p:${pg.key}`} className="bc-folder-header" data-project-id={pg.project?.id}>
            <button
              type="button"
              className={styles.folderHeaderMain}
              aria-expanded={!collapsed}
              onClick={() => toggleProjectGroupCollapsed(pg.key)}
              title={pg.project ? `${pg.label} (${pg.project.id})` : 'Sessions not filed under any project'}
            >
              <span className="bc-folder-chevron">{collapsed ? '▸' : '▾'}</span>
              <span className="bc-folder-icon">{pg.project ? '▦' : '·'}</span>
              <span className="bc-folder-name">{pg.label}</span>
              <span className="bc-folder-count">{pg.sessions.length}</span>
            </button>
            {pg.project && (
              <button
                type="button"
                className={styles.projectGroupOpen}
                onClick={() => onOpenProjects(pg.project?.id ?? null)}
                title={`Open ${pg.label}`}
                aria-label={`Open project ${pg.label}`}
              >
                ↗
              </button>
            )}
            <FolderQuestionRollupMarker
              waitingCount={waitingCount}
              collapsed={collapsed}
              folderLabel={pg.label}
              onReveal={() =>
                setCollapsedProjectGroups((prev) => {
                  if (!prev.has(pg.key)) return prev
                  const next = new Set(prev)
                  next.delete(pg.key)
                  return next
                })
              }
            />
          </div>,
        )
        if (collapsed) continue
        for (const s of pg.sessions) pushSessionRows(s, `p:${pg.key}:`)
      }
      return out
    }

    for (const g of groups) {
      const sessions = g.sessions
      // chat-core now emits a group for every folder the SERVER holds, including ones
      // holding nothing — an empty folder is a real row and the sidebar has to be able
      // to draw it (and, once the context-menu child lands, file into it). The one case
      // it is only noise is an active transcript search, where the user is reading hits
      // and a stack of zero-count headers pushes them off screen. Chip filters keep
      // their empty headers, which is what bridge-ui does. Whether a search should
      // flatten the grouping ENTIRELY, as bridge-ui's does, is a product call and it
      // belongs to the search-ranking todo (`31314d6d`), not here.
      if (sessions.length === 0 && searchActive) continue
      // The collapse key is the real folder name — '' for the unfoldered bucket — while
      // 'active' is only its label. Keying by the label would make a server folder
      // actually named "active" collapse the unfoldered sessions along with it.
      const folder = g.folder
      const label = folder || 'active'
      const collapsed = collapsedFolders.has(folder)
      // The rollup count. Counted over `g.sessions`, which IS the filtered set, so a
      // session the active chips exclude is not counted — see the exclusions comment on
      // `SessionRow`. Archived sessions are excluded by folder, so the whole Archive
      // folder header carries no marker either: the user archived those, which is the act
      // of saying they were not going to continue them.
      //
      // It counts the LOADED window, like every other number on this sidebar — the same
      // population as the `bc-folder-count` beside it, so the two cannot disagree. A
      // waiting session on an older page is still not counted, but that is now a CHOICE
      // rather than a limit: the cross-session signals read knows about it, and the count
      // deliberately stays consistent with the row count beside it. Surfacing those is the
      // inbox's job, and there is no inbox here yet.
      const waitingCount = isArchivedFolder(folder)
        ? 0
        : sessions.reduce(
            (n, s) => (waitingOnOperator(s) ? n + 1 : n),
            0,
          )
      out.push(
        // A DIV, not a button, since the header now holds a second control. The class is
        // still bridge-ui's `bc-folder-header` — that rule styles a flex row and does not
        // care about the tag — and the chevron/name/count moved into an inner button so
        // the two controls are siblings rather than a button nested in a button.
        <div
          key={`h:${folder}`}
          className="bc-folder-header"
          onContextMenu={(e) => openFolderMenu(e, folder)}
        >
          <button
            type="button"
            className={styles.folderHeaderMain}
            aria-expanded={!collapsed}
            onClick={() => toggleFolderCollapsed(folder)}
          >
            <span className="bc-folder-chevron">{collapsed ? '▸' : '▾'}</span>
            <span className="bc-folder-icon">📁</span>
            <span className="bc-folder-name">{label}</span>
            <span className="bc-folder-count">{sessions.length}</span>
          </button>
          <FolderQuestionRollupMarker
            waitingCount={waitingCount}
            collapsed={collapsed}
            folderLabel={label}
            onReveal={() => expandFolder(folder)}
          />
        </div>,
      )
      if (collapsed) continue
      for (const s of sessions) pushSessionRows(s, '')
    }
    return out
  }, [
    groups,
    searchActive,
    activeId,
    machineDisplay,
    harnessMap,
    basePath,
    starters,
    collapsedFolders,
    select,
    prefetch,
    archive,
    unarchive,
    rename,
    waitingOnOperator,
    sessionsWithOpenOperatorQuestion,
    openFolderMenu,
    openSessionMenu,
    openQuestionSessionId,
    toggleQuestionPanel,
    closeQuestionPanel,
    toggleFolderCollapsed,
    expandFolder,
    // ⚠️ All three, or the tree is stale in a way nothing reports. `subagentsOf`'s
    // identity changes when a fetch lands or the store gains a child, and that is the
    // only signal that a caret should now exist; omitting it leaves rows that spawned
    // something looking like rows that did not, forever.
    subagentsOf,
    expandedSubagentParents,
    toggleSubagents,
    projectGroups,
    mergedEntries,
    openConnectedServerSession,
    onOpenConnectedServerSession,
    projectsOpen,
    collapsedProjectGroups,
    toggleProjectGroupCollapsed,
    onOpenProjects,
  ])

  // The session the open menu points at, resolved from the loaded window so the menu
  // can tick the folder it is already in and offer "Mark done" or its undo. Null while
  // no session menu is open, or if the row left the window under the open menu.
  const ctxSession = useMemo(() => {
    if (ctxMenu?.type !== 'session') return null
    for (const g of groups) {
      for (const s of g.sessions) if (s.sessionId === ctxMenu.sessionId) return s
    }
    return null
  }, [ctxMenu, groups])

  // What the bare "+ New" button will actually launch: the instance `newTarget` resolved,
  // and the harness and machine that instance belongs to. Read off the SAME
  // `newTarget.opts` the click passes, not a second resolution of the prefs ladder, so the
  // icons cannot name one target while the click starts another. Undefined all the way
  // down when nothing is recorded — the button then shows no target because it has none,
  // rather than naming a guess.
  const newInstance = newTarget.opts ? instanceMap.get(newTarget.opts.instanceId) : undefined
  const newHarness = newTarget.opts ? harnessMap.get(newTarget.opts.harness) : undefined
  // The instance's own embedded machine first, the registry second — the same order
  // `SessionHeader` and `NewSessionMenu` read it in, so all three name one machine even
  // on the first paint, before the `/machines` poll has answered.
  const newMachine =
    newInstance?.machine ?? (newInstance ? machineMap.get(newInstance.machine_id) : undefined)

  // `sessionListColumn` is what makes the virtualizer below size itself: bridge-ui's
  // `bc-session-list` is a scrolling block, and this list scrolls one step further in
  // (see the rule in Chat.module.css).
  return (
    <div className={`bc-session-list ${styles.sessionListColumn}`}>
      <div className="bc-new-session">
        <div className="bc-new-session-wrap">
          <div className="bc-new-session-split">
            {/* The bare button aims at the user's recorded last instance/harness. It
                used to pass nothing, so a one-click new chat was born with no target
                and the first message landed on whatever the server defaulted to. With
                nothing recorded, `opts` is undefined and the chat still opens — the
                click is never blocked, it just has no target to name. */}
            <button
              className="bc-new-session-btn"
              onClick={() => {
                newSession(newTarget.opts)
                onAfterOpenSession()
              }}
              disabled={!newTarget.ready}
              title={
                !newTarget.ready
                  ? 'Loading your last-used instance…'
                  : newInstance
                    ? `New chat in ${newInstance.name}${newHarness ? ` (${newHarness.label}` : ''}${newMachine ? ` on ${newMachine.name})` : newHarness ? ')' : ''}`
                    : 'New chat'
              }
            >
              <span className="bc-new-session-plus" aria-hidden>+</span>
              <span className="bc-new-session-label">New</span>
              {/* The target the click lands on, in the same shape bridge-ui's sidebar
                  uses (`SessionList.tsx`): harness icon @ machine emoji, sharing that
                  stylesheet's `bc-new-session-target*` classes. The harness's own image
                  when it has one and its emoji otherwise — both come from the server's
                  HarnessInfo, so there is no icon table here to drift from it, and no
                  fallback glyph when a harness registers neither. */}
              {newInstance && (
                <span
                  className="bc-new-session-target"
                  aria-label={`${newHarness?.label || newInstance.harness_type} on ${newMachine?.name || 'machine'}`}
                >
                  {newHarness?.image ? (
                    <img
                      className="bc-new-session-target-img"
                      src={`${basePath}${newHarness.image}`}
                      alt=""
                    />
                  ) : newHarness?.emoji ? (
                    <span className="bc-new-session-target-emoji" aria-hidden>
                      {newHarness.emoji}
                    </span>
                  ) : null}
                  {newMachine?.emoji && (
                    <>
                      <span className="bc-new-session-target-at" aria-hidden>@</span>
                      <span className="bc-new-session-target-emoji" aria-hidden>
                        {newMachine.emoji}
                      </span>
                    </>
                  )}
                </span>
              )}
            </button>
            <button
              {...{ [NEW_SESSION_MENU_TOGGLE_ATTRIBUTE]: '' }}
              className="bc-new-session-caret-btn"
              onClick={() => setNewMenu((menu) => (menu?.at === 'new-button' ? null : { scope: { kind: 'all' }, at: 'new-button' }))}
              aria-haspopup="menu"
              aria-expanded={newMenuOpen}
              title="Pick a harness / instance"
              aria-label="Choose harness or environment"
            >
              <span className="bc-new-session-caret" aria-hidden>▾</span>
            </button>
          </div>
          {newMenuOpen && newMenu && renderNewSessionMenu(newMenu.scope)}
        </div>
        {/* Same slot bridge-ui gives it (`SessionList.tsx:463`): the last child of
            `bc-new-session`, which is a flex row whose `bc-new-session-wrap` takes the
            remaining width, so the button sits hard against the sidebar's right edge. */}
        <button
          className="bc-sidebar-collapse-btn"
          onClick={onToggleCollapse}
          title="Collapse sessions"
          aria-label="Collapse sessions"
        >
          ◂
        </button>
      </div>

      {/* The orchestrator's one row, between "+ New" and the search box: a click opens
          its home session as a chat, through the same `select` a session row uses; its
          count and ▸ drop Needs you down under it, with a way to the signals page. */}
      {orchestratorBasePath && (
        <OrchestratorRow
          homeSessionId={homeSession.sessionId}
          homeSessionError={homeSession.error}
          onOpenHomeSession={() => void homeSession.open()}
          onOpenSignalsPage={onOpenSignalsPage}
          onSelectSession={select}
          selected={!workGraphOpen && !projectsOpen && !openConversationRoomID && !openConnectedServerSession && homeSession.sessionId !== null && homeSession.sessionId === activeId}
        />
      )}

      {/* Conversations where someone tagged wrote after your last word, from
          multichat. A row opens the conversation where the thread is. */}
      <AwaitingReplySidebarSection openRoomID={openConversationRoomID} onOpenConversation={onOpenConversation} />

      {/* With no orchestrator row to carry the count, Needs you stays in the column.
          Given `select`, not `selectSession`, so opening a session from a card closes
          the mobile drawer exactly as clicking a row does. */}
      {!orchestratorBasePath && <SignalsInbox onSelectSession={select} onOpenSignalsPage={onOpenSignalsPage} />}

      {/* One search box and one set of filters for every server: the filter this
          server's store holds is copied into each connected server's store by its feed
          below. The filters open as a pop-up from the funnel, so they never take the
          list's height. */}
      <div className="bc-session-search" ref={filtersRef}>
        <input
          type="search"
          className="bc-session-search-input"
          value={filter.search}
          onChange={(e) => set({ search: e.target.value })}
          placeholder="Search…"
          title="Search session names and transcripts"
          aria-label="Search session names and transcripts"
        />
        {/* "results" is the number of rows on screen. That is deliberately NOT the
            "transcript matches" the shortfall line below counts: this one includes name
            matches and is cut down by the filter chips, that one is what the server
            found. */}
        {searchActive && (
          <span className="bc-session-search-status" role="status">
            {searching
              ? 'searching…'
              : `${listedSessionCount} result${listedSessionCount === 1 ? '' : 's'}`}
          </span>
        )}
        <button
          type="button"
          className={`bc-sidebar-page-btn bc-filter-funnel ${filtersOpen ? 'bc-sidebar-page-btn-selected' : ''} ${filterBadgeCount > 0 ? 'bc-filter-funnel-active' : ''}`}
          onClick={() => setFiltersOpen((v) => !v)}
          aria-expanded={filtersOpen}
          aria-haspopup="dialog"
          title={filterBadgeCount > 0 ? `Filters and view: ${filterBadgeCount} on` : 'Filters and view'}
          aria-label="Filters"
        >
          ⏷
          {filterBadgeCount > 0 && <span className="bc-filter-badge">{filterBadgeCount}</span>}
        </button>
        {/* The work graph and projects pages, as small buttons at the end of the search
            row rather than rows styled like sessions. Each is drawn only when the host
            proxies its store. */}
        {gitActivityBasePath && (
          <button
            className={`bc-sidebar-page-btn ${workGraphOpen ? 'bc-sidebar-page-btn-selected' : ''}`}
            onClick={onOpenWorkGraph}
            aria-current={workGraphOpen || undefined}
            title="Work graph: each repo's commit graph, colored by the agent session that made each commit"
            aria-label="Work graph"
          >
            ⎇
          </button>
        )}
        {projectStore.enabled && (
          <button
            className={`bc-sidebar-page-btn ${projectsOpen ? 'bc-sidebar-page-btn-selected' : ''}`}
            onClick={() => onOpenProjects(null)}
            aria-current={projectsOpen || undefined}
            title="Projects: what each is for, its open cards, branches, deploys and filed sessions"
            aria-label="Projects"
          >
            ▦
          </button>
        )}

        {filtersOpen && (
          <div className="bc-inst-filter-body bc-filter-popover" role="dialog" aria-label="Filters and view">
            {/* How the list is shown, as chips in the same row style as the filters
                below. "hide archived" is counted in the badge, since it hides rows. */}
            <div className="bc-inst-filter-chips bc-class-filter-row">
              <span className="bc-class-filter-label">view</span>
              {showServerChoice && (
                <button
                  type="button"
                  className={`bc-inst-chip bc-class-chip ${groupByServer ? 'bc-inst-chip-active' : ''}`}
                  onClick={toggleGroupByServer}
                  aria-pressed={groupByServer}
                  title="A tab per server. Off, every server's sessions share one list, newest first"
                >
                  <span className="bc-class-chip-name">by server</span>
                </button>
              )}
              {projectStore.enabled && (
                <button
                  type="button"
                  className={`bc-inst-chip bc-class-chip ${groupByProject ? 'bc-inst-chip-active' : ''}`}
                  onClick={toggleGroupByProject}
                  aria-pressed={groupByProject}
                  disabled={mergedListOn}
                  title={
                    mergedListOn
                      ? 'Projects are this server’s, so the one list of every server is not grouped by them. Turn on “by server” to group this server’s tab'
                      : 'Group the list under projects instead of folders'
                  }
                >
                  <span className="bc-class-chip-name">by project</span>
                </button>
              )}
              <button
                type="button"
                className={`bc-inst-chip bc-class-chip ${hideArchived ? 'bc-inst-chip-active' : ''}`}
                onClick={toggleHideArchived}
                aria-pressed={hideArchived}
                title="Leave archived sessions out of the list"
              >
                <span className="bc-class-chip-name">hide archived</span>
              </button>
            </div>
            {AXES.map((axis) => {
              const opts = axisOptions[axis]
              // Show an axis when it offers a real choice (>1 option) or has an active
              // selection to clear. MODE is a canonical axis the old chat always exposes,
              // so keep it whenever it has ANY value — otherwise a homogeneous loaded set
              // (all `events`) silently drops the whole group.
              const show = opts.length > 1 || filter[axis].length > 0 || (axis === 'mode' && opts.length > 0)
              return show ? (
                <ChipRow
                  key={axis}
                  label={AXIS_LABELS[axis] ?? axis}
                  options={opts}
                  selected={filter[axis]}
                  basePath={basePath}
                  onToggle={(value) => set({ [axis]: toggle(filter[axis], value) })}
                />
              ) : null
            })}
            {/* The chips are counted over this server's sessions. The machine chips name
                this server's instances, so they filter this server's list only. */}
            {showServerChoice && (
              <div className="bc-filter-popover-note">Counts are this server’s. Machine applies to this server only.</div>
            )}
            {activeFilterCount > 0 && (
              <button
                className="bc-inst-filter-clear"
                onClick={() =>
                  set({ harness: [], status: [], type: [], purpose: [], mode: [], machine: [], principal: [] })
                }
              >
                show all
              </button>
            )}
          </div>
        )}
      </div>

      {/* A transcript search that failed is not a transcript search that found
          nothing: the rows on screen are the local name matches only, and every
          content-only match is missing. */}
      {searchActive && searchError && (
        <div className={styles.searchReach} role="status">
          Transcript search failed ({searchError}) — showing name matches only.
        </div>
      )}

      {/* Content search matches transcripts server-side, but the list can only paint
          sessions it has loaded, so hits for sessions outside the window render
          nothing. Measured on this box: for a 100-hit page, 55–92% of hits fall
          outside it. Without this line the shortfall is completely invisible. */}
      {contentSearchReach && contentSearchReach.hiddenHitCount > 0 && !shownConnectedServer && (
        <div className={styles.searchReach} role="status">
          {contentSearchReach.shownHitCount} of {contentSearchReach.truncated ? 'at least ' : ''}
          {contentSearchReach.hitCount} transcript {contentSearchReach.hitCount === 1 ? 'match' : 'matches'} shown
          {showServerChoice ? ` on ${thisServerLabel(thisServerName)}` : ''}
          {/* "may", not "will". Paging is not guaranteed to reach a hidden hit: the
              search index also holds transcripts whose session row is gone. */}
          {moreSessions ? ' — loading older sessions may surface more.' : '.'}
        </div>
      )}

      {/* One feed per readable connected server, mounted whichever view is on: it
          keeps that server's filter in step with the search box and reports what it
          shows, for its tab's count and the one list. */}
      {readableConnections.map((connectionId) => (
        <ConnectedServerSidebarFeed
          key={connectionId}
          connectionId={connectionId}
          filter={filter}
          onReport={reportConnectedList}
        />
      ))}

      {serverTabsOn && <ServerTabStrip tabs={serverTabs} shown={shownTab} onShow={showServerTab} />}

      {shownConnectedServer ? (
        <div className={`bc-server-tab-scroll ${styles.list}`}>
          <ConnectedServerTabBody
            server={shownConnectedServer}
            open={openConnectedServerSession?.connectionId === shownConnectedServer.connectionId ? openConnectedServerSession : null}
            onOpen={onOpenConnectedServerSession}
            hideArchived={hideArchived}
          />
        </div>
      ) : (<>
      {projectStore.enabled && groupByProject && !mergedListOn && projectStore.error && (
        <div className={styles.projectStoreErrorRow}>
          <span className={styles.groupByProjectError} title={projectStore.error}>
            project-store did not answer, so the list shows folders: {projectStore.error}
          </span>
        </div>
      )}

      {/* The list is stale the moment the stream drops, and a stale sidebar is silent by
          nature — the rows keep rendering, they just stop changing. This strip is the only
          thing that says so. SyncEngine reconnects with backoff on its own, so this
          clears itself. */}
      {!connected && visibleSessionCount > 0 && (
        <div className={styles.sidebarDisconnected}>
          {connState === 'closed' ? 'Disconnected — list is frozen' : 'Reconnecting — list may be stale'}
        </div>
      )}
      {(loading || (!connected && !anyFilterActive)) && listedSessionCount === 0 ? (
        <div className="bc-session-list-empty">
          {connected ? 'Loading sessions…' : 'Connecting…'}
        </div>
      ) : (
        <>
          {listedSessionCount === 0 ? (
            // An empty list has three causes and the user can only act on two of them,
            // so naming which one it is IS the message. Search first, because an active
            // search is the narrowest claim and the one the user just made; "Searching…"
            // leads, because while the transcript half is still out any verdict is a
            // verdict on half the evidence.
            <div className="bc-session-list-empty">
              {searching
                ? 'Searching…'
                : searchActive
                  ? 'No sessions match this search'
                  : activeFilterCount > 0
                    ? 'No sessions match the active filter'
                    : 'No sessions yet'}
            </div>
          ) : (
            <VList ref={listRef} className={styles.list} onScroll={onScroll}>
              {rows}
            </VList>
          )}
          {/* The sidebar holds one page of the newest sessions; this box has thousands.
              The button is the only thing that says so, and it lives OUTSIDE the
              virtualizer so it survives a filter that empties the list, which is
              precisely when the loaded window is too narrow. In the one list it pages
              every server that has more. */}
          {listHasOlderSessions && (
            <button
              type="button"
              className={`bc-session-show-more ${styles.sessionPagerShim}`}
              onClick={loadOlderSessionsShown}
              disabled={listLoadingOlderSessions}
              aria-busy={listLoadingOlderSessions}
            >
              {listLoadingOlderSessions ? 'Loading older sessions…' : 'Load older sessions'}
            </button>
          )}
        </>
      )}
      </>)}

      {/* A refused mutation reverts the row, and a revert on its own is
          indistinguishable from a race — which is precisely how the archive button
          spent its whole life 404ing in silence. Both hooks report the last refusal
          here. */}
      {(folderError || actionError) && (
        <div className={styles.sidebarError} role="alert">
          {folderError || actionError}
        </div>
      )}

      {/* The context menu lives OUTSIDE the VList: it is positioned in viewport
          coordinates from the click, and a virtualized child would be unmounted the
          moment its row scrolled out from under the open menu. */}
      {ctxMenu && (
        <div
          className="bc-ctx-menu"
          style={{ top: ctxMenu.y, left: ctxMenu.x }}
          onClick={(e) => e.stopPropagation()}
        >
          {ctxMenu.type === 'session' && (
            <>
              {/* Archived is a folder fact, not a state fact: `mark-done` moves the
                  session into the canonical Archive folder, and that is what the undo
                  reverses. `ARCHIVE_FOLDER` is chat-core's single spelling of it. */}
              <button
                className="bc-ctx-menu-item"
                onClick={() => {
                  const id = ctxMenu.sessionId
                  const done = ctxSession?.folderName === ARCHIVE_FOLDER
                  closeMenu()
                  if (done) unarchive(id)
                  else archive(id)
                }}
              >
                {ctxSession?.folderName === ARCHIVE_FOLDER ? '↺ Unmark / unarchive' : '✓ Mark done'}
              </button>
              <div className="bc-ctx-menu-divider" />
              <div className="bc-ctx-menu-label">Move to folder</div>
              {/* Only offered when there is a folder to leave. Un-filing an archived
                  session through here would clear the folder without clearing the
                  completed state, so that case belongs to the undo above. */}
              {ctxSession?.folderName && ctxSession.folderName !== ARCHIVE_FOLDER && (
                <button
                  className="bc-ctx-menu-item"
                  onClick={() => {
                    const id = ctxMenu.sessionId
                    closeMenu()
                    moveSessionToFolder(id, '')
                  }}
                >
                  ↩ Remove from folder
                </button>
              )}
              {/* The server's folder list, in the server's order — the SAME array the
                  sidebar groups by, so the menu can never offer a folder that would
                  paint nowhere. */}
              {folders.map((f) => (
                <button
                  key={f}
                  className={`bc-ctx-menu-item ${ctxSession?.folderName === f ? 'bc-ctx-menu-item-active' : ''}`}
                  onClick={() => {
                    const id = ctxMenu.sessionId
                    closeMenu()
                    moveSessionToFolder(id, f)
                  }}
                >
                  📁 {f}
                </button>
              ))}
            </>
          )}

          {ctxMenu.type === 'folder' && folderForm !== 'rename' && (
            <>
              <button
                className="bc-ctx-menu-item"
                onClick={() => {
                  setFolderDraft(ctxMenu.folder)
                  setFolderForm('rename')
                }}
              >
                ✎ Rename folder
              </button>
              {/* Says what it does. `DELETE /folders/{name}` un-files every session in
                  the folder in the same transaction — nothing is deleted but the
                  folder — and a menu item that read only "Delete folder" would leave
                  the user guessing which. */}
              <button
                className="bc-ctx-menu-item bc-ctx-menu-item-danger"
                onClick={() => {
                  const name = ctxMenu.folder
                  closeMenu()
                  deleteFolder(name)
                }}
              >
                🗑 Delete “{ctxMenu.folder}” (sessions are un-filed, not deleted)
              </button>
              <div className="bc-ctx-menu-divider" />
            </>
          )}

          {folderForm ? (
            <div className="bc-ctx-new-folder">
              <input
                ref={folderInputRef}
                className="bc-ctx-new-folder-input"
                value={folderDraft}
                onChange={(e) => setFolderDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') submitFolderForm()
                  if (e.key === 'Escape') {
                    setFolderForm(null)
                    setFolderDraft('')
                  }
                }}
                placeholder={folderForm === 'rename' ? 'New folder name' : 'Folder name'}
                aria-label={folderForm === 'rename' ? 'New folder name' : 'New folder name'}
              />
              <button className="bc-ctx-new-folder-btn" onClick={submitFolderForm}>
                ✓
              </button>
            </div>
          ) : (
            <button className="bc-ctx-menu-item" onClick={() => setFolderForm('new')}>
              + New folder
            </button>
          )}
        </div>
      )}
    </div>
  )
}

function ChipRow({
  label,
  options,
  selected,
  basePath,
  onToggle,
}: {
  label: string
  options: ChipOption[]
  selected: string[]
  /** Resolves an option's `image` (a server-relative path) to a full URL, the same way
   *  the session rows below this do. */
  basePath: string
  onToggle: (value: string) => void
}) {
  return (
    <div className="bc-inst-filter-chips bc-class-filter-row">
      <span className="bc-class-filter-label">{label}</span>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          className={`bc-inst-chip bc-class-chip ${selected.includes(o.value) ? 'bc-inst-chip-active' : ''}`}
          onClick={() => onToggle(o.value)}
          // Facet counts tally the LOADED window only (see selectFacets), and the
          // sidebar is one page deep until the user pages. This tooltip used to read
          // "<label> · N session(s)", which sounds like a total for the box and is
          // wrong by thousands. Name the population the number actually describes.
          title={`${o.label} · ${o.count} of the loaded session(s)`}
        >
          {/* The harness mark, mirroring the session rows: the server's logo when the
              harness registers one, else its emoji, else nothing. bridge-ui's harness
              chips fall back once further, to a `·` — but those chips are icon-only, so
              a mark is the whole chip and an empty one would be an unlabeled button.
              These carry a name and a count, so a missing mark costs nothing and a dot
              standing in for one would appear on every status and type chip too. */}
          {o.image ? (
            <img className="bc-inst-chip-img" src={`${basePath}${o.image}`} alt="" />
          ) : o.emoji ? (
            <span className="bc-inst-chip-emoji">{o.emoji}</span>
          ) : null}
          <span className="bc-class-chip-name">{o.label}</span>
          <span className={styles.chipCount}>{o.count}</span>
        </button>
      ))}
    </div>
  )
}

interface SessionRowProps {
  session: SessionSummary
  /** Server-registered metadata for this session's harness — the single source for
   *  its logo image / emoji. Undefined when the server hasn't registered the harness. */
  harnessInfo?: HarnessInfo
  /** Resolves `harnessInfo.image` (a server-relative path) to a full URL. */
  basePath: string
  /** Who started the session, drawn as initials after its name. Absent when the
   *  caller sees only one starter's sessions. */
  starter?: SessionStarter
  displayState: string
  /** Whether an open QUESTION signal the operator is to answer is recorded against this
   *  session, regardless of what the session is doing now. Always false for a background
   *  session. Resolved once for the whole list and passed down, so a sidebar of 200 rows
   *  costs one request rather than 200. */
  hasOpenOperatorQuestion: boolean
  active: boolean
  /** Whether THIS row's question dropdown is the one open. The sidebar allows one at a
   *  time and owns the id, so a row is only ever told about itself. */
  questionOpen: boolean
  onSelect: (id: string) => void
  onPrefetch: (id: string) => void
  onArchive: (id: string) => void
  onUnarchive: (id: string) => void
  onRename: (id: string, name: string) => void
  /** Right-click opens the folder / mark-done menu, the only way to file or unfile a
   *  session. Takes the event so the menu can be drawn at the pointer. */
  onContextMenu: (e: React.MouseEvent, sessionId: string) => void
  /** Open this row's question dropdown, or close it if it is already the open one. */
  onToggleQuestion: (sessionId: string) => void
  /** Escape or a click outside the dropdown. */
  onDismissQuestion: () => void
  /** How many sessions this one spawned. Zero draws no disclosure control at all —
   *  most sessions spawn nothing, and a caret on every row would be noise. */
  subagentCount: number
  /** Whether this row's subagents are showing. */
  subagentsExpanded: boolean
  onToggleSubagents: (sessionId: string) => void
  /** True for a row drawn UNDER its parent rather than in the list proper. It is the
   *  same session either way — this only changes the indent and the accessible name,
   *  which has to say where the row is or a screen reader hears the session twice with
   *  no way to tell the copies apart. */
  nested?: boolean
}

const SessionRow = memo(function SessionRow({
  session,
  harnessInfo,
  basePath,
  starter,
  displayState,
  hasOpenOperatorQuestion,
  active,
  questionOpen,
  onSelect,
  onPrefetch,
  onArchive,
  onUnarchive,
  onRename,
  onContextMenu,
  onToggleQuestion,
  onDismissQuestion,
  subagentCount,
  subagentsExpanded,
  onToggleSubagents,
  nested = false,
}: SessionRowProps) {
  const isArchived = isArchivedFolder(session.folderName)
  const name = session.displayName || session.sessionId
  const harnessTitle = harnessInfo?.label || session.harness
  // Two deliberate exclusions, both of them product decisions rather than oversights:
  //
  //  - ARCHIVED sessions never surface a `?`. Archiving a session is the user saying they
  //    were not going to continue it; a question it stopped on is not work waiting on
  //    them, and a marker would be the list asking them to reconsider every session they
  //    have already dismissed. The row keeps its ordinary status dot — the state is still
  //    the state, and hiding that would be a lie — it just gets no way to open the
  //    question from here.
  //  - Sessions the active FILTER CHIPS exclude never surface one either, and that falls
  //    out of where the marker lives: `groups` is chat-core's filtered set, so a row that
  //    is filtered out is not rendered and has nothing to draw a marker on. The sidebar
  //    deliberately does not reach past the filter to count them — a filter is the user
  //    narrowing what they want to see, and a badge that ignored it would be a second,
  //    louder list.
  // The union — see `waitingOnOperator` in `Sidebar`. A question outlives the state that
  // raised it, so `completed` and `aborted` rows keep their `?`; `awaiting_permission`
  // mints no signal, so the state half still has to be here. Both halves are the
  // operator's only: a background session's row keeps its plain status dot.
  const awaitingHuman =
    !isArchived &&
    (hasOpenOperatorQuestion || isSessionAwaitingOperator({ type: session.type, state: displayState }))
  const backgroundWaiting = !isOperatorSessionType(session.type) && isAwaitingHumanState(displayState)

  // A subagent that is still working. Marked explicitly rather than left to the status
  // dot, because the dot's job is the state vocabulary and this list is opened to answer
  // one narrower question — which of these is still going. ⚠️ It cannot be read off a
  // terminal state instead: subagent sessions settle to `idle`, not `completed`, so
  // "finished" and "never started" are the same value.
  const running = nested && isRunningState(displayState)

  return (
    <div
      className={[
        'bc-session-item',
        'bc-session-row-grid',
        active ? 'bc-session-item-selected' : '',
        nested ? styles.subagentRow : '',
        running ? styles.subagentRowRunning : '',
      ]
        .filter(Boolean)
        .join(' ')}
      onMouseEnter={() => onPrefetch(session.sessionId)}
      onContextMenu={(e) => onContextMenu(e, session.sessionId)}
    >
      {/* The row is four fixed columns (the `bc-session-row-grid` grid): the status, the
          select button holding the harness mark and the name, the subagent toggle, and
          the archive button. Every column is drawn on every row, empty when it has
          nothing to say, so a row with subagents or a question lines its name up with one
          that has neither.

          The status column: the question marker when the session waits on a person,
          otherwise the plain dot. The marker takes the dot's job for those states rather
          than sitting beside one — two indicators for one fact would read as two facts.
          Outside `bc-session-item-main` because the marker is a button; the dot is not,
          so a click on it selects the row like a click on the name. */}
      {awaitingHuman ? (
        <SessionQuestionMarker
          sessionId={session.sessionId}
          sessionName={name}
          displayState={displayState}
          isActiveSession={active}
          open={questionOpen}
          onToggle={onToggleQuestion}
          onDismiss={onDismissQuestion}
        />
      ) : (
        <span className="bc-session-status" onClick={() => onSelect(session.sessionId)}>
          {/* A background session waiting on a human keeps its true state, but drawn
              still and grey: the pulsing amber `?` is what tells the operator a row is
              theirs, and this one is not. Its signals are in the inbox's Background
              agents group. */}
          <StatusDot
            state={displayState}
            title={backgroundWaiting ? `${displayState} (background session — not counted as needing you)` : displayState}
            className={backgroundWaiting ? 'bc-status-dot-background-waiting' : undefined}
          />
        </span>
      )}
      <button className="bc-session-item-main" onClick={() => onSelect(session.sessionId)}>
        {/* Harness mark: mirror bridge-ui SessionList — the server logo image when the
            harness registers one, else its emoji, else a neutral dot. Never the literal
            harness id. */}
        <span className="bc-session-harness" title={harnessTitle}>
          {harnessInfo?.image ? (
            <img src={`${basePath}${harnessInfo.image}`} alt="" />
          ) : (
            <span className="bc-session-harness-emoji">{harnessInfo?.emoji || '·'}</span>
          )}
        </span>
        <EditableName
          value={name}
          onSave={(next) => onRename(session.sessionId, next)}
          className="bc-session-label"
        />
        {starter && (
          <span className="bc-session-starter" title={starter.title} aria-label={starter.title} data-principal-id={starter.principalId}>
            {starter.initials}
          </span>
        )}
      </button>
      {/* The subagent toggle, at the right-hand end of the row where it moves nothing.
          Outside `bc-session-item-main` because buttons do not nest. Its slot is empty
          rather than holding a disabled caret when nothing was spawned — most sessions
          spawn nothing, and a dead caret on every row is noise. */}
      {subagentCount > 0 ? (
        <button
          type="button"
          className={styles.subagentToggle}
          aria-expanded={subagentsExpanded}
          aria-label={
            subagentsExpanded
              ? `Hide the ${subagentCount} sessions ${name} spawned`
              : `Show the ${subagentCount} sessions ${name} spawned`
          }
          title={`${subagentCount} subagent session${subagentCount === 1 ? '' : 's'}`}
          onClick={() => onToggleSubagents(session.sessionId)}
        >
          <span aria-hidden>{subagentsExpanded ? '▾' : '▸'}</span>
          <span className={styles.subagentToggleCount}>{subagentCount}</span>
        </button>
      ) : (
        <span className="bc-session-slot-empty" aria-hidden />
      )}
      <span
        className="bc-session-menu-btn"
        role="button"
        tabIndex={0}
        title={isArchived ? 'Unarchive' : 'Archive'}
        aria-label={isArchived ? 'Unarchive' : 'Archive'}
        data-archived={isArchived ? 'true' : 'false'}
        onClick={(e) => {
          e.stopPropagation()
          if (isArchived) onUnarchive(session.sessionId)
          else onArchive(session.sessionId)
        }}
      >
        {isArchived ? <UnarchiveIcon /> : <ArchiveIcon />}
      </span>
    </div>
  )
})

/** The archive button's icons: a file box, and the same box with an arrow coming back
 *  out of it. Drawn rather than emoji, which render at a different size and weight on
 *  every platform (🗄 is a filing cabinet on some and a blank box on others). A host
 *  theme can replace them: the button carries `data-archived`. */
const archiveIconProps = {
  viewBox: '0 0 16 16',
  width: 14,
  height: 14,
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.5,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
  'aria-hidden': true,
} as const

function ArchiveIcon() {
  return (
    <svg {...archiveIconProps}>
      <rect x="1.5" y="2.5" width="13" height="3.5" rx="0.8" />
      <path d="M2.8 6v6.7c0 .5.4.8.8.8h8.8c.4 0 .8-.3.8-.8V6" />
      <path d="M6.3 9h3.4" />
    </svg>
  )
}

function UnarchiveIcon() {
  return (
    <svg {...archiveIconProps}>
      <path d="M2.8 7v5.7c0 .5.4.8.8.8h8.8c.4 0 .8-.3.8-.8V7" />
      <path d="M8 11V2.5M5.3 5.2 8 2.5l2.7 2.7" />
    </svg>
  )
}
