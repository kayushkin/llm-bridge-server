import { createContext, useContext } from 'react'
import type { NavEntry, PageGroup } from '../pages'

/** The shell's navigation, as `BridgeLayout` worked it out: the groups that have a
 *  page, and every listed page. `BridgeLayout` draws it as its two rows; when the
 *  chat's minimal chrome takes the screen over and those rows are gone, the chat's
 *  session drawer draws the same entries, so a phone can still leave the chat. */
export interface ShellNavigation {
  groups: readonly PageGroup[]
  entries: readonly NavEntry[]
}

export const ShellNavigationContext = createContext<ShellNavigation | null>(null)

/** Null outside a `BridgeLayout` — a host that mounts the chat on its own. */
export function useShellNavigation(): ShellNavigation | null {
  return useContext(ShellNavigationContext)
}
