import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../context'

interface Source {
  id: number
  name: string
  repo_url: string
  branch: string
  trust_tier: string
  license?: string
  last_skill_count: number
  last_synced_at?: number
  enabled: boolean
}

interface Skill {
  id: number
  source_id: number
  source_name: string
  name: string
  display_name?: string
  description?: string
  tags?: string[]
  file_count: number
  source_path: string
  installed: boolean
  install_path?: string
  installed_at?: number
}

interface SyncResult {
  found: number
  inserted: number
  updated: number
  pruned: number
  commit: string
  errors?: string[]
}

const TIERS: Record<string, string> = {
  official: 'bsk-tier-official',
  community: 'bsk-tier-community',
  personal: 'bsk-tier-personal',
}

export function BridgeSkills() {
  const { fetch: apiFetch, skillStoreBasePath } = useBridgeConfig()

  const [sources, setSources] = useState<Source[]>([])
  const [skills, setSkills] = useState<Skill[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const [query, setQuery] = useState('')
  const [sourceFilter, setSourceFilter] = useState('')
  const [installedOnly, setInstalledOnly] = useState(false)
  const [busy, setBusy] = useState<Record<number, boolean>>({})
  const [syncing, setSyncing] = useState<number | null>(null)
  const [syncResult, setSyncResult] = useState<Record<number, SyncResult>>({})

  const refetch = useCallback(async () => {
    if (!skillStoreBasePath) return
    setLoading(true)
    setError(null)
    try {
      const [srcRes, skillRes] = await Promise.all([
        apiFetch(`${skillStoreBasePath}/sources`),
        apiFetch(`${skillStoreBasePath}/skills?limit=5000`),
      ])
      if (!srcRes.ok || !skillRes.ok) throw new Error('skill-store unreachable')
      setSources(await srcRes.json())
      setSkills(await skillRes.json())
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [apiFetch, skillStoreBasePath])

  useEffect(() => { refetch() }, [refetch])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return skills.filter(s => {
      if (sourceFilter && s.source_name !== sourceFilter) return false
      if (installedOnly && !s.installed) return false
      if (!q) return true
      return s.name.toLowerCase().includes(q) || (s.description || '').toLowerCase().includes(q)
    })
  }, [skills, query, sourceFilter, installedOnly])

  const installedCount = useMemo(() => skills.filter(s => s.installed).length, [skills])

  const toggle = useCallback(async (skill: Skill, force = false) => {
    if (!skillStoreBasePath) return
    setBusy(prev => ({ ...prev, [skill.id]: true }))
    try {
      const action = skill.installed ? 'uninstall' : 'install'
      const qs = action === 'install' && force ? '?force=true' : ''
      const res = await apiFetch(`${skillStoreBasePath}/skills/${skill.id}/${action}${qs}`, { method: 'POST' })
      if (res.status === 409) {
        const body = await res.json().catch(() => ({}))
        const ok = window.confirm(`${body.error || 'Conflict'}\n\nReplace the existing install?`)
        if (ok) {
          await toggle(skill, true)
          return
        }
      } else if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body.error || `HTTP ${res.status}`)
      }
      await refetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(prev => ({ ...prev, [skill.id]: false }))
    }
  }, [apiFetch, skillStoreBasePath, refetch])

  const syncSource = useCallback(async (source: Source) => {
    if (!skillStoreBasePath) return
    setSyncing(source.id)
    setError(null)
    try {
      const res = await apiFetch(`${skillStoreBasePath}/sources/${source.id}/sync`, { method: 'POST' })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body.error || `HTTP ${res.status}`)
      }
      const data = await res.json()
      setSyncResult(prev => ({ ...prev, [source.id]: data }))
      await refetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSyncing(null)
    }
  }, [apiFetch, skillStoreBasePath, refetch])

  if (!skillStoreBasePath) {
    return (
      <div className="bsk-container">
        <div className="bridge-error">Skill-store is not configured. Pass <code>skillStoreBasePath</code> to <code>&lt;BridgeProvider&gt;</code>.</div>
      </div>
    )
  }

  return (
    <div className="bsk-container">
      <div className="bsk-header">
        <h2 className="bsk-title">Skills</h2>
        <p className="bsk-subtitle">Install skills into <code>~/.claude/skills</code> so Claude Code picks them up.</p>
      </div>

      {error && <div className="bridge-error">{error}</div>}

      <section className="bsk-sources">
        <h3 className="bsk-section-title">Sources</h3>
        <div className="bsk-source-grid">
          {sources.map(src => {
            const result = syncResult[src.id]
            return (
              <div key={src.id} className="bsk-source-card">
                <div className="bsk-source-head">
                  <span className="bsk-source-name">{src.name}</span>
                  <span className={`bsk-tier ${TIERS[src.trust_tier] || ''}`}>{src.trust_tier}</span>
                  {src.license && <span className="bsk-license">{src.license}</span>}
                </div>
                <div className="bsk-source-meta">
                  <a href={src.repo_url.replace(/\.git$/, '')} target="_blank" rel="noreferrer" className="bsk-source-url">{src.repo_url}</a>
                </div>
                <div className="bsk-source-footer">
                  <span className="bsk-source-count">{src.last_skill_count} skills</span>
                  <button
                    className="bsk-sync-btn"
                    onClick={() => syncSource(src)}
                    disabled={syncing === src.id}
                  >
                    {syncing === src.id ? 'Syncing…' : 'Sync'}
                  </button>
                </div>
                {result && (
                  <div className="bsk-source-result">
                    +{result.inserted} / ~{result.updated} / -{result.pruned}
                    {result.errors && result.errors.length > 0 && <span className="bsk-source-warn"> · {result.errors.length} warnings</span>}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      </section>

      <section className="bsk-skills">
        <div className="bsk-toolbar">
          <input
            className="bsk-search"
            type="text"
            placeholder={`Search ${skills.length} skills…`}
            value={query}
            onChange={e => setQuery(e.target.value)}
          />
          <select className="bsk-filter" value={sourceFilter} onChange={e => setSourceFilter(e.target.value)}>
            <option value="">All sources</option>
            {sources.map(s => <option key={s.id} value={s.name}>{s.name}</option>)}
          </select>
          <label className="bsk-check">
            <input type="checkbox" checked={installedOnly} onChange={e => setInstalledOnly(e.target.checked)} />
            <span>Installed only ({installedCount})</span>
          </label>
          <span className="bsk-count">{filtered.length} / {skills.length}</span>
        </div>

        {loading ? (
          <div className="bsk-empty">Loading…</div>
        ) : filtered.length === 0 ? (
          <div className="bsk-empty">No skills match.</div>
        ) : (
          <ul className="bsk-list">
            {filtered.map(skill => (
              <li key={skill.id} className={`bsk-row ${skill.installed ? 'bsk-installed' : ''}`}>
                <div className="bsk-row-main">
                  <div className="bsk-row-head">
                    <span className="bsk-skill-name">{skill.name}</span>
                    <span className="bsk-source-chip">{skill.source_name}</span>
                    {skill.file_count > 0 && <span className="bsk-file-count">{skill.file_count} files</span>}
                  </div>
                  {skill.description && <div className="bsk-row-desc">{skill.description}</div>}
                  {skill.tags && skill.tags.length > 0 && (
                    <div className="bsk-tags">
                      {skill.tags.slice(0, 6).map(t => <span key={t} className="bsk-tag">{t}</span>)}
                    </div>
                  )}
                </div>
                <div className="bsk-row-actions">
                  <button
                    className={`bsk-toggle ${skill.installed ? 'bsk-toggle-on' : 'bsk-toggle-off'}`}
                    onClick={() => toggle(skill)}
                    disabled={busy[skill.id]}
                    title={skill.installed ? skill.install_path : 'Install to ~/.claude/skills'}
                  >
                    {busy[skill.id] ? '…' : skill.installed ? 'Installed' : 'Install'}
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}
