import { useEffect, useMemo } from 'react'
import { useConnState, useFilters, useSessionList, type FilterState, type SessionSummary } from '@kayushkin/chat-core'
import { connectedServerSessionKey, type ConnectedServerListing, type ConnectedServerSessionRef } from '../../serverListings'
import { mirroredFilterPatch } from '../../sidebarServerTabs'
import { OnConnectedServer, useConnectedServers } from './ConnectedServers'
import { isArchivedFolder } from './bridgeAdapters'
import { StatusDot } from './StatusDot'
import styles from './Chat.module.css'

// The connected servers' part of the chat sidebar: the tab strip, one server's
// tab, a row that can sit in the one list beside this server's rows, and the
// feed that keeps each server's store in step with the sidebar's search.

/** One tab of the sidebar's server strip. `key` is `THIS_SERVER_TAB_KEY` or a
 *  connection id. */
export interface ServerTab {
  key: string
  label: string
  /** The whole sentence: what the server is, and why it cannot be read. */
  title: string
  /** Sessions it shows, or null when there is nothing to count. */
  count: number | null
  /** Why it cannot be read, in a word, or null when it can. */
  blockedWord: string | null
}

/** The strip of server tabs above the list. */
export function ServerTabStrip({ tabs, shown, onShow }: {
  tabs: readonly ServerTab[]
  shown: string
  onShow: (key: string) => void
}) {
  return (
    <div className="bc-server-tabs" role="tablist" aria-label="Servers">
      {tabs.map(tab => (
        <button
          key={tab.key}
          type="button"
          role="tab"
          aria-selected={tab.key === shown}
          className={`bc-server-tab${tab.key === shown ? ' bc-server-tab-shown' : ''}${tab.blockedWord ? ' bc-server-tab-blocked' : ''}`}
          onClick={() => onShow(tab.key)}
          title={tab.title}
          data-server-tab={tab.label}
        >
          <span className="bc-server-tab-name">{tab.label}</span>
          {tab.blockedWord ? (
            <span className="bc-server-tab-state">{tab.blockedWord}</span>
          ) : tab.count !== null ? (
            <span className="bc-server-tab-count">{tab.count}</span>
          ) : null}
        </button>
      ))}
    </div>
  )
}

/** A connected server's tab as the strip draws it. */
export function connectedServerTab(server: ConnectedServerListing, count: number | null, readable: boolean): ServerTab {
  const naming = server.labelFromServer
    ? `${server.label}, a connected server`
    : `${server.label}, a connected server (the name typed when connecting: that server has not reported a name of its own)`
  return {
    key: server.connectionId,
    label: server.label,
    title: server.blockedReason ? `${naming}. ${server.blockedReason}` : naming,
    count: readable ? count : null,
    blockedWord: server.blockedWord ?? (server.blockedReason === null && !readable ? 'connecting' : null),
  }
}

/** One session on a connected server. `serverLabel` puts the server's name on
 *  the row, for the one list where rows from every server sit together. */
export function ConnectedServerSessionRow({ connectionId, serverName, session, active, onOpen, serverLabel }: {
  connectionId: string
  /** The server's name, for the row's title. */
  serverName: string
  session: SessionSummary
  active: boolean
  onOpen: (ref: ConnectedServerSessionRef) => void
  serverLabel?: string
}) {
  return (
    <div
      className={`bc-session-item bc-server-session-item${active ? ' bc-session-item-selected' : ''}`}
      data-connection-id={connectionId}
      data-session-id={session.sessionId}
    >
      <span className="bc-session-status">
        <StatusDot state={session.state} title={session.state} />
      </span>
      <button
        type="button"
        className="bc-session-item-main"
        onClick={() => onOpen({ connectionId, sessionId: session.sessionId })}
        title={`${session.displayName || session.sessionId} — ${session.harness}, on ${serverName}`}
      >
        <span className="bc-session-label">{session.displayName || session.sessionId}</span>
      </button>
      {serverLabel && <span className="bc-session-server-tag" title={`On ${serverLabel}`}>{serverLabel}</span>}
    </div>
  )
}

/** A connected server's tab: its sessions under its own folders, live from its
 *  own store; or, when it cannot be read, why. */
export function ConnectedServerTabBody({ server, open, onOpen, hideArchived }: {
  server: ConnectedServerListing
  /** The remote session on screen, so its row reads as selected. */
  open: ConnectedServerSessionRef | null
  onOpen: (ref: ConnectedServerSessionRef) => void
  hideArchived: boolean
}) {
  const { storeOf } = useConnectedServers()
  if (server.blockedReason !== null) {
    return <div className="bc-server-folder-why">{server.blockedReason}</div>
  }
  if (!storeOf(server.connectionId)) {
    return <div className="bc-session-list-empty">Connecting to {server.label}…</div>
  }
  return (
    <OnConnectedServer connectionId={server.connectionId}>
      <ReadableServerTabBody server={server} open={open} onOpen={onOpen} hideArchived={hideArchived} />
    </OnConnectedServer>
  )
}

/** Runs against the connected server's own store (mounted by `OnConnectedServer`). */
function ReadableServerTabBody({ server, open, onOpen, hideArchived }: {
  server: ConnectedServerListing
  open: ConnectedServerSessionRef | null
  onOpen: (ref: ConnectedServerSessionRef) => void
  hideArchived: boolean
}) {
  const { groups: allGroups, loading, moreSessions, loadingOlderSessions, loadOlderSessions } = useSessionList()
  const { filter } = useFilters()
  const connState = useConnState()
  const groups = useMemo(
    () => (hideArchived ? allGroups.filter(g => !isArchivedFolder(g.folder)) : allGroups),
    [hideArchived, allGroups],
  )
  const count = useMemo(() => groups.reduce((n, g) => n + g.sessions.length, 0), [groups])
  const filtered = filter.search.trim() !== '' || filter.harness.length + filter.status.length + filter.type.length + filter.purpose.length + filter.mode.length > 0
  return (
    <div className="bc-server-tab-body">
      {connState !== 'open' && count > 0 && (
        <div className={styles.sidebarDisconnected}>
          {connState === 'closed' ? 'Disconnected — list is frozen' : 'Reconnecting — list may be stale'}
        </div>
      )}
      {loading && count === 0 && <div className="bc-session-list-empty">Loading sessions…</div>}
      {!loading && count === 0 && (
        <div className="bc-session-list-empty">
          {filtered ? `No sessions on ${server.label} match` : `No sessions on ${server.label} that this server may see`}
        </div>
      )}
      {groups.map(group => group.sessions.length === 0 ? null : (
        <div key={`g:${group.folder}`} className="bc-server-folder-group">
          {group.folder && (
            <div className="bc-folder-header bc-server-folder-subheader">
              <span className="bc-folder-icon" aria-hidden>📁</span>
              <span className="bc-folder-name">{group.folder}</span>
              <span className="bc-folder-count">{group.sessions.length}</span>
            </div>
          )}
          {group.sessions.map(session => (
            <ConnectedServerSessionRow
              key={connectedServerSessionKey(server.connectionId, session.sessionId)}
              connectionId={server.connectionId}
              serverName={server.label}
              session={session}
              active={open?.sessionId === session.sessionId}
              onOpen={onOpen}
            />
          ))}
        </div>
      ))}
      {moreSessions && (
        <button
          type="button"
          className="bc-session-show-more"
          onClick={loadOlderSessions}
          disabled={loadingOlderSessions}
          aria-busy={loadingOlderSessions}
        >
          {loadingOlderSessions ? 'Loading older sessions…' : 'Load older sessions'}
        </button>
      )}
    </div>
  )
}

/** What a connected server's store holds for the sidebar right now. */
export interface ConnectedServerListReport {
  /** Its visible sessions, every server flattened, archived ones left in. */
  sessions: SessionSummary[]
  loading: boolean
  moreSessions: boolean
  loadingOlderSessions: boolean
  loadOlderSessions: () => void
}

/** Keeps one connected server's store in step with the sidebar: copies this
 *  server's search and shared filter axes into it, and reports what it then
 *  shows, for the tab's count and the one list. Renders nothing. */
export function ConnectedServerSidebarFeed({ connectionId, filter, onReport }: {
  connectionId: string
  /** This server's filter, the one the sidebar's search box and chips edit. */
  filter: FilterState
  onReport: (connectionId: string, report: ConnectedServerListReport | null) => void
}) {
  return (
    <OnConnectedServer connectionId={connectionId}>
      <SidebarFeed connectionId={connectionId} filter={filter} onReport={onReport} />
    </OnConnectedServer>
  )
}

function SidebarFeed({ connectionId, filter, onReport }: {
  connectionId: string
  filter: FilterState
  onReport: (connectionId: string, report: ConnectedServerListReport | null) => void
}) {
  const { filter: theirs, set } = useFilters()
  const { groups, loading, moreSessions, loadingOlderSessions, loadOlderSessions } = useSessionList()

  useEffect(() => {
    const patch = mirroredFilterPatch(filter, theirs)
    if (patch) set(patch)
  }, [filter, theirs, set])

  const sessions = useMemo(() => groups.flatMap(g => g.sessions), [groups])
  useEffect(() => {
    onReport(connectionId, { sessions, loading, moreSessions, loadingOlderSessions, loadOlderSessions })
  }, [connectionId, sessions, loading, moreSessions, loadingOlderSessions, loadOlderSessions, onReport])
  useEffect(() => () => onReport(connectionId, null), [connectionId, onReport])
  return null
}
