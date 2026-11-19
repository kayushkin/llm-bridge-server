import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from '../context'
import { useBridgeInstances } from '../useBridgeInstances'
import type { BridgeInstance, InstanceCredential } from '../types'
import type { Credential } from '@kayushkin/llm-bridge-types'

const PROVIDERS = [
  { id: 'anthropic', label: 'Anthropic' },
  { id: 'openai', label: 'OpenAI' },
  { id: 'google', label: 'Google' },
  { id: 'github', label: 'GitHub' },
]

interface ModelHealth {
  model: string
  last_success_at: string
  last_success_ms: number
  avg_response_ms: number
  last_error_at: string
  last_error?: string
  consecutive_errors?: number
}

interface ModelCredRef {
  id: string
  provider: string
  label: string
  auth_type: string
  api_key_masked?: string
  token_masked?: string
  priority: number
  enabled: boolean
  expires_at: number
}

interface ModelEntry {
  id: string
  provider: string
  name: string
  max_tokens: number
  input_cost: number
  output_cost: number
  enabled: boolean
  priority: number
  health: ModelHealth | null
  credentials: ModelCredRef[]
}

function timeAgo(dateStr: string): string {
  if (!dateStr || dateStr === '0001-01-01T00:00:00Z') return 'never'
  const d = new Date(dateStr)
  if (d.getTime() === 0) return 'never'
  const seconds = Math.floor((Date.now() - d.getTime()) / 1000)
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

function modelHealthState(h: ModelHealth | null): { icon: string; cls: string } {
  if (!h || (!h.last_success_at && !h.last_error_at) ||
      (h.last_success_at === '0001-01-01T00:00:00Z' && h.last_error_at === '0001-01-01T00:00:00Z')) {
    return { icon: '❓', cls: 'ba-model-unknown' }
  }
  const successTime = new Date(h.last_success_at).getTime()
  const errorTime = new Date(h.last_error_at).getTime()
  const thirtyMin = 30 * 60 * 1000
  const now = Date.now()
  if (successTime > 0 && (now - successTime) < thirtyMin && successTime > errorTime) {
    return { icon: '\u{1F7E2}', cls: 'ba-model-healthy' }
  }
  if (errorTime > successTime && errorTime > 0) {
    return { icon: '\u{1F534}', cls: 'ba-model-error' }
  }
  if (successTime > 0) {
    return { icon: '\u{1F7E1}', cls: 'ba-model-stale' }
  }
  return { icon: '❓', cls: 'ba-model-unknown' }
}

export function BridgeAuth() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [models, setModels] = useState<ModelEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showAddKey, setShowAddKey] = useState(false)
  const [addForm, setAddForm] = useState({ name: '', provider: 'anthropic', owner: '', api_key: '' })
  const [saving, setSaving] = useState(false)
  const instances = useBridgeInstances()
  const [bindingsCache, setBindingsCache] = useState<Record<string, InstanceCredential[]>>({})
  const [expandedCred, setExpandedCred] = useState<string | null>(null)
  const [bindForm, setBindForm] = useState<{ credentialId: string; instance_id: string; priority: number; max_concurrent: number } | null>(null)

  const fetchCredentials = useCallback(async () => {
    try {
      const res = await apiFetch(`${basePath}/credentials`)
      if (!res.ok) { setError(`HTTP ${res.status}`); return }
      setCredentials(await res.json() || [])
      setError(null)
    } catch (err) { setError(`${err}`) }
    finally { setLoading(false) }
  }, [apiFetch, basePath])

  const fetchModels = useCallback(async () => {
    try {
      const res = await apiFetch(`${basePath}/models`)
      if (!res.ok) return
      setModels(await res.json() || [])
    } catch { /* models section is optional — don't block the page */ }
  }, [apiFetch, basePath])

  useEffect(() => {
    fetchCredentials()
    fetchModels()
    const interval = setInterval(() => { fetchCredentials(); fetchModels() }, 30000)
    return () => clearInterval(interval)
  }, [fetchCredentials, fetchModels])

  useEffect(() => {
    for (const inst of instances.instances) {
      instances.getCredentials(inst.id).then(creds => {
        setBindingsCache(prev => ({ ...prev, [inst.id]: creds }))
      })
    }
  }, [instances.instances, instances.getCredentials])

  const toggleCredential = useCallback(async (id: string, enabled: boolean) => {
    try {
      await apiFetch(`${basePath}/credentials/toggle`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, enabled }),
      })
      setCredentials(prev => prev.map(c => c.id === id ? { ...c, enabled } : c))
    } catch { /* ignore */ }
  }, [apiFetch, basePath])

  const handleAddKey = useCallback(async () => {
    if (!addForm.name.trim() || !addForm.owner.trim() || !addForm.api_key.trim()) return
    setSaving(true)
    try {
      const res = await apiFetch(`${basePath}/credentials`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: addForm.name.trim(), provider: addForm.provider, owner: addForm.owner.trim(), type: 'api_key', key: addForm.api_key.trim() }),
      })
      if (!res.ok) { setError(`Failed to add key: ${res.statusText}`); return }
      setAddForm({ name: '', provider: 'anthropic', owner: '', api_key: '' })
      setShowAddKey(false)
      await fetchCredentials()
    } catch (err) { setError(`Failed to add key: ${err}`) }
    finally { setSaving(false) }
  }, [addForm, fetchCredentials, apiFetch, basePath])

  const handleDelete = useCallback(async (id: string) => {
    if (!confirm(`Delete credential "${id}"?`)) return
    try {
      const res = await apiFetch(`${basePath}/credentials/${encodeURIComponent(id)}`, { method: 'DELETE' })
      if (res.ok || res.status === 204) await fetchCredentials()
      else setError(`Delete failed: ${res.statusText}`)
    } catch (err) { setError(`Delete failed: ${err}`) }
  }, [fetchCredentials, apiFetch, basePath])

  const credentialBindings = useCallback((credId: string): Array<{ instance: BridgeInstance; binding: InstanceCredential }> => {
    const results: Array<{ instance: BridgeInstance; binding: InstanceCredential }> = []
    for (const inst of instances.instances) {
      const bindings = bindingsCache[inst.id] || []
      const binding = bindings.find(b => b.credential_id === credId)
      if (binding) results.push({ instance: inst, binding })
    }
    return results
  }, [instances.instances, bindingsCache])

  const handleBind = useCallback(async () => {
    if (!bindForm) return
    const ok = await instances.bindCredential(bindForm.instance_id, bindForm.credentialId, bindForm.priority, bindForm.max_concurrent)
    if (ok) {
      const creds = await instances.getCredentials(bindForm.instance_id)
      setBindingsCache(prev => ({ ...prev, [bindForm.instance_id]: creds }))
      setBindForm(null)
    }
  }, [bindForm, instances])

  const handleUnbind = useCallback(async (instanceId: string, credId: string) => {
    const ok = await instances.unbindCredential(instanceId, credId)
    if (ok) {
      const creds = await instances.getCredentials(instanceId)
      setBindingsCache(prev => ({ ...prev, [instanceId]: creds }))
    }
  }, [instances])

  const isExpired = (expiresAt: number) => expiresAt > 0 && expiresAt < Date.now()

  return (
    <div className="ba-container">
      <div className="ba-header">
        <h2>Auth Management</h2>
        <button className="ba-add-btn" onClick={() => setShowAddKey(true)}>+ Add API Key</button>
      </div>

      {error && <div className="bridge-error">{error} <button className="ba-dismiss" onClick={() => setError(null)}>dismiss</button></div>}

      {showAddKey && (
        <div className="ba-form-card">
          <h3>Add API Key</h3>
          <div className="ba-form-grid">
            <label><span>Name / Label</span><input value={addForm.name} onChange={e => setAddForm(f => ({ ...f, name: e.target.value }))} placeholder="my-anthropic-key" /></label>
            <label><span>Provider</span>
              <select value={addForm.provider} onChange={e => setAddForm(f => ({ ...f, provider: e.target.value }))}>
                {PROVIDERS.map(p => <option key={p.id} value={p.id}>{p.label}</option>)}
              </select>
            </label>
            <label><span>Owner</span><input value={addForm.owner} onChange={e => setAddForm(f => ({ ...f, owner: e.target.value }))} placeholder="you@example.com" /></label>
            <label className="ba-span-full"><span>API Key</span><input type="password" value={addForm.api_key} onChange={e => setAddForm(f => ({ ...f, api_key: e.target.value }))} placeholder="sk-ant-..." /></label>
          </div>
          <div className="ba-form-actions">
            <button className="ba-save-btn" onClick={handleAddKey} disabled={saving || !addForm.name.trim() || !addForm.owner.trim() || !addForm.api_key.trim()}>{saving ? 'Saving...' : 'Save'}</button>
            <button className="ba-cancel-btn" onClick={() => { setShowAddKey(false); setAddForm({ name: '', provider: 'anthropic', owner: '', api_key: '' }) }}>Cancel</button>
          </div>
        </div>
      )}

      {loading ? (
        <div className="ba-loading">Loading credentials...</div>
      ) : credentials.length === 0 ? (
        <div className="ba-empty">No credentials configured. Add an API key to get started.</div>
      ) : (
        <div className="ba-cred-grid">
          {credentials.map(cred => {
            const bindings = credentialBindings(cred.id)
            const isExp = isExpired(cred.expires_at)
            const expanded = expandedCred === cred.id

            return (
              <div key={cred.id} className={`ba-cred-card ${!cred.enabled ? 'ba-cred-disabled' : ''} ${isExp ? 'ba-cred-expired' : ''}`}>
                <div className="ba-cred-header">
                  <span className="ba-cred-provider">{cred.provider}</span>
                  <span className="ba-cred-label">{cred.label}</span>
                  <span className="ba-auth-type">{cred.auth_type}</span>
                  {isExp && <span className="ba-expired-badge">expired</span>}
                </div>

                <div className="ba-cred-details">
                  {cred.api_key_masked && <span className="ba-masked-key">{cred.api_key_masked}</span>}
                  {cred.token_masked && <span className="ba-masked-key">{cred.token_masked}</span>}
                  {(cred.error_count ?? 0) > 0 && <span className="ba-error-count">{cred.error_count} errors</span>}
                  {cred.last_error && <span className="ba-last-error" title={cred.last_error}>{cred.last_error}</span>}
                </div>

                <button className="ba-bindings-toggle" onClick={() => setExpandedCred(expanded ? null : cred.id)}>
                  {expanded ? '\u25BE' : '\u25B8'} Bound to {bindings.length} instance{bindings.length !== 1 ? 's' : ''}
                </button>

                {expanded && (
                  <div className="ba-bindings-section">
                    {bindings.map(({ instance, binding }) => (
                      <div key={instance.id} className="ba-binding-row">
                        <span>{instance.name}</span>
                        <span className="ba-binding-pri">pri {binding.priority}</span>
                        <button className="ba-unbind-btn" onClick={() => handleUnbind(instance.id, cred.id)}>x</button>
                      </div>
                    ))}
                    {bindForm?.credentialId === cred.id ? (
                      <div className="ba-bind-form">
                        <select value={bindForm.instance_id} onChange={e => setBindForm(f => f ? { ...f, instance_id: e.target.value } : f)}>
                          <option value="">Select instance</option>
                          {instances.instances.filter(i => i.enabled && !bindings.some(b => b.instance.id === i.id)).map(i => (
                            <option key={i.id} value={i.id}>{i.name} ({i.harness_type})</option>
                          ))}
                        </select>
                        <input type="number" placeholder="Pri" value={bindForm.priority} onChange={e => setBindForm(f => f ? { ...f, priority: parseInt(e.target.value) || 0 } : f)} style={{ width: 50 }} />
                        <input type="number" placeholder="Max" value={bindForm.max_concurrent} onChange={e => setBindForm(f => f ? { ...f, max_concurrent: parseInt(e.target.value) || 1 } : f)} style={{ width: 50 }} />
                        <button className="ba-save-btn" onClick={handleBind} disabled={!bindForm.instance_id}>Bind</button>
                        <button className="ba-cancel-btn" onClick={() => setBindForm(null)}>x</button>
                      </div>
                    ) : (
                      <button className="ba-add-bind-btn" onClick={() => setBindForm({ credentialId: cred.id, instance_id: '', priority: 0, max_concurrent: 1 })}>+ Bind to instance</button>
                    )}
                  </div>
                )}

                <div className="ba-cred-actions">
                  <button className={`ba-toggle-btn ${cred.enabled ? 'ba-toggle-on' : 'ba-toggle-off'}`} onClick={() => toggleCredential(cred.id, !cred.enabled)}>
                    {cred.enabled ? 'Enabled' : 'Disabled'}
                  </button>
                  <button className="ba-delete-btn" onClick={() => handleDelete(cred.id)}>Delete</button>
                </div>
              </div>
            )
          })}
        </div>
      )}

      {models.length > 0 && (
        <div className="ba-models-section">
          <div className="ba-models-header">
            <h2>Models</h2>
            <span className="ba-models-count">{models.length}</span>
          </div>
          <div className="ba-models-table">
            <div className="ba-models-row ba-models-thead">
              <span className="ba-col-status">Status</span>
              <span className="ba-col-name">Model</span>
              <span className="ba-col-provider">Provider</span>
              <span className="ba-col-priority">Pri</span>
              <span className="ba-col-cost">Cost (in/out)</span>
              <span className="ba-col-response">Avg Response</span>
              <span className="ba-col-last">Last Success</span>
            </div>
            {models.map(m => {
              const hs = modelHealthState(m.health)
              return (
                <div key={m.id} className="ba-model-group">
                  <div className={`ba-models-row ${!m.enabled ? 'ba-model-disabled' : ''} ${hs.cls}`}>
                    <span className="ba-col-status">
                      <span className="ba-status-icon">{hs.icon}</span>
                      <span className={`ba-enabled-badge ${m.enabled ? 'ba-badge-on' : 'ba-badge-off'}`}>
                        {m.enabled ? 'ON' : 'OFF'}
                      </span>
                    </span>
                    <span className="ba-col-name">
                      <strong>{m.name}</strong>
                      <span className="ba-model-id">{m.id}</span>
                    </span>
                    <span className="ba-col-provider">{m.provider}</span>
                    <span className="ba-col-priority">{m.priority}</span>
                    <span className="ba-col-cost">${m.input_cost} / ${m.output_cost}</span>
                    <span className="ba-col-response">
                      {m.health && m.health.avg_response_ms > 0
                        ? `${(m.health.avg_response_ms / 1000).toFixed(1)}s`
                        : '—'}
                    </span>
                    <span className="ba-col-last">
                      {m.health ? timeAgo(m.health.last_success_at) : '—'}
                    </span>
                  </div>
                  {m.credentials && m.credentials.length > 0 && (
                    <div className="ba-cred-subrows">
                      {m.credentials.map(c => (
                        <div key={c.id} className={`ba-cred-subrow ${!c.enabled ? 'ba-model-disabled' : ''}`}>
                          <span className="ba-cred-sub-icon">{c.enabled ? '↳' : '⏸'}</span>
                          <span className="ba-cred-sub-label">
                            {c.label || c.id}
                            {c.api_key_masked && <span className="ba-model-id"> {c.api_key_masked}</span>}
                            {c.token_masked && <span className="ba-model-id"> {c.token_masked}</span>}
                          </span>
                          <span className="ba-cred-sub-type">{c.auth_type}</span>
                          <span className="ba-cred-sub-pri">pri {c.priority}</span>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}
