import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from '../../context'
import type { FetchFn } from '../../types'

/** Ask orchestratord for the llm-bridge session id of the orchestrator's one long-lived chat
 *  session. Throws with orchestratord's own `error` text when it answers 502 (it could
 *  not create or verify the session), and with the status for anything else that is
 *  not a 200. Never returns an empty id: a 200 without one is an error too. */
export async function fetchHomeSessionId(apiFetch: FetchFn, orchestratorBasePath: string): Promise<string> {
  const response = await apiFetch(`${orchestratorBasePath}/home-session`)
  const body = (await response.json().catch(() => null)) as { session_id?: string; error?: string } | null
  if (!response.ok) throw new Error(body?.error || `home-session: HTTP ${response.status}`)
  if (!body?.session_id) throw new Error('home-session: the answer names no session_id')
  return body.session_id
}

export interface HomeSession {
  /** The home session's id, once orchestratord has named it. */
  sessionId: string | null
  /** Why the last ask failed, verbatim. Cleared by the next ask that succeeds. */
  error: string | null
  /** Ask orchestratord for the id again and open that session with `openSession`. Does
   *  not open anything when the ask fails; the reason lands in `error`. */
  open: () => Promise<void>
}

/** The orchestrator's home session: its id, asked for once on mount so the sidebar
 *  can mark its row and read its state, and asked for again on every open, so a
 *  session orchestratord has replaced is never the one opened. `openSession` is the
 *  sidebar's own row-click path. With no `orchestratorBasePath` it asks nothing. */
export function useHomeSession(openSession: (sessionId: string) => void): HomeSession {
  const { fetch: apiFetch, orchestratorBasePath } = useBridgeConfig()
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const ask = useCallback(async (): Promise<string | null> => {
    try {
      const id = await fetchHomeSessionId(apiFetch, orchestratorBasePath)
      setSessionId(id)
      setError(null)
      return id
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return null
    }
  }, [apiFetch, orchestratorBasePath])

  useEffect(() => {
    if (orchestratorBasePath) void ask()
  }, [orchestratorBasePath, ask])

  const open = useCallback(async () => {
    const id = await ask()
    if (id) openSession(id)
  }, [ask, openSession])

  return { sessionId, error, open }
}
