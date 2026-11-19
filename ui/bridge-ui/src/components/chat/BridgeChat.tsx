import { ChatConversationPane } from '../messages/ChatConversationPane'
import { OPEN_CONVERSATION_IN_CHAT_EVENT, type OpenConversationInChatDetail } from '../../chatConversationEvents'
import { Fragment, useCallback, useEffect, useRef, useState, useMemo } from 'react'
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom'
import { useBridgeConfig } from '../../context'
import {
  useActiveSession,
  useSessionStatus,
  useComposer,
  useSessionActions,
  useSessionControls,
} from '@kayushkin/chat-core'
import { createPortal } from 'react-dom'
import { readSessionDeeplink, writeSessionParam, initialSessionDeeplinkState, type SessionDeeplinkState } from '../../sessionDeeplink'
import { BridgeAttach } from '../BridgeAttach'
import { GitPanel } from '../GitPanel'
import { ChromeSheet } from '../minimal/ChromeSheet'
import { useMinimalChrome, useRegisterMinimalChrome, MOBILE_BREAKPOINT } from '../minimal/MinimalChromeContext'
import { MinimalTopBar } from '../minimal/MinimalTopBar'
import { SessionDrawer } from '../minimal/SessionDrawer'
import { LinkedKanbanPanel } from './LinkedKanbanPanel'
import { SplitDragHandle } from './SplitDragHandle'
import Sidebar from './Sidebar'
import { WorkGraphView } from './WorkGraphView'
import { ProjectsView } from './ProjectsView'
import { ConnectedServersProvider } from './ConnectedServers'
import { ConnectedServerPane } from './ConnectedServerPane'
import { OpenConnectedServerSessionContext } from './refChipServer'
import {
  readConnectedServerSessionRef,
  sameConnectedServerSessionRef,
  writeConnectedServerSessionRef,
  type ConnectedServerSessionRef,
} from '../../serverListings'
import MinimalPaneSwitch from './MinimalPaneSwitch'
import TurnList from './TurnList'
import Timeline from './Timeline'
import Composer from './Composer'
import {
  SessionSettingsInline,
  SessionSettingsPanel,
  useSessionSettings,
} from './SessionSettings'
import AwaitingYouBanner from './AwaitingYouBanner'
import SessionStatusLine, { useStatusSlotContent } from './SessionStatusLine'
import BudgetBanner from './BudgetBanner'
import SessionHeader from './SessionHeader'
import { useNewSessionTarget, type NewSessionTarget } from './useNewSessionTarget'
import { useGitRepos } from './useGitRepos'
import { useAttachToken } from './useAttachToken'
import { loadMarkdownPref, saveMarkdownPref } from './threadPersistence'
import { loadSidebarCollapsed, saveSidebarCollapsed } from './sidebarPersistence'
import {
  DEFAULT_MOBILE_PANE,
  growSharesOfVisiblePanes,
  loadMobilePane,
  loadPaneSizes,
  loadPanesHidden,
  PANE_KEYS,
  saveMobilePane,
  savePaneSizes,
  savePanesHidden,
  visiblePanes,
  type PaneDrawable,
  type PaneKey,
  type PaneSizes,
  type PanesHidden,
} from './panePersistence'

/** `?view=` values, each a surface drawn in place of the thread: the work graph,
 *  each repo's commits colored by the session that made them, the projects
 *  (`&project=` opens one project's page), a multichat conversation
 *  (`&room=`), opened from the sidebar's Waiting on you section, and a session
 *  on a connected server (`&server=` the connection, `&server_session=` that
 *  server's own session id; none for a new one), opened from that server's
 *  folder in the sidebar. The orchestrator
 *  is not one: it is an ordinary session, its home session, opened like any
 *  other. Nor is the signals page, which has its own route (`routes.signals`)
 *  because it shows no session; `?view=signals`, its old address, is sent there. */
const WORKSPACE_VIEWS = ['work-graph', 'projects', 'conversation', 'connected-server'] as const
type WorkspaceView = (typeof WORKSPACE_VIEWS)[number]

function workspaceViewOf(param: string | null): WorkspaceView | null {
  return WORKSPACE_VIEWS.find((view) => view === param) ?? null
}

/** States in which the session is busy — it drives the live streaming indicator AND
 *  the composer's Stop button, which is why it must be the server's answer rather than
 *  the client's own in-flight-POST flag. `paused` is deliberately absent: a parked
 *  session is not producing anything and there is nothing to interrupt; the composer
 *  shows it its own ⏸ marker instead. */
const STREAMING_STATES = new Set([
  'running',
  'model_generating',
  'tool_running',
  'starting',
  'compacting',
])

/** The chat: sidebar + thread + panes, on the `@kayushkin/chat-core` data layer — it
 *  renders from an in-memory store and the network only reconciles.
 *
 *  Mounts no provider. `<Bridge>` supplies both: `BridgeProvider` (config, and the
 *  one `MinimalChromeProvider` the tab row and this page's phone chrome must share
 *  — bridge-ui `936ec04`) and chat-core's `ChatProvider`, held above the router so
 *  the session store survives a switch to another tab and back.
 *
 *  Plus the cold-load bootstrap.
 *
 *  Landing on `/` used to show an empty pane, because nothing ever opened a chat:
 *  `activeId` starts null (no session is restored across a reload) and no pending pane
 *  was opened either, so the composer typed into a draft that would be sent to whatever
 *  instance the server happened to default to. The bootstrap opens ONE pending chat,
 *  aimed at the same recorded instance the "+ New" button uses.
 *
 *  It fires once, and only after prefs and instances have both loaded — acting earlier
 *  reads an empty prefs snapshot and targets nothing. It re-checks `activeId` at fire
 *  time rather than trusting the value it was mounted with: a session clicked in the
 *  sidebar while prefs were still in flight must not be replaced by a new chat. */
export function BridgeChat() {
  const [searchParams] = useSearchParams()
  const { routes } = useBridgeConfig()
  // The signals page's old address. Sent on before the chat mounts, so a bookmark
  // neither opens a session nor starts a new chat on its way through.
  if (searchParams.get('view') === 'signals') return <Navigate to={routes.signals} replace />
  return <BridgeChatPage />
}

function BridgeChatPage() {
  const target = useNewSessionTarget()
  const { newSession } = useSessionActions()
  const { id: activeId, summary: activeSummary, select } = useActiveSession()
  const bootstrapped = useRef(false)

  // The mobile chrome. Below 640px the library engages `minimal`, and THIS component is
  // the surface that answers it: the top bar, the pane switch, the session drawer and
  // the controls sheet all render under the flag, and `useRegisterMinimalChrome` is how
  // the page says so.
  //
  // ⚠️ Registering is not bookkeeping — it is what re-enables hiding dash's site header
  // (`DashboardLayout.module.css` keys off the `bridge-minimal-chrome` body class, and
  // the library sets that class only once a surface has registered). Until this call
  // existed, `/` engaged minimal mode and drew none of the replacement, so the
  // library deliberately left every navigation in place. See bridge-ui `936ec04`.
  const { minimal, setDrawerOpen, override, setOverride } = useMinimalChrome()
  useRegisterMinimalChrome(minimal)

  // The re-engage pill's condition, and the only thing that needs a width here. It asks
  // what `minimal` cannot: the user has forced the full layout, but is this still a
  // viewport that WOULD have gone minimal? Without it, "Show full layout" is a one-way
  // door — the override is persisted, so the page would come back full every load with
  // no control on screen to undo it.
  const [viewportWidth, setViewportWidth] = useState(
    () => typeof window === 'undefined' ? 1024 : window.innerWidth,
  )
  useEffect(() => {
    if (typeof window === 'undefined') return
    const onResize = () => setViewportWidth(window.innerWidth)
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])
  const showReengage = !minimal && override === 'full' && viewportWidth < MOBILE_BREAKPOINT

  // Deeplink, both directions. `?session=<bridge_id>` opens that session, and the param
  // then keeps naming whatever session is active — so a chat tab can be bookmarked,
  // shared and reloaded back into. Producers of links in this shape: BridgeKanban,
  // BridgeOrchestrator, RefChip's chatHref, and dash's own Orchestrator page — which is
  // now a thin mount of BridgeOrchestrator. All three live in bridge-ui, and all three
  // build the target from `routes` since bridge-ui's own `hrefFor` and its hardcoded
  // paths were deleted (bridge-ui `fea0521`).
  //
  // The reconciler is bridge-ui's, reused whole: the read and the write would otherwise
  // fight, because the write effect sees the PREVIOUS active id in the same commit the
  // read applies a new one, and would push the stale id straight back into the URL for
  // the read to treat as a fresh deeplink. `awaiting` holds writes until focus lands on
  // the id that was read. See sessionDeeplink.ts's header.
  //
  // Note `routes.chat` still resolves to `/` — the producers above land on the ORIGINAL
  // chat, not here. Repointing them is the daily-driver cutover's call, not this one's.
  const [searchParams, setSearchParams] = useSearchParams()
  const navigate = useNavigate()
  const { routes } = useBridgeConfig()
  const deeplink = useRef<SessionDeeplinkState>(initialSessionDeeplinkState)

  // Whole-sidebar collapse. It lives HERE rather than in `Sidebar` because the collapsed
  // state replaces that component with the strip — a flag the sidebar owned would be
  // unmounted along with the sidebar the moment it was set.
  //
  // Read in the `useState` initializer, not in an effect: the first paint has to be the
  // stored answer, or every load of a collapsed page draws the full sidebar and folds it
  // away a frame later. Same rule the folder collapse and the MD/TXT preference follow.
  //
  // ⚠️ The strip REPLACES the sidebar rather than hiding it with a class. bridge-ui also
  // puts `bc-sidebar-collapsed` on its container, and that class is styled by nothing —
  // `grep bc-sidebar-collapsed styles.css` is empty, in bridge-ui and in dash. It is a
  // hook with no rule behind it, so copying it would only add a second thing to keep in
  // step with the render. What is observable is the strip, and that is what the spec
  // asserts.
  //
  // The control ships at every width down to the 640px minimal breakpoint, and no lower.
  // It used to ship at every width full stop, because bridge-ui hides its strip in the
  // minimal chrome (`styles.css:4371`) on the strength of having a `SessionDrawer` to
  // reach the list with, and chat had none — so hiding it here would have taken the
  // collapse away with nothing to replace it. chat now draws that drawer, so the reason
  // for the divergence is spent and the strip follows bridge-ui's rule again.
  //
  // Between 640px and 768px it still matters and is still drawn: the stylesheet turns it
  // into a full-width 36px bar under 768px (`styles.css:2981-2991`) and `.bc-main` stacks,
  // which is the width where giving the thread the sidebar's 40vh back is worth the most
  // and where nothing else reaches the list.
  const [sidebarCollapsed, setSidebarCollapsed] = useState(loadSidebarCollapsed)

  // The write rides with the state change rather than sitting in an effect keyed on the
  // flag, so a value that arrived from storage is never written straight back — only a
  // choice the user actually made is recorded.
  //
  // Deliberately NOT the functional `setSidebarCollapsed(c => …)` form that bridge-ui's
  // `toggleSessionList` uses (`BridgeChat.tsx:179`): React may call an updater twice, and
  // a `localStorage` write inside one is a side effect in a function that is required to
  // be pure. `Sidebar.tsx` records the same reasoning for the folder toggle. Reading the
  // flag off the render instead costs a dep and nothing else — there are two callers and
  // both are clicks: the collapse button inside `Sidebar`, reached through the
  // `onToggleCollapse` prop passed below, and the strip button that expands it again.
  const toggleSidebar = useCallback(() => {
    const next = !sidebarCollapsed
    saveSidebarCollapsed(next)
    setSidebarCollapsed(next)
  }, [sidebarCollapsed])

  // The signals page ("Needs you", opened out), the work graph or the projects take the thread
  // pane's place. It is state here, read from `?view=` once on load and written back
  // by the URL effect below, so it survives a reload and can be linked to. It sits
  // beside `?session=` rather than replacing it: the session stays active behind the
  // view and comes back when the view closes.
  const [workspaceView, setWorkspaceView] = useState<WorkspaceView | null>(
    () => workspaceViewOf(searchParams.get('view')),
  )
  // The project whose page the projects view shows, `?project=`; null is the
  // landing page. Written back by the same URL effect as `view`, for the same
  // reason: that effect must be the only writer.
  const [openProjectId, setOpenProjectId] = useState<string | null>(
    () => searchParams.get('project') || null,
  )
  // The multichat room the conversation view shows, `?room=`. Written back by the
  // same URL effect, for the same reason.
  const [openConversationRoomId, setOpenConversationRoomId] = useState<string | null>(
    () => searchParams.get('room') || null,
  )
  // The connected server's session the pane shows, `?server=` and
  // `?server_session=`. Written back by the same URL effect, for the same
  // reason. Kept apart from `?session=`, which always names one of this
  // server's sessions.
  const [openConnectedServer, setOpenConnectedServer] = useState<ConnectedServerSessionRef | null>(
    () => readConnectedServerSessionRef(searchParams),
  )
  // The work graph's repo (repo-store id) and selected commit, `?repo=` and
  // `?commit=`, so a repo or commit chip can link straight to them. Written
  // back by the same URL effect, for the same reason.
  const [workGraphFocus, setWorkGraphFocus] = useState<{ repoId: number | null; commitSha: string | null }>(() => {
    const repoId = Number(searchParams.get('repo'))
    return {
      repoId: Number.isInteger(repoId) && repoId > 0 ? repoId : null,
      commitSha: searchParams.get('commit') || null,
    }
  })
  const onWorkGraphFocusChange = useCallback((repoId: number | null, commitSha: string | null) => {
    setWorkGraphFocus(current =>
      current.repoId === repoId && current.commitSha === commitSha ? current : { repoId, commitSha })
  }, [])

  // Declared BEFORE the bootstrap effect so an inbound deeplink claims the bootstrap
  // latch first. Otherwise a cold `/?session=<id>` opens a pending "New chat"
  // over the top of the session the link asked for.
  useEffect(() => {
    const { open, state } = readSessionDeeplink(searchParams.get('session'), deeplink.current)
    deeplink.current = state
    if (!open) return
    bootstrapped.current = true
    select(open)
  }, [searchParams, select])

  useEffect(() => {
    if (bootstrapped.current || !target.ready) return
    bootstrapped.current = true
    if (activeId) return
    newSession(target.opts)
  }, [target.ready, target.opts, activeId, newSession])

  useEffect(() => {
    // What stops this from clobbering an inbound deeplink is the `awaiting` latch inside
    // writeSessionParam, not the ordering of these effects: until focus lands on the id
    // that was read, every write is held. BridgeChat additionally guards on "the URL has
    // been read at least once" because it RESTORES a last-used session, so focus can be
    // non-null before the read runs. chat has no such restore — its cold load opens a
    // pending pane and `activeId` stays null — so that guard is inert here, and carrying
    // it would only make a future effect reordering silently work instead of visibly
    // breaking. Measured: with the guard absent and this effect declared first, the whole
    // deeplink spec still passes.
    //
    // It writes `?view=` too, and it must be the ONLY writer of both. react-router's
    // `setSearchParams` builds on the params of the last render, even in its function
    // form, so two writers in one click overwrite each other: opening a session from the
    // signals page removed `view`, then this effect wrote `session` over a copy that
    // still had it, and the page stayed open.
    const { write, value, state } = writeSessionParam(activeId, deeplink.current)
    deeplink.current = state
    const next = new URLSearchParams(searchParams)
    let changed = false
    if (write) {
      if (value) next.set('session', value)
      else next.delete('session')
      changed = true
    }
    if (workspaceViewOf(next.get('view')) !== workspaceView) {
      if (workspaceView) next.set('view', workspaceView)
      else next.delete('view')
      changed = true
    }
    const shownProjectId = workspaceView === 'projects' ? openProjectId : null
    if ((next.get('project') || null) !== shownProjectId) {
      if (shownProjectId) next.set('project', shownProjectId)
      else next.delete('project')
      changed = true
    }
    const shownRoomId = workspaceView === 'conversation' ? openConversationRoomId : null
    if ((next.get('room') || null) !== shownRoomId) {
      if (shownRoomId) next.set('room', shownRoomId)
      else next.delete('room')
      changed = true
    }
    const workGraphShown = workspaceView === 'work-graph'
    const shownRepo = workGraphShown && workGraphFocus.repoId !== null ? String(workGraphFocus.repoId) : null
    const shownCommit = workGraphShown ? workGraphFocus.commitSha : null
    if ((next.get('repo') || null) !== shownRepo) {
      if (shownRepo) next.set('repo', shownRepo)
      else next.delete('repo')
      changed = true
    }
    if ((next.get('commit') || null) !== shownCommit) {
      if (shownCommit) next.set('commit', shownCommit)
      else next.delete('commit')
      changed = true
    }
    const shownConnectedServer = workspaceView === 'connected-server' ? openConnectedServer : null
    if (writeConnectedServerSessionRef(next, shownConnectedServer)) changed = true
    if (!changed) return
    // Replace, not push: browsing sessions must not fill the back stack with one entry
    // per session looked at.
    setSearchParams(next, { replace: true })
  }, [activeId, workspaceView, openProjectId, openConversationRoomId, openConnectedServer, workGraphFocus, searchParams, setSearchParams])

  const afterOpenSession = useCallback(() => {
    setWorkspaceView(null)
    if (minimal) setDrawerOpen(false)
  }, [minimal, setDrawerOpen])
  const openSignalsPage = useCallback(() => {
    if (minimal) setDrawerOpen(false)
    navigate(routes.signals)
  }, [minimal, setDrawerOpen, navigate, routes.signals])
  const openWorkGraph = useCallback(() => {
    setWorkspaceView('work-graph')
    if (minimal) setDrawerOpen(false)
  }, [minimal, setDrawerOpen])
  /** Open a multichat conversation where the thread is. */
  const openConversation = useCallback((roomId: string) => {
    setOpenConversationRoomId(roomId)
    setWorkspaceView('conversation')
    if (minimal) setDrawerOpen(false)
  }, [minimal, setDrawerOpen])
  // A pop-up drawn by the layout asks for a conversation this way: the chat
  // reads `?view=` only when it mounts.
  useEffect(() => {
    const onOpen = (event: Event) => openConversation((event as CustomEvent<OpenConversationInChatDetail>).detail.roomID)
    window.addEventListener(OPEN_CONVERSATION_IN_CHAT_EVENT, onOpen)
    return () => window.removeEventListener(OPEN_CONVERSATION_IN_CHAT_EVENT, onOpen)
  }, [openConversation])
  /** Open a session on a connected server where the thread is, or a new
   *  session there for a null id. */
  const openConnectedServerSession = useCallback((ref: ConnectedServerSessionRef) => {
    setOpenConnectedServer(current => (sameConnectedServerSessionRef(current, ref) ? current : ref))
    setWorkspaceView('connected-server')
    if (minimal) setDrawerOpen(false)
  }, [minimal, setDrawerOpen])
  /** Open the projects view: one project's page, or the landing page for null. */
  const openProjects = useCallback((projectId: string | null) => {
    setOpenProjectId(projectId)
    setWorkspaceView('projects')
    if (minimal) setDrawerOpen(false)
  }, [minimal, setDrawerOpen])

  // One element, rendered in one of two places: in the row on a desktop, inside the
  // drawer on a phone. Not two copies — a second mounted `Sidebar` would open a second
  // session-list subscription and hold its own filter, search and folder-collapse state,
  // so the list in the drawer would answer differently from the one behind it.
  const sidebar = (
    <Sidebar
      newTarget={target}
      onToggleCollapse={toggleSidebar}
      onAfterOpenSession={afterOpenSession}
      onOpenSignalsPage={openSignalsPage}
      onOpenWorkGraph={openWorkGraph}
      workGraphOpen={workspaceView === 'work-graph'}
      onOpenProjects={openProjects}
      projectsOpen={workspaceView === 'projects'}
      onOpenConversation={openConversation}
      openConversationRoomID={workspaceView === 'conversation' ? openConversationRoomId : null}
      onOpenConnectedServerSession={openConnectedServerSession}
      openConnectedServerSession={workspaceView === 'connected-server' ? openConnectedServer : null}
    />
  )

  return (
    // `bc-minimal` is bridge-ui's own container class and every rule this mode needs is
    // already in the stylesheet dash loads: it collapses `bc-main` to a column, hides the
    // session list and its strip, and gives the composer a safe-area gutter. This ships
    // no CSS.
    <ConnectedServersProvider>
    <OpenConnectedServerSessionContext.Provider value={openConnectedServerSession}>
    <div className={`bc-container ${minimal ? 'bc-minimal' : ''}`}>
      {minimal && (
        <MinimalTopBar
          title={
            workspaceView === 'work-graph' ? 'Work graph'
              : workspaceView === 'projects' ? 'Projects'
                : workspaceView === 'conversation' ? 'Conversation'
                : workspaceView === 'connected-server' ? 'Connected server'
                : activeSummary?.displayName || activeSummary?.sessionId || ''
          }
        />
      )}
      <div className="bc-main">
        {/* The strip is the desktop's way back to the list; on a phone that job belongs
            to the drawer, and the stylesheet hides both the list and the strip under
            `bc-minimal` anyway. Rendering neither here keeps the DOM honest about it. */}
        {!minimal && (sidebarCollapsed ? (
          <button
            className="bc-sidebar-strip"
            onClick={toggleSidebar}
            title="Show sessions"
            aria-label="Show sessions"
          >
            <span className="bc-sidebar-strip-chevron" aria-hidden>▸</span>
            <span className="bc-sidebar-strip-label">Sessions</span>
          </button>
        ) : sidebar)}
        <div className="bc-workspaces">
          {workspaceView === 'projects' ? (
            <ProjectsView
              projectId={openProjectId}
              onOpenProject={setOpenProjectId}
              onSelectSession={sessionId => {
                select(sessionId)
                setWorkspaceView(null)
              }}
              onClose={() => setWorkspaceView(null)}
            />
          ) : workspaceView === 'conversation' && openConversationRoomId ? (
            <ChatConversationPane roomID={openConversationRoomId} onClose={() => setWorkspaceView(null)} />
          ) : workspaceView === 'connected-server' && openConnectedServer ? (
            <ConnectedServerPane
              sessionRef={openConnectedServer}
              onSessionChange={setOpenConnectedServer}
              onClose={() => setWorkspaceView(null)}
            />
          ) : workspaceView === 'work-graph' ? (
            <WorkGraphView
              initialRepoId={workGraphFocus.repoId}
              initialCommitSha={workGraphFocus.commitSha}
              onFocusChange={onWorkGraphFocusChange}
              onSelectSession={sessionId => {
                select(sessionId)
                setWorkspaceView(null)
              }}
              onClose={() => setWorkspaceView(null)}
            />
          ) : (
            <ThreadPane newTarget={target} />
          )}
        </div>
      </div>
      {minimal && (
        <>
          {/* The drawer draws the list whatever `sidebarCollapsed` says. Collapsing is a
              desktop arrangement of a row that is not on screen here, and honoring it
              would leave the phone with no way to reach the list at all. */}
          <SessionDrawer>{sidebar}</SessionDrawer>
          <ChromeSheet />
        </>
      )}
      {showReengage && (
        <button
          type="button"
          className="bc-mc-reengage"
          onClick={() => setOverride(null)}
          aria-label="Switch to mobile layout"
        >Use mobile layout</button>
      )}
    </div>
    </OpenConnectedServerSessionContext.Provider>
    </ConnectedServersProvider>
  )
}

/** The active session's thread: header (metadata + view controls), the virtualized
 *  turn/timeline view, and the composer.
 *
 *  `newTarget` is passed down rather than re-resolved here because `useNewSessionTarget`
 *  owns a `useBridgePrefs`, and that hook holds its own local snapshot of the prefs
 *  record with no shared context behind it. A second instance would issue a second
 *  `GET /bridge-prefs` and then keep a copy that drifts from the one the Sidebar writes —
 *  and the controls bar's write-back MERGES over its snapshot, so a stale copy is how a
 *  saved spend ceiling gets resurrected after the user cleared it. One instance, passed. */
export function ThreadPane({
  newTarget,
  terminalUnavailableBecause,
}: {
  newTarget: NewSessionTarget
  /** Why this thread cannot draw a terminal, when it cannot: a connected
   *  server's session is read through the connection's proxy, which the
   *  terminal's WebSocket does not cross. The Terminal tab is then not drawn,
   *  and a pty session says why instead. */
  terminalUnavailableBecause?: string
}) {
  const { id, summary, select } = useActiveSession()

  // `controlsSlot` is the sheet's portal target, non-null exactly while `ChromeSheet` is
  // mounted. See the render below for why the portal is gated on `minimal` as well.
  const { minimal, controlsSlot } = useMinimalChrome()

  // Which single pane the mobile chrome draws. A separate record from `panesHidden`
  // below, and `panePersistence.ts` records why: one pane wide, "both" is not an
  // available answer, so the two questions cannot share a row.
  const [mobilePane, setMobilePaneState] = useState(loadMobilePane)
  const setMobilePane = useCallback((pane: PaneKey) => {
    saveMobilePane(pane)
    setMobilePaneState(pane)
  }, [])

  // Which panes are on screen. Read as a `useState` initializer for the same reason the
  // sidebar collapse and the MD/TXT preference are: the first paint has to be the stored
  // answer, or a user who reads Turns beside Timeline gets one pane and a second one
  // sliding in a frame later, every load.
  const [panesHidden, setPanesHidden] = useState(loadPanesHidden)

  // The write rides with the state change rather than sitting in an effect keyed on the
  // record, so a value that arrived from storage is never written straight back — only a
  // choice the user actually made is recorded.
  //
  // Deliberately NOT the functional `setPanesHidden(p => …)` form: React may call an
  // updater twice, and a `localStorage` write inside one is a side effect in a function
  // that is required to be pure. `Sidebar.tsx` and `toggleSidebar` above record the same
  // reasoning. Reading the record off the render costs a dep and nothing else.
  //
  // The sizes companion this comment used to say was deliberately absent. It was, and the
  // reason it gave was sound: the record's only non-reset writer is a drag handle, and
  // until the line below there was no handle here to write it, so it would have been
  // state whose one writer always wrote the same value. That is no longer true — the
  // resizer is mounted in the split row further down (noteboard `b62e8744`).
  const togglePane = useCallback((key: PaneKey) => {
    const next = { ...panesHidden, [key]: !panesHidden[key] }
    savePanesHidden(next)
    setPanesHidden(next)
  }, [panesHidden])

  // A plain click on a view tab: this pane on screen, every other one off. Most of the
  // time a person wants one view, and toggling meant opening the new one and then closing
  // the old one. Side by side is still one Shift-click away (`togglePane`). Every key is
  // written, so a pane this session cannot draw (Terminal on an events session) is hidden
  // too and does not reappear on the next pty session by surprise.
  const showOnlyPane = useCallback((key: PaneKey) => {
    const next = Object.fromEntries(PANE_KEYS.map((k) => [k, k !== key])) as PanesHidden
    savePanesHidden(next)
    setPanesHidden(next)
  }, [])

  // How the two panes divide the row. Same `useState` initializer as the record above,
  // for the same reason: a stored 3:1 that arrived a frame after paint would show the
  // user an even split and then snap.
  //
  // ⚠️ Hiding a pane deliberately does NOT reset this, and that is the one place chat
  // departs from bridge-ui, which resets a revealed pane's siblings to 1
  // (`togglePane` in `Workspace.tsx`). Its reason does not carry: with seven panes, revealing an
  // eighth into a layout somebody dragged gives it whatever share is left over, so a
  // reset is the only sane arrival. There are exactly two panes here and a drag conserves
  // their total, so the stored ratio IS the arrangement the user made — hiding Timeline
  // to read a wide transcript and bringing it back should return the widths they set,
  // not silently undo them.
  const [paneSizes, setPaneSizesState] = useState(loadPaneSizes)

  // Called once per pointer-move for the length of a drag, so unlike the toggles above
  // this writes storage at frame rate. Measured rather than assumed before leaving it
  // that way: the row is ~40 bytes and `setItem` of it costs single-digit microseconds,
  // some three orders of magnitude under a 16ms frame, so a drag cannot be what makes
  // one drop.
  //
  // The write still rides with the setter rather than moving into an effect keyed on
  // `paneSizes` — the reason `savePanesHidden`'s comment gives holds here too, and an
  // effect would additionally write back the value that was just READ from storage on
  // mount. There is no drag-end hook to defer to in any case: `SplitDragHandle` reports
  // positions, not gestures, which is what lets one implementation serve three callers.
  const setPaneSizes = useCallback((next: PaneSizes) => {
    savePaneSizes(next)
    setPaneSizesState(next)
  }, [])

  // Whether the Turns pane draws its raw audit rendering. NOT persisted, deliberately:
  // it was never persisted as part of the old exclusive switch either, and Raw is an
  // audit surface someone drops into to answer one question. Opening every future
  // session in it because of one look is not a preference anyone expressed.
  const [raw, setRaw] = useState(false)

  // Read as a `useState` initializer, not in an effect: the first paint has to be the
  // stored answer. Rendering markdown and then swapping to plain text a frame later
  // reflows every assistant reply in the pane, and the user watching it has no way to
  // tell that from the toggle mis-firing on its own.
  const [markdown, setMarkdownState] = useState(loadMarkdownPref)

  // The write rides with the state change rather than sitting in an effect keyed on
  // `markdown`, so a value that arrived from storage is never written straight back —
  // only a choice the user actually made is recorded.
  const setMarkdown = useCallback((next: boolean) => {
    saveMarkdownPref(next)
    setMarkdownState(next)
  }, [])

  // Whether the session is producing output: the server's state, and nothing else.
  //
  // This used to be two witnesses OR'd — the list row's state, and an activity folded
  // off the open session's own frames — because the row rides the session-LIST stream
  // and could lag a seconds-old session's whole first turn (measured 2026-08-25,
  // br_1787617143776318105: 12.6s). The state now arrives on the session's own stream
  // too, as a `session_status` event, and chat-core keeps whichever copy is newer, so
  // the row is as fresh as the frames were and there is one witness.
  const status = useSessionStatus(id)
  const streaming = !!status && STREAMING_STATES.has(status.state)

  // Observability, deliberately permanent: the streaming chain has broken twice in
  // ways only visible on a live session (a summary that lags a fresh session; two
  // event-id spaces), and each diagnosis needed exactly these four values at the
  // moment of failure. Written on every change; read from the console or by an
  // instrumented browser run.
  useEffect(() => {
    ;(window as unknown as { __chatStreaming?: object }).__chatStreaming = {
      id,
      streaming,
      statusAsOf: status?.as_of ?? null,
      summaryState: summary?.state ?? null,
    }
  }, [id, streaming, status, summary])

  // The repo list behind the Git pane, and the selection it reads.
  //
  // Held here rather than inside the pane for the reason `useGitRepos` states: the
  // pane takes its list as props so a second reader — the repo dropdown the original
  // chat puts in its header — can share one selection. chat has no such dropdown
  // yet; the state is in the right place for the day it does.
  //
  // The session's own state is the refetch signal, so the working tree is re-read at
  // every turn boundary. That is the moment it matters: an agent that has just
  // finished a turn is exactly the agent that has just finished editing files, and a
  // diff from before the turn is a diff of the wrong tree. `summary.state` is a
  // string here and reads as `''` with no session — `useGitRepos` fetches nothing
  // without a session id, so the empty signal never reaches the wire.
  const gitRepos = useGitRepos(id, summary?.state ?? '')

  // The terminal, for a session running in pty mode.
  //
  // `mode` is the SESSION's, not the harness's: `GET /sessions` reports `events` or
  // `pty` per session (6,069 and 23 of them respectively on this host), and only a pty
  // session has a hub to join. The harness's own `pty` boolean is a different question —
  // whether it CAN be switched — and gates the toggle rather than the pane.
  const isPty = summary?.mode === 'pty'
  // No token is asked for where no terminal can be drawn: the request would
  // only fail.
  const attach = useAttachToken(id, terminalUnavailableBecause ? '' : summary?.mode ?? '')
  const terminalDrawable = isPty && !terminalUnavailableBecause

  // ⚠️ ONE `useSessionControls` for the whole pane, and that is the point of it being here
  // rather than in `SessionSettings`. Its `compacting` is hook-LOCAL `useState` (chat-core
  // `hooks.ts`), set by whichever hook instance called `compact()` — so a second call
  // of the hook inside `TurnList` would hand it an independent flag that never goes true,
  // and the strip would simply never appear. Both consumers must read the SAME instance.
  //
  // The controls object is passed down whole rather than destructured here: the settings
  // components use five of its seven members and own the reasoning about each, and
  // splitting them across a prop list would put that reasoning in two files.
  const controls = useSessionControls(id)

  // ⚠️ Same one-instance rule as `useSessionControls` above, and for the same reason:
  // `useComposer`'s `error` is hook-LOCAL state, set by whichever instance's stop()
  // or resume() threw. The Composer calls those; the turns pane's status slot renders
  // the result. Both must read the SAME instance, so it lives here and is passed down.
  const composer = useComposer(id)
  // Which failed action `composer.error` describes, for the status slot's phrasing
  // ("couldn't stop — still running: …" vs a bare message). Set by the Composer's
  // handlers — the error string alone cannot say which button it belongs to.
  const [composerFailedAction, setComposerFailedAction] = useState<'stop' | 'resume' | null>(
    null,
  )

  // Resolved ONCE here for the same reason `controls` is. The settings now render in three
  // places — the top bar, the header's details dropdown, and the mobile chrome sheet — and
  // each needs the same capability set, model list and pending-pane state. Calling the hook
  // per site would give each its own copy of the local model/effort state, so a pick made
  // in the dropdown would not show up on the bar.
  const settings = useSessionSettings(newTarget, controls)

  // What is on screen, asked once for both readings. On a desktop the panes are
  // independent flags and either, both or neither can be up. Under the minimal chrome the
  // thread is one pane wide, so the switch's answer is the whole answer — and exactly one
  // pane is always up, which is why the empty-state hint below cannot be reached there.
  //
  // The desktop answer is a LIST in draw order rather than a flag per pane, because
  // there are three of them now and the boundaries are between whichever ones survive:
  // hiding Timeline makes Turns and the kanban cards neighbors, with one handle, and a
  // per-pane flag cannot say that.
  //
  // `attach` is the one pane the stored record cannot decide alone: a session in events
  // mode has no terminal to draw whatever flag is stored for it, so drawability is passed
  // in as a fact about the session. The mobile branch answers the same question the same
  // way — a phone that had stored `attach` and then opened an events session would
  // otherwise draw an empty pane with no way back to Turns.
  const paneDrawable = useMemo(() => ({ attach: terminalDrawable }), [terminalDrawable])
  usePaneNumberKeys(paneDrawable, minimal, showOnlyPane, togglePane, setMobilePane)
  const desktopPanes = visiblePanes(panesHidden, paneDrawable)
  const panes =
    minimal
      ? mobilePane === 'attach' && !terminalDrawable
        ? [DEFAULT_MOBILE_PANE]
        : [mobilePane]
      : desktopPanes

  // Built once and shared: the Turns pane draws the status slot, and the fallback below
  // draws it when that pane is hidden. Two readers, one description of what the session
  // is doing.
  const composerStatus = useMemo(
    () => ({
      paused: composer.paused,
      resumable: composer.resumable,
      error: composer.error,
      failedAction: composerFailedAction,
    }),
    [composer.paused, composer.resumable, composer.error, composerFailedAction],
  )
  // The SAME derivation the Turns pane runs. Not local state — every input is a store
  // read or a prop — so computing it twice cannot disagree with itself, unlike the
  // compacting flag, which is hook-local and has to be threaded down from one instance.
  const sessionStatus = useStatusSlotContent(id, controls.compacting, composerStatus)
  // The status slot lives at the foot of the transcript (`e0753eb`: one fixed-height
  // slot whose content swaps in place, rather than four strips popping between the
  // transcript and the composer). That home is right, and it has a hole: the Turns pane
  // can be turned off, and the slot went with it — measured, `role="status"` dropped to
  // zero, so "stopped", "paused", "disconnected", a failed send and "Compacting context…"
  // all went dark on a page still showing Timeline or Cards.
  //
  // A session being stopped is not a fact about the transcript pane, so it cannot be
  // hidden by turning that pane off. Rendered here only when the pane is absent, which
  // keeps ONE slot on screen: the two mounts are exclusive by construction, not by
  // agreement.
  const turnsPaneHidden = !panes.includes('turns')

  // Scoped to the split row rather than `document`, so the lookup cannot reach a
  // `data-pane` on some other surface. dash serves bridge-ui's own chat at `/` and this
  // page at `/` — one document, and the library's panes carry the same attribute.
  const splitRowRef = useRef<HTMLDivElement>(null)

  // Each visible pane's share of the row, not its stored size. This comment used to say a
  // lone pane fills the row whatever its grow number is, and that is false below 1: flex
  // items whose grow numbers add up to less than 1 take only that fraction of the free
  // space. A drag that left Turns at 0.4 drew a lone Turns pane at its 240px floor with an
  // empty block beside it — on a phone, where one pane is all there is, on every visit.
  // `growSharesOfVisiblePanes` says why scaling is safe. Passed to a lone pane too, with
  // no `resizable ?` guard: the shares already give a lone pane the whole row.
  const paneGrowShares = growSharesOfVisiblePanes(paneSizes, panes)
  const paneStyle = (key: PaneKey) => ({ flex: `${paneGrowShares[key]} 1 0` })

  // On a phone the whole top bar is hidden (`.bc-workspace-minimal .bc-header`), so the
  // settings that now live on it would be unreachable. They go into the chrome sheet
  // instead — BOTH clusters, because the sheet has the vertical room the bar does not and
  // there is no reason to keep the bar's one-line triage down there.
  //
  // It keeps the `bc-controls-bar` class purely so `.bc-mc-controls-slot > .bc-controls-bar`
  // still restacks it full-width inside the sheet. There is no longer any such element on
  // a desktop — that is the point of the merge.
  const settingsSheet = (
    <div className="bc-controls-bar">
      <SessionSettingsInline settings={settings} controls={controls} />
      <SessionSettingsPanel settings={settings} controls={controls} />
    </div>
  )

  return (
    // `bc-workspace-minimal` is what hides the header, whose job bridge-ui's own top bar
    // has taken over at this width.
    //
    // ⚠️ It hides the WHOLE header, and the header is now where every session setting
    // lives — so this flag decides what is reachable on a phone, not merely what it looks
    // like. The settings ride into the chrome sheet through the portal below, which is why
    // that portal is not optional chrome.
    //
    // What is still only in the header, and so still off-screen here: the permission
    // select, Tools, System Prompt, Mark done and the nav arrows. That is a placement
    // question rather than a defect — nothing is unreachable, because the sheet's "Show
    // full layout" restores the desktop layout in one tap and the re-engage pill brings
    // the user back. Recorded on noteboard `96afb01c`.
    <div className={`bc-workspace bc-workspace-focused ${minimal ? 'bc-workspace-minimal' : ''}`}>
      {minimal ? (
        // Rendered here rather than beside the top bar as bridge-ui does it, and the
        // divergence has a reason: bridge-ui's switch sits at container level because it
        // drives whichever of N workspaces has focus. chat draws exactly one, so the
        // control belongs with the panes it switches and the state stays in this
        // component. The position on screen is the same either way — it takes the slot
        // the header has just given up, directly under the top bar.
        <MinimalPaneSwitch pane={mobilePane} onPick={setMobilePane} drawable={paneDrawable} />
      ) : null}
      <SessionHeader
        panesHidden={panesHidden}
        paneDrawable={paneDrawable}
        togglePane={togglePane}
        showOnlyPane={showOnlyPane}
        raw={raw}
        setRaw={setRaw}
        markdown={markdown}
        setMarkdown={setMarkdown}
        settings={settings}
        controls={controls}
      />

      {/* A flex row, so revealing the second pane puts it BESIDE the first rather than
          replacing it — which is the whole feature. `bc-chat-split` is bridge-ui's own
          class and its rule (`styles.css:652`) is already in the stylesheet dash loads;
          both pane roots are `flex: 1 1 0` with a min-width there too, so this ships no
          CSS of its own.

          The row is rendered even when one pane is hidden. Mounting it conditionally
          would remount the surviving pane on every toggle, throwing away its scroll
          position — and the pane the user did NOT touch is the one they were reading. */}
      <div className="bc-chat-split" ref={splitRowRef}>
        {/* One handle per ADJACENT pair of visible panes, so two panes draw one boundary
            and three draw two. Each handle names the pair it sits between, which is what
            lets a drag conserve that pair's total and leave every other pane's share
            untouched — the third pane must not move because its neighbors renegotiated.

            `SplitDragHandle` is bridge-ui's, exported for this (`cdb2f0f`) rather than
            copied: its props are an axis, a class name and two accessors, so it needed no
            library change to serve a host with its own split. `bc-split-resizer` is
            bridge-ui's class and its rule is already in the stylesheet dash loads, so this
            ships no CSS.

            Under the minimal chrome `panes` holds exactly one entry, so a phone draws no
            handle — and needs none, with nothing to trade width against. */}
        {panes.map((pane, index) => (
          <Fragment key={pane}>
            {index > 0 && (
              <SplitDragHandle
                axis="horizontal"
                className="bc-split-resizer"
                resolveDraggedPair={() => {
                  const row = splitRowRef.current
                  if (!row) return null
                  const before = panes[index - 1]
                  const elementBefore = row.querySelector<HTMLElement>(`[data-pane="${before}"]`)
                  const elementAfter = row.querySelector<HTMLElement>(`[data-pane="${pane}"]`)
                  // Null rather than a guess. The handle reads this as "not resizable" and
                  // does nothing, which is the honest answer if a pane is not on screen —
                  // measuring the pair from a missing element would write a ratio derived
                  // from a zero width.
                  if (!elementBefore || !elementAfter) return null
                  return {
                    elementBefore,
                    elementAfter,
                    growUnitsBefore: paneSizes[before],
                    growUnitsAfter: paneSizes[pane],
                  }
                }}
                commitGrowUnits={({ growUnitsBefore, growUnitsAfter }) =>
                  setPaneSizes({
                    ...paneSizes,
                    [panes[index - 1]]: growUnitsBefore,
                    [pane]: growUnitsAfter,
                  })
                }
              />
            )}
            {pane === 'turns' && (
              <TurnList
                sessionId={id}
                view={raw ? 'raw' : 'turns'}
                markdown={markdown}
                streaming={streaming}
                compacting={controls.compacting}
                composerStatus={composerStatus}
                style={paneStyle('turns')}
              />
            )}
            {pane === 'timeline' && <Timeline sessionId={id} style={paneStyle('timeline')} />}
            {/* bridge-ui's own pane, mounted unmodified. It self-fetches off the provider
                config, which is why the `kanbanStoreBasePath` this page was missing had to
                land first — without it the pane reads "not configured" and draws nothing,
                the same way the Orchestrator row did before `4fcdd99`.

                `onToggleCollapse` is the pane's own close control, and it hides the pane
                the same way the header toggle does, so the two agree on one record. */}
            {/* bridge-ui's own pane, mounted unmodified — the same deal as the kanban
                pane below, except that its repo list is the CALLER's. See `useGitRepos`.

                This is the one pane the original chat had that dash could not reach any
                other way: `/orchestrator` and `/kanban` are pages in their own right,
                and `GitPanel` was mounted from the chat workspace and nowhere else. */}
            {pane === 'git' && id && (
              <GitPanel
                sessionId={id}
                refetchSignal={summary?.state ?? ''}
                gitRepos={gitRepos.repos}
                selectedRepo={gitRepos.selectedRepo}
                setSelectedRepo={gitRepos.setSelectedRepo}
                gitReposLoading={gitRepos.loading}
                gitReposError={gitRepos.error}
                refreshGitRepos={gitRepos.refresh}
                onToggleCollapse={() => togglePane('git')}
                style={paneStyle('git')}
                paneKey="git"
              />
            )}
            {/* The terminal. `BridgeAttach` is bridge-ui's, mounted unmodified — it owns
                the xterm instance and the WebSocket; what dash owns is the token, and
                `useAttachToken` says why that is read from the server rather than kept
                from the mode-switch response.

                Each state is drawn rather than collapsed into "no terminal": a session
                whose pty hub has gone is told how to get one back, and a gateway that
                cannot be reached says so. The two are not the same situation and a
                single empty pane would make them look it. */}
            {pane === 'attach' && id && (
              <div
                className="bc-split-pane bc-split-pane-attach"
                style={paneStyle('attach')}
                data-pane="attach"
              >
                <div className="bc-split-pane-header">
                  <span className="bc-split-pane-title">Terminal</span>
                  <span className="bc-spacer" />
                  <button
                    type="button"
                    className="bc-split-collapse-btn"
                    aria-label="Hide terminal"
                    title="Hide terminal"
                    onClick={() => togglePane('attach')}
                  >
                    ×
                  </button>
                </div>
                {attach.state.status === 'ready' && (
                  <BridgeAttach sessionId={id} attachToken={attach.state.token} />
                )}
                {attach.state.status === 'loading' && (
                  <div className="bc-git-empty">Connecting…</div>
                )}
                {attach.state.status === 'absent' && (
                  <div className="bc-git-empty">
                    This session has no live terminal. Switch its mode to pty to start one.
                  </div>
                )}
                {attach.state.status === 'error' && (
                  <div className="bc-git-error">Terminal: {attach.state.message}</div>
                )}
              </div>
            )}
            {pane === 'kanban' && id && (
              <LinkedKanbanPanel
                sessionId={id}
                onToggleCollapse={() => togglePane('kanban')}
                style={paneStyle('kanban')}
                paneKey="kanban"
              />
            )}
          </Fragment>
        ))}
        {panes.length === 0 && (
          // Hiding everything is reachable and recoverable — the toggles are in the
          // header, which is still on screen — so it is answered with a hint rather than
          // forbidden by a control that refuses the last click. Same choice bridge-ui
          // made, and `bc-split-empty` is its class and its existing rule.
          <div className="bc-split-empty">
            <div className="bc-split-empty-hint">
              Every pane is hidden. Use the pane toggles above to bring one back.
            </div>
          </div>
        )}
      </div>

      {isPty && terminalUnavailableBecause && (
        <div className="bc-terminal-unavailable" role="note">Terminal: {terminalUnavailableBecause}</div>
      )}
      <BudgetBanner sessionId={id} />
      {id && <AwaitingYouBanner key={id} sessionId={id} />}
      {/* Portaled into the sheet on a phone, and rendered NOWHERE else — on a desktop these
          same controls are on the top bar, which is the whole point of the merge. Rendering
          both would put two model pickers on screen.

          Gated on `minimal` AND the slot, not the slot alone. `controlsSlot` is state set
          from `ChromeSheet`'s callback ref, so on the commit where the viewport widens and
          the sheet unmounts it is briefly a detached node — portaling into that renders the
          settings nowhere for a frame. Reading `minimal` drops the portal in the same commit
          that took the sheet away, by which time the header is back. */}
      {minimal && controlsSlot ? createPortal(settingsSheet, controlsSlot) : null}
      {turnsPaneHidden && sessionStatus !== null && (
        <SessionStatusLine status={sessionStatus} onOpenSession={select} />
      )}
      <Composer
        sessionId={id}
        turnRunning={streaming}
        composer={composer}
        onFailedAction={setComposerFailedAction}
      />
    </div>
  )
}

/** The number keys pick a view: 1 is the first view tab, 2 the second, and so on, in the
 *  order the tabs are drawn. On its own a key shows that pane alone; with Shift it adds or
 *  removes it, the same as clicking a tab. On a phone it picks the one pane shown.
 *
 *  Ignored while typing — in an input, a textarea, a select or anything editable — and
 *  with Alt, Ctrl or ⌘ held, so it never takes a keystroke meant for text or for the
 *  browser. Reads `event.code`, not `event.key`, because Shift turns "1" into "!". */
function usePaneNumberKeys(
  paneDrawable: PaneDrawable,
  minimal: boolean,
  showOnlyPane: (key: PaneKey) => void,
  togglePane: (key: PaneKey) => void,
  setMobilePane: (key: PaneKey) => void,
): void {
  useEffect(() => {
    const drawable = PANE_KEYS.filter((key) => key !== 'attach' || paneDrawable.attach === true)
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.altKey || event.ctrlKey || event.metaKey || event.repeat) return
      const target = event.target as HTMLElement | null
      if (target && (target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName))) return
      const match = /^Digit([1-9])$/.exec(event.code)
      if (!match) return
      const key = drawable[Number(match[1]) - 1]
      if (!key) return
      event.preventDefault()
      if (minimal) setMobilePane(key)
      else if (event.shiftKey) togglePane(key)
      else showOnlyPane(key)
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [paneDrawable, minimal, showOnlyPane, togglePane, setMobilePane])
}
