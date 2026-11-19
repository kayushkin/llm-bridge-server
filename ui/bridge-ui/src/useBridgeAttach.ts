import { useCallback, useEffect, useRef, useState } from 'react'
import { useBridgeConfig } from './context'

// useBridgeAttach is the WebSocket-side counterpart of useBridgeSession,
// for sessions running in pty mode. It owns one WS to
// `${basePath}/sessions/{id}/attach?token=...` and surfaces:
//
//   - status: connection lifecycle.
//   - role:   "writer" or "reader", set from the server's first text
//             frame after attach. Readers should not wire keystrokes.
//   - send/resize/close: outbound writes (binary, control, control).
//   - onData: subscribe to inbound binary frames (raw pty bytes). Returns
//             an unsubscribe function, idiomatic for useEffect cleanup.
//
// The hook does not own any terminal renderer — it's the transport layer.
// A sibling component (BridgeAttach + xterm.js) consumes this hook and
// pumps bytes both directions.

export type AttachStatus = 'idle' | 'connecting' | 'open' | 'closed' | 'error'
export type AttachRole = 'writer' | 'reader'

// Inbound text-frame schema. Matches llm-bridge-server's attachControl.
interface AttachControl {
  type: string
  code?: number
  signal?: string
  rows?: number
  cols?: number
  role?: string
}

export interface AttachExit {
  code: number
  signal: string
}

export interface UseBridgeAttachReturn {
  status: AttachStatus
  role: AttachRole | null
  /** Last error message; clears when a new connect succeeds. */
  error: string | null
  /** Server-reported exit; set once when the pty terminates. */
  exit: AttachExit | null
  /** Send binary frame (raw pty stdin). No-op if socket isn't open. */
  send: (data: ArrayBuffer | Uint8Array) => void
  /** Send {type:"resize",...} control frame. No-op if socket isn't open. */
  resize: (rows: number, cols: number) => void
  /** Send {type:"close"} then close the WS. Safe to call repeatedly. */
  close: () => void
  /** Subscribe to inbound binary frames. Returns unsubscribe. */
  onData: (cb: (data: ArrayBuffer) => void) => () => void
}

export interface UseBridgeAttachOptions {
  sessionId: string
  attachToken: string
  /** When false the hook stays idle (useful for gating on session.mode). */
  enabled?: boolean
}

// wsUrlFor turns a relative basePath like "/api/bridge" into a fully
// qualified ws(s):// URL on the current page origin. Absolute basePaths
// (http://x or https://x) are honored verbatim — useful for dev setups
// pointing at bridge-server on a different host.
function wsUrlFor(basePath: string, sessionId: string, token: string): string {
  const path = `/sessions/${encodeURIComponent(sessionId)}/attach?token=${encodeURIComponent(token)}`
  // Absolute URL (http(s)://...) — swap scheme.
  if (/^https?:\/\//i.test(basePath)) {
    return basePath.replace(/^http/i, 'ws') + path
  }
  // Page-relative — derive from window.location.
  const proto = typeof window !== 'undefined' && window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const host = typeof window !== 'undefined' ? window.location.host : ''
  return `${proto}//${host}${basePath}${path}`
}

export function useBridgeAttach(opts: UseBridgeAttachOptions): UseBridgeAttachReturn {
  const { sessionId, attachToken, enabled = true } = opts
  const { basePath } = useBridgeConfig()

  const [status, setStatus] = useState<AttachStatus>('idle')
  const [role, setRole] = useState<AttachRole | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [exit, setExit] = useState<AttachExit | null>(null)

  // Mutable refs: WS instance + binary subscribers list. These live across
  // renders without forcing re-renders when callbacks register/unregister.
  const wsRef = useRef<WebSocket | null>(null)
  const subsRef = useRef<Set<(data: ArrayBuffer) => void>>(new Set())

  // sendCtrl is keyed off wsRef so it survives re-renders without
  // tearing the socket down — the actual function identity is stable.
  const sendCtrl = useCallback((ctrl: AttachControl) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify(ctrl))
  }, [])

  const send = useCallback((data: ArrayBuffer | Uint8Array) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    // Reader-role clients can still call send(); the server silently
    // drops their input frames. We don't gate here so the caller's
    // keystroke pump stays simple — role is exposed for UI affordances.
    ws.send(data)
  }, [])

  const resize = useCallback((rows: number, cols: number) => {
    if (rows <= 0 || cols <= 0) return
    sendCtrl({ type: 'resize', rows, cols })
  }, [sendCtrl])

  const close = useCallback(() => {
    const ws = wsRef.current
    if (!ws) return
    // Best-effort goodbye — server treats {type:"close"} as a clean
    // detach. Then close the WS itself; readyState guards prevent
    // sending on an already-closed socket.
    if (ws.readyState === WebSocket.OPEN) {
      try { ws.send(JSON.stringify({ type: 'close' })) } catch { /* peer gone */ }
    }
    try { ws.close() } catch { /* already closing */ }
  }, [])

  const onData = useCallback((cb: (data: ArrayBuffer) => void): (() => void) => {
    subsRef.current.add(cb)
    return () => { subsRef.current.delete(cb) }
  }, [])

  // Lifecycle: open WS when enabled + sessionId + token are present;
  // tear down cleanly on unmount or input change. Status transitions
  // are the only React state updates from inside the socket handlers.
  useEffect(() => {
    if (!enabled || !sessionId || !attachToken) {
      setStatus('idle')
      return
    }

    setStatus('connecting')
    setRole(null)
    setError(null)
    setExit(null)

    const url = wsUrlFor(basePath, sessionId, attachToken)
    const ws = new WebSocket(url)
    ws.binaryType = 'arraybuffer'
    wsRef.current = ws

    ws.onopen = () => {
      setStatus('open')
    }

    ws.onmessage = (ev: MessageEvent) => {
      const data = ev.data
      if (data instanceof ArrayBuffer) {
        // Fan binary out to every subscriber. Subscribers are responsible
        // for not throwing — but if one does, isolate from the rest so a
        // single bad listener can't break the terminal feed.
        for (const cb of subsRef.current) {
          try { cb(data) } catch { /* listener bug; keep going */ }
        }
        return
      }
      // Text frame: JSON control envelope.
      let ctrl: AttachControl
      try {
        ctrl = JSON.parse(data as string) as AttachControl
      } catch {
        return
      }
      if (ctrl.type === 'role' && (ctrl.role === 'writer' || ctrl.role === 'reader')) {
        setRole(ctrl.role)
      } else if (ctrl.type === 'exit') {
        setExit({ code: ctrl.code ?? 0, signal: ctrl.signal ?? '' })
      }
      // Other control types (signal et al.) — ignore quietly to mirror
      // the server's "drop unknown rather than tear down" posture.
    }

    ws.onerror = () => {
      // The browser's WebSocket API doesn't expose useful error detail;
      // surface a generic message and let the close handler set final
      // status. We avoid setStatus('error') here so the natural
      // close→'closed' transition still runs.
      setError('attach websocket error')
    }

    ws.onclose = (ev: CloseEvent) => {
      // Clean close (1000) and "no status" (1005, common for client-
      // initiated close) both count as 'closed'. Anything else is an
      // unexpected drop — surface as 'error' so the UI can reattach.
      if (ev.code === 1000 || ev.code === 1005) {
        setStatus('closed')
      } else {
        setStatus('error')
        if (!error) setError(`attach closed (${ev.code} ${ev.reason || 'no reason'})`)
      }
      if (wsRef.current === ws) wsRef.current = null
    }

    return () => {
      // React strict-mode and effect-rerun cleanup: signal a clean
      // close and let the browser run the close handshake.
      if (ws.readyState === WebSocket.OPEN) {
        try { ws.send(JSON.stringify({ type: 'close' })) } catch { /* peer gone */ }
      }
      try { ws.close(1000, 'unmount') } catch { /* already closing */ }
      if (wsRef.current === ws) wsRef.current = null
    }
    // basePath is read once per (re)connect; if it changes we want a new socket.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, sessionId, attachToken, basePath])

  return { status, role, error, exit, send, resize, close, onData }
}
