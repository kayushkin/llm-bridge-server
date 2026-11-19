import { describe, expect, it } from 'vitest'
import type { ServiceSetting, ServiceSettings } from '@kayushkin/llm-bridge-types'
import type { BridgeConfig } from '../src/context'
import {
  serviceSettingsByKind, serviceSettingsSourcesOf, serviceSettingsSummary, serviceSettingsURL, serviceSettingURL, serviceSettingValueText,
} from '../src/serviceSettings'
import { BRIDGE_PAGES } from '../src/pages'

const setting = (over: Partial<ServiceSetting>): ServiceSetting => ({
  key: 'signal_classifier.model', environment_variable: 'LLMBRIDGE_SIGNAL_CLASSIFIER_MODEL', kind: 'behavior', value_type: 'string',
  description: 'the model', value: 'claude-haiku-4-5', is_set: true, source: 'stored', default_value: 'claude-haiku-4-5',
  required: false, editable: true, notes: [], ...over,
})
const described = (settings: ServiceSetting[]): ServiceSettings => ({
  service: 'llm-bridge-server', settings,
  kinds: [
    { kind: 'behavior', description: 'Changes what the service does.' },
    { kind: 'wiring', description: 'The address of something else.' },
    { kind: 'secret', description: 'A credential.' },
  ],
  sources: [],
})

describe('serviceSettingsSourcesOf', () => {
  it('asks the bridge first, then each store the host proxies, and none it does not', () => {
    const sources = serviceSettingsSourcesOf({ basePath: '/api/bridge', kanbanStoreBasePath: '/api/kanban', grantStoreBasePath: '', multichatBasePath: '/api/multichat' } as BridgeConfig)
    expect(sources.map(s => [s.serviceName, s.basePath])).toEqual([
      ['llm-bridge-server', '/api/bridge'], ['kanban-store', '/api/kanban'], ['multichat', '/api/multichat'],
    ])
  })
  it('lists the seven services that had no base path, after the fifteen, in order', () => {
    const sources = serviceSettingsSourcesOf({
      basePath: '/api/bridge', schedulerBasePath: '/api/scheduler', logStoreBasePath: '/api/log-store',
      jobStoreBasePath: '/api/jobs', quoteStoreBasePath: '/api/quotes', predictionStoreBasePath: '/api/predictions',
      eventStoreBasePath: '/api/events', authStoreBasePath: '/api/authstore',
    } as BridgeConfig)
    expect(sources.map(s => [s.serviceName, s.basePath])).toEqual([
      ['llm-bridge-server', '/api/bridge'], ['scheduler', '/api/scheduler'], ['log-store', '/api/log-store'],
      ['job-store', '/api/jobs'], ['quote-store', '/api/quotes'], ['prediction-store', '/api/predictions'],
      ['event-store', '/api/events'], ['auth-store', '/api/authstore'],
    ])
  })
  it('lists the host itself last, under its own base path', () => {
    const sources = serviceSettingsSourcesOf({
      basePath: '/api/bridge', authStoreBasePath: '/api/authstore', hostBasePath: '/api/dash',
    } as BridgeConfig)
    expect(sources.map(s => [s.configKey, s.basePath])).toEqual([
      ['basePath', '/api/bridge'], ['authStoreBasePath', '/api/authstore'], ['hostBasePath', '/api/dash'],
    ])
    expect(serviceSettingsURL(sources[2].basePath)).toBe('/api/dash/settings')
  })
  it('does not list a host that names no base path of its own', () => {
    const sources = serviceSettingsSourcesOf({ basePath: '/api/bridge', hostBasePath: '' } as BridgeConfig)
    expect(sources.map(s => s.configKey)).toEqual(['basePath'])
  })
  it('builds the two routes of the convention', () => {
    expect(serviceSettingsURL('/api/bridge')).toBe('/api/bridge/settings')
    expect(serviceSettingURL('/api/bridge', 'session.idle_timeout')).toBe('/api/bridge/settings/session.idle_timeout')
  })
})

describe('serviceSettingsByKind', () => {
  it('groups in the order the service served its kinds and drops empty groups', () => {
    const groups = serviceSettingsByKind(described([
      setting({ key: 'service_token', kind: 'secret' }), setting({ key: 'a' }), setting({ key: 'b' }),
    ]))
    expect(groups.map(g => [g.kind, g.settings.map(s => s.key)])).toEqual([['behavior', ['a', 'b']], ['secret', ['service_token']]])
  })
  it('keeps a setting whose kind the service did not list', () => {
    const groups = serviceSettingsByKind(described([setting({ key: 'database.path', kind: 'path' })]))
    expect(groups).toEqual([{ kind: 'path', description: '', settings: [expect.objectContaining({ key: 'database.path' })] }])
  })
})

describe('serviceSettingValueText', () => {
  it('never shows a secret, only whether it is set', () => {
    expect(serviceSettingValueText(setting({ kind: 'secret', value: '', is_set: true }))).toBe('set')
    expect(serviceSettingValueText(setting({ kind: 'secret', value: '', is_set: false }))).toBe('not set')
  })
  it('says when a required setting is missing', () => {
    expect(serviceSettingValueText(setting({ kind: 'wiring', value: '', is_set: false, required: true }))).toBe('not set (required)')
    expect(serviceSettingValueText(setting({}))).toBe('claude-haiku-4-5')
  })
})

describe('serviceSettingsSummary', () => {
  it('counts what can be changed here and what the environment set', () => {
    expect(serviceSettingsSummary(described([
      setting({}), setting({ key: 'listen_address', kind: 'wiring', editable: false, source: 'environment' }), setting({ key: 'x', editable: false, source: 'default' }),
    ]))).toBe('3 settings · 1 changed here · 1 set by the environment')
  })
})

describe('the Service settings page in the registry', () => {
  it('sits in the System group and needs nothing a host can leave out', () => {
    const page = BRIDGE_PAGES.find(p => p.route === 'serviceSettings')
    expect(page?.group).toBe('system')
    expect(page?.listed).toBe(true)
    expect(page?.available).toBeUndefined()
  })
})
