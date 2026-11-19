import { useCallback, useMemo } from 'react'
import { useBridgeConfig } from './context'
import { SharedPoll, loadJSONList, sharedPoll, useSharedPoll } from './sharedPoll'
import type { FetchFn } from './types'
import type { Principal } from '@kayushkin/principal-store-types'
import type { PrincipalKind } from './principalStoreClient'

// The principal directory: every principal principal-store knows, indexed by
// id, shared by every component on the page that needs to turn a
// `principal_000001` into a name.
//
// One request, not one per card. A board renders hundreds of tiles and each
// tile's assignee chips need the directory, so the fetch lives in a SharedPoll
// keyed on (fetch, base path): the first subscriber loads it, the rest attach
// to the answer. The directory changes when someone joins or leaves, which is
// rare, so the refresh interval is long — it exists so a principal created
// while a board is open eventually resolves, not to track anything live.
//
// Disabled principals are IN the directory on purpose (`include_disabled=true`).
// A card assigned to someone who has since left must still show who, struck
// through — and `pickablePrincipals` is what keeps them out of the picker.

export const PRINCIPALS_REFRESH_INTERVAL_MS = 10 * 60 * 1000

/** How many principals one read asks for. principal-store caps a page at this;
 *  a directory larger than it would need paging, which nothing here does yet. */
export const PRINCIPALS_PAGE_LIMIT = 500

export function principalsListURL(principalStoreBasePath: string): string {
  return `${principalStoreBasePath}/principals?include_disabled=true&limit=${PRINCIPALS_PAGE_LIMIT}`
}

export function principalIsDisabled(principal: Principal): boolean {
  return principal.disabled_at > 0
}

export function indexPrincipalsByID(list: Principal[]): Map<string, Principal> {
  return new Map(list.map(p => [p.id, p]))
}

/** The initials an avatar chip shows for a human: the first letter of the first
 *  two words of the display name. "Slava Kayushkin" → "SK", "Priya" → "P". */
export function principalInitials(displayName: string): string {
  const words = displayName.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return '?'
  return words.slice(0, 2).map(w => w[0]!.toUpperCase()).join('')
}

export type PrincipalKindFilter = 'all' | PrincipalKind

export interface PickablePrincipalsFilter {
  /** Case-insensitive substring of `display_name`. Empty matches everyone. */
  query: string
  kind: PrincipalKindFilter
  /** Ids to leave out — the card's current assignees, so the picker never
   *  offers someone who is already on the card. */
  excludeIDs: Iterable<string>
}

/** The principals a picker may offer. Disabled principals are never offered:
 *  they stay in the directory so existing assignments resolve, but assigning
 *  work to someone who left is the thing kanban-store's 400 exists to refuse,
 *  and a picker should not present a choice the server will reject. */
export function pickablePrincipals(list: Principal[], filter: PickablePrincipalsFilter): Principal[] {
  const excluded = new Set(filter.excludeIDs)
  const needle = filter.query.trim().toLowerCase()
  return list.filter(p => {
    if (principalIsDisabled(p)) return false
    if (excluded.has(p.id)) return false
    if (filter.kind !== 'all' && p.kind !== filter.kind) return false
    if (needle && !p.display_name.toLowerCase().includes(needle)) return false
    return true
  })
}

export interface PrincipalsDirectory {
  /** False when the host passed no `principalStoreBasePath`. Everything that
   *  renders an assignee hides itself on false; nothing was fetched. */
  enabled: boolean
  /** Every principal, disabled ones included, by id. */
  byId: Map<string, Principal>
  /** Every principal, disabled ones included. Filter through
   *  `pickablePrincipals` before offering any of them. */
  list: Principal[]
  /** True until the first attempt has settled, one way or the other. */
  loading: boolean
  /** The last load's failure, e.g. `HTTP 502`. Set while `list` is empty means
   *  the directory is UNKNOWN, not empty — render the error, never "nobody". A
   *  later failure keeps the last good list and sets this beside it. */
  error: string | null
  refresh: () => Promise<void>
}

function principalsPoll(fetchFn: FetchFn, principalStoreBasePath: string): SharedPoll<Principal[]> {
  return sharedPoll(fetchFn, `principals ${principalStoreBasePath}`, () =>
    new SharedPoll<Principal[]>(
      () => principalStoreBasePath
        ? loadJSONList<Principal>(fetchFn, principalsListURL(principalStoreBasePath))
        // No store configured: nothing to load, and `enabled` below says so.
        : Promise.resolve({ ok: true, value: [] }),
      [],
      PRINCIPALS_REFRESH_INTERVAL_MS,
    ))
}

/**
 * usePrincipals — the shared principal directory, for resolving assignee ids
 * to people and for the assignee picker.
 */
export function usePrincipals(): PrincipalsDirectory {
  const { fetch: fetchFn, principalStoreBasePath } = useBridgeConfig()
  const enabled = !!principalStoreBasePath
  const poll = principalsPoll(fetchFn, principalStoreBasePath)
  const snapshot = useSharedPoll(poll)
  const refresh = useCallback(() => poll.refresh(), [poll])
  return useMemo(() => ({
    enabled,
    byId: indexPrincipalsByID(snapshot.data),
    list: snapshot.data,
    loading: enabled && snapshot.loading,
    error: enabled ? snapshot.error : null,
    refresh,
  }), [enabled, snapshot, refresh])
}
