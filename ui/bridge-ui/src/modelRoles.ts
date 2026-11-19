import { ServiceSettingValueTypeModelRole, type ServiceSetting, type ServiceSettings } from '@kayushkin/llm-bridge-types'
import type { ModelStoreModel, ModelStoreRoleModelsBody, ModelStoreRoles } from './types-model-store'
import type { FetchFn } from './types'

// The Models page's pure rules, and the model_role drop-down's on the Service
// settings page. Nothing here names a role or a model: the roles come from
// model-store's `GET /api/roles` (`canonical`), the models from `GET /api/models`.

/** model-store roots its API at /api, and the host proxies it whole. */
export function modelStoreRolesURL(modelStoreBasePath: string): string {
  return `${modelStoreBasePath}/api/roles`
}

export function modelStoreModelsURL(modelStoreBasePath: string): string {
  return `${modelStoreBasePath}/api/models`
}

/** A setting whose value is a model-store role name rather than free text. */
export function isModelRoleSetting(setting: ServiceSetting): boolean {
  return setting.value_type === ServiceSettingValueTypeModelRole
}

/** A role's models in the order they are tried. A role model-store has no list
 *  for is an empty list, which the page shows as such. */
export function roleModelList(roles: ModelStoreRoles, role: string): string[] {
  return roles.models[role] ?? []
}

/** "gpt-5.6-terra, then claude-haiku-4-5-20251001": the first model, then the
 *  fallbacks. Empty when the role lists no model. */
export function roleModelsText(models: readonly string[]): string {
  if (models.length === 0) return ''
  const [first, ...rest] = models
  return rest.length === 0 ? first : `${first}, then ${rest.join(', then ')}`
}

/** One option of a role drop-down. */
export interface RoleOption {
  role: string
  /** "balanced — gpt-5.6-terra, then claude-haiku-4-5-20251001". */
  label: string
  /** The role is not one model-store lists: the setting's current value is
   *  kept as an option so the page shows it rather than a different role. */
  unknown: boolean
}

/** The drop-down's options: model-store's canonical roles in its order, and
 *  the current value too when it is not one of them, marked unknown. */
export function roleOptions(roles: ModelStoreRoles, currentValue: string): RoleOption[] {
  const options: RoleOption[] = roles.canonical.map(role => ({ role, label: roleOptionLabel(roles, role), unknown: false }))
  if (currentValue !== '' && !roles.canonical.includes(currentValue)) {
    options.push({ role: currentValue, label: `${currentValue} — not a model-store role`, unknown: true })
  }
  return options
}

export function roleOptionLabel(roles: ModelStoreRoles, role: string): string {
  const text = roleModelsText(roleModelList(roles, role))
  return text === '' ? `${role} — no models` : `${role} — ${text}`
}

/** One row of the Background calls table: a model_role setting and the
 *  service that holds it. */
export interface BackgroundCallRow {
  /** The config field of the base path the service answered at; the page
   *  writes the setting back through the same service. */
  sourceKey: string
  basePath: string
  service: string
  setting: ServiceSetting
}

/** Every model_role setting a service described, in the service's order. */
export function backgroundCallRowsOf(sourceKey: string, basePath: string, described: ServiceSettings): BackgroundCallRow[] {
  return (described.settings ?? [])
    .filter(isModelRoleSetting)
    .map(setting => ({ sourceKey, basePath, service: described.service, setting }))
}

/** "$1.00 in · $5.00 out per million tokens". */
export function modelPriceText(model: Pick<ModelStoreModel, 'input_cost' | 'output_cost'>): string {
  return `$${model.input_cost.toFixed(2)} in · $${model.output_cost.toFixed(2)} out per million tokens`
}

/** The models a role's list can take next: enabled, not already in the list,
 *  by provider then id. */
export function addableModels(models: readonly ModelStoreModel[], list: readonly string[]): ModelStoreModel[] {
  return models
    .filter(model => model.enabled && !list.includes(model.id))
    .sort((a, b) => a.provider.localeCompare(b.provider) || a.id.localeCompare(b.id))
}

/** What the page says about a model in a role's list that model-store would
 *  not use as it stands: gone from the registry, or turned off. Null when it
 *  is fine. */
export function listedModelProblem(models: readonly ModelStoreModel[], id: string): string | null {
  const model = models.find(m => m.id === id)
  if (!model) return 'not in model-store'
  if (!model.enabled) return 'disabled'
  return null
}

/** The list with the model at `index` moved by `offset` places; unchanged when
 *  the move would leave the list. */
export function moveModel(list: readonly string[], index: number, offset: number): string[] {
  const target = index + offset
  if (index < 0 || index >= list.length || target < 0 || target >= list.length) return [...list]
  const next = [...list]
  const [moved] = next.splice(index, 1)
  next.splice(target, 0, moved)
  return next
}

export function removeModel(list: readonly string[], index: number): string[] {
  return list.filter((_, i) => i !== index)
}

export function appendModel(list: readonly string[], id: string): string[] {
  return list.includes(id) ? [...list] : [...list, id]
}

export function sameModelList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((id, i) => id === b[i])
}

/** `POST /api/roles` replacing a role's whole list. */
export function roleModelsSaveBody(role: string, models: readonly string[]): ModelStoreRoleModelsBody {
  return { role, models: [...models] }
}

/** The error text of a refused request: model-store answers `{"error": …}`;
 *  anything else is shown as it came. */
export function modelStoreErrorText(status: number, body: string): string {
  let message = body.trim()
  try {
    const parsed = JSON.parse(body) as unknown
    if (parsed && typeof parsed === 'object' && typeof (parsed as { error?: unknown }).error === 'string') {
      message = (parsed as { error: string }).error
    }
  } catch {
    // Not JSON: the body as it came is the message.
  }
  return `${status}: ${message}`
}

async function readModelStore<T>(fetchFn: FetchFn, url: string): Promise<T> {
  const res = await fetchFn(url)
  if (!res.ok) throw new Error(`GET ${url} → ${modelStoreErrorText(res.status, await res.text())}`)
  return await res.json() as T
}

export function fetchModelStoreRoles(fetchFn: FetchFn, modelStoreBasePath: string): Promise<ModelStoreRoles> {
  return readModelStore<ModelStoreRoles>(fetchFn, modelStoreRolesURL(modelStoreBasePath))
}

export function fetchModelStoreModels(fetchFn: FetchFn, modelStoreBasePath: string): Promise<ModelStoreModel[]> {
  return readModelStore<ModelStoreModel[]>(fetchFn, modelStoreModelsURL(modelStoreBasePath))
}

/** Replaces a role's list. A refusal is model-store's own text. */
export async function saveRoleModels(fetchFn: FetchFn, modelStoreBasePath: string, role: string, models: readonly string[]):
  Promise<{ ok: true } | { ok: false; error: string }> {
  try {
    const res = await fetchFn(modelStoreRolesURL(modelStoreBasePath), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(roleModelsSaveBody(role, models)),
    })
    if (!res.ok) return { ok: false, error: `POST /api/roles → ${modelStoreErrorText(res.status, await res.text())}` }
    return { ok: true }
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) }
  }
}
