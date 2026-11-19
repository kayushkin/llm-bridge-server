// The Kanban page's servers: this one, and each connected server whose boards
// can be read through this server's proxy for the connection. Boards stay on
// the server that holds them; the page reads each live and copies nothing.
//
// A server is named in the URL by its own id (`?server=server_…`), never by a
// connection id or a name: the id is the one the server made for itself and
// never changes, while each side numbers its connections differently and a
// name can be edited. No `?server=` means this server.
//
// The pure rules the page draws from; `components/BridgeKanban.tsx` fetches.

import type { Board } from '@kayushkin/kanban-store-types'
import type { PeerConnectionView, ServerDescription } from '@kayushkin/llm-bridge-types'
import { KANBAN_LAST_BOARD_STORAGE_KEY } from './constants'
import {
  CONNECTED_SERVER_PARAM,
  CONNECTED_SERVER_SESSION_PARAM,
  connectedServerListings,
  thisServerLabel,
} from './serverListings'

/** The Kanban page's query parameter naming the server whose boards it shows. */
export const KANBAN_SERVER_PARAM = 'server'

export interface KanbanServer {
  /** The server's own id. Empty only for this server while `GET /server` has
   *  not answered. */
  serverId: string
  /** The connection this server reaches it by; null for this server. */
  connectionId: string | null
  label: string
  /** Why its boards cannot be read now, or null when they can. */
  blockedReason: string | null
}

/** This server first, then every live connected server once, in sidebar
 *  order. A server reached by two connections is listed by the first that
 *  can be read. A server that has not reported its id cannot be named in a
 *  URL, so it is listed blocked, saying why. */
export function kanbanServers(
  thisServer: Pick<ServerDescription, 'id' | 'name'> | null,
  views: readonly PeerConnectionView[],
): KanbanServer[] {
  const servers: KanbanServer[] = [{
    serverId: thisServer?.id ?? '',
    connectionId: null,
    label: thisServerLabel(thisServer?.name),
    blockedReason: null,
  }]
  const viewOf = new Map(views.map(view => [view.connection.id, view]))
  const listed = new Set<string>()
  for (const listing of connectedServerListings(views)) {
    const view = viewOf.get(listing.connectionId)
    if (!view || view.connection.removed_at) continue
    const serverId = view.other_server_id?.trim() ?? ''
    if (serverId && (listed.has(serverId) || serverId === thisServer?.id)) continue
    if (serverId) listed.add(serverId)
    servers.push({
      serverId,
      connectionId: listing.connectionId,
      label: listing.label,
      blockedReason: serverId
        ? listing.blockedReason
        : 'it has not reported its server id, so its boards cannot be addressed; a build older than server ids never will',
    })
  }
  return servers
}

/** The server `?server=` names: this server when the parameter is empty or
 *  names this server, the connected server with that id otherwise, and null
 *  when no listed server has it. */
export function kanbanServerNamed(servers: readonly KanbanServer[], serverParam: string | null): KanbanServer | null {
  const wanted = serverParam?.trim() ?? ''
  const thisServer = servers[0]
  if (!wanted || wanted === thisServer.serverId) return thisServer
  return servers.find(server => server.connectionId !== null && server.serverId === wanted) ?? null
}

/** What `?server=` holds for a server: nothing for this one, its id for any other. */
export function kanbanServerParamValue(server: KanbanServer): string | null {
  return server.connectionId === null ? null : server.serverId
}

/** Where the board last opened on a server is kept. This server keeps the key
 *  it always had, which the board settings page also reads. */
export function kanbanLastBoardStorageKey(server: KanbanServer): string {
  return server.connectionId === null ? KANBAN_LAST_BOARD_STORAGE_KEY : `${KANBAN_LAST_BOARD_STORAGE_KEY}:${server.serverId}`
}

/** The chat page's address for a session a card links to. A card's session
 *  link holds the id of a session on the card's own server, so on a connected
 *  server it opens that server's session, never this server's session with
 *  the same id. */
export function kanbanSessionHref(chatRoute: string, server: KanbanServer, sessionId: string): string {
  if (server.connectionId === null) return `${chatRoute}?session=${encodeURIComponent(sessionId)}`
  const params = new URLSearchParams()
  params.set(CONNECTED_SERVER_PARAM, server.connectionId)
  params.set(CONNECTED_SERVER_SESSION_PARAM, sessionId)
  return `${chatRoute}?${params.toString()}`
}

/** One server's boards as the board picker shows them. */
export type ServerBoards =
  | { state: 'loading' }
  | { state: 'loaded'; boards: Board[] }
  | { state: 'failed'; error: string }
  | { state: 'blocked'; reason: string }

/** The picker's heading for a server's group of boards. */
export function serverBoardsHeading(server: KanbanServer, boards: ServerBoards): string {
  switch (boards.state) {
    case 'loaded': return server.label
    case 'loading': return `${server.label} — reading boards…`
    case 'failed': return `${server.label} — boards could not be read`
    case 'blocked': return `${server.label} — ${boards.reason}`
  }
}
