import { useCallback, useEffect, useRef } from 'react'
import { Link } from 'react-router-dom'
import { useActiveSession, usePendingSession } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import type { ConnectedServerSessionRef } from '../../serverListings'
import { ConnectedServerThread } from './ConnectedServerThread'
import { OnConnectedServer, useConnectedServers } from './ConnectedServers'

/** A connected server's session in the chat's thread pane, in place of this
 *  server's: the same thread view a session here gets (header, controls,
 *  panes, composer), running against that server.
 *  For a null session id it is the new session the New session menu started
 *  in that server's store; the first message starts it there.
 *
 *  Reads the same store the sidebar's tab for that server reads, so the
 *  row clicked and the thread shown cannot disagree. */
export function ConnectedServerPane({
  sessionRef,
  onSessionChange,
  onClose,
}: {
  sessionRef: ConnectedServerSessionRef
  /** The session this pane now shows, when it changed here: a new session
   *  that its first message created. */
  onSessionChange: (ref: ConnectedServerSessionRef) => void
  onClose: () => void
}) {
  const { servers, connectionsLoaded, storeOf } = useConnectedServers()
  const { routes } = useBridgeConfig()
  const server = servers.find(f => f.connectionId === sessionRef.connectionId)

  let blocked: string | null = null
  if (!server) {
    blocked = connectionsLoaded
      ? `This server has no connection ${sessionRef.connectionId}. It may have been removed; the Peers page lists the ones there are.`
      : 'Reading the connected servers…'
  } else if (server.blockedReason) {
    blocked = server.blockedReason
  } else if (!storeOf(server.connectionId)) {
    blocked = `Connecting to ${server.label}…`
  }

  return (
    <div className="bc-workspace bc-connected-server-pane">
      <header className="bc-connected-server-head">
        <span className="bc-connected-server-name" title={server ? `A session on ${server.label}, a connected server` : undefined}>
          🖥 {server ? server.label : sessionRef.connectionId}
        </span>
        {server && <Link to={`${routes.peers}/${encodeURIComponent(server.connectionId)}`} className="bc-connected-server-peers-link">Connection</Link>}
        <button type="button" className="bp-cancel" onClick={onClose} aria-label="Close the connected server's session">Close</button>
      </header>
      {blocked ? (
        <div className="bc-connected-server-blocked">{blocked}</div>
      ) : (
        <OnConnectedServer connectionId={sessionRef.connectionId}>
          <ConnectedServerSessionView sessionRef={sessionRef} serverName={server ? server.label : sessionRef.connectionId} onSessionChange={onSessionChange} />
        </OnConnectedServer>
      )}
    </div>
  )
}

/** Runs against the connected server's store, and keeps it and the URL on
 *  the same session both ways: the URL names the session the pane opens, and
 *  a session the store moves to by itself — a fork, a new session's first
 *  message, a chip's ↗ — goes back into the URL. */
function ConnectedServerSessionView({
  sessionRef,
  serverName,
  onSessionChange,
}: {
  sessionRef: ConnectedServerSessionRef
  serverName: string
  onSessionChange: (ref: ConnectedServerSessionRef) => void
}) {
  const openSession = useCallback(
    (sessionId: string) => onSessionChange({ connectionId: sessionRef.connectionId, sessionId }),
    [onSessionChange, sessionRef.connectionId],
  )
  const { id: activeId, select } = useActiveSession()
  const pending = usePendingSession()
  const wantedId = sessionRef.sessionId

  // Whether the store has reached the session the URL names. Until it has, a
  // different active id is the store still on its previous session, not a move
  // to report.
  const reachedWanted = useRef(false)
  // For a new session: the active id when the pane opened, which is not the
  // session the first message creates.
  const activeIdAtOpen = useRef(activeId)

  useEffect(() => {
    reachedWanted.current = false
    activeIdAtOpen.current = activeId
    if (wantedId && wantedId !== activeId) select(wantedId)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wantedId])

  useEffect(() => {
    if (!activeId) return
    if (wantedId) {
      if (activeId === wantedId) { reachedWanted.current = true; return }
      if (reachedWanted.current) openSession(activeId)
      return
    }
    if (activeId !== activeIdAtOpen.current) openSession(activeId)
  }, [activeId, wantedId, openSession])

  const thread = <ConnectedServerThread connectionId={sessionRef.connectionId} serverName={serverName} openSession={openSession} />
  if (!wantedId) {
    // The New session menu picks the instance and starts the session in this
    // server's store before opening the pane, so there is no second picker here.
    return pending ? thread : (
      <p className="bp-empty">
        Start a session on {serverName} from New ▾ or the + on its server in the sidebar; the first message starts it there.
      </p>
    )
  }
  if (activeId !== wantedId) return <div className="bc-connected-server-blocked">Opening the session…</div>
  return thread
}
