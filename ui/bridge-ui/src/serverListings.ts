// The chat sidebar's servers: this one and each server it is connected to.
// A connected server's sessions stay on that
// server and are read live through this server's proxy for the connection
// (`peerConnectionProxyBasePath`); nothing of theirs is stored here. Every
// remote session is addressed by the connection id AND its own session id,
// because two servers' ids can be the same string.
//
// The pure rules the sidebar, the chat page and the connected-server page
// draw from.

import { PeerAccessLevelAdministrator, type PeerConnectionView } from '@kayushkin/llm-bridge-types'
import { peerConnectionReach, peerConnectionReachText } from './peers'

/** This server's own description of itself. */
export const THIS_SERVER_PATH = '/server'

/** The label of this server's folder: its own name, or "This server" while
 *  it has none. */
export function thisServerLabel(name: string | null | undefined): string {
  const trimmed = name?.trim() ?? ''
  return trimmed || 'This server'
}

/** A connected server's name as that server reports it
 *  (`other_server_name`, its own `server.name` setting), and whether that is
 *  what the label shows. While it has reported none — no name set, or a server
 *  too old to say — the label is the name an administrator typed when making
 *  the connection, and `fromServer` is false so the folder can say so. */
export function connectedServerLabel(view: PeerConnectionView): { label: string; fromServer: boolean } {
  const reported = view.other_server_name?.trim() ?? ''
  if (reported) return { label: reported, fromServer: true }
  return { label: view.connection.display_name, fromServer: false }
}

/** Why the connected server cannot be read now, in plain words, or null when
 *  it can. The proxy would refuse with the same reason; saying it first spares
 *  a page of failed reads, and lets a folder say why it is empty. */
export function connectedServerBlockedReason(view: PeerConnectionView): string | null {
  const { connection } = view
  const name = connectedServerLabel(view).label
  if (connection.removed_at) {
    return `${name} was disconnected at ${new Date(connection.removed_at).toLocaleString()}${connection.remove_reason ? `: ${connection.remove_reason}` : ''}.`
  }
  if (!view.access_held) {
    return `${name} has not given this server any access yet, so there is nothing to open. Its administrator gives a level on its own Peers page.`
  }
  if (peerConnectionReach(view) === 'socket closed') {
    return `${name} is not connected now: the socket between the two servers is closed. It reconnects by itself when the server that dials out is running and online.`
  }
  if (peerConnectionReach(view) === 'not answering') {
    return `${name} is ${peerConnectionReachText(view)}: the socket between the two servers is open, but its bridge has stopped answering requests${view.last_probe_failure ? ` (${view.last_probe_failure})` : ''}. Nothing can be opened there until it answers again; this server keeps checking.`
  }
  return null
}

/** A short word for a folder that cannot be read, for its header. The whole
 *  sentence goes in its title. */
export function connectedServerBlockedWord(view: PeerConnectionView): string | null {
  if (view.connection.removed_at) return 'removed'
  if (!view.access_held) return 'no access'
  if (peerConnectionReach(view) === 'socket closed') return 'offline'
  if (peerConnectionReach(view) === 'not answering') return 'not answering'
  return null
}

export interface ConnectedServerListing {
  connectionId: string
  /** The `server_…` id the connected server reports, or empty while it has
   *  reported none. What a `<id>@<server_id>` reference is matched on. */
  serverId: string
  label: string
  /** False while the label is the typed connection name, not the server's own. */
  labelFromServer: boolean
  /** Null when the server can be read now. */
  blockedReason: string | null
  blockedWord: string | null
}

/** The connected servers' folders, in sidebar order: the ones that can be
 *  read first, then the ones that cannot, then removed connections, each
 *  part by label. Every connection is listed, so a server that went offline
 *  or took its access back stays visible, greyed, saying why. */
export function connectedServerListings(views: readonly PeerConnectionView[]): ConnectedServerListing[] {
  const rank = (view: PeerConnectionView): number =>
    view.connection.removed_at ? 2 : connectedServerBlockedReason(view) ? 1 : 0
  return [...views]
    .sort((a, b) => rank(a) - rank(b) || connectedServerLabel(a).label.localeCompare(connectedServerLabel(b).label))
    .map(view => {
      const { label, fromServer } = connectedServerLabel(view)
      return {
        connectionId: view.connection.id,
        serverId: view.other_server_id ?? '',
        label,
        labelFromServer: fromServer,
        blockedReason: connectedServerBlockedReason(view),
        blockedWord: connectedServerBlockedWord(view),
      }
    })
}

/** The connections whose servers can be read now, by id: the ones the chat
 *  keeps a live session store for. */
export function readableConnectionIds(folders: readonly ConnectedServerListing[]): string[] {
  return folders.filter(folder => folder.blockedReason === null).map(folder => folder.connectionId)
}

/** `?server=` names the connection whose session the chat shows, and
 *  `?server_session=` that server's own session id. They are separate from
 *  `?session=`, which always names one of this server's sessions, so a link
 *  to a local session means the same thing it always did. */
export const CONNECTED_SERVER_PARAM = 'server'
export const CONNECTED_SERVER_SESSION_PARAM = 'server_session'

/** A session on a connected server: which connection, and that server's id
 *  for it, or null for a new session not started yet. */
export interface ConnectedServerSessionRef {
  connectionId: string
  sessionId: string | null
}

export function readConnectedServerSessionRef(params: URLSearchParams): ConnectedServerSessionRef | null {
  const connectionId = params.get(CONNECTED_SERVER_PARAM)?.trim() ?? ''
  if (!connectionId) return null
  const sessionId = params.get(CONNECTED_SERVER_SESSION_PARAM)?.trim() ?? ''
  return { connectionId, sessionId: sessionId || null }
}

/** Writes `ref` into `params`, or removes both params for null. Answers
 *  whether anything changed, so the caller writes the URL only then. */
export function writeConnectedServerSessionRef(params: URLSearchParams, ref: ConnectedServerSessionRef | null): boolean {
  const wantServer = ref?.connectionId ?? null
  const wantSession = ref?.sessionId ?? null
  let changed = false
  if ((params.get(CONNECTED_SERVER_PARAM) || null) !== wantServer) {
    if (wantServer) params.set(CONNECTED_SERVER_PARAM, wantServer)
    else params.delete(CONNECTED_SERVER_PARAM)
    changed = true
  }
  if ((params.get(CONNECTED_SERVER_SESSION_PARAM) || null) !== wantSession) {
    if (wantSession) params.set(CONNECTED_SERVER_SESSION_PARAM, wantSession)
    else params.delete(CONNECTED_SERVER_SESSION_PARAM)
    changed = true
  }
  return changed
}

export function sameConnectedServerSessionRef(
  a: ConnectedServerSessionRef | null,
  b: ConnectedServerSessionRef | null,
): boolean {
  if (a === null || b === null) return a === b
  return a.connectionId === b.connectionId && a.sessionId === b.sessionId
}

/** The key a remote session row is drawn under: the connection id and the
 *  session id together, so two servers' rows for the same id never share one. */
export function connectedServerSessionKey(connectionId: string, sessionId: string): string {
  return `${connectionId}/${sessionId}`
}

/** The connections whose servers this one can decide for: readable now, and
 *  holding the administrator level there, which their approve and decline
 *  routes ask for. Their waiting requests and notices are listed on this
 *  server's Signals page beside its own. */
export function connectionsThisServerAdministers(views: readonly PeerConnectionView[]): PeerConnectionView[] {
  return views.filter(
    view => connectedServerBlockedReason(view) === null && view.access_held?.level === PeerAccessLevelAdministrator,
  )
}

/** The connection to the server a `<id>@<server_id>` reference names, or null
 *  when no live connection reports that id. Matched on the server's own id,
 *  never its name or a connection id. A removed connection is no way to reach
 *  it. */
export function connectionOfServerId(servers: readonly ConnectedServerListing[], serverId: string): ConnectedServerListing | null {
  if (!serverId) return null
  return servers.find(server => server.serverId === serverId && server.blockedWord !== 'removed') ?? null
}
