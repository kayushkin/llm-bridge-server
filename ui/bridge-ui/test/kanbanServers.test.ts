import { describe, expect, it } from 'vitest'
import type { PeerAccess, PeerConnection, PeerConnectionView } from '@kayushkin/llm-bridge-types'
import { KANBAN_LAST_BOARD_STORAGE_KEY } from '../src/constants'
import {
  kanbanLastBoardStorageKey,
  kanbanServerNamed,
  kanbanServerParamValue,
  kanbanServers,
  kanbanSessionHref,
  serverBoardsHeading,
} from '../src/kanbanServers'

const THIS_SERVER_ID = 'server_01M64KCNK3EBR2XV06QZNY6729'
const LAPTOP_ID = 'server_01M64X1V01QDBDE16TEQP2QHKS'

const connection = (overrides: Partial<PeerConnection> = {}): PeerConnection => ({
  id: 'peer_connection_000001',
  display_name: 'typed name',
  transport: 'dial_out',
  role: 'accepting',
  created_by: 'principal_000001',
  created_at: '2026-10-27T10:00:00Z',
  ...overrides,
})

const held = {
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
  other_server_name: 'laptop',
  other_server_id: LAPTOP_ID,
  ...overrides,
} as PeerConnectionView)

const thisServer = { id: THIS_SERVER_ID, name: 'main' }

describe('the Kanban page servers', () => {
  it('lists this server first, then each connected server by the name it reports', () => {
    const servers = kanbanServers(thisServer, [view()])
    expect(servers).toEqual([
      { serverId: THIS_SERVER_ID, connectionId: null, label: 'main', blockedReason: null },
      { serverId: LAPTOP_ID, connectionId: 'peer_connection_000001', label: 'laptop', blockedReason: null },
    ])
  })

  it('lists this server before GET /server answers, with no id yet', () => {
    expect(kanbanServers(null, [])).toEqual([{ serverId: '', connectionId: null, label: 'This server', blockedReason: null }])
  })

  it('leaves out removed connections, and lists a server reached twice once, by the readable connection', () => {
    const servers = kanbanServers(thisServer, [
      view({ connection: connection({ id: 'c-offline', display_name: 'b' }), socket_connected: false }),
      view({ connection: connection({ id: 'c-live', display_name: 'a' }) }),
      view({ connection: connection({ id: 'c-removed', removed_at: '2026-10-28T10:00:00Z' }), other_server_id: 'server_01M64X1V01QDBDE16TEQP2QHKT' }),
    ])
    expect(servers.map(server => server.connectionId)).toEqual([null, 'c-live'])
  })

  it('lists a server that reported no id as blocked, saying why', () => {
    const [, old] = kanbanServers(thisServer, [view({ other_server_id: '' })])
    expect(old.serverId).toBe('')
    expect(old.blockedReason).toMatch(/has not reported its server id/)
  })

  it('carries the reason a connected server cannot be read', () => {
    const [, offline] = kanbanServers(thisServer, [view({ socket_connected: false })])
    expect(offline.blockedReason).toMatch(/not connected now/)
  })
})

describe('the server ?server= names', () => {
  const servers = kanbanServers(thisServer, [view()])

  it('is this server with no parameter or with its own id', () => {
    expect(kanbanServerNamed(servers, null)?.connectionId).toBeNull()
    expect(kanbanServerNamed(servers, '  ')?.connectionId).toBeNull()
    expect(kanbanServerNamed(servers, THIS_SERVER_ID)?.connectionId).toBeNull()
  })

  it('is the connected server with that id, and null for an id no server has', () => {
    expect(kanbanServerNamed(servers, LAPTOP_ID)?.connectionId).toBe('peer_connection_000001')
    expect(kanbanServerNamed(servers, 'server_01M64X1V01QDBDE16TEQP2QHKZ')).toBeNull()
  })

  it('never matches a server with no id to an empty parameter', () => {
    const withOld = kanbanServers(null, [view({ other_server_id: '' })])
    expect(kanbanServerNamed(withOld, '')?.connectionId).toBeNull()
  })

  it('is written as nothing for this server and the id for a connected one', () => {
    expect(kanbanServerParamValue(servers[0])).toBeNull()
    expect(kanbanServerParamValue(servers[1])).toBe(LAPTOP_ID)
  })
})

describe('what the page keeps and links per server', () => {
  const [local, laptop] = kanbanServers(thisServer, [view()])

  it("keeps this server's last board under the key the settings page reads, and another's apart", () => {
    expect(kanbanLastBoardStorageKey(local)).toBe(KANBAN_LAST_BOARD_STORAGE_KEY)
    expect(kanbanLastBoardStorageKey(laptop)).toBe(`${KANBAN_LAST_BOARD_STORAGE_KEY}:${LAPTOP_ID}`)
  })

  it("opens a card's session on the card's own server", () => {
    expect(kanbanSessionHref('/chat', local, 'br_1')).toBe('/chat?session=br_1')
    expect(kanbanSessionHref('/chat', laptop, 'br_1')).toBe('/chat?server=peer_connection_000001&server_session=br_1')
  })

  it("heads a server's boards by its label and what is wrong", () => {
    expect(serverBoardsHeading(laptop, { state: 'loaded', boards: [] })).toBe('laptop')
    expect(serverBoardsHeading(laptop, { state: 'failed', error: 'x' })).toBe('laptop — boards could not be read')
    expect(serverBoardsHeading(laptop, { state: 'blocked', reason: 'offline' })).toBe('laptop — offline')
  })
})
