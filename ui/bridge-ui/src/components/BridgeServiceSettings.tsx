import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ServiceSetting, ServiceSettings } from '@kayushkin/llm-bridge-types'
import { useBridgeConfig } from '../context'
import {
  fetchServiceSettings, putServiceSetting, serviceSettingsByKind, serviceSettingsSourcesOf, serviceSettingsSummary,
  serviceSettingsURL, serviceSettingValueText, type ServiceSettingsAnswer, type ServiceSettingsSource,
} from '../serviceSettings'
import { isModelRoleSetting, roleModelList, roleModelsText, roleOptions } from '../modelRoles'
import { useModelStoreRoles, type ModelStoreRolesState } from '../useModelStoreRoles'
import { SettingsSection } from './settings/SettingsSection'

/** Service settings: every backend this host proxies, asked to describe its own
 *  configuration (`GET {base}/settings`, llm-bridge's `msg.ServiceSettings`).
 *  A service that answers is drawn from its answer — what each setting is, the
 *  value in force and whether the environment, the service's own record or a
 *  built-in default decided it. Behavior settings the service stores are
 *  edited here and apply with no restart; addresses, paths and secrets are
 *  shown with their variable name and change only where the process is started.
 *  A service that does not answer is listed as such: that list is what is left
 *  to convert. */
export function BridgeServiceSettings() {
  const config = useBridgeConfig()
  const sources = useMemo(() => serviceSettingsSourcesOf(config), [config])
  // Read once for every model_role setting on the page.
  const { state: roles } = useModelStoreRoles()
  return (
    <div className="bh-container bss-sections">
      <header>
        <h2 className="bh-title">Service settings</h2>
        <p className="bh-subtitle">
          Each service describes its own configuration. <strong>Behavior</strong> settings are stored by the service and
          changed here, with no restart. <strong>Wiring</strong>, <strong>paths</strong> and <strong>secrets</strong> belong to the
          environment the process is started in: they are shown with the variable that sets them, and a secret shows only
          whether it is set.
        </p>
      </header>
      {sources.map(source => <ServicePanel key={source.configKey} source={source} roles={roles} />)}
    </div>
  )
}

type PanelState = { phase: 'loading' } | ServiceSettingsAnswer

function ServicePanel({ source, roles }: { source: ServiceSettingsSource; roles: ModelStoreRolesState }) {
  const { fetch: apiFetch } = useBridgeConfig()
  const [state, setState] = useState<PanelState>({ phase: 'loading' })
  const [expanded, setExpanded] = useState(false)

  useEffect(() => {
    let canceled = false
    void fetchServiceSettings(apiFetch, source.basePath).then(answer => {
      if (canceled) return
      setState(answer)
      if (answer.phase === 'described') setExpanded(true)
    })
    return () => { canceled = true }
  }, [apiFetch, source.basePath])

  /** One write. The service answers with its whole description, so the page
   *  shows what is now in force rather than what was typed. */
  const save = useCallback(async (key: string, value: string): Promise<string | null> => {
    const result = await putServiceSetting(apiFetch, source.basePath, key, value)
    if (!result.ok) return result.error
    setState({ phase: 'described', described: result.described })
    return null
  }, [apiFetch, source.basePath])

  if (state.phase !== 'described') {
    return (
      <SettingsSection id={`service-settings:${source.serviceName}`} title={source.serviceName} scope="global"
        badges={<span className="bh-flag">{state.phase === 'loading' ? 'asking…' : 'does not describe its settings'}</span>}>
        {state.phase === 'silent' && (
          <span className="bss-help">
            <code>GET {serviceSettingsURL(source.basePath)}</code> answered {state.status}. Its configuration is read from its
            environment and is not visible here yet.
          </span>
        )}
        {state.phase === 'failed' && <div className="bridge-error bss-error">{state.message}</div>}
      </SettingsSection>
    )
  }

  const { described } = state
  return (
    <SettingsSection id={`service-settings:${described.service}`} title={described.service} scope="global"
      storedBy={serviceSettingsSummary(described)}
      collapsible={{ expanded, onToggle: () => setExpanded(v => !v) }}>
      {serviceSettingsByKind(described).map(group => (
        <div key={group.kind} className="bsv-group" data-setting-kind={group.kind}>
          <h4 className="bsv-group-title">{group.kind}</h4>
          {group.description && <p className="bss-help">{group.description}</p>}
          <ul className="bh-list">
            {group.settings.map(setting => <SettingRow key={setting.key} setting={setting} describedSources={described.sources ?? []} roles={roles} onSave={save} />)}
          </ul>
        </div>
      ))}
    </SettingsSection>
  )
}

function SettingRow({ setting, describedSources, roles, onSave }: {
  setting: ServiceSetting
  describedSources: ServiceSettings['sources']
  roles: ModelStoreRolesState
  onSave: (key: string, value: string) => Promise<string | null>
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(setting.value)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const sourceMeaning = describedSources.find(s => s.source === setting.source)?.description ?? ''

  const submit = async () => {
    setBusy(true)
    const refusal = await onSave(setting.key, draft)
    setBusy(false)
    setError(refusal)
    if (refusal === null) setEditing(false)
  }

  return (
    <li className="bh-row" data-setting-key={setting.key}>
      <div className="bh-row-head">
        <span className="bh-event">{setting.key}</span>
        <code className="bh-matcher" title="The environment variable the process reads for this setting">{setting.environment_variable}</code>
        <span className={`bh-flag bsv-source-${setting.source}`} title={sourceMeaning}>{setting.source}</span>
        {setting.required && <span className="bh-flag">required</span>}
        {setting.editable && (
          <span className="bh-row-actions">
            <button type="button" className="bp-cancel" disabled={busy}
              onClick={() => { setDraft(setting.value); setError(null); setEditing(v => !v) }}>{editing ? 'Cancel' : 'Change'}</button>
          </span>
        )}
      </div>
      <p className="bss-help bsv-description">{setting.description}</p>
      {!editing && (
        <div className="bsv-value">
          <code className={setting.is_set ? '' : 'bsv-unset'}>{serviceSettingValueText(setting)}</code>
          {isModelRoleSetting(setting) && setting.is_set && <RoleModelsNote role={setting.value} roles={roles} />}
          {setting.default_value && setting.source !== 'default' && <span className="bss-help">default {setting.default_value}</span>}
        </div>
      )}
      {editing && isModelRoleSetting(setting) && (
        <div className="bsv-edit">
          <ModelRoleSelect value={draft} roles={roles} disabled={busy} label={`New role for ${setting.key}`} onChange={setDraft} />
          <span className="bss-help">model-store role{setting.default_value ? ` · default ${setting.default_value}` : ''}</span>
          <button type="button" className="bi-save-btn" disabled={busy || draft === setting.value || roles.phase !== 'loaded'} onClick={() => { void submit() }}>{busy ? 'Saving…' : 'Save'}</button>
        </div>
      )}
      {editing && !isModelRoleSetting(setting) && (
        <div className="bsv-edit">
          <input aria-label={`New value for ${setting.key}`} value={draft} spellCheck={false} disabled={busy}
            placeholder={setting.value_type} onChange={e => setDraft(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void submit() }} />
          <span className="bss-help">{setting.value_type}{setting.default_value ? ` · default ${setting.default_value}` : ''}</span>
          <button type="button" className="bi-save-btn" disabled={busy || draft === setting.value} onClick={() => { void submit() }}>{busy ? 'Saving…' : 'Save'}</button>
        </div>
      )}
      {(setting.notes ?? []).map(note => <p key={note} className="bss-help bsv-note">{note}</p>)}
      {error && <div className="bridge-error bss-error">{error}</div>}
    </li>
  )
}

/** A drop-down of model-store's roles, each with the models it resolves to.
 *  While the roles cannot be read it says why, and offers nothing to pick. */
export function ModelRoleSelect({ value, roles, disabled, label, onChange }: {
  value: string
  roles: ModelStoreRolesState
  disabled: boolean
  label: string
  onChange: (role: string) => void
}) {
  if (roles.phase === 'loading') return <span className="bss-help">reading model-store roles…</span>
  if (roles.phase === 'failed') return <div className="bridge-error bss-error">{roles.error}</div>
  return (
    <select aria-label={label} value={value} disabled={disabled} onChange={e => onChange(e.target.value)}>
      {value === '' && <option value="" disabled>choose a role</option>}
      {roleOptions(roles.roles, value).map(option => (
        <option key={option.role} value={option.role}>{option.label}</option>
      ))}
    </select>
  )
}

/** The models a role resolves to, beside the role's name. */
export function RoleModelsNote({ role, roles }: { role: string; roles: ModelStoreRolesState }) {
  if (roles.phase === 'loading') return null
  if (roles.phase === 'failed') return <span className="bss-error">{roles.error}</span>
  if (!roles.roles.canonical.includes(role)) return <span className="bss-error">not a model-store role</span>
  const text = roleModelsText(roleModelList(roles.roles, role))
  return <span className="bss-help">{text === '' ? 'no models' : text}</span>
}
