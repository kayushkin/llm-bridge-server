import { describe, expect, it } from 'vitest'
import type { PeerAccess, PeerConnection, PeerConnectionView } from '@kayushkin/llm-bridge-types'
import {
  connectionOfServerId,
  CONNECTED_SERVER_PARAM,
  CONNECTED_SERVER_SESSION_PARAM,
  connectedServerBlockedReason,
  connectedServerBlockedWord,
  connectedServerListings,
  connectedServerLabel,
  connectedServerSessionKey,
  connectionsThisServerAdministers,
  readConnectedServerSessionRef,
  readableConnectionIds,
  sameConnectedServerSessionRef,
  thisServerLabel,
  writeConnectedServerSessionRef,
} from '../src/serverListings'

const connection = (overrides: Partial<PeerConnection> = {}): PeerConnection => ({
  id: 'peer_connection_000001',
  display_name: 'typed name',
  transport: 'dial_out',
  role: 'accepting',
  created_by: 'principal_000001',
  created_at: '2026-10-27T10:00:00Z',
  ...overrides,
})

const held: PeerAccess = {
  id: 'peer_access_000001',
  connection_id: 'peer_connection_000001',
  remote_grant_id: 'peer_grant_000009',
  remote_peer_principal_id: 'principal_000040',
  level: 'administrator',
} as PeerAccess

const view = (overrides: Partial<PeerConnectionView> = {}): PeerConnectionView => ({
  connection: connection(),
  socket_connected: true,
  access_held: held,
  other_server_name: '',
  ...overrides,
} as PeerConnectionView)

describe('server folder labels', () => {
  it("labels this server's folder by its own name, and says This server without one", () => {
    expect(thisServerLabel('main')).toBe('main')
    expect(thisServerLabel('  ')).toBe('This server')
    expect(thisServerLabel(null)).toBe('This server')
  })

  it("prefers the name a connected server reports over the typed one, and says which it used", () => {
    expect(connectedServerLabel(view({ other_server_name: 'laptop' }))).toEqual({ label: 'laptop', fromServer: true })
    expect(connectedServerLabel(view())).toEqual({ label: 'typed name', fromServer: false })
  })
})

describe('which connected servers can be read', () => {
  it('reads a dial-out connection with an open socket and access held', () => {
    expect(connectedServerBlockedReason(view())).toBeNull()
  })

  it('refuses a closed socket, a connection with no access held, and a removed one, saying why', () => {
    expect(connectedServerBlockedReason(view({ socket_connected: false }))).toMatch(/not connected now/)
    expect(connectedServerBlockedReason(view({ access_held: undefined }))).toMatch(/not given this server any access/)
    expect(connectedServerBlockedReason(view({ connection: connection({ removed_at: '2026-10-27T11:00:00Z' }) }))).toMatch(/disconnected/)
  })

  it('refuses an open socket whose bridge stopped answering, like an offline one, saying since when', () => {
    const stuck = view({ answering: false, not_answering_since: '2026-10-29T21:16:00Z', last_probe_failure: 'context deadline exceeded' })
    expect(connectedServerBlockedReason(stuck)).toMatch(/connected, not answering since .*context deadline exceeded/)
    expect(connectedServerBlockedWord(stuck)).toBe('not answering')
    expect(readableConnectionIds(connectedServerListings([stuck]))).toEqual([])
    expect(connectedServerBlockedReason(view({ answering: true }))).toBeNull()
  })

  it('orders readable servers first, then unreadable, then removed, and keeps every one', () => {
    const folders = connectedServerListings([
      view({ connection: connection({ id: 'c-removed', display_name: 'a', removed_at: '2026-10-27T11:00:00Z' }) }),
      view({ connection: connection({ id: 'c-offline', display_name: 'b' }), socket_connected: false }),
      view({ connection: connection({ id: 'c-zed', display_name: 'zed' }) }),
      view({ connection: connection({ id: 'c-alpha', display_name: 'alpha' }) }),
    ])
    expect(folders.map(f => f.connectionId)).toEqual(['c-alpha', 'c-zed', 'c-offline', 'c-removed'])
    expect(folders.map(f => f.blockedWord)).toEqual([null, null, 'offline', 'removed'])
    expect(readableConnectionIds(folders)).toEqual(['c-alpha', 'c-zed'])
  })
})

describe('the connected server session in the URL', () => {
  it('reads nothing without ?server=, and a new session with no ?server_session=', () => {
    expect(readConnectedServerSessionRef(new URLSearchParams('session=br_1'))).toBeNull()
    expect(readConnectedServerSessionRef(new URLSearchParams(`${CONNECTED_SERVER_PARAM}=c1`))).toEqual({ connectionId: 'c1', sessionId: null })
  })

  it("writes both params beside this server's ?session= without touching it, and clears them", () => {
    const params = new URLSearchParams('session=br_local')
    expect(writeConnectedServerSessionRef(params, { connectionId: 'c1', sessionId: 'br_remote' })).toBe(true)
    expect(params.get('session')).toBe('br_local')
    expect(params.get(CONNECTED_SERVER_PARAM)).toBe('c1')
    expect(params.get(CONNECTED_SERVER_SESSION_PARAM)).toBe('br_remote')
    expect(writeConnectedServerSessionRef(params, { connectionId: 'c1', sessionId: 'br_remote' })).toBe(false)
    expect(readConnectedServerSessionRef(params)).toEqual({ connectionId: 'c1', sessionId: 'br_remote' })
    expect(writeConnectedServerSessionRef(params, null)).toBe(true)
    expect(params.toString()).toBe('session=br_local')
  })

  it('keys two servers’ sessions with the same id apart', () => {
    expect(connectedServerSessionKey('c1', 'br_1')).not.toBe(connectedServerSessionKey('c2', 'br_1'))
    expect(sameConnectedServerSessionRef({ connectionId: 'c1', sessionId: 'br_1' }, { connectionId: 'c2', sessionId: 'br_1' })).toBe(false)
    expect(sameConnectedServerSessionRef(null, null)).toBe(true)
  })
})

describe('connected servers whose waiting messages this server decides', () => {
  it('lists a readable connection holding administrator there, and no other', () => {
    const administered = view()
    const messagesOnly = view({ connection: connection({ id: 'peer_connection_000002' }), access_held: { ...held, level: 'messages' } })
    const noAccess = view({ connection: connection({ id: 'peer_connection_000003' }), access_held: undefined })
    const socketClosed = view({ connection: connection({ id: 'peer_connection_000004' }), socket_connected: false })
    const removed = view({ connection: connection({ id: 'peer_connection_000005', removed_at: '2026-10-28T10:00:00Z' }) })
    expect(
      connectionsThisServerAdministers([administered, messagesOnly, noAccess, socketClosed, removed]).map(v => v.connection.id),
    ).toEqual(['peer_connection_000001'])
  })
})

describe('connectionOfServerId', () => {
  const listing = (connectionId: string, serverId: string, blockedWord: string | null = null) => ({
    connectionId, serverId, label: connectionId, labelFromServer: true, blockedReason: blockedWord ? 'why' : null, blockedWord,
  })
  it('finds the connection by the server’s own id, never a removed one', () => {
    const servers = [listing('c1', 'server_01AAAAAAAAAAAAAAAAAAAAAAAA', 'removed'), listing('c2', 'server_01AAAAAAAAAAAAAAAAAAAAAAAA'), listing('c3', '')]
    expect(connectionOfServerId(servers, 'server_01AAAAAAAAAAAAAAAAAAAAAAAA')?.connectionId).toBe('c2')
    expect(connectionOfServerId(servers, 'server_01BBBBBBBBBBBBBBBBBBBBBBBB')).toBeNull()
    expect(connectionOfServerId(servers, '')).toBeNull()
  })
})
