import type { FilterState, SessionSummary } from '@kayushkin/chat-core'

// The chat sidebar's server tabs, and the one list it shows when they are off.
//
// With a connected server, the sidebar either splits into a tab per server — this
// server's first — or shows every server's sessions in one list, newest first, each
// row from another server tagged with that server's name. One search box and one
// set of filters sit above both, so the filter that this server's store holds is
// copied to each connected server's store.

/** A session in the one list: this server's, or a connected server's. */
export type MergedSessionEntry =
  | { server: 'this'; session: SessionSummary }
  | { server: 'connected'; connectionId: string; serverLabel: string; session: SessionSummary }

/** Every server's sessions in one list, the newest activity first. Ties keep
 *  the order they came in, so a list that has not changed does not reshuffle. */
export function mergeSessionsNewestFirst(entries: readonly MergedSessionEntry[]): MergedSessionEntry[] {
  return entries
    .map((entry, index) => ({ entry, index, at: Date.parse(entry.session.updatedAt) }))
    .sort((a, b) => {
      // A time that does not parse sorts last rather than poisoning the order.
      const aAt = Number.isNaN(a.at) ? -Infinity : a.at
      const bAt = Number.isNaN(b.at) ? -Infinity : b.at
      return bAt - aAt || a.index - b.index
    })
    .map(({ entry }) => entry)
}

/** The heading a session's local day goes under: "Today", "Yesterday", or the
 *  date. `now` is passed in so the rule can be tested. */
export function sessionDayHeading(updatedAt: string, now: Date): string {
  const at = new Date(updatedAt)
  if (Number.isNaN(at.getTime())) return 'Unknown day'
  const startOfDay = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
  const days = Math.round((startOfDay(now) - startOfDay(at)) / 86_400_000)
  if (days === 0) return 'Today'
  if (days === 1) return 'Yesterday'
  return at.toLocaleDateString(undefined, {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    ...(at.getFullYear() === now.getFullYear() ? {} : { year: 'numeric' }),
  })
}

/** The tab to show: the stored one while its server is still listed, else this
 *  server's. A server that went away is not remembered as open. */
export function shownServerTab(storedKey: string, thisServerKey: string, connectionIds: readonly string[]): string {
  return storedKey === thisServerKey || connectionIds.includes(storedKey) ? storedKey : thisServerKey
}

/** The axes copied from this server's filter to a connected server's. `machine`
 *  is not one: its values are this server's instance ids, which mean nothing
 *  there. `folder` is not either: folders are each server's own. */
export const MIRRORED_FILTER_AXES = ['harness', 'status', 'type', 'purpose', 'mode'] as const

/** What to set on a connected server's filter so it matches this server's on
 *  the search and the mirrored axes, or null when it already does. */
export function mirroredFilterPatch(from: FilterState, to: FilterState): Partial<FilterState> | null {
  const patch: Partial<FilterState> = {}
  if (from.search !== to.search) patch.search = from.search
  for (const axis of MIRRORED_FILTER_AXES) {
    const a = from[axis]
    const b = to[axis]
    if (a.length !== b.length || a.some((value, i) => value !== b[i])) patch[axis] = [...a]
  }
  return Object.keys(patch).length > 0 ? patch : null
}
