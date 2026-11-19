// Where a session runs: the harness instance llm-bridge-server bound it to, and
// through that instance, the environment (machine).
//
// A card records its sessions as links, and every session record carries its
// `instance_id`, so the card already knows where each agent ran. This is the
// lookup that puts it on screen.
//
// The answer is cached for the life of the page, per base path and session id,
// and that is safe for one reason: the server binds a session to its instance
// when it creates the session (`resolveInstance` in llm-bridge-server's
// sessions.go) and never moves it. The session's state and activity change, and
// are not cached here — `CardTiming` reads those live. A failed lookup is
// dropped from the cache so the next render asks again.

import { useEffect, useState } from 'react'
import { useBridgeConfig } from './context'
import type { FetchFn } from './types'

export interface SessionRunsOn {
  sessionID: string
  /** harness-store's instance id, e.g. `inst-cc-local`. */
  instanceID: string
  /** The session's harness, e.g. `claude_code`. Always the instance's own. */
  harness: string
}

const runsOnBySession = new Map<string, Promise<SessionRunsOn>>()

export function fetchSessionRunsOn(fetchFn: FetchFn, basePath: string, sessionID: string): Promise<SessionRunsOn> {
  const key = `${basePath}\n${sessionID}`
  const cached = runsOnBySession.get(key)
  if (cached) return cached
  const lookup = fetchFn(`${basePath}/sessions/${encodeURIComponent(sessionID)}`).then(async res => {
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
    const session = await res.json() as { instance_id?: unknown; harness?: unknown }
    if (typeof session?.instance_id !== 'string' || !session.instance_id) {
      throw new Error('the session record names no instance')
    }
    return {
      sessionID,
      instanceID: session.instance_id,
      harness: typeof session.harness === 'string' ? session.harness : '',
    }
  })
  runsOnBySession.set(key, lookup)
  lookup.catch(() => { runsOnBySession.delete(key) })
  return lookup
}

/** `runsOn` is null until the bridge answers; `error` says why it could not. */
export function useSessionRunsOn(sessionID: string | null): { runsOn: SessionRunsOn | null; error: string | null } {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [runsOn, setRunsOn] = useState<SessionRunsOn | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    setRunsOn(null)
    setError(null)
    if (!sessionID) return
    let canceled = false
    fetchSessionRunsOn(fetchFn, basePath, sessionID)
      .then(value => { if (!canceled) setRunsOn(value) })
      .catch(err => { if (!canceled) setError(err instanceof Error ? err.message : String(err)) })
    return () => { canceled = true }
  }, [sessionID, fetchFn, basePath])

  return { runsOn, error }
}
