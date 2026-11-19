import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from '../../context'

/**
 * The token that lets a browser join a pty session's terminal.
 *
 * ## Why this is fetched and not remembered
 *
 * `POST /sessions/{id}/mode` answers a switch to pty with an `attach_token`, and
 * bridge-ui caches that answer in a per-session map (`useBridgeSession`'s
 * `attachTokens`). That map is memory: reload the tab and the token is gone, and the
 * only way bridge-ui gets another is to switch the session's mode AGAIN — which kills
 * and respawns the harness process to re-mint a token the server was already holding.
 *
 * `GET /sessions/{id}/attach-token` exists precisely so a client does not have to do
 * that; the gateway's own comment says so (`attach.go:52-56`). So this reads the
 * server's token whenever the session is in pty mode, which makes the terminal
 * survive a refresh and makes the mode switch a thing you do once.
 *
 * ## A 404 is an answer, not a failure
 *
 * The endpoint 404s when the session has no live pty hub — a session that WAS pty and
 * whose process has since gone. That is not an error to report in red; it is the
 * server saying "there is no terminal here, switch the mode to spin one up". It comes
 * back as `absent`, which the pane renders as that sentence.
 *
 * Any other failure IS reported. A gateway that is down and a session with no
 * terminal are different situations and must not read alike.
 */
export type AttachTokenState =
  /** Not a pty session, so no token is wanted. */
  | { status: 'idle' }
  | { status: 'loading' }
  /** Ready to attach. */
  | { status: 'ready'; token: string }
  /** The session has no live pty hub — a mode switch would create one. */
  | { status: 'absent' }
  | { status: 'error'; message: string }

export function useAttachToken(
  sessionId: string | null,
  mode: string,
): { state: AttachTokenState; refresh: () => void } {
  const { fetch: bridgeFetch, basePath } = useBridgeConfig()
  const [state, setState] = useState<AttachTokenState>({ status: 'idle' })
  const [refreshTick, setRefreshTick] = useState(0)

  const refresh = useCallback(() => setRefreshTick(tick => tick + 1), [])

  const isPty = mode === 'pty'

  useEffect(() => {
    if (!sessionId || !isPty) {
      setState({ status: 'idle' })
      return
    }
    let canceled = false
    setState({ status: 'loading' })
    bridgeFetch(`${basePath}/sessions/${sessionId}/attach-token`)
      .then(async response => {
        if (response.status === 404) return null
        if (!response.ok) throw new Error(`${response.status} ${await response.text()}`)
        return (await response.json()) as { attach_token?: string }
      })
      .then(body => {
        if (canceled) return
        if (body === null) {
          setState({ status: 'absent' })
          return
        }
        const token = body.attach_token ?? ''
        // An empty token is not a token. Reported as absent rather than handed to
        // `BridgeAttach`, which would open a WebSocket the server must then refuse.
        setState(token ? { status: 'ready', token } : { status: 'absent' })
      })
      .catch((err: unknown) => {
        if (canceled) return
        setState({
          status: 'error',
          message: err instanceof Error ? err.message : String(err),
        })
      })
    return () => {
      canceled = true
    }
  }, [sessionId, isPty, refreshTick, bridgeFetch, basePath])

  return { state, refresh }
}
