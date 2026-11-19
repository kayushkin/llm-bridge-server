import { useMemo } from 'react'
import { useBridgeConfig } from './context'
import { SharedPoll, loadJSONList, sharedPoll, useSharedPoll } from './sharedPoll'
import type { FetchFn, HarnessInfo } from './types'

/** The one `/harnesses` poll for this (fetch, basePath) — the same store the
 *  instances and machines hooks use, keyed on a different URL.
 *
 *  Exported for the render checks, which have no way to reach the key otherwise:
 *  this function is the copy-paste of the instances hook, and a key copied along
 *  with it would silently serve the instances answer here. Not part of the public
 *  API — `index.ts` exports only the hook. */
export function harnessesPoll(fetchFn: FetchFn, basePath: string): SharedPoll<HarnessInfo[]> {
  return sharedPoll(fetchFn, `harnesses ${basePath}`, () =>
    new SharedPoll<HarnessInfo[]>(() => loadJSONList<HarnessInfo>(fetchFn, `${basePath}/harnesses`), []))
}

// The registered harnesses, as a list and as a `name → HarnessInfo` map.
//
// This lived in dash as a page-local hook, and inside this library as five
// separate one-shot fetches — one in each component that needed a harness's
// label, emoji, logo or availability. Both copies existed because there was no
// shared hook; there is now, and there is one poll behind it however many
// components ask.
//
// `basePath` is returned alongside because a HarnessInfo's `image` is
// server-relative: rendering the logo means `${basePath}${h.image}`, and a
// caller that only has the map cannot build that.
//
// Single source of truth: label, emoji, image, tint and availability all come
// from the server's HarnessInfo. There is no client-side fallback map. A
// harness the server has not registered simply has no entry, and callers fall
// through to whatever the session itself reports.
export function useBridgeHarnesses() {
  const { fetch: fetchFn, basePath } = useBridgeConfig()

  const poll = useMemo(() => harnessesPoll(fetchFn, basePath), [fetchFn, basePath])
  const { data: harnesses, loading, error } = useSharedPoll(poll)
  const refresh = poll.refresh

  const harnessMap = useMemo(() => harnessMapOf(harnesses), [harnesses])

  return useMemo(
    () => ({ harnesses, harnessMap, basePath, loading, error, refresh }),
    [harnesses, harnessMap, basePath, loading, error, refresh],
  )
}

/** The list as a `name → HarnessInfo` map. `name` is the id the rest of the
 *  system joins on: a session names its harness, an instance names its
 *  `harness_type`, and both look the row up here. */
export function harnessMapOf(harnesses: readonly HarnessInfo[]): Map<string, HarnessInfo> {
  const m = new Map<string, HarnessInfo>()
  for (const h of harnesses) m.set(h.name, h)
  return m
}

/** Identity of the harness *set*, for a caller that must react to a harness
 *  appearing or disappearing but not to one merely changing state.
 *
 *  Now that `/harnesses` is polled rather than fetched once, a harness coming
 *  back up delivers a new list on the next tick. A component that seeds editable
 *  state from that list — the settings form does — would reseed on that tick and
 *  discard whatever the user had typed and not yet saved. Keying its effect on
 *  this string instead of the list means only a real membership change reseeds.
 *
 *  JSON rather than a joined string because the key has to be injective: any
 *  separator can also appear inside a name, and two names that ran together
 *  would read as one, hiding exactly the membership change this exists to catch.
 *  Read it back with `harnessNamesFromKey`. */
export function harnessNameKey(harnesses: readonly HarnessInfo[]): string {
  return JSON.stringify(harnesses.map(h => h.name))
}

/** The names that went into `harnessNameKey`. */
export function harnessNamesFromKey(key: string): string[] {
  return JSON.parse(key) as string[]
}
