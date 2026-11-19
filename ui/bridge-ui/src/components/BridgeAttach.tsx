import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
// Pull xterm's stylesheet through the import graph so Vite (in dash)
// bundles it automatically — saves the consumer from having to
// remember a separate `import '@xterm/xterm/css/xterm.css'` at app
// startup. tsc passes the import through as-is in the compiled .js.
import '@xterm/xterm/css/xterm.css'
import { useBridgeAttach } from '../useBridgeAttach'
import type { AttachExit } from '../useBridgeAttach'

// BridgeAttach renders a live pty session as an xterm.js terminal,
// driven by the useBridgeAttach hook (which owns the WebSocket).

export interface BridgeAttachProps {
  sessionId: string
  attachToken: string
  /** Toggle for the parent to pause the WS without unmounting. */
  enabled?: boolean
  /** Called when the user clicks the explicit detach button. */
  onDetach?: () => void
  /** Optional className on the outer wrapper for layout/theming overrides. */
  className?: string
}

export function BridgeAttach({
  sessionId,
  attachToken,
  enabled = true,
  onDetach,
  className,
}: BridgeAttachProps) {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  // Track whether the terminal has been mounted into the DOM. xterm
  // refuses to write until .open(el) has been called, so we gate the
  // hook's onData pump on this rather than dropping early bytes.
  const [termReady, setTermReady] = useState(false)

  const attach = useBridgeAttach({ sessionId, attachToken, enabled })
  // Hoist hook values into a ref so the resize observer can call into
  // the current attach instance without re-creating the observer on
  // every status change.
  const attachRef = useRef(attach)
  attachRef.current = attach

  // Mount / unmount the terminal. Recreated when sessionId changes so
  // the scrollback from a previous session doesn't bleed into a new
  // one — there's no API to fully reset xterm's scrollback short of
  // reconstruction.
  useEffect(() => {
    const el = containerRef.current
    if (!el) return

    const term = new Terminal({
      convertEol: false,
      cursorBlink: true,
      // Default 24x80 matches the server's initial pty size; FitAddon
      // overrides it once the container has measurable dimensions.
      rows: 24,
      cols: 80,
      // Smooth scroll back to bottom on new output — matches the
      // expectation of someone running the CLI directly.
      scrollback: 10000,
      // Hand keystrokes off to onData rather than xterm's own write
      // — we want every byte to round-trip the server.
      allowProposedApi: true,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(el)
    termRef.current = term
    fitRef.current = fit
    setTermReady(true)

    // Keystrokes → outbound binary frames. xterm.onData yields the
    // exact byte sequence a real pty would receive (escape sequences
    // for arrows, function keys, paste, etc.), pre-encoded the way
    // the upstream CLI expects.
    const encoder = new TextEncoder()
    const keyDisposable = term.onData((data: string) => {
      const live = attachRef.current
      if (!live || live.status !== 'open' || live.role !== 'writer') return
      live.send(encoder.encode(data))
    })

    // Initial size sync — wait one frame so the container has its
    // final dimensions before measuring.
    const raf = requestAnimationFrame(() => {
      try {
        fit.fit()
        const live = attachRef.current
        if (live && live.status === 'open') {
          live.resize(term.rows, term.cols)
        }
      } catch { /* container not yet sized */ }
    })

    return () => {
      cancelAnimationFrame(raf)
      keyDisposable.dispose()
      term.dispose()
      termRef.current = null
      fitRef.current = null
      setTermReady(false)
    }
  }, [sessionId])

  // Inbound binary → terminal. Subscribe to the hook once the term is
  // mounted; the hook's onData returns its unsubscribe so cleanup is
  // tied to the unsubscribe rather than to the WS lifecycle.
  useEffect(() => {
    if (!termReady) return
    const unsub = attach.onData((data: ArrayBuffer) => {
      const term = termRef.current
      if (!term) return
      term.write(new Uint8Array(data))
    })
    return unsub
  }, [termReady, attach.onData])

  // Resize observer. Refits xterm on container size changes and
  // pushes the new dimensions through the WS so the upstream CLI
  // redraws in the user's geometry. Resizes from a reader-role
  // attacher are dropped server-side, but we send them anyway — a
  // future writer-promotion will then have the latest size.
  useEffect(() => {
    const el = containerRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      const fit = fitRef.current
      const term = termRef.current
      if (!fit || !term) return
      try { fit.fit() } catch { return }
      const live = attachRef.current
      if (live && live.status === 'open') {
        live.resize(term.rows, term.cols)
      }
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // Send the initial resize once the WS is open. Status flips
  // 'connecting' → 'open' before the role frame arrives, so we resize
  // here rather than waiting for role — the server happily accepts a
  // resize from a brand-new attacher (it'll just take effect once
  // they're promoted to writer if currently a reader).
  useEffect(() => {
    if (attach.status !== 'open') return
    const term = termRef.current
    const fit = fitRef.current
    if (!term || !fit) return
    try { fit.fit() } catch { return }
    attach.resize(term.rows, term.cols)
  }, [attach.status])

  const handleDetach = () => {
    attach.close()
    if (onDetach) onDetach()
  }

  return (
    <div className={`bridge-attach ${className ?? ''}`}>
      <div className="bridge-attach-header">
        <span className="bridge-attach-status">
          {statusLabel(attach.status, attach.role, attach.exit)}
        </span>
        {attach.role === 'reader' && attach.status === 'open' && (
          <span className="bridge-attach-readonly">read-only</span>
        )}
        <button className="bridge-attach-detach" onClick={handleDetach}>
          detach
        </button>
      </div>
      <div ref={containerRef} className="bridge-attach-term" />
      {attach.error && attach.status !== 'open' && (
        <div className="bridge-attach-error">{attach.error}</div>
      )}
    </div>
  )
}

function statusLabel(
  status: ReturnType<typeof useBridgeAttach>['status'],
  role: ReturnType<typeof useBridgeAttach>['role'],
  exit: AttachExit | null,
): string {
  if (exit) {
    const sig = exit.signal ? ` (${exit.signal})` : ''
    return `exited code ${exit.code}${sig}`
  }
  switch (status) {
    case 'idle': return 'idle'
    case 'connecting': return 'connecting…'
    case 'open': return role ? `attached (${role})` : 'attached'
    case 'closed': return 'detached'
    case 'error': return 'attach error'
  }
}
