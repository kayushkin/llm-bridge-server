import { describe, expect, it } from 'vitest'
import type { PeerAccessLevel, PeerConnection, PeerConnectionInvite, PeerConnectionView, PeerUserMapping, ServiceSettings } from '@kayushkin/llm-bridge-types'
import type { Principal } from '@kayushkin/principal-store-types'
import { DEFAULT_BRIDGE_ROUTES, type BridgeConfig } from '../src/context'
import {
  EMPTY_PEER_CONNECT_FORM,
  liveFirstNewestFirst,
  peerAccessLevelIsReady,
  peerAccessLevelName,
  peerAccessLevelProvisionPath,
  peerConnectRequestBody,
  peerConnectionBridgeConfig,
  CONNECTED_SERVER_PATH_SOURCES,
  peerConnectionInviteRequestBody,
  peerConnectionInviteState,
  peerConnectionProxyBasePath,
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
} from '../src/peers'

const connection = (overrides: Partial<PeerConnection> = {}): PeerConnection => ({
  id: 'peer_connection_000001',
  display_name: 'laptop',
  transport: 'dial_out',
  role: 'accepting',
  created_by: 'principal_000001',
  created_at: '2026-10-27T10:00:00Z',
  ...overrides,
})

const view = (overrides: Partial<PeerConnectionView> = {}): PeerConnectionView => ({
  connection: connection(),
  socket_connected: false,
  ...overrides,
})

const level = (overrides: Partial<PeerAccessLevel> = {}): PeerAccessLevel => ({
  key: 'run_sessions',
  display_name: 'Run sessions',
  description: 'start sessions on this server',
  needs_group: true,
  ...overrides,
})

describe('peer paths and bodies', () => {
  it("mounts a connected server under its connection's proxy, with the id escaped", () => {
    expect(peerConnectionProxyBasePath('/api/bridge', 'peer_connection_000001')).toBe('/api/bridge/peer-connections/peer_connection_000001/proxy')
    expect(peerConnectionRemovePath('a/b')).toBe('/peer-connections/a%2Fb/remove')
    expect(peerConnectionRevokeGrantPath('c1')).toBe('/peer-connections/c1/revoke-grant')
    expect(peerAccessLevelProvisionPath('run_sessions')).toBe('/peer-access-levels/run_sessions/provision')
  })

  it('sends the connect form trimmed, and leaves out an empty address for this server so it dials out', () => {
    const form = { baseURL: ' https://b.example.com/ ', passphrase: ' p ', displayName: ' main ', nameForThisServer: ' laptop ', thisServerBaseURL: '  ' }
    expect(peerConnectRequestBody(form)).toEqual({ base_url: 'https://b.example.com/', passphrase: 'p', display_name: 'main', name_for_this_server: 'laptop' })
    expect(peerConnectRequestBody({ ...form, thisServerBaseURL: ' https://a.example.com ' }).this_server_base_url).toBe('https://a.example.com')
    expect(peerConnectRequestBody(EMPTY_PEER_CONNECT_FORM)).not.toHaveProperty('this_server_base_url')
  })

  it('names a group only for the group level', () => {
    expect(peerGrantRequestBody('run_sessions', 'principal_000040')).toEqual({ level: 'run_sessions' })
    expect(peerGrantRequestBody('group', ' principal_000040 ')).toEqual({ level: 'group', group_principal_id: 'principal_000040' })
  })

  it('sends a passphrase lifetime and a trimmed reason', () => {
    expect(peerConnectionInviteRequestBody(3600)).toEqual({ expires_in_seconds: 3600 })
    expect(peerReasonRequestBody(' moved ')).toEqual({ reason: 'moved' })
  })
})

describe('access levels', () => {
  it('can be granted once no group is needed or the group exists', () => {
    expect(peerAccessLevelIsReady(level())).toBe(false)
    expect(peerAccessLevelIsReady(level({ group_principal_id: 'principal_000050' }))).toBe(true)
    expect(peerAccessLevelIsReady(level({ key: 'administrator', needs_group: false }))).toBe(true)
  })
  it("names a level by the server's list, or its key when the list does not offer it", () => {
    expect(peerAccessLevelName([level()], 'run_sessions')).toBe('Run sessions')
    expect(peerAccessLevelName([level()], 'observe')).toBe('observe')
    expect(peerAccessLevelName(null, 'run_sessions')).toBe('run_sessions')
  })
})

describe('how two servers reach each other', () => {
  it('says which side dials', () => {
    expect(peerTransportDescription(connection({ transport: 'direct', remote_base_url: 'https://b.example.com' }))).toMatch(/^direct/)
    expect(peerTransportDescription(connection({ remote_base_url: 'https://b.example.com' }))).toBe('dial-out: this server keeps a socket open to the other')
    expect(peerTransportDescription(connection())).toBe('dial-out: the other server keeps a socket open to this one')
  })
  it('reads a dial-out connection as reachable only while its socket is open', () => {
    expect(peerConnectionReach(view())).toBe('socket closed')
    expect(peerConnectionReach(view({ socket_connected: true }))).toBe('socket open')
    expect(peerConnectionReach(view({ connection: connection({ transport: 'direct' }) }))).toBe('direct')
    expect(peerConnectionReach(view({ connection: connection({ removed_at: '2026-10-27T11:00:00Z' }), socket_connected: true }))).toBe('removed')
  })
  it('reads an open socket whose bridge stopped answering as not answering, with since when', () => {
    expect(peerConnectionReach(view({ socket_connected: true, answering: true }))).toBe('socket open')
    const stuck = view({ socket_connected: true, answering: false, not_answering_since: '2026-10-29T21:16:00Z', last_probe_failure: 'context deadline exceeded' })
    expect(peerConnectionReach(stuck)).toBe('not answering')
    expect(peerConnectionReachText(stuck)).toMatch(/^connected, not answering since /)
    expect(peerConnectionReach(view({ socket_connected: false, answering: false }))).toBe('socket closed')
  })
})

describe('peerConnectionInviteState', () => {
  const invite = (overrides: Partial<PeerConnectionInvite> = {}): PeerConnectionInvite => ({
    id: 'peer_connection_invite_000001',
    created_by: 'principal_000001',
    created_at: '2026-10-27T10:00:00Z',
    expires_at: '2026-10-27T11:00:00Z',
    ...overrides,
  })
  const now = new Date('2026-10-27T10:30:00Z')
  it('waits until it expires, and a use wins over the expiry', () => {
    expect(peerConnectionInviteState(invite(), now)).toBe('waiting')
    expect(peerConnectionInviteState(invite({ expires_at: '2026-10-27T10:30:00Z' }), now)).toBe('expired')
    expect(peerConnectionInviteState(invite({ expires_at: '2026-10-27T10:00:00Z', redeemed_at: '2026-10-27T09:55:00Z' }), now)).toBe('used')
  })
})

describe('liveFirstNewestFirst', () => {
  it('puts live rows above ended ones, each newest first', () => {
    const rows = [
      { id: 'old-live', created_at: '2026-10-01T00:00:00Z', live: true },
      { id: 'new-ended', created_at: '2026-10-20T00:00:00Z', live: false },
      { id: 'new-live', created_at: '2026-10-10T00:00:00Z', live: true },
    ]
    expect(liveFirstNewestFirst(rows, r => r.live).map(r => r.id)).toEqual(['new-live', 'old-live', 'new-ended'])
  })
})

describe('thisServerPublicURL', () => {
  const settings = (value: string): ServiceSettings => ({
    service: 'llm-bridge-server',
    settings: [{ key: 'peer_connections.public_url', environment_variable: 'LLMBRIDGE_PEER_CONNECTIONS_PUBLIC_URL', value } as ServiceSettings['settings'][number]],
    kinds: [],
    sources: [],
  })
  it("reads the server's peer_connections.public_url setting, and nothing when it is unset", () => {
    expect(thisServerPublicURL(settings(' https://a.example.com/bridge '))).toBe('https://a.example.com/bridge')
    expect(thisServerPublicURL(settings(''))).toBe('')
    expect(thisServerPublicURL(null)).toBe('')
  })
})

describe('peerRouteErrorText', () => {
  it('puts plain words before the server message for a refusal a person can act on', async () => {
    const res = new Response(JSON.stringify({ error: { code: 'peer_not_connected', message: 'no socket' } }), { status: 503 })
    const text = await peerRouteErrorText(res, 'Open')
    expect(text).toMatch(/^Open: The other server is not connected now/)
    expect(text).toMatch(/The server said: no socket \(peer_not_connected\)$/)
  })
  it("shows any other code with llm-bridge-server's message", async () => {
    const res = new Response(JSON.stringify({ error: { code: 'bad_request', message: 'level is required' } }), { status: 400 })
    expect(await peerRouteErrorText(res, 'Give access')).toBe('Give access: level is required (bad_request)')
  })
  it('falls to the raw text, then the status', async () => {
    expect(await peerRouteErrorText(new Response('llm-bridge unavailable', { status: 502 }), 'Connect')).toBe('Connect: llm-bridge unavailable')
    expect(await peerRouteErrorText(new Response('', { status: 403 }), 'Connect')).toBe('Connect: HTTP 403')
  })
})

describe('peerConnectionBridgeConfig', () => {
  const fetchFn = async () => new Response()
  const local = {
    fetch: fetchFn,
    basePath: '/api/bridge',
    kanbanStoreBasePath: '/api/kanban',
    noteboardBasePath: '/api/noteboard',
    referenceStoreBasePath: '/api/bridge/reference-store',
    photoStoreBasePath: '/api/photos',
    renderHarnessExtension: () => null,
    routes: DEFAULT_BRIDGE_ROUTES,
  } as unknown as BridgeConfig

  it("points llm-bridge-server's routes at the connection's proxy and empties this host's stores", () => {
    const peer = peerConnectionBridgeConfig(local, 'peer_connection_000001')
    expect(peer.basePath).toBe('/api/bridge/peer-connections/peer_connection_000001/proxy')
    expect(peer.noteboardBasePath).toBe('')
    expect(peer.referenceStoreBasePath).toBe('/api/bridge/peer-connections/peer_connection_000001/proxy/reference-store')
    expect(peer.photoStoreBasePath).toBe('')
    expect(peer.renderHarnessExtension).toBeNull()
  })

  it("reaches kanban-store through the other server's rooted /kanban-store/ gateway", () => {
    const peer = peerConnectionBridgeConfig(local, 'peer_connection_000001')
    expect(peer.kanbanStoreBasePath).toBe('/api/bridge/peer-connections/peer_connection_000001/proxy/kanban-store')
  })

  it("reaches grant-store through the other server's gateway, whose shape matches this host's path", () => {
    const peer = peerConnectionBridgeConfig({ ...local, grantStoreBasePath: '/api/grants' } as BridgeConfig, 'peer_connection_000001')
    expect(peer.grantStoreBasePath).toBe('/api/bridge/peer-connections/peer_connection_000001/proxy/grant-store')
  })

  it('decides every string path of BridgeConfig, and no path of this host survives except through the proxy', () => {
    const everyPath = Object.fromEntries(Object.keys(CONNECTED_SERVER_PATH_SOURCES).map(field => [field, `/api/host/${field}`]))
    const peer = peerConnectionBridgeConfig({ ...local, ...everyPath } as unknown as BridgeConfig, 'peer_connection_000001')
    for (const field of Object.keys(CONNECTED_SERVER_PATH_SOURCES)) {
      const value = (peer as unknown as Record<string, string>)[field]
      expect(value === '' || value.startsWith('/api/host/basePath/peer-connections/peer_connection_000001/proxy'), field).toBe(true)
    }
  })

  it("keeps the host fetch and the pages, and empties only the routes that take this host's ids", () => {
    const peer = peerConnectionBridgeConfig(local, 'peer_connection_000001')
    expect(peer.fetch).toBe(fetchFn)
    expect(peer.routes.chat).toBe(DEFAULT_BRIDGE_ROUTES.chat)
    expect(peer.routes.peers).toBe(DEFAULT_BRIDGE_ROUTES.peers)
    expect(peer.routes.effectiveConfig).toBe('')
    expect(peer.routes.card).toBe('')
    expect(peer.routes.previews).toBe('')
    expect(local.routes).toBe(DEFAULT_BRIDGE_ROUTES)
    expect(DEFAULT_BRIDGE_ROUTES.effectiveConfig).not.toBe('')
  })
})

describe('people from connected servers', () => {
  it('lists one server\'s mappings by its peer principal, escaped', () => {
    expect(peerUserMappingsListPath('principal_000145')).toBe('/peer-user-mappings?peer_principal_id=principal_000145')
    expect(peerUserMappingsListPath('a b&c')).toBe('/peer-user-mappings?peer_principal_id=a%20b%26c')
    expect(peerUserMappingRemovePath('peer_user_mapping_000001')).toBe('/peer-user-mappings/peer_user_mapping_000001/remove')
  })

  it('sends the three ids trimmed', () => {
    expect(peerUserMappingRequestBody(' principal_000145 ', ' principal_000001 ', 'principal_000002 ')).toEqual({
      peer_principal_id: 'principal_000145',
      remote_principal_id: 'principal_000001',
      local_principal_id: 'principal_000002',
    })
  })

  it('treats a mapping with removed_at as removed', () => {
    const mapping: PeerUserMapping = {
      id: 'peer_user_mapping_000001', peer_principal_id: 'principal_000145', remote_principal_id: 'principal_000001',
      local_principal_id: 'principal_000002', created_by: 'principal_000001', created_at: '2026-10-27T10:00:00Z',
    }
    expect(peerUserMappingIsRemoved(mapping)).toBe(false)
    expect(peerUserMappingIsRemoved({ ...mapping, removed_at: '2026-10-27T11:00:00Z' })).toBe(true)
  })

  it('offers only principals that act as a caller and are not disabled', () => {
    const principal = (id: string, actsAsCaller: boolean, disabledAt = 0): Principal => ({
      id, seq: 1, kind: actsAsCaller ? 'human' : 'group', display_name: id, email: '', disabled_at: disabledAt,
      created_at: 0, updated_at: 0, is_administrator: false, acts_in_this_deployment: true,
      acts_as_caller: actsAsCaller, restricted_to_its_grants: false,
    } as Principal)
    const choices = peerUserMappingLocalChoices([principal('person', true), principal('group', false), principal('left', true, 5)])
    expect(choices.map(p => p.id)).toEqual(['person'])
  })

  it('says when the other server removed the connection', () => {
    expect(peerConnectionRemovedBy(connection({ removed_at: 'x', removed_by_other_server: true }))).toBe('the other server removed it')
    expect(peerConnectionRemovedBy(connection({ removed_at: 'x', removed_by: 'principal_000001' }))).toBe('removed by principal_000001')
  })

  it('explains an unmapped person in plain words before the server\'s message', async () => {
    const res = new Response(JSON.stringify({ error: { code: 'peer_user_not_mapped', message: 'principal_000009 of principal_000145 is not mapped' } }), { status: 403 })
    const text = await peerRouteErrorText(res, 'Open')
    expect(text).toContain('not mapped to anyone here')
    expect(text).toContain('(peer_user_not_mapped)')
  })
})

describe('a connected server and the host’s chips', () => {
  it('never draws a connected server’s records with this host’s chips', () => {
    const host = { renderReferenceChip: () => null, renderHarnessExtension: null, routes: DEFAULT_BRIDGE_ROUTES, basePath: '/api/bridge' } as unknown as BridgeConfig
    expect(peerConnectionBridgeConfig(host, 'peer_connection_000001').renderReferenceChip).toBeNull()
  })
})
