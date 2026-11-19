import { useEffect, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import type { EffectiveConfig, EffectiveSetting } from '@kayushkin/llm-bridge-types'
import { useBridgeConfig } from '../context'
import { useBridgeHarnesses } from '../useBridgeHarnesses'
import { useBridgeInstances } from '../useBridgeInstances'
import { usePrincipals } from '../usePrincipals'
import { useBundles } from '../useBundles'
import { useKanban } from '../useKanban'
import {
  effectiveConfigInputsAreAskable,
  effectiveConfigInputsFromParams,
  effectiveConfigQuery,
  type EffectiveConfigInputs,
} from '../effectiveConfigQuery'

/** What a session is given, setting by setting, with the layer that decided
 *  each. `?session=<id>` reads a stored session; otherwise the form below is a
 *  dry run for a session that does not exist yet, and its inputs live in the
 *  URL so a board settings page or a chat header can link straight to one.
 *
 *  The page draws what llm-bridge-server answers and adds nothing: the layer
 *  legend, the records, the notes and the warnings are the server's, computed
 *  by the same code the spawn runs. */
export function BridgeEffectiveConfig() {
  const { fetch: apiFetch, basePath, routes, principalStoreBasePath, bundleStoreBasePath, kanbanStoreBasePath } = useBridgeConfig()
  const [params, setParams] = useSearchParams()
  const sessionID = params.get('session') ?? ''
  const inputs = useMemo(() => effectiveConfigInputsFromParams(params), [params])
  const query = effectiveConfigQuery(inputs)

  const [result, setResult] = useState<EffectiveConfig | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const url = sessionID
    ? `${basePath}/sessions/${encodeURIComponent(sessionID)}/effective-config`
    : effectiveConfigInputsAreAskable(inputs) ? `${basePath}/effective-config?${query}` : ''

  useEffect(() => {
    if (!url) { setResult(null); setError(null); return }
    let canceled = false
    setLoading(true)
    apiFetch(url).then(async res => {
      const text = await res.text()
      if (canceled) return
      if (!res.ok) {
        // The server's refusal, verbatim: it names what is missing.
        setError(`HTTP ${res.status}: ${text}`)
        setResult(null)
        return
      }
      setError(null)
      setResult(JSON.parse(text) as EffectiveConfig)
    }).catch(err => {
      if (canceled) return
      setError(err instanceof Error ? err.message : String(err))
      setResult(null)
    }).finally(() => { if (!canceled) setLoading(false) })
    return () => { canceled = true }
  }, [apiFetch, url])

  const setInputs = (next: Partial<EffectiveConfigInputs>) => {
    const merged = { ...inputs, ...next }
    setParams(new URLSearchParams(effectiveConfigQuery(merged)), { replace: true })
  }
  const clearSession = () => setParams(new URLSearchParams(), { replace: true })

  return (
    <div className="bec-container">
      <header className="bec-header">
        <h2 className="bec-title">Effective config</h2>
        <p className="bec-subtitle">
          What a session is given, setting by setting, and which layer decided it. Read for a stored session, or as a dry
          run for one that does not exist yet — computed by the same code the spawn runs, so this is what would happen.
        </p>
      </header>

      {sessionID ? (
        <div className="bec-subject">
          <span>Session <code>{sessionID}</code></span>
          <button type="button" className="bp-cancel" onClick={clearSession}>Dry run instead</button>
        </div>
      ) : (
        <DryRunForm
          inputs={inputs}
          onChange={setInputs}
          showPrincipals={!!principalStoreBasePath}
          showBundles={!!bundleStoreBasePath}
          showBoards={!!kanbanStoreBasePath}
        />
      )}

      {loading && <div className="bi-loading">Asking the server…</div>}
      {error && <div className="bridge-error bec-error">{error}</div>}
      {!sessionID && !url && !error && (
        <p className="bec-hint">Pick a harness, an instance or a board to start a dry run — or open this page from a session's details.</p>
      )}
      {result && <EffectiveConfigTable config={result} routes={routes} />}
    </div>
  )
}

function DryRunForm({ inputs, onChange, showPrincipals, showBundles, showBoards }: {
  inputs: EffectiveConfigInputs
  onChange: (next: Partial<EffectiveConfigInputs>) => void
  showPrincipals: boolean
  showBundles: boolean
  showBoards: boolean
}) {
  const { harnesses } = useBridgeHarnesses()
  const { instances } = useBridgeInstances()
  const principals = usePrincipals()
  const bundles = useBundles()
  const kanban = useKanban(null)
  const instanceOptions = instances.filter(i => i.enabled && (!inputs.harness || i.harness_type === inputs.harness))
  const tagsKey = JSON.stringify(inputs.tags)
  const [tagsText, setTagsText] = useState(inputs.tags.join(', '))
  useEffect(() => { setTagsText(inputs.tags.join(', ')) }, [tagsKey]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <form className="bec-form" onSubmit={e => e.preventDefault()}>
      <label className="bks-field">
        <span className="bks-field-label">Harness</span>
        <select value={inputs.harness} onChange={e => onChange({ harness: e.target.value, instanceId: '' })}>
          <option value="">— from the instance or board —</option>
          {harnesses.map(h => <option key={h.name} value={h.name}>{h.label || h.name}</option>)}
        </select>
      </label>
      <label className="bks-field">
        <span className="bks-field-label">Instance</span>
        <select value={inputs.instanceId} onChange={e => onChange({ instanceId: e.target.value })}>
          <option value="">— the harness's one enabled instance, or the board's default —</option>
          {instanceOptions.map(i => <option key={i.id} value={i.id}>{i.name || i.id} ({i.harness_type})</option>)}
        </select>
      </label>
      {showPrincipals && (
        <label className="bks-field">
          <span className="bks-field-label">Principal</span>
          <select value={inputs.principalId} onChange={e => onChange({ principalId: e.target.value })}>
            <option value="">— the board's, else the global default, else none —</option>
            {principals.list.map(p => <option key={p.id} value={p.id}>{p.display_name} ({p.id})</option>)}
          </select>
        </label>
      )}
      <label className="bks-field">
        <span className="bks-field-label">Agent</span>
        <input value={inputs.agentId} placeholder="agent slug or agent-store id; blank for none" onChange={e => onChange({ agentId: e.target.value })} />
      </label>
      {showBundles && (
        <label className="bks-field">
          <span className="bks-field-label">Bundle</span>
          <select value={inputs.bundleId} onChange={e => onChange({ bundleId: e.target.value })}>
            <option value="">— the board's, else none —</option>
            {(bundles.bundles ?? []).map(b => <option key={b.id} value={String(b.id)}>{b.display_name || b.name} (#{b.id})</option>)}
          </select>
        </label>
      )}
      {showBoards && (
        <>
          <label className="bks-field">
            <span className="bks-field-label">Board</span>
            <select value={inputs.boardId} onChange={e => onChange({ boardId: e.target.value, cardId: '' })}>
              <option value="">— none —</option>
              {kanban.boards.map(b => <option key={b.id} value={b.id}>{b.name}</option>)}
            </select>
          </label>
          {inputs.boardId && (
            <>
              <label className="bks-field">
                <span className="bks-field-label">Card</span>
                <input value={inputs.cardId} placeholder="card id; blank to use the tags below" onChange={e => onChange({ cardId: e.target.value })} />
              </label>
              <label className="bks-field">
                <span className="bks-field-label">Card tags</span>
                <input
                  value={tagsText}
                  placeholder="tags a card would carry, comma-separated"
                  onChange={e => setTagsText(e.target.value)}
                  onBlur={() => onChange({ tags: tagsText.split(',').map(t => t.trim()).filter(Boolean) })}
                />
              </label>
            </>
          )}
        </>
      )}
    </form>
  )
}

function EffectiveConfigTable({ config, routes }: { config: EffectiveConfig; routes: ReturnType<typeof useBridgeConfig>['routes'] }) {
  const legend = new Map(config.layers.map(l => [l.layer, l.description]))
  return (
    <div className="bec-result">
      {config.subject.dry_run && (
        <p className="bec-subject-line">
          Dry run on <code>{config.subject.harness}</code>
          {config.subject.instance_id && <> · instance <code>{config.subject.instance_id}</code></>}
          {config.subject.principal_id && <> · as <code>{config.subject.principal_id}</code></>}
          {config.subject.bundle_id && <> · bundle <code>{config.subject.bundle_id}</code></>}
          {config.subject.board_id && <> · board <Link to={`${routes.kanbanSettings}?board=${encodeURIComponent(config.subject.board_id)}`}><code>{config.subject.board_id}</code></Link></>}
          {config.subject.tags && config.subject.tags.length > 0 && <> · tags {config.subject.tags.join(', ')}</>}
        </p>
      )}
      {config.warnings.length > 0 && (
        <ul className="bec-warnings">
          {config.warnings.map((w, i) => <li key={i} className="bridge-error">{w}</li>)}
        </ul>
      )}
      <table className="bec-table">
        <thead>
          <tr><th>Setting</th><th>Value</th><th>Decided by</th><th>Stored in</th><th>Notes</th></tr>
        </thead>
        <tbody>
          {config.settings.map(s => (
            <tr key={s.key} data-setting={s.key} data-layer={s.layer}>
              <th scope="row"><code>{s.key}</code></th>
              <td className="bec-value"><SettingValue setting={s} /></td>
              <td>
                <span className={`bec-layer bec-layer-${s.layer}`} title={legend.get(s.layer) ?? ''}>{s.layer.replace('_', ' ')}</span>
                {s.detail && <div className="bec-detail">{s.detail}</div>}
              </td>
              <td className="bec-record">{s.record ? <code>{s.record}</code> : '—'}</td>
              <td>
                {s.notes && s.notes.length > 0 && (
                  <ul className="bec-notes">{s.notes.map((n, i) => <li key={i}>{n}</li>)}</ul>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <details className="bec-legend">
        <summary>Layers, narrowest first</summary>
        <dl>
          {config.layers.map(l => (
            <div key={l.layer} className="bec-legend-row">
              <dt><span className={`bec-layer bec-layer-${l.layer}`}>{l.layer.replace('_', ' ')}</span></dt>
              <dd>{l.description}</dd>
            </div>
          ))}
        </dl>
      </details>
    </div>
  )
}

function SettingValue({ setting }: { setting: EffectiveSetting }) {
  const v = setting.value
  if (v === null || v === undefined) return <span className="bec-none">—</span>
  if (Array.isArray(v)) {
    if (v.length === 0) return <span className="bec-none">none</span>
    return (
      <ul className="bec-list">
        {v.map((item, i) => {
          if (item && typeof item === 'object' && 'id' in (item as Record<string, unknown>)) {
            const row = item as { id: number | string; name?: string }
            return <li key={i}>{row.name ? <>{row.name} <code>#{row.id}</code></> : <code>#{row.id}</code>}</li>
          }
          return <li key={i}><code>{String(item)}</code></li>
        })}
      </ul>
    )
  }
  if (typeof v === 'object') return <code>{JSON.stringify(v)}</code>
  return <code>{String(v)}</code>
}
