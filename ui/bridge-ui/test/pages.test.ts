import { describe, expect, it } from 'vitest'
import { DEFAULT_BRIDGE_ROUTES, type BridgeConfig } from '../src/context'
import { BRIDGE_PAGES, PAGE_GROUPS, bridgePagesShown, groupForPath, navEntriesFor, type HostPage } from '../src/pages'

const config = (overrides: Partial<BridgeConfig> = {}): BridgeConfig => ({
  fetch: async () => new Response(),
  basePath: '/api/bridge',
  skillStoreBasePath: '', toolStoreBasePath: '', permissionStoreBasePath: '', kanbanStoreBasePath: '',
  principalStoreBasePath: '', grantStoreBasePath: '', bundleStoreBasePath: '', repoStoreBasePath: '',
  mailBasePath: '', mailPagePath: '', noteboardBasePath: '', bridgeAdapterBasePath: '',
  orchestratorBasePath: '', usageStoreBasePath: '',
  schedulerBasePath: '', logStoreBasePath: '', jobStoreBasePath: '', quoteStoreBasePath: '',
  predictionStoreBasePath: '', eventStoreBasePath: '', authStoreBasePath: '', hostBasePath: '', previewsBasePath: '', workGraphStoreBasePath: '', projectStoreBasePath: '', modelStoreBasePath: '',
  routes: DEFAULT_BRIDGE_ROUTES,
  ...overrides,
} as BridgeConfig)
const flags = { showConformance: true, showServiceInventory: true }

describe('the page registry', () => {
  it('names every route this library ships exactly once', () => {
    const routed = BRIDGE_PAGES.map(p => p.route).sort()
    const shipped = Object.keys(DEFAULT_BRIDGE_ROUTES).filter(k => k !== 'notes').sort()
    expect(routed).toEqual(shipped)
  })

  it('puts every page in a known group', () => {
    const known = new Set(PAGE_GROUPS.map(g => g.key))
    for (const p of BRIDGE_PAGES) expect(known.has(p.group), p.route).toBe(true)
  })
})

describe('navEntriesFor', () => {
  it('lists a store-backed page only when the host proxies the store', () => {
    const without = navEntriesFor(config(), flags).map(e => e.label)
    expect(without).not.toContain('Skills')
    expect(without).not.toContain('Kanban')
    const withStores = navEntriesFor(config({ skillStoreBasePath: '/api/skill-store', kanbanStoreBasePath: '/api/kanban' }), flags).map(e => e.label)
    expect(withStores).toContain('Skills')
    expect(withStores).toContain('Kanban')
  })

  it('honors the two shell flags', () => {
    const labels = navEntriesFor(config(), { showConformance: false, showServiceInventory: false }).map(e => e.label)
    expect(labels).not.toContain('Conformance')
    expect(labels).not.toContain('Service inventory')
  })

  it('never lists a sub-page', () => {
    const tos = navEntriesFor(config({ kanbanStoreBasePath: '/api/kanban' }), flags).map(e => e.to)
    expect(tos).not.toContain('/kanban/settings')
    expect(tos).not.toContain('/card')
  })

  it("appends the host's pages after its own, in the host's order", () => {
    const host: HostPage[] = [
      { path: '/calendar', label: 'Calendar', group: 'personal', element: null },
      { path: '/services', label: 'Services', group: 'system', element: null },
    ]
    const entries = navEntriesFor(config(), flags, host)
    expect(entries.slice(-2).map(e => e.to)).toEqual(['/calendar', '/services'])
    expect(entries.find(e => e.to === '/services')?.group).toBe('system')
  })
})

describe('onlyBridgePages', () => {
  it('lists only the named pages, beside the host pages', () => {
    const host: HostPage[] = [{ path: '/raid', label: 'Raid', group: 'work', element: null }]
    const labels = navEntriesFor(config({ kanbanStoreBasePath: '/api/kanban' }), { ...flags, onlyBridgePages: ['chat', 'sessions'] }, host).map(e => e.label)
    expect(labels).toEqual(['Chat', 'Sessions', 'Raid'])
  })
  it('routes only the named pages, and every page when none are named', () => {
    expect(bridgePagesShown(['chat', 'card']).map(p => p.route)).toEqual(['chat', 'card'])
    expect(bridgePagesShown()).toBe(BRIDGE_PAGES)
  })
})

describe('groupForPath', () => {
  const host: HostPage[] = [{ path: '/calendar', label: 'Calendar', group: 'personal', element: null }]
  it.each([
    ['/', 'work'],
    ['/sessions', 'work'],
    ['/card/3042653c-340b-4d3a-b790-d600c690853f', 'work'],
    ['/kanban/settings?board=x'.split('?')[0], 'work'],
    ['/instances', 'agents'],
    ['/principals', 'access'],
    ['/service-inventory', 'system'],
    ['/calendar', 'personal'],
    ['/calendar/2026-09', 'personal'],
  ])('%s belongs to %s', (path, group) => {
    expect(groupForPath(path, DEFAULT_BRIDGE_ROUTES, host)).toBe(group)
  })

  it('answers null for a path no page owns, rather than guessing', () => {
    expect(groupForPath('/nowhere', DEFAULT_BRIDGE_ROUTES, host)).toBeNull()
  })

  it('does not let the root swallow every path', () => {
    expect(groupForPath('/instances', DEFAULT_BRIDGE_ROUTES)).toBe('agents')
  })
})

describe('host sub-pages', () => {
  const hostPages: HostPage[] = [
    { path: '/life', label: 'Life', group: 'personal', element: null, routeSuffix: '/*' },
    { path: '/day', label: 'Day', group: 'personal', element: null, listed: false, routeSuffix: '/*' },
  ]
  it('lists a host page unless it says it is not listed', () => {
    const labels = navEntriesFor(config(), flags, hostPages).map(e => e.label)
    expect(labels).toContain('Life')
    expect(labels).not.toContain('Day')
  })
  it('lights up the group of an unlisted host page', () => {
    expect(groupForPath('/day/2024-05-03', DEFAULT_BRIDGE_ROUTES, hostPages)).toBe('personal')
  })
})
