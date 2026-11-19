import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../context'

// Mirror the Go shapes from permission-store. Kept locally rather than
// pulling from the canonical types because permission-store is its own
// service with its own type ownership — bridge-ui calls it via REST.
interface Rule {
  id: string
  scope: string
  priority: number
  tool: string
  pattern?: string
  outcome: 'allow' | 'deny' | 'ask'
  message?: string
  enabled: boolean
  created_at: string
  updated_at: string
}

interface Atom {
  kind: 'call' | 'redirect' | 'unparseable'
  argv?: string[]
  line?: string
  op?: string
  target?: string
  reason?: string
}

interface AuditEntry {
  id: string
  ts: string
  bridge_id?: string
  instance_id?: string
  tool: string
  input?: unknown
  atoms?: Atom[]
  outcome: string
  matched_rule_id?: string
  resolved_by?: string
}

interface EvaluateResponse {
  outcome: string
  matched_rule_id?: string
  message?: string
  atoms: Atom[]
}

type Tab = 'rules' | 'audit' | 'test'

export function BridgePermissions() {
  const { fetch: apiFetch, permissionStoreBasePath } = useBridgeConfig()
  const [tab, setTab] = useState<Tab>('rules')

  if (!permissionStoreBasePath) {
    return (
      <div className="bperm-container">
        <p className="bperm-error">permissionStoreBasePath is not configured. Set it on BridgeProvider to enable this tab.</p>
      </div>
    )
  }

  return (
    <div className="bperm-container">
      <div className="bperm-tabs">
        <button className={`bperm-tab ${tab === 'rules' ? 'bperm-tab-active' : ''}`} onClick={() => setTab('rules')}>Rules</button>
        <button className={`bperm-tab ${tab === 'audit' ? 'bperm-tab-active' : ''}`} onClick={() => setTab('audit')}>Audit</button>
        <button className={`bperm-tab ${tab === 'test' ? 'bperm-tab-active' : ''}`} onClick={() => setTab('test')}>Test</button>
      </div>
      {tab === 'rules' && <RulesTab apiFetch={apiFetch} basePath={permissionStoreBasePath} />}
      {tab === 'audit' && <AuditTab apiFetch={apiFetch} basePath={permissionStoreBasePath} />}
      {tab === 'test' && <TestTab apiFetch={apiFetch} basePath={permissionStoreBasePath} />}
    </div>
  )
}

// --- Rules tab ----------------------------------------------------------

function RulesTab({ apiFetch, basePath }: { apiFetch: ReturnType<typeof useBridgeConfig>['fetch']; basePath: string }) {
  const [rules, setRules] = useState<Rule[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [editing, setEditing] = useState<Rule | null>(null)
  const [creating, setCreating] = useState(false)

  const refetch = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await apiFetch(`${basePath}/rules`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json() as Rule[] | null
      setRules(data || [])
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [apiFetch, basePath])

  useEffect(() => { refetch() }, [refetch])

  const grouped = useMemo(() => {
    const order = (s: string) => s.startsWith('instance:') ? 0 : s.startsWith('bridge:') ? 1 : 2
    const sorted = [...rules].sort((a, b) =>
      order(a.scope) - order(b.scope)
      || a.priority - b.priority
      || a.id.localeCompare(b.id)
    )
    const groups: Record<string, Rule[]> = {}
    for (const r of sorted) {
      if (!groups[r.scope]) groups[r.scope] = []
      groups[r.scope].push(r)
    }
    return groups
  }, [rules])

  const toggleEnabled = async (rule: Rule) => {
    try {
      const res = await apiFetch(`${basePath}/rules/${encodeURIComponent(rule.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: !rule.enabled }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      refetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const deleteRule = async (rule: Rule) => {
    if (!confirm(`Delete rule "${rule.id}"?`)) return
    try {
      const res = await apiFetch(`${basePath}/rules/${encodeURIComponent(rule.id)}`, { method: 'DELETE' })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      refetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  if (loading) return <div className="bperm-loading">Loading rules…</div>

  return (
    <>
      {error && <p className="bperm-error">{error}</p>}
      <div className="bperm-toolbar">
        <button className="bperm-btn-primary" onClick={() => setCreating(true)}>+ New rule</button>
        <span className="bperm-count">{rules.length} rule{rules.length === 1 ? '' : 's'}</span>
      </div>
      {Object.entries(grouped).map(([scope, scopeRules]) => (
        <div key={scope} className="bperm-scope-block">
          <h3 className="bperm-scope-title">{scope}</h3>
          <table className="bperm-table">
            <thead>
              <tr>
                <th>Pri</th>
                <th>Tool</th>
                <th>Pattern</th>
                <th>Outcome</th>
                <th>ID</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {scopeRules.map(r => (
                <tr key={r.id} className={r.enabled ? '' : 'bperm-row-disabled'}>
                  <td>{r.priority}</td>
                  <td>{r.tool}</td>
                  <td><code>{r.pattern || '—'}</code></td>
                  <td className={`bperm-outcome bperm-outcome-${r.outcome}`}>{r.outcome}</td>
                  <td className="bperm-rule-id"><code>{r.id}</code></td>
                  <td className="bperm-actions">
                    <label className="bperm-toggle">
                      <input type="checkbox" checked={r.enabled} onChange={() => toggleEnabled(r)} />
                      <span>{r.enabled ? 'on' : 'off'}</span>
                    </label>
                    <button onClick={() => setEditing(r)}>Edit</button>
                    <button onClick={() => deleteRule(r)}>Delete</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
      {(editing || creating) && (
        <RuleEditor
          rule={editing}
          apiFetch={apiFetch}
          basePath={basePath}
          onClose={() => { setEditing(null); setCreating(false); refetch() }}
        />
      )}
    </>
  )
}

function RuleEditor({
  rule,
  apiFetch,
  basePath,
  onClose,
}: {
  rule: Rule | null
  apiFetch: ReturnType<typeof useBridgeConfig>['fetch']
  basePath: string
  onClose: () => void
}) {
  const [scope, setScope] = useState(rule?.scope ?? 'global')
  const [priority, setPriority] = useState(rule?.priority ?? 200)
  const [tool, setTool] = useState(rule?.tool ?? 'Bash')
  const [pattern, setPattern] = useState(rule?.pattern ?? '')
  const [outcome, setOutcome] = useState<Rule['outcome']>(rule?.outcome ?? 'allow')
  const [message, setMessage] = useState(rule?.message ?? '')
  const [enabled, setEnabled] = useState(rule?.enabled ?? true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const save = async () => {
    setSaving(true)
    setError(null)
    try {
      const body = { scope, priority, tool, pattern, outcome, message, enabled }
      const url = rule
        ? `${basePath}/rules/${encodeURIComponent(rule.id)}`
        : `${basePath}/rules`
      const res = await apiFetch(url, {
        method: rule ? 'PATCH' : 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const text = await res.text()
        throw new Error(`HTTP ${res.status} ${text}`)
      }
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="bperm-modal-backdrop" onClick={onClose}>
      <div className="bperm-modal" onClick={e => e.stopPropagation()}>
        <h3>{rule ? `Edit ${rule.id}` : 'New rule'}</h3>
        <div className="bperm-form">
          <label>Scope <input value={scope} onChange={e => setScope(e.target.value)} placeholder="global | bridge:&lt;id&gt; | instance:&lt;id&gt;" /></label>
          <label>Priority <input type="number" value={priority} onChange={e => setPriority(parseInt(e.target.value || '100', 10))} /></label>
          <label>Tool <input value={tool} onChange={e => setTool(e.target.value)} placeholder="Bash | Read | * | ..." /></label>
          <label>Pattern <input value={pattern} onChange={e => setPattern(e.target.value)} placeholder="^git status (RE2 regex; empty = match any)" /></label>
          <label>Outcome
            <select value={outcome} onChange={e => setOutcome(e.target.value as Rule['outcome'])}>
              <option value="allow">allow</option>
              <option value="deny">deny</option>
              <option value="ask">ask</option>
            </select>
          </label>
          <label>Message <input value={message} onChange={e => setMessage(e.target.value)} placeholder="Surfaced on deny / ask" /></label>
          <label className="bperm-toggle">
            <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />
            <span>Enabled</span>
          </label>
        </div>
        {error && <p className="bperm-error">{error}</p>}
        <div className="bperm-modal-actions">
          <button onClick={onClose} disabled={saving}>Cancel</button>
          <button className="bperm-btn-primary" onClick={save} disabled={saving}>{rule ? 'Save' : 'Create'}</button>
        </div>
      </div>
    </div>
  )
}

// --- Audit tab ----------------------------------------------------------

function AuditTab({ apiFetch, basePath }: { apiFetch: ReturnType<typeof useBridgeConfig>['fetch']; basePath: string }) {
  const [filterBridge, setFilterBridge] = useState('')
  const [filterTool, setFilterTool] = useState('')
  const [limit, setLimit] = useState(50)
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refetch = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const params = new URLSearchParams()
      if (filterBridge) params.set('bridge_id', filterBridge)
      if (filterTool) params.set('tool', filterTool)
      params.set('limit', String(limit))
      const res = await apiFetch(`${basePath}/audit?${params}`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json() as AuditEntry[] | null
      setEntries(data || [])
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [apiFetch, basePath, filterBridge, filterTool, limit])

  useEffect(() => { refetch() }, [refetch])

  return (
    <>
      <div className="bperm-toolbar">
        <input placeholder="bridge_id" value={filterBridge} onChange={e => setFilterBridge(e.target.value)} />
        <input placeholder="tool" value={filterTool} onChange={e => setFilterTool(e.target.value)} />
        <label>limit <input type="number" value={limit} onChange={e => setLimit(parseInt(e.target.value || '50', 10))} style={{ width: 60 }} /></label>
        <button onClick={refetch} disabled={loading}>{loading ? 'Loading…' : 'Refresh'}</button>
      </div>
      {error && <p className="bperm-error">{error}</p>}
      <table className="bperm-table">
        <thead>
          <tr>
            <th>Time</th>
            <th>Tool</th>
            <th>Atoms</th>
            <th>Outcome</th>
            <th>Matched rule</th>
            <th>Bridge</th>
          </tr>
        </thead>
        <tbody>
          {entries.map(e => (
            <tr key={e.id}>
              <td>{e.ts.replace('T', ' ').slice(0, 19)}</td>
              <td>{e.tool}</td>
              <td>
                <code>{(e.atoms || []).filter(a => a.kind === 'call').map(a => a.line).join(' ; ') || '—'}</code>
              </td>
              <td className={`bperm-outcome bperm-outcome-${e.outcome}`}>{e.outcome}</td>
              <td className="bperm-rule-id"><code>{e.matched_rule_id || '—'}</code></td>
              <td className="bperm-rule-id"><code>{e.bridge_id || '—'}</code></td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}

// --- Test tab -----------------------------------------------------------

function TestTab({ apiFetch, basePath }: { apiFetch: ReturnType<typeof useBridgeConfig>['fetch']; basePath: string }) {
  const [tool, setTool] = useState('Bash')
  const [input, setInput] = useState('{"command":"git status && rm -rf /tmp"}')
  const [bridgeID, setBridgeID] = useState('')
  const [result, setResult] = useState<EvaluateResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const evaluate = async () => {
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      let parsedInput: unknown
      try {
        parsedInput = JSON.parse(input)
      } catch (e) {
        throw new Error(`input is not valid JSON: ${e instanceof Error ? e.message : String(e)}`)
      }
      const res = await apiFetch(`${basePath}/evaluate`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tool, input: parsedInput, bridge_id: bridgeID || undefined }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setResult(await res.json() as EvaluateResponse)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <p className="bperm-subtitle">Run a tool call through the splitter + rule engine without actually executing it. Useful for crafting Bash atom regexes.</p>
      <div className="bperm-form">
        <label>Tool <input value={tool} onChange={e => setTool(e.target.value)} /></label>
        <label>bridge_id (optional) <input value={bridgeID} onChange={e => setBridgeID(e.target.value)} placeholder="for instance/bridge-scope rules" /></label>
        <label>Input JSON
          <textarea
            value={input}
            onChange={e => setInput(e.target.value)}
            rows={5}
            spellCheck={false}
            style={{ fontFamily: 'monospace', fontSize: '0.85em' }}
          />
        </label>
      </div>
      <div className="bperm-toolbar">
        <button className="bperm-btn-primary" onClick={evaluate} disabled={busy}>{busy ? 'Evaluating…' : 'Evaluate'}</button>
      </div>
      {error && <p className="bperm-error">{error}</p>}
      {result && (
        <div className="bperm-test-result">
          <p>
            Outcome: <strong className={`bperm-outcome bperm-outcome-${result.outcome}`}>{result.outcome}</strong>
            {result.matched_rule_id && <> &middot; matched <code>{result.matched_rule_id}</code></>}
          </p>
          {result.message && <p>Message: {result.message}</p>}
          <h4>Atoms ({result.atoms.length})</h4>
          <pre className="bperm-pre">{JSON.stringify(result.atoms, null, 2)}</pre>
        </div>
      )}
    </>
  )
}
