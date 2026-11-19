import { useEffect, useState, useCallback, useMemo } from 'react'
import { useBridgeConfig } from '../context'
import { useBridgePrefs } from '../useBridgePrefs'
import { useBridgeHarnesses, harnessNameKey, harnessNamesFromKey } from '../useBridgeHarnesses'
import {
  PermissionModeAsk,
  PermissionModeAuto,
  PermissionModeBypass,
  type BridgePrefs,
  type FetchFn,
  type HarnessDefaults,
  type HarnessInfo,
} from '../types'
import { SourceFoldersEditor } from './SourceFoldersEditor'
import { SETTINGS_SCOPE_INDEX, SETTINGS_SCOPE_LABEL, SettingsSection, saveResultOf, useSectionSave } from './settings/SettingsSection'

interface ModelInfo {
  id: string
  name: string
  provider: string
  enabled: boolean
  input_cost: number
  output_cost: number
}

const EFFORT_OPTIONS = ['low', 'medium', 'high', 'xhigh', 'max']

const COMMON_TOOLS = [
  'Bash', 'Read', 'Write', 'Edit', 'Glob', 'Grep', 'Agent', 'WebFetch', 'WebSearch',
  'NotebookEdit', 'TodoWrite', 'AskUserQuestion',
]

/** The global settings page. It holds the two widest scopes — global and
 *  per-harness — and an index saying where every narrower scope is edited, so a
 *  setting is looked for on the page of the thing it applies to and found there.
 *  Every section is a SettingsSection: same frame, same scope and owner badges,
 *  same footer, and a refused save shown in the store's words. */
export function BridgeSettings() {
  const { fetch: apiFetch, basePath, renderHarnessExtension, routes } = useBridgeConfig()
  const bridgePrefs = useBridgePrefs({ fetch: apiFetch, endpoint: `${basePath}/bridge-prefs` })
  const { harnesses } = useBridgeHarnesses()
  const [models, setModels] = useState<ModelInfo[]>([])

  useEffect(() => {
    apiFetch(`${basePath}/models`).then(r => r.ok ? r.json() : []).then((data: ModelInfo[]) => {
      setModels(data.filter(m => m.enabled))
    }).catch(() => {})
  }, [apiFetch, basePath])

  // Keyed on the harness NAMES rather than the list: `/harnesses` is polled, and
  // a tick that only reports availability must not reseed the forms below.
  const nameKey = harnessNameKey(harnesses)
  const harnessNames = useMemo(() => harnessNamesFromKey(nameKey), [nameKey])

  const routeFor = (key: typeof SETTINGS_SCOPE_INDEX[number]['route']): string => routes[key]

  return (
    <div className="bset-container bss-sections">
      <SettingsSection id="scope-index" title="Where settings live" scope="global"
        help="A setting is edited on the page of the thing it applies to. A narrower scope overrides a wider one when a setting exists at both: session over board over instance over harness over global.">
        <table className="bss-index">
          <tbody>
            {SETTINGS_SCOPE_INDEX.map(row => (
              <tr key={row.scope}>
                <th scope="row">{SETTINGS_SCOPE_LABEL[row.scope]}</th>
                <td>{row.where} {row.route !== 'settings' && routeFor(row.route) && <a href={routeFor(row.route)}>open</a>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </SettingsSection>

      <SourceFoldersEditor />

      <PermissionsModeSection
        apiFetch={apiFetch}
        basePath={basePath}
        prefs={bridgePrefs.prefs}
        loaded={bridgePrefs.loaded}
        refreshPrefs={bridgePrefs.refreshPrefs}
      />

      {harnesses.map(h => (
        <HarnessDefaultsSection
          key={h.name}
          harness={h}
          models={models}
          basePath={basePath}
          saved={bridgePrefs.getDefaults(h.name)}
          loaded={bridgePrefs.loaded}
          seedKey={`${nameKey}:${harnessNames.length}`}
          onSave={defaults => bridgePrefs.setHarnessDefaults(h.name, defaults)}
          extension={renderHarnessExtension?.(h.name)}
        />
      ))}
    </div>
  )
}

/** Everything a form can say about one harness's defaults, trimmed to what is
 *  set: an empty model is "use the harness default" and is not stored. */
function cleanDefaults(defaults: HarnessDefaults): HarnessDefaults {
  const cleaned: HarnessDefaults = {}
  if (defaults.model) cleaned.model = defaults.model
  if (defaults.effort) cleaned.effort = defaults.effort
  if (defaults.max_budget !== undefined && defaults.max_budget > 0) cleaned.max_budget = defaults.max_budget
  if (defaults.disabled_tools?.length) cleaned.disabled_tools = [...defaults.disabled_tools].sort()
  return cleaned
}

function HarnessDefaultsSection({ harness: h, models, basePath, saved, loaded, seedKey, onSave, extension }: {
  harness: HarnessInfo
  models: ModelInfo[]
  basePath: string
  saved: HarnessDefaults
  loaded: boolean
  seedKey: string
  onSave: (defaults: HarnessDefaults) => Promise<void>
  extension: React.ReactNode
}) {
  const [expanded, setExpanded] = useState(false)
  const [draft, setDraft] = useState<HarnessDefaults>(saved)
  // Re-seed when the record arrives or the harness set changes, not on every
  // poll tick — the same rule the whole-page form followed before.
  const savedKey = JSON.stringify(cleanDefaults(saved))
  useEffect(() => { setDraft(saved) }, [savedKey, seedKey, loaded]) // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = JSON.stringify(cleanDefaults(draft)) !== savedKey
  const save = useSectionSave()
  const hasCapability = (cap: string) => h.capabilities?.includes(cap)
  const label = h.label || h.name
  const update = (field: keyof HarnessDefaults, value: unknown) => setDraft(prev => ({ ...prev, [field]: value }))
  const toggleTool = (tool: string) => setDraft(prev => {
    const current = prev.disabled_tools || []
    return { ...prev, disabled_tools: current.includes(tool) ? current.filter(t => t !== tool) : [...current, tool] }
  })

  return (
    <SettingsSection
      id={`harness:${h.name}`}
      title={label}
      scope="harness"
      storedBy={`llm-bridge-server · bridge-prefs.defaults.${h.name}`}
      precedence="Applied when a session is created on this harness. A bundle's model and effort, and a session's own settings in the chat header, outrank these."
      collapsible={{ expanded, onToggle: () => setExpanded(e => !e) }}
      badges={<>
        {h.image ? <img className="bset-harness-img" src={`${basePath}${h.image}`} alt="" /> : <span className="bset-emoji">{h.emoji || ''}</span>}
        {!h.available && <span className="bset-unavail-badge">unavailable</span>}
      </>}
      save={{ dirty, state: save, onSave: () => save.run(() => saveResultOf(onSave(cleanDefaults(draft)))), label: 'Save defaults' }}
    >
      {hasCapability('model') && (
        <div className="bset-field">
          <label>Default model</label>
          <select value={draft.model || ''} onChange={e => update('model', e.target.value)}>
            <option value="">&mdash; Use harness default &mdash;</option>
            {models.filter(m => !h.supported_providers?.length || h.supported_providers.includes(m.provider)).map(m => (
              <option key={m.id} value={m.id}>{m.id} (${m.input_cost}/${m.output_cost} MTok)</option>
            ))}
          </select>
        </div>
      )}
      {hasCapability('effort') && (
        <div className="bset-field">
          <label>Effort level</label>
          <select value={draft.effort || ''} onChange={e => update('effort', e.target.value)}>
            <option value="">&mdash; Default &mdash;</option>
            {EFFORT_OPTIONS.map(e => <option key={e} value={e}>{e}</option>)}
          </select>
        </div>
      )}
      {hasCapability('budget') && (
        <div className="bset-field">
          <label>Max budget ($)</label>
          <input type="number" step="0.5" min="0" placeholder="No limit" value={draft.max_budget ?? ''} onChange={e => update('max_budget', e.target.value ? parseFloat(e.target.value) : undefined)} />
        </div>
      )}
      {hasCapability('tools') && (
        <div className="bset-field">
          <label>Disabled tools</label>
          <div className="bset-tool-grid">
            {COMMON_TOOLS.map(tool => {
              const disabled = draft.disabled_tools?.includes(tool)
              return (
                <button key={tool} type="button" className={`bset-tool-chip ${disabled ? 'bset-tool-disabled' : ''}`} onClick={() => toggleTool(tool)}>
                  {tool}
                </button>
              )
            })}
          </div>
        </div>
      )}
      <div className="bset-caps-info">
        <span className="bset-caps-label">Capabilities:</span>
        {h.capabilities?.map(c => <span key={c} className="bset-cap-badge">{c}</span>)}
      </div>
      {extension}
    </SettingsSection>
  )
}

// The global permission mode: the default for new sessions (snapshotted into
// HarnessConfig at create time) and the fallback for legacy sessions without a
// per-session mode. Saved as bridge-prefs.permission_mode through its own
// endpoint; bridge-server reads it on every prehook call so a change takes
// effect at once for every active and future session.
function PermissionsModeSection({ apiFetch, basePath, prefs, loaded, refreshPrefs }: {
  apiFetch: FetchFn
  basePath: string
  prefs: BridgePrefs
  loaded: boolean
  refreshPrefs: () => Promise<void>
}) {
  // The mode the user just picked, held only while its POST is in flight — the
  // optimistic overlay so the select does not snap back for a round trip.
  const [pending, setPending] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  // Prefer the new field; fall back to the legacy bool so older prefs files
  // migrate visibly on first render.
  const stored = prefs.permission_mode
    ? prefs.permission_mode
    : prefs.bypass_permissions ? PermissionModeBypass : PermissionModeAsk
  const mode = pending ?? stored

  const handleChange = useCallback(async (next: string) => {
    if (next === mode) return
    setBusy(true)
    setError(null)
    setSaved(false)
    setPending(next)
    try {
      const res = await apiFetch(`${basePath}/bridge/permission-mode`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode: next }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`)
      // This endpoint writes the same record `useBridgePrefs` holds, so the
      // shared copy is stale until it is read again.
      await refreshPrefs()
      setSaved(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      // Either way the record is the source of truth again: the new mode on
      // success, the server's unchanged mode on failure, which is the revert.
      setPending(null)
      setBusy(false)
    }
  }, [mode, apiFetch, basePath, refreshPrefs])

  if (!loaded) return null

  return (
    <SettingsSection
      id="permission-mode"
      title="Permission mode"
      scope="global"
      storedBy="llm-bridge-server · bridge-prefs.permission_mode"
      precedence="Each new session snapshots this at creation; the mode control in the chat header overrides it for one session. Bypass makes permission-store's rules moot; Ask and Auto route through them."
      status={{ busy, error, saved }}
    >
      <div className="bset-bypass-row">
        <label className="bset-mode-row">
          <strong>Mode:</strong>
          <select
            className="bset-mode-select"
            value={mode}
            disabled={busy}
            onChange={e => handleChange(e.target.value)}
          >
            <option value={PermissionModeAsk}>Ask — gate every novel tool call</option>
            <option value={PermissionModeAuto}>Auto — allow reads, edits, planning; ask for shell/fetch/agent</option>
            <option value={PermissionModeBypass}>Bypass — allow every tool call</option>
          </select>
        </label>
      </div>
      <p className="bss-help">
        {mode === PermissionModeBypass && 'Bypass is ON. Every tool call auto-approves immediately; Codex sessions launch with sandbox=danger-full-access + approval=never. Permission rules in /permissions are ignored. AskUserQuestion still pauses for your answer.'}
        {mode === PermissionModeAuto && 'Auto-mode allows the bridge-defined safe-tool set (Read, Glob, Grep, LS, Edit, Write, MultiEdit, NotebookRead, NotebookEdit, TodoWrite, ExitPlanMode). Other tools still route through permission-store rules.'}
        {mode === PermissionModeAsk && 'Ask-mode routes every tool call through permission-store rules. Manage rules at /permissions; pending prompts surface inline in chat.'}
      </p>
    </SettingsSection>
  )
}

