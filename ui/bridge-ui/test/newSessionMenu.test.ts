import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { HarnessInfo, Instance, Machine } from '@kayushkin/llm-bridge-types'
import { BridgeContext, type BridgeConfig } from '../src/context'
import { NewSessionInstanceGroups } from '../src/components/chat/NewSessionMenu'
import {
  newSessionHarnessGroups,
  newSessionMenuServers,
  nextMenuItemIndex,
  sameMenuScope,
} from '../src/newSessionMenu'
import { peerConnectionBridgeConfig } from '../src/peers'
import type { ConnectedServerListing } from '../src/serverListings'
import { harnessesPoll, harnessMapOf } from '../src/useBridgeHarnesses'
import { instancesPoll } from '../src/useBridgeInstances'
import { machinesPoll } from '../src/useBridgeMachines'

const instance = (overrides: Partial<Instance>): Instance => ({
  id: 'inst-1',
  harness_type: 'claude_code',
  name: 'CC',
  machine_id: 'm-1',
  max_concurrent_sessions: 1,
  enabled: true,
  created_at: '2026-10-28T00:00:00Z',
  updated_at: '2026-10-28T00:00:00Z',
  ...overrides,
} as Instance)

const harness = (name: string, available: boolean): HarnessInfo =>
  ({ name, label: name, emoji: '', available, capabilities: [] } as HarnessInfo)

const folder = (connectionId: string, label: string, blockedReason: string | null = null): ConnectedServerListing => ({
  connectionId,
  label,
  labelFromServer: true,
  blockedReason,
  blockedWord: blockedReason ? 'offline' : null,
})

describe('the servers the New session menu lists', () => {
  const folders = [folder('peer_connection_000001', 'laptop'), folder('peer_connection_000002', 'desk', 'desk is not connected now.')]

  it('puts this server first under its own name, then every connected server in the sidebar order', () => {
    const servers = newSessionMenuServers('main', folders, { kind: 'all' })
    expect(servers.map(s => s.label)).toEqual(['main', 'laptop', 'desk'])
    expect(servers[0].connectionId).toBeNull()
    expect(newSessionMenuServers(null, [], { kind: 'all' }).map(s => s.label)).toEqual(['This server'])
  })

  it('keeps a server that cannot be read, with its reason, so it does not vanish while offline', () => {
    const desk = newSessionMenuServers('main', folders, { kind: 'all' }).find(s => s.label === 'desk')
    expect(desk?.blockedReason).toBe('desk is not connected now.')
  })

  it("shows only the server whose folder's + opened it", () => {
    expect(newSessionMenuServers('main', folders, { kind: 'this-server' }).map(s => s.label)).toEqual(['main'])
    expect(newSessionMenuServers('main', folders, { kind: 'connected-server', connectionId: 'peer_connection_000001' }).map(s => s.label)).toEqual(['laptop'])
    expect(newSessionMenuServers('main', folders, { kind: 'connected-server', connectionId: 'gone' })).toEqual([])
  })

  it('tells one folder scope from another', () => {
    expect(sameMenuScope({ kind: 'this-server' }, { kind: 'this-server' })).toBe(true)
    expect(sameMenuScope({ kind: 'connected-server', connectionId: 'a' }, { kind: 'connected-server', connectionId: 'b' })).toBe(false)
    expect(sameMenuScope({ kind: 'all' }, { kind: 'this-server' })).toBe(false)
  })
})

describe("one server's instances", () => {
  it('groups enabled instances by harness, ordered by name, with availability from that registry', () => {
    const groups = newSessionHarnessGroups(
      [
        instance({ id: 'a', harness_type: 'codex', name: 'Codex' }),
        instance({ id: 'b', harness_type: 'claude_code', name: 'CC' }),
        instance({ id: 'c', harness_type: 'claude_code', name: 'Off', enabled: false }),
        instance({ id: 'd', harness_type: 'aider', name: 'Aider' }),
      ],
      harnessMapOf([harness('codex', true), harness('claude_code', false)]),
    )
    expect(groups.map(g => [g.harness, g.available, g.instances.map(i => i.id)])).toEqual([
      ['aider', false, ['d']],
      ['claude_code', false, ['b']],
      ['codex', true, ['a']],
    ])
  })
})

describe('arrow keys', () => {
  it('move through the items, wrapping, and jump with Home and End', () => {
    expect(nextMenuItemIndex(-1, 3, 'ArrowDown')).toBe(0)
    expect(nextMenuItemIndex(2, 3, 'ArrowDown')).toBe(0)
    expect(nextMenuItemIndex(0, 3, 'ArrowUp')).toBe(2)
    expect(nextMenuItemIndex(-1, 3, 'ArrowUp')).toBe(2)
    expect(nextMenuItemIndex(1, 3, 'Home')).toBe(0)
    expect(nextMenuItemIndex(1, 3, 'End')).toBe(2)
    expect(nextMenuItemIndex(-1, 0, 'ArrowDown')).toBe(-1)
  })
})

describe("a connected server's instances in the menu", () => {
  it("are read through that server's proxy, with availability from its own harness registry, not this host's", async () => {
    const requested: string[] = []
    const answers: Record<string, unknown> = {
      '/api/bridge/instances': [instance({ id: 'local-cc', name: 'Local CC', machine_id: 'm-local' })],
      '/api/bridge/harnesses': [harness('claude_code', true)],
      '/api/bridge/machines': [{ id: 'm-local', name: 'here', transport: 'local' } as Machine],
      '/api/bridge/peer-connections/peer_connection_000001/proxy/instances': [
        instance({ id: 'remote-cc', name: 'Laptop CC', machine_id: 'm-laptop' }),
        instance({ id: 'remote-codex', harness_type: 'codex', name: 'Laptop Codex', machine_id: 'm-laptop' }),
      ],
      '/api/bridge/peer-connections/peer_connection_000001/proxy/harnesses': [harness('claude_code', false), harness('codex', true)],
      '/api/bridge/peer-connections/peer_connection_000001/proxy/machines': [{ id: 'm-laptop', name: 'laptop-wsl', transport: 'local' } as Machine],
    }
    const fetchFn = (async (url: string) => {
      requested.push(url)
      const body = answers[url]
      return body === undefined ? new Response('not here', { status: 404 }) : new Response(JSON.stringify(body), { status: 200 })
    }) as unknown as BridgeConfig['fetch']
    const local = { fetch: fetchFn, basePath: '/api/bridge', routes: {} } as unknown as BridgeConfig
    const remote = peerConnectionBridgeConfig(local, 'peer_connection_000001')

    for (const base of [local.basePath, remote.basePath]) {
      await instancesPoll(fetchFn, base).refresh()
      await harnessesPoll(fetchFn, base).refresh()
      await machinesPoll(fetchFn, base).refresh()
    }

    const html = renderToStaticMarkup(
      createElement(BridgeContext.Provider, { value: remote }, createElement(NewSessionInstanceGroups, { onPick: () => {} })),
    )
    expect(html).toContain('Laptop CC')
    expect(html).toContain('Laptop Codex')
    expect(html).toContain('laptop-wsl')
    expect(html).not.toContain('Local CC')
    // The laptop's registry says claude_code is down and codex is up; this host's says the opposite for claude_code.
    const buttons = html.split('<button').slice(1).map(part => part.slice(0, part.indexOf('</button>')))
    const ccButton = buttons.find(b => b.includes('>Laptop CC<')) ?? ''
    const codexButton = buttons.find(b => b.includes('>Laptop Codex<')) ?? ''
    expect(ccButton).toContain('disabled')
    expect(ccButton).toContain('bc-new-session-avail-off')
    expect(codexButton).not.toContain('disabled')
    expect(codexButton).toContain('bc-new-session-avail-on')
    expect(requested.filter(url => url.includes('/proxy/')).sort()).toEqual([
      '/api/bridge/peer-connections/peer_connection_000001/proxy/harnesses',
      '/api/bridge/peer-connections/peer_connection_000001/proxy/instances',
      '/api/bridge/peer-connections/peer_connection_000001/proxy/machines',
    ])
  })
})
