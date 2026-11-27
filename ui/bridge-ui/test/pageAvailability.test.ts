import { describe, expect, it } from 'vitest'
import type { BridgeConfig } from '../src/context'
import { BRIDGE_PAGE_AVAILABILITY, bridgePageAvailableWithoutShellFlags } from '../src/pageAvailability'
import { BRIDGE_PAGES } from '../src/pages'

describe('page availability', () => {
  it('is the one rule the navigation reads', () => {
    for (const page of BRIDGE_PAGES) expect(page.available).toBe(BRIDGE_PAGE_AVAILABILITY[page.route])
  })
  it('counts a page whose store the host does not carry as unavailable', () => {
    const config = { kanbanStoreBasePath: '/issues', principalStoreBasePath: '', multichatBasePath: '' } as BridgeConfig
    expect(bridgePageAvailableWithoutShellFlags('kanban', config)).toBe(true)
    expect(bridgePageAvailableWithoutShellFlags('principals', config)).toBe(false)
    expect(bridgePageAvailableWithoutShellFlags('messageWaiting', config)).toBe(false)
    expect(bridgePageAvailableWithoutShellFlags('instances', config)).toBe(true)
  })
})
