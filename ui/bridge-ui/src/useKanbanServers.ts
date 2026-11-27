import { useEffect, useMemo, useState } from 'react'
import type { Board } from '@kayushkin/kanban-store-types'
import type { PeerConnectionView, ServerDescription } from '@kayushkin/llm-bridge-types'
import type { BridgeConfig } from './context'
import { PEER_CONNECTIONS_PATH, peerConnectionBridgeConfig, peerRouteErrorText } from './peers'
import { THIS_SERVER_PATH } from './serverListings'
import { kanbanServers, type KanbanServer, type ServerBoards } from './kanbanServers'

/** How often the connection list is read again: whether a server can be read
 *  changes without this page doing anything. */
const CONNECTIONS_POLL_MS = 30_000

export interface KanbanServersValue {
  servers: KanbanServer[]
  /** False until both this server's description and the connection list have
   *  answered once, so a `?server=` link is not called unknown too early. */
  loaded: boolean
  /** Why this server's description or the connection list could not be read. */
  error: string | null
}

/** This server and the servers it is connected to, for the Kanban page. Only
 *  an administrator may list connections; anyone else sees this server alone,
 *  which is not an error. */
export function useKanbanServers(config: BridgeConfig): KanbanServersValue {
  const { fetch: fetchFn, basePath } = config
  const [thisServer, setThisServer] = useState<ServerDescription | null>(null)
  const [thisServerRead, setThisServerRead] = useState(false)
  const [views, setViews] = useState<PeerConnectionView[]>([])
  const [viewsRead, setViewsRead] = useState(false)
  const [thisServerError, setThisServerError] = useState<string | null>(null)
  const [viewsError, setViewsError] = useState<string | null>(null)

  useEffect(() => {
    let canceled = false
    void (async () => {
      try {
        const res = await fetchFn(`${basePath}${THIS_SERVER_PATH}`)
        if (canceled) return
        if (!res.ok) { setThisServerError(await peerRouteErrorText(res, `GET ${THIS_SERVER_PATH}`)); return }
        setThisServer((await res.json()) as ServerDescription)
        setThisServerError(null)
      } catch (err) {
        if (!canceled) setThisServerError(`GET ${THIS_SERVER_PATH}: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        if (!canceled) setThisServerRead(true)
      }
    })()
    return () => { canceled = true }
  }, [fetchFn, basePath])

  useEffect(() => {
    let canceled = false
    const read = async () => {
      if (typeof document !== 'undefined' && document.hidden) return
      try {
        const res = await fetchFn(`${basePath}${PEER_CONNECTIONS_PATH}`)
        if (canceled) return
        if (res.status === 401 || res.status === 403) { setViews([]); setViewsError(null); return }
        if (!res.ok) { setViewsError(await peerRouteErrorText(res, `GET ${PEER_CONNECTIONS_PATH}`)); return }
        setViews((await res.json()) as PeerConnectionView[])
        setViewsError(null)
      } catch (err) {
        if (!canceled) setViewsError(`GET ${PEER_CONNECTIONS_PATH}: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        if (!canceled) setViewsRead(true)
      }
    }
    void read()
    const timer = setInterval(() => void read(), CONNECTIONS_POLL_MS)
    return () => { canceled = true; clearInterval(timer) }
  }, [fetchFn, basePath])

  const servers = useMemo(() => kanbanServers(thisServer, views), [thisServer, views])
  return {
    servers,
    loaded: thisServerRead && viewsRead,
    error: [thisServerError, viewsError].filter(Boolean).join(' · ') || null,
  }
}

/** The key a server's boards are kept under: its connection id, or this server's. */
export function serverBoardsKey(server: KanbanServer): string {
  return server.connectionId ?? 'this-server'
}

/** Every listed server's boards but the shown one's, each read once through
 *  that server's config: this server's kanban-store, or a connected server's
 *  `/issues/` gateway through the connection's proxy. The shown server's
 *  boards are the board page's own read (`useKanban`), which keeps them current
 *  as boards are made and deleted, so reading them here too would only repeat
 *  that request. A server that cannot be read is not asked.
 *
 *  Read again when the connections, their reasons, or the shown server change —
 *  not when a server's id arrives: this server's id is empty until `GET /server`
 *  answers, and keying on it read every server's boards a second time. */
export function useBoardsOnServers(servers: readonly KanbanServer[], config: BridgeConfig, shownServerKey: string | null): ReadonlyMap<string, ServerBoards> {
  const [boards, setBoards] = useState<ReadonlyMap<string, ServerBoards>>(new Map())
  const serversKey = servers
    .filter(server => serverBoardsKey(server) !== shownServerKey)
    .map(server => `${serverBoardsKey(server)}:${server.blockedReason ?? ''}`)
    .join('\n')

  useEffect(() => {
    let canceled = false
    const initial = new Map<string, ServerBoards>()
    const asked = servers.filter(server => serverBoardsKey(server) !== shownServerKey)
    for (const server of asked) {
      initial.set(serverBoardsKey(server), server.blockedReason ? { state: 'blocked', reason: server.blockedReason } : { state: 'loading' })
    }
    setBoards(initial)
    for (const server of asked) {
      if (server.blockedReason) continue
      const serverConfig = server.connectionId ? peerConnectionBridgeConfig(config, server.connectionId) : config
      const key = serverBoardsKey(server)
      void (async () => {
        let answer: ServerBoards
        try {
          const res = await serverConfig.fetch(`${serverConfig.issuesBasePath}/api/boards`)
          answer = res.ok
            ? { state: 'loaded', boards: (await res.json()) as Board[] }
            : { state: 'failed', error: await peerRouteErrorText(res, 'GET /api/boards') }
        } catch (err) {
          answer = { state: 'failed', error: `GET /api/boards: ${err instanceof Error ? err.message : String(err)}` }
        }
        if (!canceled) setBoards(prev => new Map(prev).set(key, answer))
      })()
    }
    return () => { canceled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [serversKey, shownServerKey, config])

  return boards
}
