// What the chat's New session menu lists: one heading per server, this
// server first, then every server it is connected to, and under each the
// instances that server would start a session on, grouped by harness.
//
// A connected server's instances, machines and harness registry are its own,
// read through this server's proxy for the connection; nothing here mixes
// them with this server's. The pure rules the menu draws from.

import type { HarnessInfo, Instance } from '@kayushkin/llm-bridge-types'
import { thisServerLabel, type ConnectedServerListing } from './serverListings'

/** Which servers the menu shows: every one (the New button's caret), or one
 *  (the + on that server's folder in the sidebar). */
export type NewSessionMenuScope =
  | { kind: 'all' }
  | { kind: 'this-server' }
  | { kind: 'connected-server'; connectionId: string }

export interface NewSessionMenuServer {
  /** Stable key: `this-server`, or the connection id. */
  key: string
  label: string
  /** Null for this server. */
  connectionId: string | null
  /** Why nothing can be started there now, or null. */
  blockedReason: string | null
}

export const THIS_SERVER_MENU_KEY = 'this-server'

/** The servers under the menu's headings, in order: this server first, then
 *  the connected servers in the sidebar's order. A server that cannot be
 *  read stays listed with its reason, so it does not vanish from the menu
 *  while it is offline. */
export function newSessionMenuServers(
  thisServerName: string | null,
  connectedServers: readonly ConnectedServerListing[],
  scope: NewSessionMenuScope,
): NewSessionMenuServer[] {
  const thisServer: NewSessionMenuServer = {
    key: THIS_SERVER_MENU_KEY,
    label: thisServerLabel(thisServerName),
    connectionId: null,
    blockedReason: null,
  }
  const others: NewSessionMenuServer[] = connectedServers.map(folder => ({
    key: folder.connectionId,
    label: folder.label,
    connectionId: folder.connectionId,
    blockedReason: folder.blockedReason,
  }))
  switch (scope.kind) {
    case 'this-server':
      return [thisServer]
    case 'connected-server':
      return others.filter(server => server.connectionId === scope.connectionId)
    case 'all':
      return [thisServer, ...others]
  }
}

export interface NewSessionHarnessGroup {
  harness: string
  /** From the server's own harness registry. A harness the registry does not
   *  list cannot be reached there, so it is unavailable, never assumed on. */
  available: boolean
  instances: Instance[]
}

/** One server's enabled instances, grouped by harness and ordered by harness
 *  name. */
export function newSessionHarnessGroups(
  instances: readonly Instance[],
  harnessMap: ReadonlyMap<string, HarnessInfo>,
): NewSessionHarnessGroup[] {
  const byHarness = new Map<string, Instance[]>()
  for (const instance of instances) {
    if (!instance.enabled) continue
    const list = byHarness.get(instance.harness_type) ?? []
    list.push(instance)
    byHarness.set(instance.harness_type, list)
  }
  return [...byHarness.entries()]
    .map(([harness, list]) => ({ harness, available: harnessMap.get(harness)?.available ?? false, instances: list }))
    .sort((a, b) => a.harness.localeCompare(b.harness))
}

/** The item the arrow keys move to: the next or previous one, wrapping, or
 *  the first or last for Home and End. -1 when there is none. */
export function nextMenuItemIndex(current: number, count: number, key: string): number {
  if (count === 0) return -1
  switch (key) {
    case 'ArrowDown': return current < 0 ? 0 : (current + 1) % count
    case 'ArrowUp': return current < 0 ? count - 1 : (current - 1 + count) % count
    case 'Home': return 0
    case 'End': return count - 1
    default: return current
  }
}

/** Whether two scopes name the same servers: which folder's menu is open. */
export function sameMenuScope(a: NewSessionMenuScope, b: NewSessionMenuScope): boolean {
  if (a.kind !== b.kind) return false
  return a.kind !== 'connected-server' || a.connectionId === (b as { connectionId: string }).connectionId
}

/** Marks a button that opens and closes the menu. A press on it is not "outside
 *  the menu": closing there and then toggling on the click would open the menu
 *  again instead of closing it. */
export const NEW_SESSION_MENU_TOGGLE_ATTRIBUTE = 'data-new-session-menu-toggle'
