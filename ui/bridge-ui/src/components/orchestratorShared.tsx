import { useCallback, useEffect, useRef, useState } from 'react'
import type { JSX } from 'react'
import { useBridgeConfig } from '../context'
import { formatCost } from '../utils'

// What the Orchestrator page (`BridgeOrchestrator`) reads from the orchestrator: the
// fetch helpers and the cost header. Talking to the orchestrator happens in its home
// session, an ordinary chat session, so nothing here sends.

/** Reads a orchestrator response, or throws with the status. Every panel surfaces
 *  what this throws — an orchestrator view that quietly shows stale numbers when
 *  the orchestrator is down is worse than one that says so. */
export async function orchestratorJSON<T>(response: Response): Promise<T> {
  if (!response.ok) throw new Error(`HTTP ${response.status}`)
  return (await response.json()) as T
}

export function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** The orchestrator's own polling cadence for the read-only panels. */
const REFRESH_MS = 20000

/** Poll one orchestrator endpoint, keeping whatever it last answered and the reason
 *  the latest attempt failed. Both, deliberately: a failed refresh must not blank
 *  a panel that is still showing the last good answer, and it must not be
 *  invisible either. */
export function useOrchestratorResource<T>(path: string, initial: T): { data: T; error: string | null } {
  const { fetch: apiFetch, orchestratorBasePath } = useBridgeConfig()
  const [data, setData] = useState<T>(initial)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (!orchestratorBasePath) return
    try {
      setData(await orchestratorJSON<T>(await apiFetch(`${orchestratorBasePath}${path}`)))
      setError(null)
    } catch (e) {
      setError(messageOf(e))
    }
  }, [apiFetch, orchestratorBasePath, path])

  useEffect(() => {
    void load()
    const timer = setInterval(() => void load(), REFRESH_MS)
    return () => clearInterval(timer)
  }, [load])

  return { data, error }
}

// --- cost: this week against the limit, the home session's spend, and the
// one-shot run history from before the home session -------------------------

interface CostWindow {
  cost_usd: number
  runs: number
}
/** `GET /cost`. */
interface OrchestratorCost {
  week_start: string
  week_resets_at: string
  week_limit_usd: number
  week_spent_usd: number
  home_session: { session_id: string; spend_usd: number }
  /** The old `windows` map, computed from the frozen log of one-shot `claude -p`
   *  runs; no new runs are added to it. */
  claude_p_run_history: Record<string, CostWindow>
}

export function CostHeader(): JSX.Element | null {
  const { data, error } = useOrchestratorResource<OrchestratorCost | null>('/cost', null)
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLSpanElement | null>(null)
  useEffect(() => {
    if (!open) return
    const onDocMouseDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDocMouseDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocMouseDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  if (error && !data) return <span style={{ color: '#ef4444', fontSize: 12 }}>cost: {error}</span>
  if (!data) return null

  const over = data.week_limit_usd > 0 && data.week_spent_usd >= data.week_limit_usd
  const history = data.claude_p_run_history
  const historyRows: Array<[string, CostWindow | undefined]> = [
    ['this week', history.week],
    ['last 24h', history.last_24h],
    ['last 7 days', history.last_7d],
    ['lifetime', history.lifetime],
  ]

  return (
    <span ref={ref} className="bc-orchestrator-cost" style={{ position: 'relative', display: 'inline-block' }}>
      <button onClick={() => setOpen((o) => !o)} style={{ ...pill, color: over ? '#ef4444' : 'inherit' }}>
        week {formatCost(data.week_spent_usd)} / {data.week_limit_usd > 0 ? formatCost(data.week_limit_usd) : 'no limit'} ▾
      </button>
      {open && (
        <div style={dropdown}>
          <div style={{ display: 'flex', gap: 16, padding: '3px 4px', fontSize: 13 }}>
            <span style={{ opacity: 0.7, minWidth: 90 }}>home session</span>
            <span style={{ marginLeft: 'auto' }}>{formatCost(data.home_session.spend_usd)}</span>
          </div>
          <div style={{ display: 'flex', gap: 16, padding: '3px 4px', fontSize: 13 }}>
            <span style={{ opacity: 0.7, minWidth: 90 }}>week resets</span>
            <span style={{ marginLeft: 'auto' }}>{new Date(data.week_resets_at).toLocaleString()}</span>
          </div>
          <div style={{ fontSize: 11, opacity: 0.6, padding: '8px 4px 2px' }}>one-shot run history</div>
          {historyRows.map(([label, costWindow]) => (
            <div key={label} style={{ display: 'flex', gap: 16, padding: '3px 4px', fontSize: 13 }}>
              <span style={{ opacity: 0.7, minWidth: 90 }}>{label}</span>
              <span style={{ marginLeft: 'auto' }}>{formatCost(costWindow?.cost_usd ?? 0)}</span>
              <span style={{ opacity: 0.5, minWidth: 48, textAlign: 'right' }}>{costWindow?.runs ?? 0} runs</span>
            </div>
          ))}
        </div>
      )}
    </span>
  )
}

const pill: React.CSSProperties = { padding: '4px 10px', borderRadius: 8, border: '1px solid var(--border,#334155)', background: 'var(--bg-surface)', color: 'inherit', cursor: 'pointer', fontSize: 13 }
const dropdown: React.CSSProperties = { position: 'absolute', right: 0, top: '110%', zIndex: 10, background: 'var(--bg,#0f172a)', border: '1px solid var(--border,#334155)', borderRadius: 8, padding: 8, minWidth: 220, boxShadow: '0 6px 20px rgba(0,0,0,0.4)' }
