import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { ChatContext, ChatProvider, useChatContext, type ChatContextValue } from '@kayushkin/chat-core'
import type { PeerConnectionView, ServerDescription } from '@kayushkin/llm-bridge-types'
import { BridgeContext, useBridgeConfig, type BridgeConfig } from '../../context'
import { chatFetchOver } from '../../chatFetch'
import { PEER_CONNECTIONS_PATH, peerConnectionBridgeConfig, peerRouteErrorText } from '../../peers'
import {
  THIS_SERVER_PATH,
  connectedServerListings,
  readableConnectionIds,
  type ConnectedServerListing,
} from '../../serverListings'

// The chat's view of the servers this one is connected to.
//
// One chat-core store per connected server that can be read now, each aimed
// at this server's proxy for the connection, keeping nothing in the browser.
// The stores are mounted here, once, and handed out by connection id, so the
// sidebar's folder and the thread pane read the SAME store: one list stream
// per server, and a session opened from the sidebar is the session the pane
// shows. A server that cannot be read has no store at all, so its folder can
// never show sessions left over from before it went away.

/** How often the connection list is read again: whether a socket is open
 *  changes without this page doing anything. */
const CONNECTIONS_POLL_MS = 30_000

export interface ConnectedServersValue {
  /** This server's own name, or null while unset or not yet read. */
  thisServerName: string | null
  /** This server's own `server_…` id, or null until read. A reference that
   *  names it is this server's own. */
  thisServerId: string | null
  /** Why this server's name could not be read, or null. */
  thisServerNameError: string | null
  servers: ConnectedServerListing[]
  /** False until the connection list has answered once. */
  connectionsLoaded: boolean
  /** Why the connection list could not be read, or null. */
  connectionsError: string | null
  /** The live store for a readable connection, once it has mounted. */
  storeOf: (connectionId: string) => ChatContextValue | undefined
  /** The config a connected server's components run with. */
  configOf: (connectionId: string) => BridgeConfig
}

/** Exported for the render check, which supplies a value without mounting stores. */
export const ConnectedServersContext = createContext<ConnectedServersValue | null>(null)

export function useConnectedServers(): ConnectedServersValue {
  const value = useContext(ConnectedServersContext)
  if (!value) throw new Error('useConnectedServers must be used inside <ConnectedServersProvider>')
  return value
}

/** The connected servers where the chat page provides them, or null: a
 *  reference chip renders on pages that have no connected servers at all. */
export function useOptionalConnectedServers(): ConnectedServersValue | null {
  return useContext(ConnectedServersContext)
}

/** Runs `children` against one connected server: its chat-core store and its
 *  config, in place of this server's. Renders nothing while the store has not
 *  mounted. */
export function OnConnectedServer({ connectionId, children }: { connectionId: string; children: ReactNode }) {
  const { storeOf, configOf } = useConnectedServers()
  const store = storeOf(connectionId)
  const config = configOf(connectionId)
  if (!store) return null
  return (
    <BridgeContext.Provider value={config}>
      <ChatContext.Provider value={store}>{children}</ChatContext.Provider>
    </BridgeContext.Provider>
  )
}

export function ConnectedServersProvider({ children }: { children: ReactNode }) {
  const config = useBridgeConfig()
  const { fetch: fetchFn, basePath } = config
  const [views, setViews] = useState<PeerConnectionView[]>([])
  const [connectionsError, setConnectionsError] = useState<string | null>(null)
  const [connectionsLoaded, setConnectionsLoaded] = useState(false)
  const [thisServerName, setThisServerName] = useState<string | null>(null)
  const [thisServerId, setThisServerId] = useState<string | null>(null)
  const [thisServerNameError, setThisServerNameError] = useState<string | null>(null)
  const [stores, setStores] = useState<ReadonlyMap<string, ChatContextValue>>(new Map())

  useEffect(() => {
    let canceled = false
    void (async () => {
      try {
        const res = await fetchFn(`${basePath}${THIS_SERVER_PATH}`)
        if (canceled) return
        // A server that does not describe itself yet answers 404; its folder is
        // then "This server", which is the truth, not an error to show.
        if (res.status === 404) return
        if (!res.ok) { setThisServerNameError(await peerRouteErrorText(res, `GET ${THIS_SERVER_PATH}`)); return }
        const body = (await res.json()) as ServerDescription
        setThisServerName(body.name?.trim() || null)
        setThisServerId(body.id || null)
      } catch (err) {
        if (!canceled) setThisServerNameError(`GET ${THIS_SERVER_PATH}: ${err instanceof Error ? err.message : String(err)}`)
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
        // Only an administrator may list connections. Anyone else has no
        // connected servers to see, which is not an error.
        if (res.status === 401 || res.status === 403) { setViews([]); setConnectionsError(null); return }
        if (!res.ok) { setConnectionsError(await peerRouteErrorText(res, `GET ${PEER_CONNECTIONS_PATH}`)); return }
        setViews((await res.json()) as PeerConnectionView[])
        setConnectionsError(null)
      } catch (err) {
        if (!canceled) setConnectionsError(`GET ${PEER_CONNECTIONS_PATH}: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        if (!canceled) setConnectionsLoaded(true)
      }
    }
    void read()
    const timer = setInterval(() => void read(), CONNECTIONS_POLL_MS)
    return () => { canceled = true; clearInterval(timer) }
  }, [fetchFn, basePath])

  const servers = useMemo(() => connectedServerListings(views), [views])
  const readable = useMemo(() => readableConnectionIds(servers), [servers])
  const readableKey = readable.join('\n')

  // A store that is no longer mounted is dropped, so `storeOf` never hands out
  // one whose server went away.
  useEffect(() => {
    setStores(prev => {
      const keep = new Set(readable)
      if ([...prev.keys()].every(id => keep.has(id))) return prev
      return new Map([...prev].filter(([id]) => keep.has(id)))
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readableKey])

  const register = useCallback((connectionId: string, store: ChatContextValue | null) => {
    setStores(prev => {
      if (store === null) {
        if (!prev.has(connectionId)) return prev
        const next = new Map(prev)
        next.delete(connectionId)
        return next
      }
      if (prev.get(connectionId) === store) return prev
      return new Map(prev).set(connectionId, store)
    })
  }, [])

  const configs = useMemo(() => {
    const map = new Map<string, BridgeConfig>()
    for (const server of servers) map.set(server.connectionId, peerConnectionBridgeConfig(config, server.connectionId))
    return map
  }, [config, servers])

  const value = useMemo<ConnectedServersValue>(() => ({
    thisServerName,
    thisServerId,
    thisServerNameError,
    servers,
    connectionsLoaded,
    connectionsError,
    storeOf: (connectionId: string) => stores.get(connectionId),
    configOf: (connectionId: string) => configs.get(connectionId) ?? peerConnectionBridgeConfig(config, connectionId),
  }), [thisServerName, thisServerId, thisServerNameError, servers, connectionsLoaded, connectionsError, stores, configs, config])

  const chatFetch = useMemo(() => chatFetchOver(fetchFn), [fetchFn])

  return (
    <ConnectedServersContext.Provider value={value}>
      {readable.map(connectionId => {
        const peerConfig = configs.get(connectionId)
        if (!peerConfig) return null
        return (
          <BridgeContext.Provider key={connectionId} value={peerConfig}>
            <ChatProvider
              fetch={chatFetch}
              basePath={peerConfig.basePath}
              referenceStoreBasePath={peerConfig.referenceStoreBasePath}
              cache={false}
              keepDraftsAndFiltersInBrowser={false}
            >
              <RegisterStore connectionId={connectionId} register={register} />
            </ChatProvider>
          </BridgeContext.Provider>
        )
      })}
      {children}
    </ConnectedServersContext.Provider>
  )
}

/** Hands the store its provider made to the page, and takes it back when the
 *  provider unmounts. */
function RegisterStore({ connectionId, register }: {
  connectionId: string
  register: (connectionId: string, store: ChatContextValue | null) => void
}) {
  const store = useChatContext()
  useEffect(() => {
    register(connectionId, store)
    return () => register(connectionId, null)
  }, [connectionId, store, register])
  return null
}
