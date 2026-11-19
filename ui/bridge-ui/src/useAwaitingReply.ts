import { useCallback, useMemo } from 'react'
import type { AwaitingReplySettings, Overview, WaitingConversation } from '@kayushkin/multichat-types'
import { useBridgeConfig } from './context'
import { SharedPoll, failureOfResponse, sharedPoll, useSharedPoll, type LoadResult } from './sharedPoll'
import type { FetchFn } from './types'
import {
  AWAITING_REPLY_CLEARS_PATH, AWAITING_REPLY_PATH, AWAITING_REPLY_POLL_MS, AWAITING_REPLY_SETTINGS_PATH,
  clearBodyFor, settingsWriteBody, type AwaitingReplyClearReason,
} from './awaitingReply'
import type { SaveResult } from './components/settings/SettingsSection'

async function loadOverview(fetchFn: FetchFn, url: string): Promise<LoadResult<Overview | null>> {
  let res: Response
  try {
    res = await fetchFn(url)
  } catch (err) {
    return { ok: false, error: `GET ${AWAITING_REPLY_PATH}: ${err instanceof Error ? err.message : String(err)}`, failure: 'unreachable' }
  }
  try {
    if (!res.ok) return { ok: false, error: `GET ${AWAITING_REPLY_PATH} → ${res.status}: ${(await res.text()).trim()}`, failure: failureOfResponse(res) }
    return { ok: true, value: await res.json() as Overview }
  } catch (err) {
    return { ok: false, error: `GET ${AWAITING_REPLY_PATH}: ${err instanceof Error ? err.message : String(err)}`, failure: 'refused' }
  }
}

/** The one awaiting-reply poll for this (fetch, multichat base). The chat
 *  sidebar, the Waiting on you page and the alerts read the same answer. */
function awaitingReplyPoll(fetchFn: FetchFn, multichatBasePath: string): SharedPoll<Overview | null> {
  // A host that proxies no multichat gets a poll that never asks anyone.
  const load = multichatBasePath
    ? () => loadOverview(fetchFn, `${multichatBasePath}${AWAITING_REPLY_PATH}`)
    : async (): Promise<LoadResult<Overview | null>> => ({ ok: true, value: null })
  return sharedPoll(fetchFn, `awaiting-reply ${multichatBasePath}`, () => new SharedPoll<Overview | null>(load, null, AWAITING_REPLY_POLL_MS))
}

/** Who is waiting on a reply, read from multichat every 15 seconds while any
 *  component shows it, and the writes that change it. `enabled` is false when
 *  the host proxies no multichat; nothing is read then. */
export function useAwaitingReply() {
  const { fetch: fetchFn, multichatBasePath } = useBridgeConfig()
  const enabled = !!multichatBasePath
  const poll = useMemo(() => awaitingReplyPoll(fetchFn, multichatBasePath ?? ''), [fetchFn, multichatBasePath])
  const { data: overview, loading, error } = useSharedPoll(poll)

  const post = useCallback(async (method: string, path: string, body: unknown): Promise<SaveResult & { value?: unknown }> => {
    try {
      const res = await fetchFn(`${multichatBasePath}${path}`, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) return { ok: false, error: `${method} ${path} → ${res.status}: ${(await res.text()).trim()}` }
      const value: unknown = await res.json()
      await poll.refresh()
      return { ok: true, value }
    } catch (err) {
      return { ok: false, error: `${method} ${path}: ${err instanceof Error ? err.message : String(err)}` }
    }
  }, [fetchFn, multichatBasePath, poll])

  /** Marks a room seen up to its newest unanswered message, then reads the list again. */
  const clear = useCallback(async (entry: WaitingConversation, reason: AwaitingReplyClearReason): Promise<SaveResult> => {
    const result = await post('POST', AWAITING_REPLY_CLEARS_PATH, clearBodyFor(entry, reason))
    return result.ok ? { ok: true } : result
  }, [post])

  const saveSettings = useCallback(async (settings: AwaitingReplySettings): Promise<SaveResult> => {
    const result = await post('PUT', AWAITING_REPLY_SETTINGS_PATH, settingsWriteBody(settings))
    return result.ok ? { ok: true } : result
  }, [post])

  return { enabled, overview, loading: enabled && loading, error: enabled ? error : null, refresh: poll.refresh, clear, saveSettings }
}

