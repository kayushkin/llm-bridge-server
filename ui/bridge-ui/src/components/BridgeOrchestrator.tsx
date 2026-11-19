import { useEffect, useState } from 'react'
import type { JSX } from 'react'
import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import { formatCost } from '../utils'
import { fetchHomeSessionId } from './chat/useHomeSession'
import { ReferenceMarkdown, TextWithReferenceChips } from './chat/ReferenceText'
import { CostHeader, useOrchestratorResource } from './orchestratorShared'

// The orchestrator's dedicated page. The orchestrator itself is an ordinary
// llm-bridge chat session, its home session (`GET /home-session`), and you talk
// to it there; this page links to it. What the page keeps is what a chat does
// not show: the week's spend against the limit, the injected-context
// inspector, and the log of the one-shot runs from before the home session.
//
// Ported here from dash's own `pages/Orchestrator.tsx`, which is now a thin
// mount of this component. References go through chat-core's chips
// (`./chat/ReferenceText`) and every navigation target comes from
// `routes`.
//
// ⚠️ Mount inside chat-core's `<ChatProvider>` and a react-router router. The
// reference chips resolve their ids against llm-bridge and noteboard through
// chat-core's context, and its hooks throw without it. `BridgeProvider` is
// required too, for `orchestratorBasePath` and `routes`.

export function BridgeOrchestrator(): JSX.Element {
  const { orchestratorBasePath } = useBridgeConfig()

  if (!orchestratorBasePath) {
    return (
      <div className="bc-orchestrator" style={page}>
        <h1 style={{ margin: 0 }}>🎬 Orchestrator</h1>
        <p style={{ opacity: 0.7 }}>
          No orchestrator configured — set <code>orchestratorBasePath</code> on BridgeProvider.
        </p>
      </div>
    )
  }

  return (
    <div className="bc-orchestrator" style={page}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 14, flexWrap: 'wrap' }}>
        <h1 style={{ margin: 0 }}>🎬 Orchestrator</h1>
        <HomeSessionLink />
        <span style={{ marginLeft: 'auto' }}>
          <CostHeader />
        </span>
      </div>
      <ContextInspector />
      <Runs />
    </div>
  )
}

// --- the way into the orchestrator's chat ----------------------------------

/** A link that opens the home session on the host's chat page, or the reason
 *  orchestratord could not name it. Nothing when the host mounts no chat route. */
function HomeSessionLink(): JSX.Element | null {
  const { fetch: apiFetch, orchestratorBasePath, routes } = useBridgeConfig()
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let canceled = false
    fetchHomeSessionId(apiFetch, orchestratorBasePath).then(
      (id) => { if (!canceled) { setSessionId(id); setError(null) } },
      (e: unknown) => { if (!canceled) setError(e instanceof Error ? e.message : String(e)) },
    )
    return () => { canceled = true }
  }, [apiFetch, orchestratorBasePath])

  if (!routes.chat) return null
  if (error) return <span style={{ color: '#ef4444', fontSize: 13 }}>home session: {error}</span>
  if (!sessionId) return null
  return (
    <Link
      className="bc-orchestrator-home-session-link"
      to={`${routes.chat}?session=${encodeURIComponent(sessionId)}`}
      style={{ fontSize: 13 }}
    >
      Open the orchestrator’s chat →
    </Link>
  )
}

// --- runs log ---------------------------------------------------------------

interface OrchestratorRun {
  id: string
  at: string
  trigger: string
  model: string
  message: string
  reply: string
  injected_tokens: number
  input_tokens: number
  output_tokens: number
  cost_usd: number
  duration_ms: number
  error?: string
}

function Runs(): JSX.Element {
  const { data: runs, error } = useOrchestratorResource<OrchestratorRun[]>('/runs?limit=50', [])
  const [open, setOpen] = useState<string | null>(null)

  return (
    <section className="bc-orchestrator-runs" style={card}>
      <div style={cardHeader}>
        One-shot runs{' '}
        <span style={{ opacity: 0.5, fontWeight: 400 }}>
          · {runs.length} · history from before the home session; no new runs are added
        </span>
      </div>
      {error && <div style={{ color: '#ef4444', fontSize: 12, padding: '6px 12px' }}>{error}</div>}
      {runs.length === 0 && !error && <div style={{ opacity: 0.5, fontSize: 13, padding: 12 }}>No runs yet.</div>}
      {runs.map((r) => (
        <div key={r.id} style={{ borderTop: '1px solid var(--border,#1e293b)' }}>
          <div
            onClick={() => setOpen((o) => (o === r.id ? null : r.id))}
            style={{ display: 'flex', gap: 10, alignItems: 'center', padding: '7px 12px', cursor: 'pointer', fontSize: 12 }}
          >
            <span style={{ opacity: 0.6, minWidth: 120 }}>{r.at ? new Date(r.at).toLocaleString() : ''}</span>
            <span style={{ ...tag, background: 'rgba(99,102,241,0.18)' }}>{r.trigger}</span>
            <span style={{ opacity: 0.75, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {r.error ? <span style={{ color: '#ef4444' }}>error: {r.error}</span> : r.message}
            </span>
            <span title="injected context">inj {r.injected_tokens}</span>
            <span title="output tokens">out {r.output_tokens}</span>
            <span style={{ minWidth: 52, textAlign: 'right' }}>{formatCost(r.cost_usd)}</span>
          </div>
          {open === r.id && !r.error && (
            <div style={{ padding: '0 12px 10px', fontSize: 12 }}>
              <div style={{ opacity: 0.6, margin: '4px 0' }}>you: {r.message}</div>
              <ReferenceMarkdown text={r.reply} />
              <div style={{ opacity: 0.5, marginTop: 6 }}>
                {r.model} · {r.input_tokens} in / {r.output_tokens} out · {(r.duration_ms / 1000).toFixed(1)}s
              </div>
            </div>
          )}
        </div>
      ))}
    </section>
  )
}

// --- context inspector (the parts orchestratord injects into the orchestrator) ---

interface ContextVersion {
  part: string
  title: string
  content: string
  tokens: number
}
interface ContextPart {
  part: string
  title: string
  latest: ContextVersion
  version_count: number
}

const ORDER = ['agents', 'tasks', 'convo_summary', 'convo_current']

function ContextInspector(): JSX.Element {
  const { data: parts, error } = useOrchestratorResource<ContextPart[]>('/context', [])
  // Which parts are unfolded, so a folded one renders no body at all. A
  // `<details>` keeps its children in the DOM while closed, and each reference
  // in there is a chip that resolves its id the moment it mounts — a folded
  // dump naming forty sessions would put forty requests on the wire for
  // something nobody has looked at yet. `agents` starts open, as it always did.
  const [openParts, setOpenParts] = useState<ReadonlySet<string>>(() => new Set(['agents']))
  const setPartOpen = (part: string, open: boolean) =>
    setOpenParts((previous) => {
      const next = new Set(previous)
      if (open) next.add(part)
      else next.delete(part)
      return next
    })
  const ordered = [
    ...(ORDER.map((id) => parts.find((p) => p.part === id)).filter(Boolean) as ContextPart[]),
    ...parts.filter((p) => !ORDER.includes(p.part)),
  ]
  const total = ordered.reduce((a, p) => a + (p.latest?.tokens ?? 0), 0)

  return (
    <section className="bc-orchestrator-context" style={card}>
      <div style={cardHeader}>
        Injected context <span style={{ opacity: 0.5, fontWeight: 400 }}>· ~{total.toLocaleString()} tok</span>
      </div>
      {error && <div style={{ color: '#ef4444', fontSize: 12, padding: '6px 12px' }}>{error}</div>}
      {ordered.map((p) => (
        <details
          key={p.part}
          open={openParts.has(p.part)}
          onToggle={(e) => setPartOpen(p.part, (e.currentTarget as HTMLDetailsElement).open)}
          style={{ borderTop: '1px solid var(--border,#1e293b)' }}
        >
          <summary style={{ cursor: 'pointer', padding: '7px 12px', fontSize: 13 }}>
            {p.title}{' '}
            <span style={{ opacity: 0.5 }}>
              · ~{p.latest.tokens.toLocaleString()} tok · v{p.version_count}
            </span>
          </summary>
          {/* The dump is a payload, not prose — it is what the model will be
              handed — so it keeps its own whitespace and is never expanded
              inline: a context part naming twenty sessions would otherwise
              unfold twenty cards inside a preformatted block. */}
          {openParts.has(p.part) && (
            <pre
              style={{
                whiteSpace: 'pre-wrap',
                fontSize: 11.5,
                margin: 0,
                padding: '0 12px 10px',
                maxHeight: 300,
                overflow: 'auto',
              }}
            >
              <TextWithReferenceChips text={p.latest.content} />
            </pre>
          )}
        </details>
      ))}
    </section>
  )
}

// --- shared inline styles ---------------------------------------------------
// Carried over from the page this was ported from, so a host that mounts it
// gets the surface it already had without shipping any new CSS. The class names
// above are the themeable surface.

const page: React.CSSProperties = { padding: 20, maxWidth: 960, margin: '0 auto', color: 'var(--text,#e2e8f0)' }
const card: React.CSSProperties = { border: '1px solid var(--border,#334155)', borderRadius: 10, marginTop: 16, overflow: 'hidden' }
const cardHeader: React.CSSProperties = { padding: '9px 12px', background: 'var(--bg-surface)', fontWeight: 600, fontSize: 14 }
const tag: React.CSSProperties = { fontSize: 10, padding: '1px 6px', borderRadius: 4 }
