import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../context'
import {
  fetchServiceSettings, putServiceSetting, serviceSettingsSourcesOf, type ServiceSettingsAnswer, type ServiceSettingsSource,
} from '../serviceSettings'
import {
  addableModels, appendModel, backgroundCallRowsOf, fetchModelStoreModels, listedModelProblem, modelPriceText, moveModel,
  removeModel, roleModelList, sameModelList, saveRoleModels, type BackgroundCallRow,
} from '../modelRoles'
import type { ModelStoreModel, ModelStoreRoles } from '../types-model-store'
import { useModelStoreRoles, type ModelStoreRolesState } from '../useModelStoreRoles'
import { ModelRoleSelect, RoleModelsNote } from './BridgeServiceSettings'
import { SettingsSection } from './settings/SettingsSection'

/** Models: which model each background call uses, and what each model-store
 *  role resolves to. A background call — a classifier, a renamer, a reviewer —
 *  names a role in a service setting (value type `model_role`), never a model;
 *  model-store turns the role into an ordered list of models. The first is
 *  used, the rest are tried in order when a call fails. */
export function BridgeModels() {
  const config = useBridgeConfig()
  const sources = useMemo(() => serviceSettingsSourcesOf(config), [config])
  const { state: roles, reload: reloadRoles } = useModelStoreRoles()
  return (
    <div className="bh-container bss-sections">
      <header>
        <h2 className="bh-title">Models</h2>
        <p className="bh-subtitle">
          A background call names a model-store <strong>role</strong>, and a role is an ordered list of models.
          The <strong>first model is used</strong>; the rest are tried in order, one after another, when a call fails.
        </p>
      </header>
      <BackgroundCalls sources={sources} roles={roles} />
      <Roles roles={roles} onSaved={reloadRoles} />
    </div>
  )
}

type SourceState = { phase: 'loading' } | ServiceSettingsAnswer

/** Every model_role setting of every service the Service settings page reads,
 *  in one table. */
function BackgroundCalls({ sources, roles }: { sources: ServiceSettingsSource[]; roles: ModelStoreRolesState }) {
  const { fetch: apiFetch } = useBridgeConfig()
  const [answers, setAnswers] = useState<Record<string, SourceState>>({})

  useEffect(() => {
    let canceled = false
    setAnswers(Object.fromEntries(sources.map(source => [source.configKey, { phase: 'loading' } as SourceState])))
    for (const source of sources) {
      void fetchServiceSettings(apiFetch, source.basePath).then(answer => {
        if (!canceled) setAnswers(previous => ({ ...previous, [source.configKey]: answer }))
      })
    }
    return () => { canceled = true }
  }, [apiFetch, sources])

  const save = useCallback(async (row: BackgroundCallRow, role: string): Promise<string | null> => {
    const result = await putServiceSetting(apiFetch, row.basePath, row.setting.key, role)
    if (!result.ok) return result.error
    setAnswers(previous => ({ ...previous, [row.sourceKey]: { phase: 'described', described: result.described } }))
    return null
  }, [apiFetch])

  const rows: BackgroundCallRow[] = []
  const failures: { source: ServiceSettingsSource; message: string }[] = []
  let waiting = 0
  let silent = 0
  for (const source of sources) {
    const answer = answers[source.configKey]
    if (!answer || answer.phase === 'loading') waiting++
    else if (answer.phase === 'failed') failures.push({ source, message: answer.message })
    else if (answer.phase === 'silent') silent++
    else rows.push(...backgroundCallRowsOf(source.configKey, source.basePath, answer.described))
  }

  return (
    <SettingsSection id="models:background-calls" title="Background calls" scope="global"
      storedBy="each service's own settings (PUT /settings/{key})"
      help={<>Every setting whose value is a model-store role, from every service the Service settings page reads.
        {silent > 0 && <> {silent} of {sources.length} services do not describe their settings, so any model they call is not listed here.</>}</>}>
      {waiting > 0 && <span className="bss-help">asking {waiting} services…</span>}
      {failures.map(({ source, message }) => (
        <div key={source.configKey} className="bridge-error bss-error">{source.serviceName}: {message}</div>
      ))}
      {waiting === 0 && rows.length === 0 && (
        <span className="bss-help">No service describes a setting that names a model-store role.</span>
      )}
      {rows.length > 0 && (
        <table className="bss-index bmd-calls">
          <thead>
            <tr><th>Service</th><th>Setting</th><th>Role</th><th>Models, in the order tried</th></tr>
          </thead>
          <tbody>
            {rows.map(row => (
              <BackgroundCallRowView key={`${row.sourceKey}:${row.setting.key}`} row={row} roles={roles} onSave={save} />
            ))}
          </tbody>
        </table>
      )}
    </SettingsSection>
  )
}

function BackgroundCallRowView({ row, roles, onSave }: {
  row: BackgroundCallRow
  roles: ModelStoreRolesState
  onSave: (row: BackgroundCallRow, role: string) => Promise<string | null>
}) {
  const { setting } = row
  const [draft, setDraft] = useState(setting.value)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => { setDraft(setting.value) }, [setting.value])

  const submit = async () => {
    setBusy(true)
    const refusal = await onSave(row, draft)
    setBusy(false)
    setError(refusal)
  }

  return (
    <tr data-setting-key={setting.key}>
      <td>{row.service}</td>
      <td>
        <div>{setting.description}</div>
        <code className="bss-help">{setting.key}</code>
        {!setting.editable && <span className="bh-flag">set by {setting.source}</span>}
      </td>
      <td>
        {setting.editable
          ? (
            <div className="bsv-edit">
              <ModelRoleSelect value={draft} roles={roles} disabled={busy} label={`Role for ${setting.key}`} onChange={setDraft} />
              {draft !== setting.value && (
                <>
                  <button type="button" className="bi-save-btn" disabled={busy} onClick={() => { void submit() }}>{busy ? 'Saving…' : 'Save'}</button>
                  <button type="button" className="bp-cancel" disabled={busy} onClick={() => { setDraft(setting.value); setError(null) }}>Cancel</button>
                </>
              )}
            </div>
          )
          : <code>{setting.value}</code>}
        {error && <div className="bridge-error bss-error">{error}</div>}
      </td>
      <td><RoleModelsNote role={draft} roles={roles} /></td>
    </tr>
  )
}

type ModelsState = { phase: 'loading' } | { phase: 'loaded'; models: ModelStoreModel[] } | { phase: 'failed'; error: string }

/** Each of model-store's roles with its ordered list, editable. */
function Roles({ roles, onSaved }: { roles: ModelStoreRolesState; onSaved: () => void }) {
  const { fetch: apiFetch, modelStoreBasePath } = useBridgeConfig()
  const [models, setModels] = useState<ModelsState>({ phase: 'loading' })

  useEffect(() => {
    if (!modelStoreBasePath) return
    let canceled = false
    fetchModelStoreModels(apiFetch, modelStoreBasePath)
      .then(list => { if (!canceled) setModels({ phase: 'loaded', models: list }) })
      .catch(err => { if (!canceled) setModels({ phase: 'failed', error: err instanceof Error ? err.message : String(err) }) })
    return () => { canceled = true }
  }, [apiFetch, modelStoreBasePath])

  return (
    <SettingsSection id="models:roles" title="Roles" scope="global" storedBy="model-store (POST /api/roles)"
      help={<>Each role's models, in order. The first model is used; when a call to it fails, the next is tried, and so on down the list.</>}>
      {roles.phase === 'loading' && <span className="bss-help">reading model-store roles…</span>}
      {roles.phase === 'failed' && <div className="bridge-error bss-error">{roles.error}</div>}
      {models.phase === 'failed' && <div className="bridge-error bss-error">{models.error}</div>}
      {roles.phase === 'loaded' && roles.roles.canonical.map(role => (
        <RoleEditor key={role} role={role} roles={roles.roles}
          models={models.phase === 'loaded' ? models.models : null} onSaved={onSaved} />
      ))}
    </SettingsSection>
  )
}

function RoleEditor({ role, roles, models, onSaved }: {
  role: string
  roles: ModelStoreRoles
  /** Null until model-store's model list has been read. */
  models: ModelStoreModel[] | null
  onSaved: () => void
}) {
  const { fetch: apiFetch, modelStoreBasePath } = useBridgeConfig()
  const stored = roleModelList(roles, role)
  const [draft, setDraft] = useState<string[]>(stored)
  const [adding, setAdding] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const storedKey = stored.join('\n')
  useEffect(() => { setDraft(storedKey === '' ? [] : storedKey.split('\n')) }, [storedKey])

  const dirty = !sameModelList(draft, stored)
  const addable = models ? addableModels(models, draft) : []

  const submit = async () => {
    setBusy(true)
    const result = await saveRoleModels(apiFetch, modelStoreBasePath, role, draft)
    setBusy(false)
    if (!result.ok) { setError(result.error); return }
    setError(null)
    onSaved()
  }

  return (
    <div className="bsv-group" data-model-role={role}>
      <h4 className="bsv-group-title">{role}</h4>
      {draft.length === 0 && <p className="bss-help">No models: a call that names this role has nothing to use.</p>}
      <ol className="bh-list bmd-role-models">
        {draft.map((id, index) => {
          const model = models?.find(m => m.id === id)
          const problem = models ? listedModelProblem(models, id) : null
          return (
            <li key={id} className="bh-row" data-model-id={id}>
              <div className="bh-row-head">
                <span className="bh-flag">{index === 0 ? 'used' : `fallback ${index}`}</span>
                <span className="bh-event">{id}</span>
                {model && <span className="bss-help">{model.provider} · {modelPriceText(model)}</span>}
                {problem && <span className="bh-flag bss-error">{problem}</span>}
                <span className="bh-row-actions">
                  <button type="button" className="bp-cancel" disabled={busy || index === 0} aria-label={`Move ${id} up`}
                    onClick={() => setDraft(list => moveModel(list, index, -1))}>↑</button>
                  <button type="button" className="bp-cancel" disabled={busy || index === draft.length - 1} aria-label={`Move ${id} down`}
                    onClick={() => setDraft(list => moveModel(list, index, 1))}>↓</button>
                  <button type="button" className="bp-cancel" disabled={busy} onClick={() => setDraft(list => removeModel(list, index))}>Remove</button>
                </span>
              </div>
            </li>
          )
        })}
      </ol>
      <div className="bsv-edit">
        {models === null
          ? <span className="bss-help">reading model-store models…</span>
          : (
            <>
              <select aria-label={`Add a model to ${role}`} value={adding} disabled={busy || addable.length === 0} onChange={e => setAdding(e.target.value)}>
                <option value="">{addable.length === 0 ? 'no other enabled model' : 'add a model…'}</option>
                {addable.map(model => (
                  <option key={model.id} value={model.id}>{model.id} — {model.provider} · {modelPriceText(model)}</option>
                ))}
              </select>
              <button type="button" className="bp-cancel" disabled={busy || adding === ''}
                onClick={() => { setDraft(list => appendModel(list, adding)); setAdding('') }}>Add</button>
            </>
          )}
        <button type="button" className="bi-save-btn" disabled={busy || !dirty} onClick={() => { void submit() }}>{busy ? 'Saving…' : 'Save'}</button>
        {dirty && <button type="button" className="bp-cancel" disabled={busy} onClick={() => { setDraft(stored); setError(null) }}>Reset</button>}
      </div>
      {error && <div className="bridge-error bss-error">{error}</div>}
    </div>
  )
}
