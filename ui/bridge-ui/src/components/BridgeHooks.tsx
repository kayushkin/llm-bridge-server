import { useCallback, useEffect, useMemo, useState } from 'react'
import type { Hook, HookOptions, HookScope } from '@kayushkin/llm-bridge-types'
import { useBridgeConfig } from '../context'
import { useBridgeInstances } from '../useBridgeInstances'
import { emptyHookDraft, hookDraftOf, hookWireBodyOf, shadowedHookIDs, type HookDraft } from '../hookDraft'
import { SettingsSection, type SettingsScope } from './settings/SettingsSection'

/** Hooks: shell commands the bridge wires into a harness's own hook mechanism
 *  when a session spawns. llm-bridge-server has stored and run them since
 *  April 2026 and nothing drew them until this page. One section per scope,
 *  widest first, in the same frame as every other setting; which harnesses are
 *  wired, their known events and the scope order come from GET /hook-options,
 *  and a refusal is shown in the server's words. */
export function BridgeHooks() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const { instances } = useBridgeInstances()
  const [hooks, setHooks] = useState<Hook[] | null>(null)
  const [options, setOptions] = useState<HookOptions | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  const reload = useCallback(async () => {
    try {
      const [hooksRes, optionsRes] = await Promise.all([apiFetch(`${basePath}/hooks`), apiFetch(`${basePath}/hook-options`)])
      if (!hooksRes.ok) throw new Error(`GET /hooks → ${hooksRes.status}: ${await hooksRes.text()}`)
      if (!optionsRes.ok) throw new Error(`GET /hook-options → ${optionsRes.status}: ${await optionsRes.text()}`)
      setHooks(await hooksRes.json() as Hook[])
      setOptions(await optionsRes.json() as HookOptions)
      setLoadError(null)
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err))
    }
  }, [apiFetch, basePath])
  useEffect(() => { void reload() }, [reload])

  /** One write; the server's refusal comes back as text. */
  const write = useCallback(async (method: string, path: string, body?: unknown): Promise<string | null> => {
    try {
      const res = await apiFetch(`${basePath}${path}`, {
        method,
        headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      })
      if (!res.ok) return `${method} ${path} → ${res.status}: ${(await res.text()).trim()}`
      await reload()
      return null
    } catch (err) {
      return err instanceof Error ? err.message : String(err)
    }
  }, [apiFetch, basePath, reload])

  const shadowed = useMemo(() => shadowedHookIDs(hooks ?? [], options?.scope_kinds ?? []), [hooks, options])
  const instanceName = useCallback((id: string) => instances.find(i => i.id === id)?.name || id, [instances])
  // Widest first on the page, as the Settings index reads; the served order is narrowest first.
  const scopesWidestFirst = useMemo(() => [...(options?.scope_kinds ?? [])].reverse(), [options])

  return (
    <div className="bh-container bss-sections">
      <header>
        <h2 className="bh-title">Hooks</h2>
        <p className="bh-subtitle">
          A hook is a shell command the bridge wires into a harness's own hook mechanism when a session spawns. The
          harness calls back to the bridge, which runs the command <strong>on this server, as the bridge's user</strong>,
          with the harness's hook payload on stdin and its stdout returned to the harness untranslated.
        </p>
        {options && (
          <p className="bh-subtitle">
            Wired harnesses: {options.harnesses.map(h => h.harness).join(', ')}. When the same harness, event and matcher are
            registered at more than one scope, the narrowest wins: {options.scope_kinds.join(', then ')}.
          </p>
        )}
      </header>
      {loadError && <div className="bridge-error">{loadError}</div>}
      {!hooks || !options ? (loadError ? null : <div className="bi-loading">Loading…</div>) : (
        <>
          <HookComposer options={options} instances={instances} onCreate={body => write('POST', '/hooks', body)} />
          {scopesWidestFirst.map(kind => (
            <HookScopeSection
              key={kind}
              kind={kind}
              hooks={hooks.filter(h => h.scope_kind === kind)}
              options={options}
              instances={instances}
              shadowed={shadowed}
              instanceName={instanceName}
              write={write}
            />
          ))}
        </>
      )}
    </div>
  )
}

const SCOPE_TITLE: Record<HookScope, string> = { global: 'Every session', instance: 'Sessions on one instance', session: 'One session' }
const SCOPE_BADGE: Record<HookScope, SettingsScope> = { global: 'global', instance: 'instance', session: 'session' }

type InstanceOption = { id: string; name?: string; harness_type: string }

function HookScopeSection({ kind, hooks, options, instances, shadowed, instanceName, write }: {
  kind: HookScope
  hooks: Hook[]
  options: HookOptions
  instances: InstanceOption[]
  shadowed: Set<string>
  instanceName: (id: string) => string
  write: (method: string, path: string, body?: unknown) => Promise<string | null>
}) {
  return (
    <SettingsSection
      id={`hooks:${kind}`}
      title={SCOPE_TITLE[kind] ?? kind}
      scope={SCOPE_BADGE[kind] ?? 'global'}
      storedBy="llm-bridge-server · hook-store"
      precedence={kind === 'global'
        ? 'A hook on an instance or a session with the same harness, event and matcher replaces this one for the sessions it covers.'
        : kind === 'instance'
          ? 'Replaces a global hook with the same harness, event and matcher; a session hook replaces this one.'
          : 'Replaces an instance or global hook with the same harness, event and matcher, for that session only.'}
    >
      {hooks.length === 0 && <span className="bss-help">No hooks at this scope.</span>}
      <ul className="bh-list">
        {hooks.map(hook => (
          <HookRow
            key={hook.id}
            hook={hook}
            options={options}
            instances={instances}
            shadowed={shadowed.has(hook.id)}
            scopeLabel={kind === 'instance' ? instanceName(hook.scope_id ?? '') : kind === 'session' ? (hook.scope_id ?? '') : ''}
            write={write}
          />
        ))}
      </ul>
    </SettingsSection>
  )
}

function HookRow({ hook, options, instances, shadowed, scopeLabel, write }: {
  hook: Hook
  options: HookOptions
  instances: InstanceOption[]
  shadowed: boolean
  scopeLabel: string
  write: (method: string, path: string, body?: unknown) => Promise<string | null>
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState<HookDraft>(() => hookDraftOf(hook))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const wired = options.harnesses.some(h => h.harness === hook.harness)

  const run = async (action: () => Promise<string | null>): Promise<boolean> => {
    setBusy(true)
    const refusal = await action()
    setBusy(false)
    setError(refusal)
    return refusal === null
  }
  const save = async () => {
    const result = hookWireBodyOf(draft)
    if (!result.ok) { setError(result.error); return }
    if (await run(() => write('PATCH', `/hooks/${encodeURIComponent(hook.id)}`, result.body))) setEditing(false)
  }

  return (
    <li className={`bh-row ${hook.enabled ? '' : 'bh-row-disabled'}`} data-hook-id={hook.id}>
      <div className="bh-row-head">
        <label className="bks-check" title={hook.enabled ? 'Enabled: wired into the next spawn' : 'Disabled: stored, not wired'}>
          <input type="checkbox" checked={hook.enabled} disabled={busy}
            onChange={e => { void run(() => write('PATCH', `/hooks/${encodeURIComponent(hook.id)}`, { enabled: e.target.checked })) }} />
        </label>
        <span className="bh-harness">{hook.harness}</span>
        <span className="bh-event">{hook.event}</span>
        <code className="bh-matcher">{hook.matcher || '(every match)'}</code>
        {scopeLabel && <span className="bh-scope-id" title={hook.scope_id}>{scopeLabel}</span>}
        {shadowed && <span className="bh-flag" title="A hook on a narrower scope registers the same harness, event and matcher, and wins for the sessions it covers.">shadowed</span>}
        {!wired && <span className="bh-flag bh-flag-warn" title="The spawn wires hooks for the harnesses listed at the top of this page only; this one is stored and never runs.">never runs</span>}
        <span className="bh-row-actions">
          <button type="button" className="bp-cancel" disabled={busy} onClick={() => { setDraft(hookDraftOf(hook)); setError(null); setEditing(v => !v) }}>{editing ? 'Cancel' : 'Edit'}</button>
          <button type="button" className="bp-cancel bh-delete" disabled={busy}
            onClick={() => { if (window.confirm(`Delete this ${hook.event} hook? It is removed from hook-store, not disabled.`)) void run(() => write('DELETE', `/hooks/${encodeURIComponent(hook.id)}`)) }}>Delete</button>
        </span>
      </div>
      {!editing && <pre className="bh-command">{hook.command}</pre>}
      {editing && (
        <>
          <HookFields draft={draft} onChange={setDraft} options={options} instances={instances} />
          <div className="bss-actions">
            <button type="button" className="bi-save-btn" disabled={busy} onClick={() => { void save() }}>{busy ? 'Saving…' : 'Save hook'}</button>
          </div>
        </>
      )}
      {error && <div className="bridge-error bss-error">{error}</div>}
      <span className="bh-id">{hook.id}</span>
    </li>
  )
}

function HookComposer({ options, instances, onCreate }: {
  options: HookOptions
  instances: InstanceOption[]
  onCreate: (body: unknown) => Promise<string | null>
}) {
  const [draft, setDraft] = useState<HookDraft>(() => emptyHookDraft(options))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState(false)
  const submit = async () => {
    const result = hookWireBodyOf(draft)
    setCreated(false)
    if (!result.ok) { setError(result.error); return }
    setBusy(true)
    const refusal = await onCreate(result.body)
    setBusy(false)
    setError(refusal)
    if (refusal === null) { setCreated(true); setDraft(emptyHookDraft(options)) }
  }
  return (
    <SettingsSection id="hooks:new" title="New hook" scope={draft.scopeKind === 'global' ? 'global' : draft.scopeKind === 'instance' ? 'instance' : 'session'}
      storedBy="llm-bridge-server · hook-store"
      help="Takes effect on the next spawn of a session it covers; a running session keeps the hooks it started with."
      status={{ busy, error, saved: created }}>
      <HookFields draft={draft} onChange={setDraft} options={options} instances={instances} />
      <div className="bss-actions">
        <button type="button" className="bi-save-btn" disabled={busy} onClick={() => { void submit() }}>{busy ? 'Adding…' : 'Add hook'}</button>
      </div>
    </SettingsSection>
  )
}

function HookFields({ draft, onChange, options, instances }: {
  draft: HookDraft
  onChange: (next: HookDraft) => void
  options: HookOptions
  instances: InstanceOption[]
}) {
  const harness = options.harnesses.find(h => h.harness === draft.harness)
  const eventsListID = `bh-events-${draft.harness || 'none'}`
  const instanceOptions = instances.filter(i => !draft.harness || i.harness_type === draft.harness)
  return (
    <div className="bh-fields">
      <label className="bks-field">
        <span className="bks-field-label">Harness</span>
        <select value={draft.harness} onChange={e => onChange({ ...draft, harness: e.target.value, scopeId: draft.scopeKind === 'instance' ? '' : draft.scopeId })}>
          {!harness && draft.harness && <option value={draft.harness}>{draft.harness} (not wired)</option>}
          {options.harnesses.map(h => <option key={h.harness} value={h.harness}>{h.harness}</option>)}
        </select>
      </label>
      <label className="bks-field">
        <span className="bks-field-label">Event</span>
        <input value={draft.event} list={eventsListID} placeholder={harness && harness.known_events.length === 0 ? 'harness-native event name' : 'PreToolUse'}
          onChange={e => onChange({ ...draft, event: e.target.value })} />
        <datalist id={eventsListID}>{(harness?.known_events ?? []).map(event => <option key={event} value={event} />)}</datalist>
      </label>
      <label className="bks-field">
        <span className="bks-field-label">Matcher</span>
        <input value={draft.matcher} placeholder="empty matches everything" onChange={e => onChange({ ...draft, matcher: e.target.value })} />
        {harness?.matcher_help && <span className="bss-help">{harness.matcher_help}</span>}
      </label>
      <label className="bks-field">
        <span className="bks-field-label">Applies to</span>
        <select value={draft.scopeKind} onChange={e => onChange({ ...draft, scopeKind: e.target.value as HookScope, scopeId: '' })}>
          {[...options.scope_kinds].reverse().map(kind => <option key={kind} value={kind}>{SCOPE_TITLE[kind] ?? kind}</option>)}
        </select>
      </label>
      {draft.scopeKind === 'instance' && (
        <label className="bks-field">
          <span className="bks-field-label">Instance</span>
          <select value={draft.scopeId} onChange={e => onChange({ ...draft, scopeId: e.target.value })}>
            <option value="">— pick an instance —</option>
            {draft.scopeId && !instanceOptions.some(i => i.id === draft.scopeId) && <option value={draft.scopeId}>{draft.scopeId} (not listed)</option>}
            {instanceOptions.map(i => <option key={i.id} value={i.id}>{i.name || i.id} ({i.harness_type})</option>)}
          </select>
        </label>
      )}
      {draft.scopeKind === 'session' && (
        <label className="bks-field">
          <span className="bks-field-label">Session id</span>
          <input value={draft.scopeId} placeholder="br_…" onChange={e => onChange({ ...draft, scopeId: e.target.value })} />
        </label>
      )}
      <label className="bks-field bh-command-field">
        <span className="bks-field-label">Command (run with <code>sh -c</code> on this server)</span>
        <textarea rows={3} spellCheck={false} value={draft.command} onChange={e => onChange({ ...draft, command: e.target.value })} />
      </label>
    </div>
  )
}
