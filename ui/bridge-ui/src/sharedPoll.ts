import { useSyncExternalStore } from 'react'
import { sharedInstance } from './sharedInstance'
import type { FetchFn } from './types'

// A poll of one URL, shared by every component that asks for it.
//
// The hooks in this library used to each own their `useState` + `setInterval`,
// so a page that calls `useBridgeInstances()` from N components runs N
// 30-second polls of `/instances` and holds N copies of the answer. dash's
// dash's chat page does exactly that: four call sites — `Workspace` through
// `useNewSessionTarget`, `Sidebar`, `SessionHeader`, and `NewSessionMenu` while
// the picker is open — and three for its sibling `useBridgeMachines()`, which
// `NewSessionMenu`, `SessionHeader` and `Sidebar` share. Nothing was wrong with
// any single caller; the cost only shows up when you count the timers on the
// page.
//
// SharedPoll moves the timer and the snapshot out of the component. The first
// subscriber starts the poll, the rest attach to the answer it already has,
// and the last one to leave stops the timer. N callers cost one request.
//
// Deliberately not a cache library. It keeps the exact behavior the hooks had
// — `loading` means "the first attempt has not settled yet" and stays false
// afterwards, a failed refresh keeps the last good data and sets `error`, and
// an unchanged payload keeps the previous array so React can bail out of the
// re-render. What changes is only how many sockets that costs.
//
// One rule sits on top: a request that never got an answer is retried within
// seconds and reported only when the retries fail too (`LoadFailure`), and the
// store asks again as soon as the browser comes back online or the page comes
// back into view. A refusal from the service is still reported at once.

export const POLL_INTERVAL_MS = 30000

/** How soon to ask again after a load that never got an answer, one delay per
 *  retry. A phone on mobile data drops a request now and then, and a deploy
 *  restarts a service for a few seconds; both mend themselves within seconds,
 *  so the next try should not wait for the next 30-second tick. Measured
 *  2026-11-01: a phone missed one peer-messages read, and the chat showed
 *  "Failed to fetch" in red until the tick 30 seconds later, while the server
 *  had answered every other request. */
export const UNREACHABLE_RETRY_DELAYS_MS: readonly number[] = [1000, 2000, 4000]

export interface PollSnapshot<T> {
  readonly data: T
  readonly loading: boolean
  readonly error: string | null
}

/** How a load failed, which decides when a subscriber hears of it.
 *  - `unreachable`: this host's server never answered. The browser lost the
 *    request (fetch threw), or the web server in front of the bridge answered
 *    502, 503 or 504 with its own error page because the service behind it was
 *    down or restarting. The store retries at once and reports it only once
 *    every retry has failed too. Retrying costs only this host's own work.
 *  - `refused`: this host's server answered. That covers a refusal, an
 *    unreadable body, and a JSON 503 such as `peer_not_connected`: that is the
 *    server reporting the state of something it watches, a connected server
 *    for one, and the browser shows it rather than asking again. Calls to other
 *    machines are the server's to pace, never the browser's. */
export type LoadFailure = 'unreachable' | 'refused'

/** What one attempt to load the resource produced. A load reports failure as a
 *  value rather than throwing so the store never has to guess how to render an
 *  exception, and so each caller keeps its own error wording. */
export type LoadResult<T> = { ok: true; value: T } | { ok: false; error: string; failure: LoadFailure }

/** Gateway statuses that can mean the service behind the gateway did not answer. */
const GATEWAY_STATUSES: ReadonlySet<number> = new Set([502, 503, 504])

/** The failure kind for a response that was not ok. A gateway status counts
 *  as unreachable only when the body is not JSON: every service here answers
 *  its errors in JSON, and the web server's own error page is HTML or empty. */
export function failureOfResponse(res: Pick<Response, 'status' | 'headers'>): LoadFailure {
  if (!GATEWAY_STATUSES.has(res.status)) return 'refused'
  const contentType = res.headers?.get('Content-Type') ?? ''
  return contentType.includes('json') ? 'refused' : 'unreachable'
}

/** The text a subscriber sees once an unreachable service has stayed
 *  unreachable through every retry: since when, that the store is still
 *  trying, and the browser's own words. */
export function unreachableErrorText(since: Date, error: string): string {
  const time = since.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' })
  return `no answer since ${time}, still trying (${error})`
}

type Listener = () => void

export class SharedPoll<T> {
  readonly intervalMs: number

  private readonly load: () => Promise<LoadResult<T>>
  private readonly listeners = new Set<Listener>()
  /** Counted separately from `listeners`, because a Set collapses two
   *  subscribers that happen to pass the same function and the timer would
   *  then stop while one of them is still watching. */
  private subscribers = 0
  private snapshot: PollSnapshot<T>
  /** Serialized last payload, so an unchanged answer keeps the old identity. */
  private lastJSON = ''
  private timer: ReturnType<typeof setInterval> | null = null
  private inFlight: Promise<void> | null = null
  private readonly retryDelaysMs: readonly number[]
  /** Unreachable loads in a row, and when the first of them failed. */
  private unreachableInARow = 0
  private unreachableSince: Date | null = null
  private retryTimer: ReturnType<typeof setTimeout> | null = null

  constructor(
    load: () => Promise<LoadResult<T>>,
    initial: T,
    intervalMs: number = POLL_INTERVAL_MS,
    retryDelaysMs: readonly number[] = UNREACHABLE_RETRY_DELAYS_MS,
  ) {
    this.load = load
    this.intervalMs = intervalMs
    this.retryDelaysMs = retryDelaysMs
    this.snapshot = { data: initial, loading: true, error: null }
  }

  getSnapshot = (): PollSnapshot<T> => this.snapshot

  /** True while a timer is running, i.e. while somebody is watching. */
  get polling(): boolean {
    return this.timer !== null
  }

  get subscriberCount(): number {
    return this.subscribers
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener)
    this.subscribers++
    if (this.subscribers === 1) this.start()
    let live = true
    return () => {
      // React can call an unsubscribe twice; only the first call counts, or the
      // count goes negative and the timer outlives its last subscriber.
      if (!live) return
      live = false
      this.listeners.delete(listener)
      this.subscribers--
      if (this.subscribers === 0) this.stop()
    }
  }

  /** Load now. Concurrent calls share one request — three components each
   *  awaiting a refresh after a write used to mean three GETs. */
  refresh = (): Promise<void> => {
    if (this.inFlight) return this.inFlight
    const attempt = this.run().finally(() => {
      if (this.inFlight === attempt) this.inFlight = null
    })
    this.inFlight = attempt
    return attempt
  }

  /** The browser says the network is back, or the page is shown again after
   *  the phone put it away. Either way the last answer may be old or a failure
   *  that has since mended, so ask now rather than at the next tick. */
  private readonly refreshOnReturn = (): void => {
    if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return
    void this.refresh()
  }

  private start(): void {
    void this.refresh()
    if (this.timer === null) {
      this.timer = setInterval(() => { void this.refresh() }, this.intervalMs)
      if (typeof window !== 'undefined') window.addEventListener('online', this.refreshOnReturn)
      if (typeof document !== 'undefined') document.addEventListener('visibilitychange', this.refreshOnReturn)
    }
  }

  private stop(): void {
    if (this.timer !== null) {
      clearInterval(this.timer)
      this.timer = null
      if (typeof window !== 'undefined') window.removeEventListener('online', this.refreshOnReturn)
      if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', this.refreshOnReturn)
    }
    this.cancelRetry()
    // The snapshot is kept on purpose. A component that unmounts and comes back
    // — a tab switch, a route change — renders the last answer straight away
    // instead of flashing an empty list, and `start` refreshes it in the same
    // breath. The store is one object per URL, so holding it costs nothing.
  }

  private async run(): Promise<void> {
    let result: LoadResult<T>
    try {
      result = await this.load()
    } catch (err) {
      // `load` is meant to report failure as a value. If one ever throws
      // anyway, that must not become an unhandled rejection on the timer.
      result = { ok: false, error: `${err}`, failure: 'refused' }
    }
    if (!result.ok && result.failure === 'unreachable') {
      this.unreachableInARow++
      if (this.unreachableSince === null) this.unreachableSince = new Date()
      const delay = this.retryDelaysMs[this.unreachableInARow - 1]
      if (delay !== undefined) {
        // Keep quiet and ask again soon: most of these mend before anyone
        // could read a message about them. The last good data stays on screen.
        // Still settle `loading`, so a first load that failed waits as an empty
        // list rather than a spinner.
        this.scheduleRetry(delay)
        this.emit({ data: this.snapshot.data, loading: false, error: this.snapshot.error })
        return
      }
      this.emit({ data: this.snapshot.data, loading: false, error: unreachableErrorText(this.unreachableSince, result.error) })
      return
    }
    this.unreachableInARow = 0
    this.unreachableSince = null
    this.cancelRetry()
    if (!result.ok) {
      this.emit({ data: this.snapshot.data, loading: false, error: result.error })
      return
    }
    const json = JSON.stringify(result.value)
    if (json === this.lastJSON) {
      this.emit({ data: this.snapshot.data, loading: false, error: null })
      return
    }
    this.lastJSON = json
    this.emit({ data: result.value, loading: false, error: null })
  }

  private scheduleRetry(delayMs: number): void {
    this.cancelRetry()
    // Nobody watching means nobody to retry for.
    if (this.subscribers === 0) return
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      void this.refresh()
    }, delayMs)
  }

  private cancelRetry(): void {
    if (this.retryTimer !== null) {
      clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
  }

  private emit(next: PollSnapshot<T>): void {
    const current = this.snapshot
    if (current.data === next.data && current.loading === next.loading && current.error === next.error) {
      // Nothing a caller can see has changed. Publishing a fresh object here
      // would re-render every subscriber on every tick, which is the cost this
      // whole file exists to remove.
      return
    }
    this.snapshot = next
    for (const listener of [...this.listeners]) listener()
  }
}

/** One store per (auth'd fetch, URL). The table itself is `sharedInstance`,
 *  which the prefs record shares — see that file for why it is keyed on the
 *  fetch function rather than the URL alone. */
export function sharedPoll<T>(owner: object, key: string, create: () => SharedPoll<T>): SharedPoll<T> {
  return sharedInstance(owner, key, create)
}

/** Subscribe to a shared poll for as long as the component is mounted. */
export function useSharedPoll<T>(poll: SharedPoll<T>): PollSnapshot<T> {
  return useSyncExternalStore(poll.subscribe, poll.getSnapshot, poll.getSnapshot)
}

/** GET a JSON array, with the error wording the hooks have always used:
 *  `HTTP <status>` for a refused request and the stringified throw otherwise.
 *  A fetch that throws never reached the service; a body that will not parse
 *  did, and is refused. */
export async function loadJSONList<T>(fetchFn: FetchFn, url: string): Promise<LoadResult<T[]>> {
  let res: Response
  try {
    res = await fetchFn(url)
  } catch (err) {
    return { ok: false, error: `${err}`, failure: 'unreachable' }
  }
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}`, failure: failureOfResponse(res) }
  try {
    return { ok: true, value: ((await res.json()) as T[] | null) ?? [] }
  } catch (err) {
    return { ok: false, error: `${err}`, failure: 'refused' }
  }
}
