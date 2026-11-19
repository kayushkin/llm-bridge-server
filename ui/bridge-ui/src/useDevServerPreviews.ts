import { useSyncExternalStore } from 'react'
import { useBridgeConfig } from './context'
import type { FetchFn } from './types'
import type { DevServerPreview, DevServerPreviewList } from './devServerPreviews'

const POLL_INTERVAL_MS = 5000

export interface DevServerPreviewsState {
  previews: DevServerPreview[]
  /** Agent processes the host could not look inside; see DevServerPreviewList. */
  unreadableProcessIds: number[]
  /** The host's refusal, verbatim, or the network failure. */
  error: string | null
  /** False until the first answer, so a page can tell "none" from "not yet". */
  loaded: boolean
}

const NOT_LOADED: DevServerPreviewsState = { previews: [], unreadableProcessIds: [], error: null, loaded: false }

// One poll per list, however many components read it: every Bash row in the
// chat asks, and a request per row every few seconds would be dozens.
class DevServerPreviewsPoller {
  private state: DevServerPreviewsState = NOT_LOADED
  private readonly listeners = new Set<() => void>()
  private timer: ReturnType<typeof setInterval> | null = null

  private readonly fetchFn: FetchFn
  private readonly url: string

  constructor(fetchFn: FetchFn, url: string) {
    this.fetchFn = fetchFn
    this.url = url
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    if (this.listeners.size === 1) {
      void this.poll()
      this.timer = setInterval(() => void this.poll(), POLL_INTERVAL_MS)
    }
    return () => {
      this.listeners.delete(listener)
      if (this.listeners.size === 0 && this.timer) {
        clearInterval(this.timer)
        this.timer = null
      }
    }
  }

  snapshot = (): DevServerPreviewsState => this.state

  private async poll(): Promise<void> {
    if (typeof document !== 'undefined' && document.hidden && this.state.loaded) return
    let next: DevServerPreviewsState
    try {
      const response = await this.fetchFn(this.url)
      if (!response.ok) {
        next = { ...this.state, error: `${response.status}: ${(await response.text()).trim()}`, loaded: true }
      } else {
        const body = (await response.json()) as DevServerPreviewList
        next = { previews: body.previews, unreadableProcessIds: body.unreadable_process_ids, error: null, loaded: true }
      }
    } catch (error) {
      next = { ...this.state, error: String(error), loaded: true }
    }
    this.state = next
    for (const listener of this.listeners) listener()
  }
}

const pollers = new WeakMap<FetchFn, Map<string, DevServerPreviewsPoller>>()

function pollerFor(fetchFn: FetchFn, url: string): DevServerPreviewsPoller {
  let byUrl = pollers.get(fetchFn)
  if (!byUrl) {
    byUrl = new Map()
    pollers.set(fetchFn, byUrl)
  }
  let poller = byUrl.get(url)
  if (!poller) {
    poller = new DevServerPreviewsPoller(fetchFn, url)
    byUrl.set(url, poller)
  }
  return poller
}

const subscribeToNothing = () => () => {}
const notLoaded = () => NOT_LOADED

/** The ports agents' processes listen on, refreshed every few seconds while
 *  anything reads it. Empty, and no request made, when the host carries no
 *  preview list (`previewsBasePath` empty) or `enabled` is false. */
export function useDevServerPreviews(enabled = true): DevServerPreviewsState {
  const { fetch: fetchFn, previewsBasePath } = useBridgeConfig()
  const poller = enabled && previewsBasePath ? pollerFor(fetchFn, previewsBasePath) : null
  return useSyncExternalStore(poller?.subscribe ?? subscribeToNothing, poller?.snapshot ?? notLoaded, notLoaded)
}
