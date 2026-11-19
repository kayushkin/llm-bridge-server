import { useMemo, useSyncExternalStore } from 'react'
import { useBridgeConfig } from './context'
import { SharedPoll, sharedPoll } from './sharedPoll'
import type { PollSnapshot } from './sharedPoll'
import type { FetchFn, InstanceStatus } from './types'

// Reachability of one instance, for the machine chip's dot.
//
// `GET /instances/{id}/status` already aggregates local / SSH / runner
// liveness behind a single bool, so the chip needs nothing else. This lived as
// a private `useState` + `setInterval` inside the chat Workspace; dash's chat page needs
// the same answer for the same instance, and two copies of that effect would
// have meant two timers hitting one endpoint. Moving it onto SharedPoll makes
// N callers cost one request, the same trade `useBridgeInstances` already
// takes for `/instances`.

/** Faster than the 30s config polls: a machine going away is the thing this
 *  answer exists to report, so it is worth re-asking more often. Matches the
 *  interval the Workspace effect used. */
export const REACHABILITY_INTERVAL_MS = 15000

/** No instance to ask about — not the same as "asked and got no answer", but
 *  the chip renders both as the unknown dot. */
const UNKNOWN: PollSnapshot<boolean | null> = { data: null, loading: false, error: null }

const readUnknown = (): PollSnapshot<boolean | null> => UNKNOWN
const neverSubscribe = (): (() => void) => () => {}

/** The one status poll for this (fetch, basePath, instance). */
function reachabilityPoll(fetchFn: FetchFn, basePath: string, instanceId: string): SharedPoll<boolean | null> {
  return sharedPoll(fetchFn, `instance-status ${basePath} ${instanceId}`, () =>
    new SharedPoll<boolean | null>(async () => {
      // Every failure reports success-with-null rather than an error, and that
      // is deliberate. SharedPoll's error path keeps the last good value, so a
      // gateway that stops answering would leave a green dot on screen — the
      // exact stale "everything is fine" this chip exists to prevent. An
      // unanswered status read IS the answer: reachability unknown.
      try {
        const res = await fetchFn(`${basePath}/instances/${instanceId}/status`)
        if (!res.ok) return { ok: true, value: null }
        const data = (await res.json()) as InstanceStatus | null
        return { ok: true, value: Boolean(data?.reachable) }
      } catch {
        return { ok: true, value: null }
      }
    }, null, REACHABILITY_INTERVAL_MS))
}

/** Latest reachability for one instance, or null when it is unknown — no
 *  instance selected, or the status read did not answer.
 *
 *  Passing null/undefined starts no timer and issues no request, so a header
 *  with no session costs nothing. */
export function useInstanceReachable(instanceId: string | null | undefined): boolean | null {
  const { fetch: fetchFn, basePath } = useBridgeConfig()

  const poll = useMemo(
    () => (instanceId ? reachabilityPoll(fetchFn, basePath, instanceId) : null),
    [fetchFn, basePath, instanceId],
  )

  // Not `useSharedPoll`, because there may be no poll to subscribe to and a
  // hook cannot be called conditionally. Both fallbacks are module constants,
  // so their identity is stable and useSyncExternalStore does not resubscribe
  // on every render.
  const subscribe = poll ? poll.subscribe : neverSubscribe
  const getSnapshot = poll ? poll.getSnapshot : readUnknown

  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot).data
}
