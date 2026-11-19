import { useMemo, useState } from 'react'
import { useBridgeInstances } from '../useBridgeInstances'
import { useBridgeTools } from '../useBridgeTools'
import type { Tool } from '@kayushkin/tool-store-types'

/**
 * Top-level Tools page. Lists every tool registered in tool-store with
 * description, kind, tags, and toggles for both global enable/disable and
 * per-instance opt-in. Discovery section at the bottom shows in-process
 * locals not yet upserted into the registry.
 *
 * Global is master: per-instance toggle is disabled (and flipped off) when
 * the tool is globally disabled. UI mirrors backend semantics.
 */
export function BridgeTools() {
  const { instances, loading: instancesLoading } = useBridgeInstances()
  const [selectedInstanceID, setSelectedInstanceID] = useState<string | null>(null)
  const { tools, locals, byInstance, loading, error, setGlobal, setForInstance } = useBridgeTools(selectedInstanceID)
  const [expanded, setExpanded] = useState<Set<number>>(new Set())

  const enabledTools = instances.filter(i => i.enabled)
  const localToolNames = useMemo(() => new Set(tools.filter(t => t.kind === 'local').map(t => t.name)), [tools])
  const orphanLocals = useMemo(
    () => locals.filter(l => !localToolNames.has(l.name)),
    [locals, localToolNames],
  )

  const toggleExpanded = (id: number) => {
    setExpanded(prev => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  return (
    <div className="bt-container">
      <div className="bt-header">
        <h2>Tools</h2>
        <button className="bi-add-btn" onClick={() => window.location.reload()}>↻ Refresh</button>
      </div>

      {error && <div className="bridge-error">{error}</div>}

      <div className="bt-instance-row">
        <label>Per-instance view:</label>
        <select
          className="bt-instance-select"
          value={selectedInstanceID ?? ''}
          onChange={e => setSelectedInstanceID(e.target.value || null)}
          disabled={instancesLoading || enabledTools.length === 0}
        >
          <option value="">— none (global view) —</option>
          {enabledTools.map(i => (
            <option key={i.id} value={i.id}>
              {i.harness_type} · {i.name} ({i.id})
            </option>
          ))}
        </select>
      </div>

      {loading ? (
        <div className="bi-loading">Loading tools…</div>
      ) : tools.length === 0 ? (
        <div className="bi-empty">No tools registered. Tool-store seeds locals on startup; check that it's running.</div>
      ) : (
        <table className="bt-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>Description</th>
              <th>Tags</th>
              <th className="bt-col-toggle">Global</th>
              <th className="bt-col-toggle">{selectedInstanceID ? 'For instance' : ''}</th>
            </tr>
          </thead>
          <tbody>
            {tools.map(t => (
              <ToolRow
                key={t.id}
                tool={t}
                expanded={expanded.has(t.id)}
                onToggleExpand={() => toggleExpanded(t.id)}
                instanceSelected={!!selectedInstanceID}
                inInstance={byInstance.has(t.name)}
                onSetGlobal={(on) => setGlobal(t.id, on)}
                onSetForInstance={(on) => setForInstance(t.name, on)}
              />
            ))}
          </tbody>
        </table>
      )}

      {orphanLocals.length > 0 && (
        <div className="bt-discovery">
          <h3>Discovered locals (not registered)</h3>
          <p className="bt-hint">
            These tools are compiled into the running tool-store binary but have no registry row.
            They become enabled-able once tool-store seeds them on startup. Restart the service if missing.
          </p>
          <ul className="bt-orphan-list">
            {orphanLocals.map(l => (
              <li key={l.name}><code>{l.name}</code> — {l.description}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

interface ToolRowProps {
  tool: Tool
  expanded: boolean
  onToggleExpand: () => void
  instanceSelected: boolean
  inInstance: boolean
  onSetGlobal: (on: boolean) => Promise<boolean>
  onSetForInstance: (on: boolean) => Promise<boolean>
}

function ToolRow({
  tool, expanded, onToggleExpand, instanceSelected, inInstance, onSetGlobal, onSetForInstance,
}: ToolRowProps) {
  return (
    <>
      <tr className={`bt-row bt-row-kind-${tool.kind}`}>
        <td className="bt-name">
          <button className="bt-expand-btn" onClick={onToggleExpand} aria-label={expanded ? 'Collapse' : 'Expand'}>
            {expanded ? '▾' : '▸'}
          </button>
          <code>{tool.name}</code>
          {tool.display_name && <span className="bt-display-name">{tool.display_name}</span>}
        </td>
        <td><span className={`bt-kind bt-kind-${tool.kind}`}>{tool.kind}</span></td>
        <td className="bt-desc">{tool.description}</td>
        <td className="bt-tags">{tool.tags?.map(tag => <span key={tag} className="bt-tag">{tag}</span>)}</td>
        <td className="bt-col-toggle">
          <Toggle on={tool.enabled} onChange={onSetGlobal} />
        </td>
        <td className="bt-col-toggle">
          {instanceSelected && (
            <Toggle
              on={inInstance}
              disabled={!tool.enabled}
              title={tool.enabled ? '' : 'Globally disabled'}
              onChange={onSetForInstance}
            />
          )}
        </td>
      </tr>
      {expanded && (
        <tr className="bt-detail-row">
          <td colSpan={6}>
            <ToolDetail tool={tool} />
          </td>
        </tr>
      )}
    </>
  )
}

function ToolDetail({ tool }: { tool: Tool }) {
  return (
    <div className="bt-detail">
      {tool.env_keys && tool.env_keys.length > 0 && (
        <div className="bt-detail-block">
          <h4>Required env vars</h4>
          <ul className="bt-env-list">
            {tool.env_keys.map(k => (
              <li key={k}>
                <code>{k}</code>
                {tool.credentials?.[k] && (
                  <span className="bt-cred-bind"> → auth-store provider <code>{tool.credentials[k]}</code></span>
                )}
                {!tool.credentials?.[k] && <span className="bt-cred-missing"> (no credential mapping)</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {tool.mcp && (
        <div className="bt-detail-block">
          <h4>MCP launcher</h4>
          <pre className="bt-spec">{JSON.stringify(tool.mcp, null, 2)}</pre>
        </div>
      )}
      {tool.cli && (
        <div className="bt-detail-block">
          <h4>CLI command</h4>
          <pre className="bt-spec">{JSON.stringify(tool.cli, null, 2)}</pre>
        </div>
      )}
      {tool.local && (
        <div className="bt-detail-block">
          <h4>Local impl</h4>
          <p>Symbol: <code>{tool.local.symbol}</code> (in-process Go function)</p>
        </div>
      )}
      {tool.input_schema !== undefined && tool.input_schema !== null && (
        <div className="bt-detail-block">
          <h4>Input schema</h4>
          <pre className="bt-spec">{JSON.stringify(tool.input_schema, null, 2)}</pre>
        </div>
      )}
    </div>
  )
}

function Toggle({
  on, disabled, title, onChange,
}: {
  on: boolean
  disabled?: boolean
  title?: string
  onChange: (on: boolean) => Promise<boolean>
}) {
  const [busy, setBusy] = useState(false)
  const handle = async () => {
    if (busy || disabled) return
    setBusy(true)
    await onChange(!on)
    setBusy(false)
  }
  return (
    <button
      type="button"
      className={`bt-toggle ${on ? 'bt-toggle-on' : 'bt-toggle-off'}${disabled ? ' bt-toggle-disabled' : ''}`}
      onClick={handle}
      disabled={busy || disabled}
      title={title}
      aria-pressed={on}
    >
      <span className="bt-toggle-knob" />
    </button>
  )
}
