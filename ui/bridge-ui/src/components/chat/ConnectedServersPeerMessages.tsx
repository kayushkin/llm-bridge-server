import { useCallback, useEffect, useMemo, useState, type JSX } from 'react'
import type { PeerConnectionView } from '@kayushkin/llm-bridge-types'
import { BridgeContext, useBridgeConfig } from '../../context'
import { PEER_CONNECTIONS_PATH, peerConnectionBridgeConfig, peerRouteErrorText } from '../../peers'
import { connectedServerLabel, connectionsThisServerAdministers } from '../../serverListings'
import { PeerMessagesNeedingYou, useWaitingPeerMessages } from '../PeerMessageCards'
import styles from './Chat.module.css'

// The sections for the servers this one administers, on the Signals page and
// in the sidebar's Needs you dropdown: each
// server's own waiting requests and notices, read and decided through this
// server's proxy for the connection. Each section runs the same cards as this
// server's own, under that server's config (`peerConnectionBridgeConfig`), so
// a card reads, approves and declines on the other server, and its Start it on
// list is that server's instances.

/** How often the connection list is read again, as the chat's list does. */
const CONNECTIONS_POLL_MS = 30_000

/** How many messages wait on each connected server, reported by
 *  `ConnectedServersPeerMessages`, and their total for a Needs you count. */
export function useConnectedServerWaitingCounts(): {
  total: number
  report: (connectionID: string, waiting: number) => void
} {
  const [byConnection, setByConnection] = useState<Record<string, number>>({})
  const report = useCallback((connectionID: string, waiting: number) => {
    setByConnection(previous =>
      (previous[connectionID] ?? 0) === waiting ? previous : { ...previous, [connectionID]: waiting },
    )
  }, [])
  const total = Object.values(byConnection).reduce((sum, waiting) => sum + waiting, 0)
  return { total, report }
}

/** One section per connected server this one administers. Each reports how
 *  many messages wait there, by connection id, for a Needs you count. With
 *  `drawn` false it draws nothing and only reports: the sidebar row's count.
 *  `countInHeading` adds the section's count to its heading, where there is
 *  room for it: the Signals page, not the sidebar's dropdown. */
export function ConnectedServersPeerMessages({ drawn, countInHeading, onWaitingCount, onSelectSession }: {
  drawn: boolean
  countInHeading: boolean
  onWaitingCount: (connectionID: string, waiting: number) => void
  onSelectSession: (connectionID: string, sessionID: string) => void
}): JSX.Element | null {
  const config = useBridgeConfig()
  const { fetch: fetchFn, basePath } = config
  const [views, setViews] = useState<PeerConnectionView[]>([])
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let canceled = false
    const read = async () => {
      try {
        const res = await fetchFn(`${basePath}${PEER_CONNECTIONS_PATH}`)
        if (canceled) return
        // A server with no peer routes has no connected servers to list.
        if (res.status === 404) {
          setViews([])
          return
        }
        if (!res.ok) {
          setError(await peerRouteErrorText(res, 'read the connected servers'))
          return
        }
        setError(null)
        setViews(connectionsThisServerAdministers((await res.json()) as PeerConnectionView[]))
      } catch (err) {
        if (!canceled) setError(err instanceof Error ? err.message : String(err))
      }
    }
    void read()
    const timer = setInterval(read, CONNECTIONS_POLL_MS)
    return () => {
      canceled = true
      clearInterval(timer)
    }
  }, [fetchFn, basePath])

  if (views.length === 0 && error === null) return null
  return (
    <>
      {drawn && error !== null && <p className={styles.inboxError}>Couldn’t read the connected servers: {error}</p>}
      {views.map(view => (
        <ConnectedServerPeerMessages
          key={view.connection.id}
          view={view}
          drawn={drawn}
          countInHeading={countInHeading}
          onWaitingCount={onWaitingCount}
          onSelectSession={onSelectSession}
        />
      ))}
    </>
  )
}

function ConnectedServerPeerMessages({ view, drawn, countInHeading, onWaitingCount, onSelectSession }: {
  view: PeerConnectionView
  drawn: boolean
  countInHeading: boolean
  onWaitingCount: (connectionID: string, waiting: number) => void
  onSelectSession: (connectionID: string, sessionID: string) => void
}): JSX.Element {
  const config = useBridgeConfig()
  const connectionID = view.connection.id
  const serverConfig = useMemo(() => peerConnectionBridgeConfig(config, connectionID), [config, connectionID])
  return (
    <BridgeContext.Provider value={serverConfig}>
      <WaitingOnConnectedServer
        connectionID={connectionID}
        label={connectedServerLabel(view).label}
        drawn={drawn}
        countInHeading={countInHeading}
        onWaitingCount={onWaitingCount}
        onSelectSession={onSelectSession}
      />
    </BridgeContext.Provider>
  )
}

/** Runs under the connected server's config, so every read and decision goes
 *  to that server. */
function WaitingOnConnectedServer({ connectionID, label, drawn, countInHeading, onWaitingCount, onSelectSession }: {
  connectionID: string
  label: string
  drawn: boolean
  countInHeading: boolean
  onWaitingCount: (connectionID: string, waiting: number) => void
  onSelectSession: (connectionID: string, sessionID: string) => void
}): JSX.Element | null {
  const { waiting, error, reload } = useWaitingPeerMessages()
  useEffect(() => {
    onWaitingCount(connectionID, waiting.length)
  }, [onWaitingCount, connectionID, waiting.length])
  useEffect(() => () => onWaitingCount(connectionID, 0), [onWaitingCount, connectionID])

  if (!drawn || (waiting.length === 0 && error === null)) return null
  return (
    <section className={styles.signalsPageSection}>
      <h3 className={styles.signalsPageSectionTitle}>Waiting on {label}{countInHeading && ` · ${waiting.length}`}</h3>
      <PeerMessagesNeedingYou
        waiting={waiting}
        error={error}
        reload={reload}
        onSelectSession={sessionID => onSelectSession(connectionID, sessionID)}
      />
    </section>
  )
}
