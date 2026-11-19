import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import {
  PeerAccessLevelGroup,
  type PeerAccessLevel,
  type PeerConnectionInvite,
  type PeerConnectionInviteCreated,
  type PeerConnectionView,
  type PeerGrantRevoked,
  type PeerGrantSelf,
  type PeerUserMapping,
  type ServiceSettings,
} from '@kayushkin/llm-bridge-types'
import { useBridgeConfig } from '../context'
import { usePrincipals } from '../usePrincipals'
import { fetchServiceSettings } from '../serviceSettings'
import { connectedServerLabel } from '../serverListings'
import { PeerConnectionMessages } from './PeerMessageCards'
import {
  EMPTY_PEER_CONNECT_FORM,
  PEER_ACCESS_LEVELS_PATH,
  PEER_CONNECTIONS_PATH,
  PEER_CONNECTION_INVITES_PATH,
  PEER_CONNECTION_INVITE_LIFETIMES,
  PEER_USER_MAPPINGS_PATH,
  liveFirstNewestFirst,
  peerAccessLevelIsReady,
  peerAccessLevelName,
  peerAccessLevelProvisionPath,
  peerConnectRequestBody,
  peerConnectionCheckPath,
  peerConnectionGrantPath,
  peerConnectionInviteRequestBody,
  peerConnectionInviteState,
  peerConnectionIsRemoved,
  peerConnectionReach,
  peerConnectionReachText,
  peerConnectionRemovePath,
  peerConnectionRemovedBy,
  peerConnectionRevokeGrantPath,
  peerGrantRequestBody,
  peerReasonRequestBody,
  peerRouteErrorText,
  peerTransportDescription,
  peerUserMappingIsRemoved,
  peerUserMappingLocalChoices,
  peerUserMappingRemovePath,
  peerUserMappingRequestBody,
  peerUserMappingsListPath,
  thisServerPublicURL,
  type PeerConnectForm,
} from '../peers'

// Other llm-bridge-servers this one is connected to, and the access each way.
// A connection only says how the two servers reach each other; each server
// then grants the other a level on itself. Every route is an operator route,
// so a person who is not an administrator sees the server's 403 in each
// section. Rules in `src/peers.ts`.

function formatWhen(iso: string | undefined): string {
  return iso ? new Date(iso).toLocaleString() : ''
}

const jsonPost = (body: unknown): RequestInit => ({
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})

/** Reads one peer list, and re-reads it when asked. */
function usePeerList<T>(path: string): { rows: T[] | null; error: string | null; reload: () => void } {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [rows, setRows] = useState<T[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const reload = useCallback(() => {
    void (async () => {
      try {
        const res = await fetchFn(`${basePath}${path}`)
        if (!res.ok) { setError(await peerRouteErrorText(res, `GET ${path}`)); return }
        setRows(((await res.json()) as T[] | null) ?? [])
        setError(null)
      } catch (err) {
        setError(`GET ${path}: ${err instanceof Error ? err.message : String(err)}`)
      }
    })()
  }, [fetchFn, basePath, path])
  useEffect(() => { reload() }, [reload])
  return { rows, error, reload }
}

/** This server's own description of its settings, read once, for its
 *  `peer_connections.public_url`. */
function useThisServerSettings(): { settings: ServiceSettings | null; error: string | null } {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [settings, setSettings] = useState<ServiceSettings | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let canceled = false
    void fetchServiceSettings(fetchFn, basePath).then(answer => {
      if (canceled) return
      if (answer.phase === 'described') setSettings(answer.described)
      else if (answer.phase === 'failed') setError(answer.message)
      else setError(`GET ${basePath}/settings answered HTTP ${answer.status}`)
    })
    return () => { canceled = true }
  }, [fetchFn, basePath])
  return { settings, error }
}

type ConnectPath = 'give-passphrase' | 'enter-passphrase'

export function BridgePeers() {
  const connections = usePeerList<PeerConnectionView>(PEER_CONNECTIONS_PATH)
  const invites = usePeerList<PeerConnectionInvite>(PEER_CONNECTION_INVITES_PATH)
  const levels = usePeerList<PeerAccessLevel>(PEER_ACCESS_LEVELS_PATH)
  const [connectPath, setConnectPath] = useState<ConnectPath>('give-passphrase')
  const reloadConnections = connections.reload
  const reloadInvites = invites.reload
  return (
    <div className="bp-container bg-container pr-container">
      <header className="bg-head">
        <h2 className="bg-title">Peers</h2>
        <p className="bp-hint">
          Other llm-bridge-servers. A connection only says how two servers reach each other, and gives no access. Each
          server then gives the other a level of access on itself, and takes it back, on its own: what this server lets
          the other do here is set on this page, and what the other lets this one do is set on that server&apos;s page.
        </p>
      </header>

      <section className="pr-section" aria-label="Connected servers">
        <h3 className="pr-section-title">Connected servers</h3>
        {connections.error && <div className="bridge-error bp-error">{connections.error}</div>}
        {levels.error && <div className="bridge-error bp-error">{levels.error}</div>}
        <ConnectionsTable
          connections={connections.rows}
          levels={levels.rows}
          onChanged={reloadConnections}
          onLevelsChanged={levels.reload}
        />
      </section>

      <section className="pr-section" aria-label="Make a connection">
        <h3 className="pr-section-title">Make a connection</h3>
        <p className="bp-hint">
          At least one of the two servers needs an https address. The one with an address makes a passphrase; the
          other enters that address and the passphrase. A server with no address of its own keeps a socket open to the
          other, so it can be reached wherever it is.
        </p>
        <div className="pr-connect-paths" role="radiogroup" aria-label="Which side this server is">
          <label className="pr-connect-path">
            <input
              type="radio"
              name="pr-connect-path"
              checked={connectPath === 'give-passphrase'}
              onChange={() => setConnectPath('give-passphrase')}
            />
            <span>This server has an address: make a passphrase for the other server</span>
          </label>
          <label className="pr-connect-path">
            <input
              type="radio"
              name="pr-connect-path"
              checked={connectPath === 'enter-passphrase'}
              onChange={() => setConnectPath('enter-passphrase')}
            />
            <span>Connect to a server that has an address</span>
          </label>
        </div>
        {connectPath === 'give-passphrase' && (
          <>
            <PassphraseForm onMade={reloadInvites} />
            {invites.error && <div className="bridge-error bp-error">{invites.error}</div>}
            <InvitesTable invites={invites.rows} />
          </>
        )}
        {connectPath === 'enter-passphrase' && <ConnectForm onConnected={reloadConnections} />}
      </section>

      <section className="pr-section" aria-label="People from connected servers">
        <h3 className="pr-section-title">People from connected servers</h3>
        <p className="bp-hint">
          A request from a connected server names which of its people is asking. Map that person to one of this
          server&apos;s principals, and the request may do only what both the server&apos;s access and that principal
          allow. A person nobody mapped is refused. An administrator on the connected server names nobody and acts
          as the whole server, with exactly the access this server gave it.
        </p>
        <PeopleFromConnectedServers connections={connections.rows} />
      </section>

      <section className="pr-section" aria-label="Messages with connected servers">
        <h3 className="pr-section-title">Messages with connected servers</h3>
        <p className="bp-hint">
          A connected server may ask this one to start a session, or say that something is ready to update to, and
          this server may ask it the same. Nothing runs and nothing updates until a person on the receiving server
          approves it, and an approved request reaches its session marked as coming from the other server. What the
          other server wrote is shown as plain text.
        </p>
        <MessagesWithConnectedServers connections={connections.rows} />
      </section>
    </div>
  )
}

function ConnectionsTable({ connections, levels, onChanged, onLevelsChanged }: {
  connections: PeerConnectionView[] | null
  levels: PeerAccessLevel[] | null
  onChanged: () => void
  onLevelsChanged: () => void
}) {
  const { routes } = useBridgeConfig()
  const sorted = useMemo(
    () => [...(connections ?? [])].sort((a, b) => {
      const liveOrder = Number(peerConnectionIsRemoved(a)) - Number(peerConnectionIsRemoved(b))
      if (liveOrder !== 0) return liveOrder
      return new Date(b.connection.created_at).getTime() - new Date(a.connection.created_at).getTime()
    }),
    [connections],
  )
  return (
    <table className="bg-table pr-table">
      <thead>
        <tr><th>Server</th><th>How they reach each other</th><th>Access this server gives it</th><th>Access it gives this server</th><th></th></tr>
      </thead>
      <tbody>
        {connections === null && <tr><td colSpan={5} className="bp-empty-inline">Loading…</td></tr>}
        {connections !== null && connections.length === 0 && <tr><td colSpan={5} className="bp-empty-inline">This server is connected to no other.</td></tr>}
        {sorted.map(view => {
          const { connection } = view
          const removed = peerConnectionIsRemoved(view)
          const reach = peerConnectionReach(view)
          return (
            <tr key={connection.id} className={removed ? 'bg-row-revoked' : ''} data-peer-connection-id={connection.id}>
              <td>
                {removed
                  ? connection.display_name
                  : <Link to={`${routes.peerConnection}/${encodeURIComponent(connection.id)}`}>{connection.display_name}</Link>}
                <div className="bp-hint"><code>{connection.id}</code> · connected {formatWhen(connection.created_at)}</div>
                {removed && (
                  <span className="bp-badge bp-badge-disabled" title={connection.remove_reason}>
                    {peerConnectionRemovedBy(connection)} {formatWhen(connection.removed_at)}{connection.remove_reason ? `: ${connection.remove_reason}` : ''}
                  </span>
                )}
                {removed && connection.removal_notice && <div className="bp-hint pr-removal-notice">{connection.removal_notice}</div>}
              </td>
              <td>
                {peerTransportDescription(connection)}
                {connection.remote_base_url && <div className="bp-hint">{connection.remote_base_url}</div>}
                {!removed && <span className={`bp-badge pr-reach pr-reach-${reach.replace(' ', '-')}`} title={view.last_probe_failure || undefined}>{peerConnectionReachText(view)}</span>}
              </td>
              <td>
                {!removed && (
                  <AccessGivenCell
                    view={view}
                    levels={levels}
                    onChanged={onChanged}
                    onLevelsChanged={onLevelsChanged}
                  />
                )}
              </td>
              <td>
                {!removed && <AccessHeldCell view={view} levels={levels} />}
              </td>
              <td className="bg-actions">
                {!removed && <ReasonAction label="Remove" path={peerConnectionRemovePath(connection.id)} onDone={onChanged} />}
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

/** What this server lets the other do here: the live grant with Revoke, or
 *  the form that gives a level. */
function AccessGivenCell({ view, levels, onChanged, onLevelsChanged }: {
  view: PeerConnectionView
  levels: PeerAccessLevel[] | null
  onChanged: () => void
  onLevelsChanged: () => void
}) {
  const grant = view.access_given
  if (!grant) {
    return <GiveAccessForm connectionID={view.connection.id} levels={levels} onGiven={onChanged} onLevelsChanged={onLevelsChanged} />
  }
  return (
    <div className="pr-access">
      <span>{peerAccessLevelName(levels, grant.level)}</span>
      <div className="bp-hint">
        acts here as <code>{grant.peer_principal_id}</code>
        {grant.delivered_at ? ` · since ${formatWhen(grant.delivered_at)}` : ' · not delivered, so it cannot be used'}
      </div>
      <ReasonAction
        label="Revoke"
        path={peerConnectionRevokeGrantPath(view.connection.id)}
        onDone={onChanged}
        doneNote={answer => (answer as PeerGrantRevoked).notice}
      />
    </div>
  )
}

function GiveAccessForm({ connectionID, levels, onGiven, onLevelsChanged }: {
  connectionID: string
  levels: PeerAccessLevel[] | null
  onGiven: () => void
  onLevelsChanged: () => void
}) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const principals = usePrincipals()
  const [levelKey, setLevelKey] = useState('')
  const [groupID, setGroupID] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const level = levels?.find(l => l.key === levelKey) ?? null
  const groups = principals.list.filter(p => p.kind === 'group' && !p.disabled_at)

  const provision = async () => {
    if (!level) return
    setBusy(true)
    setError(null)
    try {
      const res = await fetchFn(`${basePath}${peerAccessLevelProvisionPath(level.key)}`, jsonPost({}))
      if (!res.ok) { setError(await peerRouteErrorText(res, 'Set up this level')); return }
      onLevelsChanged()
    } catch (err) {
      setError(`Set up this level: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
    }
  }

  const grant = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const res = await fetchFn(`${basePath}${peerConnectionGrantPath(connectionID)}`, jsonPost(peerGrantRequestBody(levelKey, groupID)))
      if (!res.ok) { setError(await peerRouteErrorText(res, 'Give access')); return }
      setLevelKey('')
      setGroupID('')
      onGiven()
    } catch (err) {
      setError(`Give access: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
    }
  }

  const needsGroupPick = levelKey === PeerAccessLevelGroup
  const ready = !!level && peerAccessLevelIsReady(level) && (!needsGroupPick || !!groupID)
  return (
    <form className="pr-give-access" onSubmit={grant} aria-label="Give access">
      <span className="bp-hint">No access given.</span>
      <select value={levelKey} onChange={e => { setLevelKey(e.target.value); setError(null) }} aria-label="Level of access">
        <option value="">{levels === null ? 'Loading levels…' : 'Pick a level'}</option>
        {(levels ?? []).map(l => <option key={l.key} value={l.key}>{l.display_name}</option>)}
      </select>
      {level && <div className="bp-hint">{level.description}</div>}
      {level && level.grants && level.grants.length > 0 && (
        <ul className="bp-hint pr-level-grants">
          {level.grants.map(g => (
            <li key={`${g.relation}:${g.resource_type}:${g.resource_id}`}>
              <code>{g.relation}</code> {g.resource_type} <code>{g.resource_id}</code>
            </li>
          ))}
        </ul>
      )}
      {level && !peerAccessLevelIsReady(level) && (
        <div className="pr-provision">
          <span className="bp-hint">This level is not set up on this server yet: it needs its group and grants made first.</span>
          <button type="button" className="bp-cancel" disabled={busy} onClick={() => { void provision() }}>
            {busy ? 'Setting up…' : 'Set up this level'}
          </button>
        </div>
      )}
      {needsGroupPick && (
        principals.enabled ? (
          <select value={groupID} onChange={e => setGroupID(e.target.value)} aria-label="Group">
            <option value="">{groups.length === 0 ? 'No group: make one on Principals' : 'Pick a group'}</option>
            {groups.map(g => <option key={g.id} value={g.id}>{g.display_name} ({g.id})</option>)}
          </select>
        ) : (
          <span className="bp-hint">principal-store is not proxied here, so there is no group to pick.</span>
        )
      )}
      <button type="submit" className="bp-submit" disabled={busy || !ready}>{busy ? 'Giving…' : 'Give access'}</button>
      {error && <div className="bridge-error bp-error">{error}</div>}
    </form>
  )
}

/** What the other server lets this one do there, and a check that the token
 *  still works. */
function AccessHeldCell({ view, levels }: { view: PeerConnectionView; levels: PeerAccessLevel[] | null }) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [result, setResult] = useState<{ ok: true; self: PeerGrantSelf } | { ok: false; error: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const held = view.access_held
  if (!held) return <span className="bp-hint">None yet. That server&apos;s administrator gives it on its own Peers page.</span>
  const check = async () => {
    setBusy(true)
    try {
      const res = await fetchFn(`${basePath}${peerConnectionCheckPath(view.connection.id)}`, jsonPost({}))
      if (!res.ok) { setResult({ ok: false, error: await peerRouteErrorText(res, 'Check') }); return }
      setResult({ ok: true, self: (await res.json()) as PeerGrantSelf })
    } catch (err) {
      setResult({ ok: false, error: `Check: ${err instanceof Error ? err.message : String(err)}` })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="pr-access">
      {/* The level key is the other server's; this server's list names it when both offer it. */}
      <span>{peerAccessLevelName(levels, held.level)}</span>
      <div className="bp-hint">acts there as <code>{held.remote_peer_principal_id}</code> · since {formatWhen(held.received_at)}</div>
      <span className="pr-check">
        <button type="button" className="bp-cancel" disabled={busy} onClick={() => { void check() }}>{busy ? 'Checking…' : 'Check'}</button>
        {result?.ok === true && (
          <span className="bp-hint pr-check-result">
            works: acts as <code>{result.self.peer_principal_id}</code> at level <code>{result.self.level}</code>
            {result.self.is_administrator ? ', an administrator' : ''}
          </span>
        )}
        {result?.ok === false && <span className="bridge-error bp-error pr-check-result">{result.error}</span>}
      </span>
    </div>
  )
}

/** A button that asks for a reason, then posts it. The reason stays on the row. */
function ReasonAction({ label, path, onDone, doneNote }: {
  label: string
  path: string
  onDone: () => void
  doneNote?: (answer: unknown) => string
}) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [note, setNote] = useState('')
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const res = await fetchFn(`${basePath}${path}`, jsonPost(peerReasonRequestBody(reason)))
      if (!res.ok) { setError(await peerRouteErrorText(res, label)); return }
      setNote(doneNote ? doneNote(await res.json()) : '')
      setOpen(false)
      onDone()
    } catch (err) {
      setError(`${label}: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
    }
  }
  if (note) return <span className="bp-hint pr-reason-note">{note}</span>
  if (!open) return <button type="button" className="bp-cancel" onClick={() => setOpen(true)}>{label}</button>
  return (
    <form className="pr-reason" onSubmit={submit}>
      <input type="text" className="bp-input" placeholder="Why" value={reason} onChange={e => setReason(e.target.value)} required autoFocus />
      <button type="submit" className="bp-cancel" disabled={busy}>{busy ? `${label}…` : label}</button>
      <button type="button" className="bp-cancel" onClick={() => setOpen(false)}>Cancel</button>
      {error && <div className="bridge-error bp-error">{error}</div>}
    </form>
  )
}

function PassphraseForm({ onMade }: { onMade: () => void }) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const thisServer = useThisServerSettings()
  const publicURL = thisServerPublicURL(thisServer.settings)
  const [lifetime, setLifetime] = useState(PEER_CONNECTION_INVITE_LIFETIMES[0].seconds)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState<PeerConnectionInviteCreated | null>(null)
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(null)
    setCreated(null)
    try {
      const res = await fetchFn(`${basePath}${PEER_CONNECTION_INVITES_PATH}`, jsonPost(peerConnectionInviteRequestBody(lifetime)))
      if (!res.ok) { setError(await peerRouteErrorText(res, 'Make passphrase')); return }
      setCreated((await res.json()) as PeerConnectionInviteCreated)
      onMade()
    } catch (err) {
      setError(`Make passphrase: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="bg-form" onSubmit={submit} aria-label="Make a connection passphrase">
      <div className="pr-public-url">
        {publicURL ? (
          <span>The other server connects to <code>{publicURL}</code>.</span>
        ) : (
          <span className="bp-hint">
            This server&apos;s own address is not set (<code>LLMBRIDGE_PEER_CONNECTIONS_PUBLIC_URL</code>, on Service settings), so this
            page cannot say which address the other server should use. Tell its administrator the https address this
            server answers at.
            {thisServer.error && <> Could not read this server&apos;s settings: {thisServer.error}</>}
          </span>
        )}
      </div>
      <label className="bg-filter">
        <span>Use within</span>
        <select value={lifetime} onChange={e => setLifetime(Number(e.target.value))}>
          {PEER_CONNECTION_INVITE_LIFETIMES.map(l => <option key={l.seconds} value={l.seconds}>{l.label}</option>)}
        </select>
      </label>
      <button type="submit" className="bp-submit bg-submit" disabled={busy}>{busy ? 'Making…' : 'Make passphrase'}</button>
      {error && <div className="bridge-error bp-error bg-form-error">{error}</div>}
      {created && (
        <div className="bg-form-outcome pr-passphrase">
          <span>
            Passphrase for <code>{created.invite.id}</code>, shown only this once. Give it to the other server&apos;s
            administrator{publicURL ? <> with the address <code>{publicURL}</code></> : null}; it works once, until{' '}
            {formatWhen(created.invite.expires_at)}. Connecting gives no access either way.
          </span>
          <code className="pr-passphrase-value">{created.passphrase}</code>
          <button type="button" className="bp-cancel" onClick={() => { void navigator.clipboard.writeText(created.passphrase) }}>Copy</button>
        </div>
      )}
    </form>
  )
}

function ConnectForm({ onConnected }: { onConnected: () => void }) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [form, setForm] = useState<PeerConnectForm>(EMPTY_PEER_CONNECT_FORM)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [connected, setConnected] = useState<PeerConnectionView | null>(null)
  const set = (field: keyof PeerConnectForm) => (value: string) => setForm(current => ({ ...current, [field]: value }))
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(null)
    setConnected(null)
    try {
      const res = await fetchFn(`${basePath}${PEER_CONNECTIONS_PATH}`, jsonPost(peerConnectRequestBody(form)))
      if (!res.ok) { setError(await peerRouteErrorText(res, 'Connect')); return }
      setConnected((await res.json()) as PeerConnectionView)
      setForm(EMPTY_PEER_CONNECT_FORM)
      onConnected()
    } catch (err) {
      setError(`Connect: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="bg-form pr-connect-form" onSubmit={submit} aria-label="Connect to a server">
      <label className="bg-filter pr-url-field">
        <span>Its address</span>
        <input type="text" placeholder="https://bridge.example.com" value={form.baseURL} onChange={e => set('baseURL')(e.target.value)} required />
      </label>
      <label className="bg-filter">
        <span>Passphrase it gave you</span>
        <input type="password" autoComplete="off" value={form.passphrase} onChange={e => set('passphrase')(e.target.value)} required />
      </label>
      <label className="bg-filter">
        <span>Call it</span>
        <input type="text" placeholder="main server" value={form.displayName} onChange={e => set('displayName')(e.target.value)} required />
      </label>
      <label className="bg-filter">
        <span>It calls this server</span>
        <input type="text" placeholder="laptop" value={form.nameForThisServer} onChange={e => set('nameForThisServer')(e.target.value)} required />
      </label>
      <label className="bg-filter pr-url-field">
        <span>This server&apos;s own address (optional)</span>
        <input type="text" placeholder="leave empty if this server has none" value={form.thisServerBaseURL} onChange={e => set('thisServerBaseURL')(e.target.value)} />
      </label>
      <p className="bp-hint pr-connect-note">
        With this server&apos;s own https address, each server calls the other directly. Left empty, this server dials
        out to the other and keeps a socket open, and requests both ways travel over it, so this server needs no
        address and can move between networks.
      </p>
      <button type="submit" className="bp-submit bg-submit" disabled={busy}>{busy ? 'Connecting…' : 'Connect'}</button>
      {error && <div className="bridge-error bp-error bg-form-error">{error}</div>}
      {connected && (
        <div className="bg-form-outcome">
          Connected to {connected.connection.display_name} ({peerTransportDescription(connected.connection)}). No access
          is given either way yet: give that server a level in the table above, and ask its administrator to give this
          one a level there.
        </div>
      )}
    </form>
  )
}

function InvitesTable({ invites }: { invites: PeerConnectionInvite[] | null }) {
  const now = new Date()
  const sorted = useMemo(() => liveFirstNewestFirst(invites ?? [], i => peerConnectionInviteState(i, new Date()) === 'waiting'), [invites])
  return (
    <table className="bg-table pr-table" aria-label="Connection passphrases">
      <thead>
        <tr><th>Passphrase</th><th>Made by</th><th>State</th></tr>
      </thead>
      <tbody>
        {invites === null && <tr><td colSpan={3} className="bp-empty-inline">Loading…</td></tr>}
        {invites !== null && invites.length === 0 && <tr><td colSpan={3} className="bp-empty-inline">No passphrase has been made.</td></tr>}
        {sorted.map(invite => {
          const state = peerConnectionInviteState(invite, now)
          return (
            <tr key={invite.id} className={state === 'waiting' ? '' : 'bg-row-revoked'} data-peer-connection-invite-id={invite.id}>
              <td><code>{invite.id}</code><div className="bp-hint">{formatWhen(invite.created_at)}</div></td>
              <td><code>{invite.created_by}</code></td>
              <td>
                {state === 'waiting' && <>waiting until {formatWhen(invite.expires_at)}</>}
                {state === 'used' && <>used {formatWhen(invite.redeemed_at)}{invite.connection_id ? <> for <code>{invite.connection_id}</code></> : null}</>}
                {state === 'expired' && <>expired {formatWhen(invite.expires_at)}</>}
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

/** For each connected server this one gives access to, which of its people
 *  act here as which principal. The mapping keys on the principal this server
 *  made for that server, so a server with no access given has nobody to map. */
function MessagesWithConnectedServers({ connections }: { connections: PeerConnectionView[] | null }) {
  if (connections === null) return <div className="bp-empty-inline">Loading…</div>
  const live = connections.filter(view => !peerConnectionIsRemoved(view))
  if (live.length === 0) return <div className="bp-empty-inline">No server is connected.</div>
  return (
    <>
      {live.map(view => {
        const serverName = connectedServerLabel(view).label
        return (
          <details key={view.connection.id} className="pm-connection-block">
            <summary>{serverName}</summary>
            <PeerConnectionMessages connectionID={view.connection.id} serverName={serverName} />
          </details>
        )
      })}
    </>
  )
}

function PeopleFromConnectedServers({ connections }: { connections: PeerConnectionView[] | null }) {
  if (connections === null) return <div className="bp-empty-inline">Loading…</div>
  const mappable = connections.filter(view => !peerConnectionIsRemoved(view) && !!view.connection.peer_principal_id)
  if (mappable.length === 0) {
    return <div className="bp-empty-inline">This server gives no connected server access, so there is nobody to map yet.</div>
  }
  return (
    <>
      {mappable.map(view => (
        <PeopleOfOneServer
          key={view.connection.id}
          serverName={view.connection.display_name}
          peerPrincipalID={view.connection.peer_principal_id as string}
        />
      ))}
    </>
  )
}

function PeopleOfOneServer({ serverName, peerPrincipalID }: { serverName: string; peerPrincipalID: string }) {
  const mappings = usePeerList<PeerUserMapping>(peerUserMappingsListPath(peerPrincipalID))
  const principals = usePrincipals()
  const sorted = useMemo(() => liveFirstNewestFirst(mappings.rows ?? [], m => !peerUserMappingIsRemoved(m)), [mappings.rows])
  const nameOf = (id: string) => principals.byId.get(id)?.display_name
  return (
    <div className="pr-people" data-peer-principal-id={peerPrincipalID}>
      <h4 className="pr-people-title">
        {serverName} <span className="bp-hint">acts here as <code>{peerPrincipalID}</code></span>
      </h4>
      {mappings.error && <div className="bridge-error bp-error">{mappings.error}</div>}
      <table className="bg-table pr-table" aria-label={`People from ${serverName}`}>
        <thead>
          <tr><th>Their person</th><th>Acts here as</th><th>Mapped by</th><th></th></tr>
        </thead>
        <tbody>
          {mappings.rows === null && <tr><td colSpan={4} className="bp-empty-inline">Loading…</td></tr>}
          {mappings.rows !== null && mappings.rows.length === 0 && (
            <tr><td colSpan={4} className="bp-empty-inline">Nobody from {serverName} is mapped, so only its administrators get anything here.</td></tr>
          )}
          {sorted.map(mapping => {
            const removed = peerUserMappingIsRemoved(mapping)
            const localName = nameOf(mapping.local_principal_id)
            return (
              <tr key={mapping.id} className={removed ? 'bg-row-revoked' : ''} data-peer-user-mapping-id={mapping.id}>
                <td><code>{mapping.remote_principal_id}</code></td>
                <td>{localName ? <>{localName} </> : null}<code>{mapping.local_principal_id}</code></td>
                <td>
                  <code>{mapping.created_by}</code><div className="bp-hint">{formatWhen(mapping.created_at)}</div>
                  {removed && (
                    <span className="bp-badge bp-badge-disabled" title={mapping.remove_reason}>
                      removed {formatWhen(mapping.removed_at)}{mapping.removed_by ? ` by ${mapping.removed_by}` : ''}{mapping.remove_reason ? `: ${mapping.remove_reason}` : ''}
                    </span>
                  )}
                </td>
                <td className="bg-actions">
                  {!removed && <ReasonAction label="Remove" path={peerUserMappingRemovePath(mapping.id)} onDone={mappings.reload} />}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <MapPersonForm peerPrincipalID={peerPrincipalID} serverName={serverName} onMapped={mappings.reload} />
    </div>
  )
}

function MapPersonForm({ peerPrincipalID, serverName, onMapped }: {
  peerPrincipalID: string
  serverName: string
  onMapped: () => void
}) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const principals = usePrincipals()
  const [remotePrincipalID, setRemotePrincipalID] = useState('')
  const [localPrincipalID, setLocalPrincipalID] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const choices = peerUserMappingLocalChoices(principals.list)
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const body = peerUserMappingRequestBody(peerPrincipalID, remotePrincipalID, localPrincipalID)
      const res = await fetchFn(`${basePath}${PEER_USER_MAPPINGS_PATH}`, jsonPost(body))
      if (!res.ok) { setError(await peerRouteErrorText(res, 'Map person')); return }
      setRemotePrincipalID('')
      setLocalPrincipalID('')
      onMapped()
    } catch (err) {
      setError(`Map person: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="bg-form pr-map-person" onSubmit={submit} aria-label={`Map a person from ${serverName}`}>
      <label className="bg-filter">
        <span>Their principal id on {serverName}</span>
        <input type="text" placeholder="principal_000001" value={remotePrincipalID} onChange={e => setRemotePrincipalID(e.target.value)} required />
      </label>
      <label className="bg-filter">
        <span>Acts here as</span>
        {principals.enabled ? (
          <select value={localPrincipalID} onChange={e => setLocalPrincipalID(e.target.value)} required aria-label="Principal here">
            <option value="">{principals.loading ? 'Loading principals…' : 'Pick a principal'}</option>
            {choices.map(p => <option key={p.id} value={p.id}>{p.display_name} ({p.id})</option>)}
          </select>
        ) : (
          <span className="bp-hint">principal-store is not proxied here, so there is no principal to pick.</span>
        )}
      </label>
      <button type="submit" className="bp-submit bg-submit" disabled={busy || !principals.enabled}>{busy ? 'Mapping…' : 'Map'}</button>
      {principals.error && <div className="bridge-error bp-error bg-form-error">principal-store: {principals.error}</div>}
      {error && <div className="bridge-error bp-error bg-form-error">{error}</div>}
    </form>
  )
}
