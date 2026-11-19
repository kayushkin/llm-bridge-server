import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../context'
import type { FetchFn } from '../types'
import { PromptSourcePanel } from './PromptSourcePanel'
import { PROMPT_OUTPUT_STATE_LABEL, renderedOutputsByPath, type PromptCollectionView, type RenderedOutputRef } from '../promptSource'

interface TrackedFile {
  id: number
  path: string
  scope: string
  agent_slug?: string
  enabled: boolean
  fs_hash?: string
  size: number
  mtime: number
  last_scanned_at: number
  status: string
  ignored_by_rule_id?: number
}

interface FileVersion {
  id: number
  tracked_file_id: number
  sha256: string
  size: number
  source: string
  machine_id?: string
  note?: string
  created_at: number
}

interface MachineSeedProfile {
  machine_id: string
  scopes: string[]
  created_at: number
  updated_at: number
}

interface MachineSeedStateRow {
  machine_id: string
  tracked_file_id: number
  observed_sha?: string
  observed_at?: number
  last_pushed_version_id?: number
  last_pushed_at?: number
}

const SCOPE_META: Record<string, { label: string; emoji: string; description: string }> = {
  global:   { label: 'Global',    emoji: '\u{1F310}', description: '~/.claude and $HOME — applies to every session' },
  project:  { label: 'Project',   emoji: '\u{1F4C1}', description: 'Project-root files — applies when the session runs in that project' },
  subagent: { label: 'Subagents', emoji: '\u{1F916}', description: '~/.claude/agents — Claude Code subagent definitions' },
  memory:   { label: 'Memory',    emoji: '\u{1F9E0}', description: '~/.claude/projects/*/memory — auto-loaded by Claude Code per project' },
  command:  { label: 'Commands',  emoji: '⚡',    description: '~/.claude/commands — slash-command definitions' },
}

const SCOPE_ORDER = ['global', 'project', 'subagent', 'memory', 'command']

// What a tracked file is, said without claiming which harness reads it: that
// is a row in agent-store's prompt_harness_deliveries, shown by the delivery
// panel, and a filename table here went stale the moment it was written. The
// three ~/.claude scopes are the exception — those folders belong to one tool.
function usedBy(f: TrackedFile): { label: string; mode: string } {
  if (f.scope === 'subagent') return { label: 'Claude Code subagent', mode: 'subagent' }
  if (f.scope === 'memory') return { label: 'Claude Code memory', mode: 'memory' }
  if (f.scope === 'command') return { label: 'Claude Code commands', mode: 'native' }
  if (f.scope === 'inber') return { label: 'inber agent', mode: 'inber' }
  return { label: 'prompt file', mode: 'native' }
}

export function BridgeFiles() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const [files, setFiles] = useState<TrackedFile[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  const [scanning, setScanning] = useState(false)
  const [scanMsg, setScanMsg] = useState<string | null>(null)
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [openId, setOpenId] = useState<number | null>(null)
  const [showIgnored, setShowIgnored] = useState(false)
  const [showMissing, setShowMissing] = useState(false)
  const [promptSourceReloadSignal, setPromptSourceReloadSignal] = useState(0)
  const [renderedByPath, setRenderedByPath] = useState<Map<string, RenderedOutputRef>>(new Map())
  const [openCollection, setOpenCollection] = useState<{ id: number; nonce: number } | null>(null)
  const onPromptViews = useCallback((views: PromptCollectionView[]) => setRenderedByPath(renderedOutputsByPath(views)), [])

  const fetchFiles = async () => {
    try {
      const res = await apiFetch(`${basePath}/files${showIgnored ? '?include_ignored=true' : ''}`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json()
      setFiles(Array.isArray(data) ? data : [])
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to fetch')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { fetchFiles() }, [showIgnored])

  const runScan = async () => {
    setScanning(true)
    setScanMsg(null)
    try {
      const res = await apiFetch(`${basePath}/files/scan`, { method: 'POST' })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const r = await res.json()
      // `updated` counts files whose content hash changed. It used to count
      // every pre-existing row, so this line read "154 updated" after every
      // scan whether or not a single byte moved. Show `unchanged` alongside it
      // so the number has a denominator and a zero is legible as a no-op.
      let msg = `Scanned ${r.scanned} · ${r.added} new · ${r.updated} updated · ${r.unchanged} unchanged · ${r.missing} missing`
      // `errors` names the files the scan could not account for. Until
      // agent-store started reporting them, a file that failed to hash or to
      // upsert was dropped from every counter above, so `Scanned 153` on a box
      // with 154 tracked files rendered as a complete run. When any are
      // present, `scanned` is an undercount and this line has to say so.
      //
      // Rendered only when the list is non-empty, and never as "0 failed": a
      // server too old to report the field would otherwise have this line
      // asserting a clean run on its behalf, which is the same lie in a new
      // place. The paths are listed because the question an agent runs a scan
      // to answer is whether one particular file landed, and a count cannot
      // answer it.
      if (Array.isArray(r.errors) && r.errors.length > 0) {
        const named = r.errors
          .map((e: { path: string; stage: string; error: string }) => `${e.path} (${e.stage}: ${e.error})`)
          .join(' · ')
        msg += ` — ${r.errors.length} unaccounted for, so scanned is an undercount: ${named}`
      }
      if (typeof r.ignored === 'number' && r.ignored > 0) msg += ` · ${r.ignored} ignored (copies and third-party files)`
      const reconciliation = r.prompt_drift_reconciliation
      if (reconciliation && Array.isArray(reconciliation.detected) && reconciliation.detected.length > 0) {
        const applied = reconciliation.detected.filter((d: { status: string }) => d.status === 'applied').length
        const held = reconciliation.detected.length - applied
        msg += ` — ${reconciliation.detected.length} prompt file${reconciliation.detected.length === 1 ? '' : 's'} edited on disk: ${applied} carried into the sections, ${held} waiting for you`
      }
      if (reconciliation && Array.isArray(reconciliation.refused) && reconciliation.refused.length > 0) {
        msg += ` — ${reconciliation.refused.join(' · ')}`
      }
      setScanMsg(msg)
      setPromptSourceReloadSignal(n => n + 1)
      await fetchFiles()
    } catch (e) {
      setScanMsg(e instanceof Error ? e.message : 'Scan failed')
    } finally {
      setScanning(false)
    }
  }

  const toggleEnabled = async (f: TrackedFile) => {
    const action = f.enabled ? 'disable' : 'enable'
    const res = await apiFetch(`${basePath}/files/${f.id}/${action}`, { method: 'POST' })
    if (res.ok) {
      const updated: TrackedFile = await res.json()
      setFiles(prev => prev.map(x => x.id === f.id ? updated : x))
    } else {
      alert(await res.text())
    }
  }

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    const shown = showMissing ? files : files.filter(f => f.status !== 'missing')
    if (!q) return shown
    return shown.filter(f =>
      f.path.toLowerCase().includes(q) ||
      (f.agent_slug || '').toLowerCase().includes(q) ||
      f.scope.toLowerCase().includes(q) ||
      usedBy(f).label.toLowerCase().includes(q)
    )
  }, [files, query, showMissing])

  const byScope = useMemo(() => {
    const m: Record<string, TrackedFile[]> = {}
    for (const f of filtered) {
      (m[f.scope] ||= []).push(f)
    }
    return m
  }, [filtered])

  const toggleSection = (scope: string) => {
    setCollapsed(prev => {
      const next = new Set(prev)
      if (next.has(scope)) next.delete(scope)
      else next.add(scope)
      return next
    })
  }

  if (loading) return <div className="bfiles-container"><p>Loading…</p></div>
  if (error) return (
    <div className="bfiles-container">
      <p className="bridge-error">Error: {error}</p>
      <button onClick={() => { setLoading(true); fetchFiles() }} className="bfiles-btn">Retry</button>
    </div>
  )

  const scopes = SCOPE_ORDER.filter(s => byScope[s]?.length)

  return (
    <div className="bfiles-container">
      <div className="bfiles-header">
        <h2>Prompt and agent files <span className="bfiles-count">{files.length}</span></h2>
        <div className="bfiles-header-right">
          <input
            type="text"
            placeholder="Search path / slug / scope / agent…"
            value={query}
            onChange={e => setQuery(e.target.value)}
            className="bfiles-search"
          />
          <button className="bfiles-btn" onClick={fetchFiles}>Refresh</button>
          <button className="bfiles-btn-primary" onClick={runScan} disabled={scanning}>
            {scanning ? 'Scanning…' : 'Scan disk'}
          </button>
        </div>
      </div>

      {scanMsg && <p className="bfiles-scan-msg">{scanMsg}</p>}

      <PromptSourcePanel apiFetch={apiFetch} basePath={basePath} reloadSignal={promptSourceReloadSignal} onViews={onPromptViews} openCollection={openCollection} />

      <MachinesSeedPanel apiFetch={apiFetch} basePath={basePath} />


      <div className="bfiles-preview-section">
        <div className="bfiles-preview-header">
          <strong>Files on disk</strong>
          <span className="bfiles-preview-hint">
            Everything the scan tracks, each with its full history. A prompt file rendered from sections is shown as
            what it is — open its sections to change it. Subagents, memory and commands are plain files and edit here.
          </span>
        </div>
        <div className="bfiles-actions">
          <label className="bfiles-meta"><input type="checkbox" checked={showMissing} onChange={e => setShowMissing(e.target.checked)} /> show files no longer on disk ({files.filter(f => f.status === 'missing').length})</label>
          <label className="bfiles-meta"><input type="checkbox" checked={showIgnored} onChange={e => setShowIgnored(e.target.checked)} /> show ignored copies and third-party files</label>
        </div>
      </div>

      {scopes.length === 0 && <p className="bfiles-empty">No files indexed. Click Scan disk.</p>}

      {scopes.map(scope => {
        const meta = SCOPE_META[scope] || { label: scope, emoji: '\u{1F4C4}', description: '' }
        const items = byScope[scope]
        const isCollapsed = collapsed.has(scope)
        return (
          <div key={scope} className={`bfiles-section ${isCollapsed ? 'bfiles-section-collapsed' : ''}`}>
            <h3
              className={`bfiles-section-title ${isCollapsed ? 'bfiles-collapsed' : ''}`}
              onClick={() => toggleSection(scope)}
            >
              <span className="bfiles-scope-emoji">{meta.emoji}</span>
              {meta.label}
              <span className="bfiles-section-count">{items.length}</span>
              {meta.description && <span className="bfiles-scope-description">{meta.description}</span>}
            </h3>
            {!isCollapsed && (
              <ul className="bfiles-file-list">
                {items
                  .slice()
                  .sort((a, b) => a.path.localeCompare(b.path))
                  .map(f => (
                    <FileRow
                      key={f.id}
                      file={f}
                      apiFetch={apiFetch}
                      basePath={basePath}
                      expanded={openId === f.id}
                      onToggle={() => setOpenId(openId === f.id ? null : f.id)}
                      onToggleEnabled={() => toggleEnabled(f)}
                      onSaved={updated => setFiles(prev => prev.map(x => x.id === updated.id ? updated : x))}
                      renderedFrom={renderedByPath.get(f.path)}
                      onOpenSections={collectionId => setOpenCollection(prev => ({ id: collectionId, nonce: (prev?.nonce ?? 0) + 1 }))}
                    />
                  ))}
              </ul>
            )}
          </div>
        )
      })}
    </div>
  )
}

function FileRow({ file, apiFetch, basePath, expanded, onToggle, onToggleEnabled, onSaved, renderedFrom, onOpenSections }: {
  file: TrackedFile
  apiFetch: FetchFn
  basePath: string
  expanded: boolean
  onToggle: () => void
  onToggleEnabled: () => void
  onSaved: (f: TrackedFile) => void
  /** Set when a prompt collection renders this file: it is then not edited here. */
  renderedFrom?: RenderedOutputRef
  onOpenSections: (collectionId: number) => void
}) {
  const [content, setContent] = useState<string | null>(null)
  const [draft, setDraft] = useState<string>('')
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const dirty = content !== null && draft !== content

  useEffect(() => {
    if (!expanded || content !== null) return
    setLoading(true)
    apiFetch(`${basePath}/files/${file.id}/content`)
      .then(async r => {
        if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`)
        return r.json()
      })
      .then(j => {
        setContent(j.content || '')
        setDraft(j.content || '')
        setErr(null)
      })
      .catch(e => setErr(e instanceof Error ? e.message : 'Load failed'))
      .finally(() => setLoading(false))
  }, [expanded, content, file.id, apiFetch, basePath])

  const save = async () => {
    setSaving(true)
    setErr(null)
    try {
      const res = await apiFetch(`${basePath}/files/${file.id}/content`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: draft }),
      })
      if (!res.ok) throw new Error(await res.text() || `HTTP ${res.status}`)
      const updated: TrackedFile = await res.json()
      setContent(draft)
      onSaved(updated)
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const basename = file.path.split('/').pop() || file.path
  const parent = file.path.substring(0, file.path.length - basename.length - 1)
  const consumer = usedBy(file)

  return (
    <li className={`bfiles-file ${file.enabled ? '' : 'bfiles-file-disabled'}`}>
      <div className="bfiles-file-header">
        <button className="bfiles-caret" onClick={onToggle}>{expanded ? '▾' : '▸'}</button>
        <button className="bfiles-file-title" onClick={onToggle}>
          <span className="bfiles-file-basename">{basename}</span>
          <span className="bfiles-file-parent">{parent}</span>
        </button>
        {renderedFrom ? (
          <span className="bfiles-usedby-tag bfiles-mode-native" title="Its text is that prompt's sections">
            rendered from {renderedFrom.scope === 'global' ? 'the host prompt' : renderedFrom.collectionTitle} · {PROMPT_OUTPUT_STATE_LABEL[renderedFrom.state]}
          </span>
        ) : (
          <span className={`bfiles-usedby-tag bfiles-mode-${consumer.mode}`} title={`Consumed by: ${consumer.label}`}>
            {consumer.label}
          </span>
        )}
        {file.agent_slug && <span className="bfiles-slug-tag">{file.agent_slug}</span>}
        {file.status === 'missing' && <span className="bfiles-missing-tag">missing</span>}
        {file.ignored_by_rule_id ? <span className="bfiles-missing-tag" title={`ignore rule ${file.ignored_by_rule_id}`}>ignored</span> : null}
        <button
          className={`bfiles-toggle-btn ${file.enabled ? 'bfiles-toggle-on' : 'bfiles-toggle-off'}`}
          onClick={onToggleEnabled}
          title={file.enabled ? 'Disable (rename to .disabled)' : 'Enable'}
        >
          {file.enabled ? 'ON' : 'OFF'}
        </button>
      </div>
      {expanded && (
        <div className="bfiles-file-body">
          {loading && <p>Loading…</p>}
          {err && <p className="bridge-error">{err}</p>}
          {content !== null && renderedFrom && (
            <>
              <div className="bfiles-actions">
                <button className="bfiles-btn-primary" onClick={() => onOpenSections(renderedFrom.collectionId)}>Open its sections</button>
                <span className="bfiles-meta">
                  This file is a render of {renderedFrom.scope === 'global' ? 'the host prompt' : `the ${renderedFrom.collectionTitle} prompt`}, so it
                  is changed in its sections, not here. An edit made to the file itself is still picked up by the next scan.
                  · {(file.size / 1024).toFixed(1)} KB · mtime {new Date(file.mtime * 1000).toLocaleString()}
                </span>
              </div>
              <pre className="bfiles-preview-content">{content}</pre>
              <FileHistory fileID={file.id} apiFetch={apiFetch} basePath={basePath} />
            </>
          )}
          {content !== null && !renderedFrom && (
            <>
              <textarea
                className="bfiles-editor"
                value={draft}
                onChange={e => setDraft(e.target.value)}
                spellCheck={false}
              />
              <div className="bfiles-actions">
                <button
                  className="bfiles-btn-primary"
                  onClick={save}
                  disabled={!dirty || saving}
                >
                  {saving ? 'Saving…' : dirty ? 'Save' : 'Saved'}
                </button>
                {dirty && (
                  <button className="bfiles-btn" onClick={() => setDraft(content)}>
                    Revert
                  </button>
                )}
                <span className="bfiles-meta">
                  {(file.size / 1024).toFixed(1)} KB · mtime {new Date(file.mtime * 1000).toLocaleString()}
                </span>
              </div>
              <FileHistory
                fileID={file.id}
                apiFetch={apiFetch}
                basePath={basePath}
                onRestore={async (versionContent) => { setDraft(versionContent) }}
              />
            </>
          )}
        </div>
      )}
    </li>
  )
}

function FileHistory({ fileID, apiFetch, basePath, onRestore }: {
  fileID: number
  apiFetch: FetchFn
  basePath: string
  /** Absent for a rendered file, which has no editor to load a version into. */
  onRestore?: (content: string) => void
}) {
  const [versions, setVersions] = useState<FileVersion[] | null>(null)
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [previewID, setPreviewID] = useState<number | null>(null)
  const [previewBody, setPreviewBody] = useState<string | null>(null)

  const load = async () => {
    setLoading(true)
    setErr(null)
    try {
      const res = await apiFetch(`${basePath}/files/${fileID}/versions`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setVersions(await res.json())
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { if (open && versions === null) load() }, [open])

  const loadPreview = async (vid: number) => {
    setPreviewID(vid)
    setPreviewBody(null)
    try {
      const res = await apiFetch(`${basePath}/versions/${vid}/content`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const j = await res.json()
      setPreviewBody(j.content || '')
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'preview failed')
    }
  }

  return (
    <div className="bfiles-history">
      <button className="bfiles-btn" onClick={() => setOpen(!open)}>
        {open ? '▾' : '▸'} History {versions ? `(${versions.length})` : ''}
      </button>
      {open && (
        <div className="bfiles-history-body">
          {loading && <p>Loading…</p>}
          {err && <p className="bridge-error">{err}</p>}
          {versions && versions.length === 0 && (
            <p className="bfiles-empty">No history yet — first save will create a version.</p>
          )}
          {versions && versions.length > 0 && (
            <ul className="bfiles-version-list">
              {versions.map(v => (
                <li key={v.id} className="bfiles-version-row">
                  <span className="bfiles-version-time">{new Date(v.created_at * 1000).toLocaleString()}</span>
                  <span className="bfiles-version-source">{v.source}</span>
                  {v.machine_id && <span className="bfiles-version-machine">from {v.machine_id}</span>}
                  <code className="bfiles-version-sha">{v.sha256.slice(0, 12)}</code>
                  <span className="bfiles-version-size">{v.size} B</span>
                  {v.note && <span className="bfiles-version-note">{v.note}</span>}
                  <button className="bfiles-btn" onClick={() => loadPreview(v.id)}>View</button>
                </li>
              ))}
            </ul>
          )}
          {previewID && previewBody !== null && (
            <div className="bfiles-version-preview">
              <div className="bfiles-version-preview-header">
                <strong>Version #{previewID}</strong>
                {onRestore && <button className="bfiles-btn" onClick={() => onRestore(previewBody)}>Load into editor</button>}
                <button className="bfiles-btn" onClick={() => { setPreviewID(null); setPreviewBody(null) }}>Close</button>
              </div>
              <pre className="bfiles-version-content">{previewBody}</pre>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function MachinesSeedPanel({ apiFetch, basePath }: { apiFetch: FetchFn; basePath: string }) {
  const [profiles, setProfiles] = useState<MachineSeedProfile[]>([])
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [statesByMachine, setStatesByMachine] = useState<Record<string, MachineSeedStateRow[]>>({})

  const load = async () => {
    setLoading(true)
    setErr(null)
    try {
      const res = await apiFetch(`${basePath}/seed/profiles`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const list: MachineSeedProfile[] = await res.json()
      setProfiles(list)
      const states: Record<string, MachineSeedStateRow[]> = {}
      await Promise.all(list.map(async p => {
        const r = await apiFetch(`${basePath}/seed/state?machine_id=${encodeURIComponent(p.machine_id)}`)
        if (r.ok) states[p.machine_id] = await r.json()
      }))
      setStatesByMachine(states)
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { if (open && profiles.length === 0 && !loading) load() }, [open])

  return (
    <div className="bfiles-machines-panel">
      <div className="bfiles-machines-header">
        <button className="bfiles-btn" onClick={() => setOpen(!open)}>
          {open ? '▾' : '▸'} Runners & seed state
        </button>
        <button className="bfiles-btn" onClick={load} disabled={loading}>
          {loading ? 'Loading…' : 'Refresh'}
        </button>
        <span className="bfiles-machines-hint">
          Per-machine view of which files each runner has on disk.
        </span>
      </div>
      {open && (
        <div className="bfiles-machines-body">
          {err && <p className="bridge-error">{err}</p>}
          {!loading && profiles.length === 0 && (
            <p className="bfiles-empty">No runners enrolled yet.</p>
          )}
          {profiles.map(p => {
            const rows = statesByMachine[p.machine_id] || []
            const observed = rows.filter(r => r.observed_sha).length
            return (
              <div key={p.machine_id} className="bfiles-machine-card">
                <strong>{p.machine_id}</strong>
                <span className="bfiles-machine-scopes">scopes: {p.scopes.join(', ')}</span>
                <span className="bfiles-machine-stats">{observed} files observed / {rows.length} state rows</span>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
