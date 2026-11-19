import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from '../../context'
import { TextWithReferenceLinks } from './ReferenceText'

// Compact in-chat view of the orchestrator's injected context. The full review
// surface (WAL, prior versions, filters) lives on the host's own orchestrator
// page — bridge-ui's own `BridgeOrchestrator`, wherever `routes.orchestrator`
// says the host mounted it; this pane is the at-a-glance "what would the
// orchestrator see right now" with per-part token counts, as an optional split
// alongside the chat.
//
// References in the dump used to be found by a local
// `\[(session|task|todo):([^\]]+)\]` — a second copy of the grammar, which the
// orchestrator page carried too and which had already drifted from it. Both
// copies are gone: `TextWithReferenceLinks` reads chat-core's parser,
// the one place that knows the orchestrator's dialect. Read that module's header
// for why this pane renders LINKS while the page renders chips — in short, this
// pane's only host mounts no `ChatProvider`, and a chip without one throws.

interface Version { part: string; title: string; content: string; tokens: number }
interface PartState { part: string; title: string; latest: Version; version_count: number }

const ORDER = ['agents', 'tasks', 'convo_summary', 'convo_current']

export interface OrchestratorPanelProps {
  onToggleCollapse: () => void
  style?: React.CSSProperties
}

export function OrchestratorPanel({ onToggleCollapse, style }: OrchestratorPanelProps) {
  const { fetch: apiFetch, orchestratorBasePath, routes } = useBridgeConfig()
  const [parts, setParts] = useState<PartState[]>([])
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (!orchestratorBasePath) return
    try {
      const r = await apiFetch(`${orchestratorBasePath}/context`)
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      setParts(await r.json()); setError(null)
    } catch (e) { setError(String((e as Error).message ?? e)) }
  }, [apiFetch, orchestratorBasePath])

  useEffect(() => {
    if (!orchestratorBasePath) return
    load()
    const t = setInterval(load, 15000)
    return () => clearInterval(t)
  }, [load, orchestratorBasePath])

  const ordered = [
    ...ORDER.map(id => parts.find(p => p.part === id)).filter(Boolean) as PartState[],
    ...parts.filter(p => !ORDER.includes(p.part)),
  ]
  const total = ordered.reduce((a, p) => a + (p.latest?.tokens ?? 0), 0)

  return (
    <div className="bc-split-pane bc-split-pane-orchestrator" style={style} data-pane="orchestrator">
      <div
        className="bc-split-pane-header bc-header-clickable"
        onClick={onToggleCollapse}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onToggleCollapse() } }}
        role="button"
        tabIndex={0}
        title="Hide orchestrator context"
      >
        <span className="bc-split-pane-title">🎬 Orchestrator context</span>
        <span className="bc-spacer" />
        <span style={{ fontSize: 12, opacity: 0.7 }}>~{total.toLocaleString()} tok</span>
        {routes.orchestrator && (
          <a href={routes.orchestrator} onClick={e => e.stopPropagation()} title="Open full review page" style={{ marginLeft: 8, textDecoration: 'none' }}>↗</a>
        )}
        <span className="bc-split-collapse-btn" aria-hidden="true" style={{ marginLeft: 8 }}>×</span>
      </div>
      <div style={{ overflow: 'auto', padding: 8, fontSize: 12 }}>
        {!orchestratorBasePath && <div style={{ opacity: 0.7 }}>No orchestrator configured — set orchestratorBasePath on BridgeProvider.</div>}
        {error && <div style={{ color: '#ef4444' }}>Orchestrator offline ({error})</div>}
        {ordered.map(p => (
          <details key={p.part} open={p.part === 'agents' || p.part === 'tasks'} style={{ marginBottom: 8 }}>
            <summary style={{ cursor: 'pointer', opacity: 0.85 }}>
              {p.title} <span style={{ opacity: 0.5 }}>· ~{p.latest.tokens.toLocaleString()} tok · v{p.version_count}</span>
            </summary>
            <pre style={{ whiteSpace: 'pre-wrap', margin: '4px 0 0', fontSize: 11.5 }}><TextWithReferenceLinks text={p.latest.content} /></pre>
          </details>
        ))}
      </div>
    </div>
  )
}
