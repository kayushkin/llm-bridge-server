import { useEffect, useMemo, useState, type FormEvent } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  ChatProvider,
  useActiveSession,
  usePendingSession,
  useSessionActions,
  useSessionList,
} from '@kayushkin/chat-core'
import type { PeerConnectionView } from '@kayushkin/llm-bridge-types'
import { BridgeContext, useBridgeConfig } from '../context'
import { useBridgeInstances } from '../useBridgeInstances'
import {
  peerConnectionBridgeConfig,
  peerConnectionPath,
  peerRouteErrorText,
  peerTransportDescription,
} from '../peers'
import { connectedServerBlockedReason } from '../serverListings'
import { chatFetchOver } from '../chatFetch'
import { ConnectedServerThread } from './chat/ConnectedServerThread'

// One connected server's sessions, read and driven through this server's
// proxy for the connection (`{basePath}/peer-connections/{id}/proxy`), which
// goes direct or over the dial-out socket as the connection says. Everything
// below the providers runs against that server: a second chat-core store
// whose `basePath` is the proxy, and a `BridgeConfig` whose every other path
// is emptied (`peerConnectionBridgeConfig`), because this host's stores know
// nothing of the other server's ids. The second store keeps nothing in the
// browser — no drafts, filters or IndexedDB cache — so it cannot mix with the
// chat's own. The other server decides what is listed and allowed, by the
// level of access it gave this one.

export function BridgePeerConnection() {
  const config = useBridgeConfig()
  const { fetch: fetchFn, basePath, routes } = config
  const { connectionId = '' } = useParams<{ connectionId: string }>()
  const [view, setView] = useState<PeerConnectionView | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let canceled = false
    void (async () => {
      const path = peerConnectionPath(connectionId)
      try {
        const res = await fetchFn(`${basePath}${path}`)
        if (!res.ok) { if (!canceled) setError(await peerRouteErrorText(res, `GET ${path}`)); return }
        const found = (await res.json()) as PeerConnectionView
        if (!canceled) setView(found)
      } catch (err) {
        if (!canceled) setError(`GET ${path}: ${err instanceof Error ? err.message : String(err)}`)
      }
    })()
    return () => { canceled = true }
  }, [fetchFn, basePath, connectionId])

  const peerConfig = useMemo(() => peerConnectionBridgeConfig(config, connectionId), [config, connectionId])
  const chatFetch = useMemo(() => chatFetchOver(fetchFn), [fetchFn])
  const blocked = view ? connectedServerBlockedReason(view) : null

  return (
    <div className="bp-container pr-link-page">
      <header className="bg-head">
        <h2 className="bg-title">
          <Link to={routes.peers}>Peers</Link> / {view ? view.connection.display_name : connectionId}
        </h2>
        {view && (
          <p className="bp-hint">
            {peerTransportDescription(view.connection)}
            {view.connection.remote_base_url ? <> · {view.connection.remote_base_url}</> : null}
            {view.access_held ? <> · acting there as <code>{view.access_held.remote_peer_principal_id}</code></> : null}
          </p>
        )}
      </header>
      {error && <div className="bridge-error bp-error">{error}</div>}
      {blocked && <div className="bridge-error bp-error">{blocked}</div>}
      {view && !blocked && (
        <BridgeContext.Provider value={peerConfig}>
          <ChatProvider
            key={view.connection.id}
            fetch={chatFetch}
            basePath={peerConfig.basePath}
            referenceStoreBasePath={peerConfig.referenceStoreBasePath}
            cache={false}
            keepDraftsAndFiltersInBrowser={false}
          >
            <ConnectedServerSessions
              connectionId={view.connection.id}
              serverName={view.other_server_name || view.connection.display_name}
            />
          </ChatProvider>
        </BridgeContext.Provider>
      )}
    </div>
  )
}

function ConnectedServerSessions({ connectionId, serverName }: { connectionId: string; serverName: string }) {
  const { groups, loading, moreSessions, loadOlderSessions } = useSessionList()
  const { id: activeID, select } = useActiveSession()
  const pending = usePendingSession()
  const sessions = useMemo(() => groups.flatMap(g => g.sessions), [groups])
  return (
    <div className="pr-link-columns">
      <aside className="pr-link-list" aria-label="Sessions on the connected server">
        <NewSessionControl />
        {loading && sessions.length === 0 && <p className="bp-empty-inline">Loading…</p>}
        {!loading && sessions.length === 0 && <p className="bp-empty-inline">The connected server shows no session to this server.</p>}
        <ul className="bp-list">
          {sessions.map(s => (
            <li key={s.sessionId}>
              <button
                type="button"
                className={`bp-row pr-session-row${s.sessionId === activeID ? ' bp-row-selected' : ''}`}
                onClick={() => select(s.sessionId)}
                data-session-id={s.sessionId}
              >
                <span className="bp-row-text">
                  <span className="bp-row-name">{s.displayName || s.sessionId}</span>
                  <span className="bp-row-email">{s.state} · {s.harness} · {new Date(s.updatedAt).toLocaleString()}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
        {moreSessions && <button type="button" className="bp-cancel" onClick={loadOlderSessions}>Older sessions</button>}
      </aside>
      <section className="pr-link-thread">
        {activeID || pending ? (
          <ConnectedServerThread connectionId={connectionId} serverName={serverName} openSession={select} />
        ) : (
          <p className="bp-empty">Pick a session, or start one on an instance of the connected server.</p>
        )}
      </section>
    </div>
  )
}

/** Opens a new session on one of the connected server's instances. Also used
 *  by the chat's connected-server pane (`ConnectedServerPane`). The session is
 *  created on the first send, as the chat does it. */
export function NewSessionControl() {
  const { instances, error } = useBridgeInstances()
  const { newSession } = useSessionActions()
  const enabled = instances.filter(i => i.enabled)
  const [instanceID, setInstanceID] = useState('')
  const start = (event: FormEvent) => {
    event.preventDefault()
    const instance = enabled.find(i => i.id === instanceID)
    if (instance) newSession({ instanceId: instance.id, harness: instance.harness_type })
  }
  return (
    <form className="pr-new-session" onSubmit={start}>
      <select value={instanceID} onChange={e => setInstanceID(e.target.value)} aria-label="Instance on the connected server">
        <option value="">{enabled.length === 0 ? 'No instance to start on' : 'Start on…'}</option>
        {enabled.map(i => <option key={i.id} value={i.id}>{i.name} ({i.harness_type})</option>)}
      </select>
      <button type="submit" className="bp-submit" disabled={!instanceID}>New</button>
      {error && <div className="bridge-error bp-error">Could not read the connected server&apos;s instances: {error}</div>}
    </form>
  )
}
