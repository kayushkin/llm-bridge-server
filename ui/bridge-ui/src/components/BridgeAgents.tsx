import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from '../context'

interface AgentEntry {
  name: string
  display_name?: string
  orchestrator: string
  emoji?: string
  project?: string
  enabled: boolean
  is_default?: boolean
  orch_emoji?: string
  status: string
  status_task?: string
  status_session_id?: string
  status_since?: number
}

export function BridgeAgents() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const [agents, setAgents] = useState<AgentEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [showAdd, setShowAdd] = useState(false)
  const [addForm, setAddForm] = useState({ slug: '', display_name: '', emoji: '', projects: '', description: '' })
  const [editing, setEditing] = useState<string | null>(null)
  const [editDesc, setEditDesc] = useState('')

  const fetchAgents = useCallback(async () => {
    try {
      const res = await apiFetch(`${basePath}/agents?expanded=true`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json()
      setAgents(data || [])
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to fetch')
    } finally {
      setLoading(false)
    }
  }, [apiFetch, basePath])

  useEffect(() => {
    fetchAgents()
    const interval = setInterval(fetchAgents, 30000)
    return () => clearInterval(interval)
  }, [fetchAgents])

  const addAgent = async () => {
    if (!addForm.slug.trim()) return
    try {
      const res = await apiFetch(`${basePath}/agents`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          slug: addForm.slug.trim(),
          display_name: addForm.display_name.trim(),
          emoji: addForm.emoji.trim(),
          projects: addForm.projects.trim(),
          description: addForm.description.trim(),
          enabled: true,
        }),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        setError(data.error || `HTTP ${res.status}`)
        return
      }
      setAddForm({ slug: '', display_name: '', emoji: '', projects: '', description: '' })
      setShowAdd(false)
      fetchAgents()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to add')
    }
  }

  const updateAgent = async (slug: string, body: Record<string, unknown>) => {
    const res = await apiFetch(`${basePath}/agents/${encodeURIComponent(slug)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      setError(data.error || `HTTP ${res.status}`)
      return false
    }
    return true
  }

  const deleteAgent = async (slug: string) => {
    if (!confirm(`Remove agent "${slug}"?`)) return
    const res = await apiFetch(`${basePath}/agents/${encodeURIComponent(slug)}`, { method: 'DELETE' })
    if (res.ok) fetchAgents()
    else setError(`Delete failed: ${res.statusText}`)
  }

  const toggleEnabled = async (a: AgentEntry) => {
    const ok = await updateAgent(a.name, {
      slug: a.name,
      display_name: a.display_name || '',
      emoji: a.emoji || '',
      projects: a.project || '',
      enabled: !a.enabled,
    })
    if (ok) fetchAgents()
  }

  const saveEdit = async (a: AgentEntry) => {
    const ok = await updateAgent(a.name, {
      slug: a.name,
      display_name: a.display_name || '',
      emoji: a.emoji || '',
      projects: a.project || '',
      description: editDesc,
      enabled: a.enabled,
    })
    if (ok) {
      setEditing(null)
      fetchAgents()
    }
  }

  const toggleSection = (orch: string) => {
    setCollapsed(prev => {
      const next = new Set(prev)
      if (next.has(orch)) next.delete(orch)
      else next.add(orch)
      return next
    })
  }

  if (loading) return <div className="bagents-container"><p>Loading...</p></div>
  if (error && agents.length === 0) return (
    <div className="bagents-container">
      <p className="bridge-error">Error: {error}</p>
      <button className="bagents-btn" onClick={() => { setLoading(true); fetchAgents() }}>Retry</button>
    </div>
  )

  const grouped = agents.reduce<Record<string, AgentEntry[]>>((acc, a) => {
    const key = a.orchestrator
    if (!acc[key]) acc[key] = []
    acc[key].push(a)
    return acc
  }, {})

  const orchIcon = (orch: string) => {
    const list = grouped[orch] ?? []
    const def = list.find(a => a.is_default)
    return (def ?? list[0])?.orch_emoji || (def ?? list[0])?.emoji || ''
  }

  return (
    <div className="bagents-container">
      <div className="bagents-header">
        <h2>Agent Registry</h2>
        <div className="bagents-header-right">
          <span className="bagents-count">{agents.length} agents</span>
          <button className="bagents-add-btn" onClick={() => setShowAdd(!showAdd)}>
            {showAdd ? 'Cancel' : '+ Add Agent'}
          </button>
        </div>
      </div>

      {error && <div className="bridge-error">{error} <button className="bagents-dismiss" onClick={() => setError(null)}>dismiss</button></div>}

      {showAdd && (
        <div className="bagents-add-form">
          <input
            className="bagents-input"
            placeholder="slug (required)"
            value={addForm.slug}
            onChange={e => setAddForm(f => ({ ...f, slug: e.target.value }))}
          />
          <input
            className="bagents-input"
            placeholder="display name"
            value={addForm.display_name}
            onChange={e => setAddForm(f => ({ ...f, display_name: e.target.value }))}
          />
          <input
            className="bagents-input bagents-emoji-input"
            placeholder="emoji"
            value={addForm.emoji}
            onChange={e => setAddForm(f => ({ ...f, emoji: e.target.value }))}
          />
          <input
            className="bagents-input"
            placeholder="projects (comma-separated)"
            value={addForm.projects}
            onChange={e => setAddForm(f => ({ ...f, projects: e.target.value }))}
          />
          <input
            className="bagents-input"
            placeholder="description"
            value={addForm.description}
            onChange={e => setAddForm(f => ({ ...f, description: e.target.value }))}
          />
          <button className="bagents-save-btn" onClick={addAgent}>Add</button>
        </div>
      )}

      {Object.entries(grouped).sort().map(([orch, orchAgents]) => {
        const isCollapsed = collapsed.has(orch)
        return (
          <div key={orch} className={`bagents-section ${isCollapsed ? 'bagents-collapsed' : ''}`}>
            <h3 className="bagents-section-title" onClick={() => toggleSection(orch)}>
              <span className="bagents-section-icon">{orchIcon(orch)}</span>
              {orch}
              <span className="bagents-section-count">{orchAgents.length}</span>
            </h3>
            {!isCollapsed && (
              <div className="bagents-grid">
                {orchAgents.map(a => (
                  <div key={`${a.name}:${a.orchestrator}`} className={`bagents-card ${a.enabled ? 'bagents-card-enabled' : 'bagents-card-disabled'}`}>
                    {editing === a.name ? (
                      <div className="bagents-edit-form">
                        <div className="bagents-edit-name">{a.emoji} {a.display_name || a.name}</div>
                        <input
                          className="bagents-input"
                          placeholder="Description"
                          value={editDesc}
                          onChange={e => setEditDesc(e.target.value)}
                          onKeyDown={e => e.key === 'Enter' && saveEdit(a)}
                        />
                        <div className="bagents-edit-actions">
                          <button className="bagents-save-btn" onClick={() => saveEdit(a)}>Save</button>
                          <button className="bagents-cancel-btn" onClick={() => setEditing(null)}>Cancel</button>
                        </div>
                      </div>
                    ) : (
                      <>
                        <div className="bagents-card-header">
                          <span className="bagents-card-name">
                            {a.emoji && <span className="bagents-card-emoji">{a.emoji}</span>}
                            {a.display_name || a.name}
                          </span>
                          <button
                            className={`bagents-toggle-btn ${a.enabled ? 'bagents-toggle-on' : 'bagents-toggle-off'}`}
                            onClick={() => toggleEnabled(a)}
                            title={a.enabled ? 'Click to disable' : 'Click to enable'}
                          >
                            {a.enabled ? 'ON' : 'OFF'}
                          </button>
                        </div>
                        {a.project && <div className="bagents-card-project">{a.project}</div>}
                        {a.status && a.status !== 'idle' && (
                          <div className="bagents-card-status">
                            <span className="bagents-status-dot" />
                            {a.status}
                            {a.status_task && <span className="bagents-status-task"> — {a.status_task}</span>}
                          </div>
                        )}
                        <div className="bagents-card-actions">
                          <button className="bagents-edit-btn" onClick={() => { setEditing(a.name); setEditDesc('') }}>Edit</button>
                          <button className="bagents-delete-btn" onClick={() => deleteAgent(a.name)}>Delete</button>
                        </div>
                      </>
                    )}
                  </div>
                ))}
              </div>
            )}
          </div>
        )
      })}

      {agents.length === 0 && (
        <div className="bagents-empty">
          <p>No agents registered. Click "Add Agent" to get started.</p>
        </div>
      )}
    </div>
  )
}
