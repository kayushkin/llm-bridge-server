import type { ServiceSetting, ServiceSettingKind, ServiceSettings } from '@kayushkin/llm-bridge-types'
import type { BridgeConfig } from './context'
import type { FetchFn } from './types'

/** One backend the page asks for its settings: the name to show before it has
 *  answered, and the base path the host proxies it at. */
export interface ServiceSettingsSource {
  /** The config field the base path came from; stable, used as a key. */
  configKey: keyof BridgeConfig
  /** The service's name, shown when it does not answer. A service that does
   *  answer names itself, and that name is shown instead. */
  serviceName: string
  basePath: string
}

/** Every base path a host can name, with the service behind it. The order is
 *  the page's order: the bridge server first, then the stores, then the host
 *  itself. The host's name is a placeholder until it answers, because the
 *  library cannot know which host mounted it; an answer names the host. */
const SOURCE_FIELDS: ReadonlyArray<{ configKey: keyof BridgeConfig; serviceName: string }> = [
  { configKey: 'basePath', serviceName: 'llm-bridge-server' },
  { configKey: 'kanbanStoreBasePath', serviceName: 'kanban-store' },
  { configKey: 'principalStoreBasePath', serviceName: 'principal-store' },
  { configKey: 'grantStoreBasePath', serviceName: 'grant-store' },
  { configKey: 'permissionStoreBasePath', serviceName: 'permission-store' },
  { configKey: 'toolStoreBasePath', serviceName: 'tool-store' },
  { configKey: 'skillStoreBasePath', serviceName: 'skill-store' },
  { configKey: 'bundleStoreBasePath', serviceName: 'bundle-store' },
  { configKey: 'repoStoreBasePath', serviceName: 'repo-store' },
  { configKey: 'usageStoreBasePath', serviceName: 'usage-store' },
  { configKey: 'noteboardBasePath', serviceName: 'noteboard' },
  { configKey: 'mailBasePath', serviceName: 'mailstack' },
  { configKey: 'multichatBasePath', serviceName: 'multichat' },
  { configKey: 'orchestratorBasePath', serviceName: 'orchestrator' },
  { configKey: 'bridgeAdapterBasePath', serviceName: 'llm-bridge-adapter' },
  { configKey: 'schedulerBasePath', serviceName: 'scheduler' },
  { configKey: 'logStoreBasePath', serviceName: 'log-store' },
  { configKey: 'jobStoreBasePath', serviceName: 'job-store' },
  { configKey: 'quoteStoreBasePath', serviceName: 'quote-store' },
  { configKey: 'predictionStoreBasePath', serviceName: 'prediction-store' },
  { configKey: 'eventStoreBasePath', serviceName: 'event-store' },
  { configKey: 'authStoreBasePath', serviceName: 'auth-store' },
  { configKey: 'workGraphStoreBasePath', serviceName: 'work-graph-store' },
  { configKey: 'projectStoreBasePath', serviceName: 'project-store' },
  { configKey: 'peopleStoreBasePath', serviceName: 'people-store' },
  { configKey: 'journalStoreBasePath', serviceName: 'journal-store' },
  { configKey: 'photoStoreBasePath', serviceName: 'photo-store' },
  { configKey: 'messageStoreBasePath', serviceName: 'message-store' },
  { configKey: 'modelStoreBasePath', serviceName: 'model-store' },
  { configKey: 'hostBasePath', serviceName: 'the host' },
]

/** The backends this host proxies, which are the ones the page can ask. A
 *  base path the host left empty is a service it does not carry. */
export function serviceSettingsSourcesOf(config: BridgeConfig): ServiceSettingsSource[] {
  const sources: ServiceSettingsSource[] = []
  for (const field of SOURCE_FIELDS) {
    const basePath = config[field.configKey]
    if (typeof basePath === 'string' && basePath !== '') sources.push({ ...field, basePath })
  }
  return sources
}

/** Where a service describes itself, by the convention llm-bridge's
 *  `servicesettings.Handler` sets: `GET {base}/settings`. */
export function serviceSettingsURL(basePath: string): string {
  return `${basePath}/settings`
}

export function serviceSettingURL(basePath: string, key: string): string {
  return `${basePath}/settings/${encodeURIComponent(key)}`
}

export interface ServiceSettingsGroup {
  kind: ServiceSettingKind
  /** What the kind means, in the service's own words. */
  description: string
  settings: ServiceSetting[]
}

/** The settings grouped by kind, in the order the service served its kinds.
 *  A setting of a kind the service did not list is kept, in a group of its
 *  own at the end, rather than dropped. */
export function serviceSettingsByKind(described: ServiceSettings): ServiceSettingsGroup[] {
  const groups: ServiceSettingsGroup[] = (described.kinds ?? []).map(kind => ({ kind: kind.kind, description: kind.description, settings: [] }))
  for (const setting of described.settings ?? []) {
    let group = groups.find(g => g.kind === setting.kind)
    if (!group) {
      group = { kind: setting.kind, description: '', settings: [] }
      groups.push(group)
    }
    group.settings.push(setting)
  }
  return groups.filter(group => group.settings.length > 0)
}

/** What the value column shows for a setting that is not being edited. A
 *  secret never carries a value, so it says whether it is set. */
export function serviceSettingValueText(setting: ServiceSetting): string {
  if (setting.kind === 'secret') return setting.is_set ? 'set' : 'not set'
  if (!setting.is_set) return setting.required ? 'not set (required)' : 'not set'
  return setting.value
}

/** One line for a service's header: how many settings it has and how many of
 *  them can be changed here. */
export function serviceSettingsSummary(described: ServiceSettings): string {
  const settings = described.settings ?? []
  const editable = settings.filter(s => s.editable).length
  const fromEnvironment = settings.filter(s => s.source === 'environment').length
  return `${settings.length} settings · ${editable} changed here · ${fromEnvironment} set by the environment`
}

/** What asking one service for its settings came to. `silent` is a service
 *  that answered something other than a description — one not converted yet;
 *  `failed` is a request that never got an answer. */
export type ServiceSettingsAnswer =
  | { phase: 'described'; described: ServiceSettings }
  | { phase: 'silent'; status: number }
  | { phase: 'failed'; message: string }

/** Asks one service for its settings. Both the Service settings page and the
 *  Models page read services through this. */
export async function fetchServiceSettings(fetchFn: FetchFn, basePath: string): Promise<ServiceSettingsAnswer> {
  try {
    const res = await fetchFn(serviceSettingsURL(basePath))
    if (!res.ok) return { phase: 'silent', status: res.status }
    const described = await res.json() as ServiceSettings
    // Some other route answering 200 at this path is not a description.
    if (!described || !Array.isArray(described.settings)) return { phase: 'silent', status: res.status }
    return { phase: 'described', described }
  } catch (err) {
    return { phase: 'failed', message: err instanceof Error ? err.message : String(err) }
  }
}

/** One write, `PUT {base}/settings/{key}`. The service answers with its whole
 *  description, so a caller redraws from what is now in force rather than what
 *  was typed. A refusal comes back as the service's own text. */
export async function putServiceSetting(fetchFn: FetchFn, basePath: string, key: string, value: string):
  Promise<{ ok: true; described: ServiceSettings } | { ok: false; error: string }> {
  try {
    const res = await fetchFn(serviceSettingURL(basePath, key), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ value }),
    })
    if (!res.ok) return { ok: false, error: `PUT /settings/${key} → ${res.status}: ${(await res.text()).trim()}` }
    return { ok: true, described: await res.json() as ServiceSettings }
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) }
  }
}
