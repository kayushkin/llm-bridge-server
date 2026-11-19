import { describe, expect, it } from 'vitest'
import type { ServiceSetting, ServiceSettings } from '@kayushkin/llm-bridge-types'
import {
  addableModels, appendModel, backgroundCallRowsOf, isModelRoleSetting, listedModelProblem, modelPriceText, modelStoreErrorText,
  modelStoreModelsURL, modelStoreRolesURL, moveModel, removeModel, roleModelsSaveBody, roleModelsText, roleOptions, sameModelList,
  saveRoleModels,
} from '../src/modelRoles'
import { fetchServiceSettings, putServiceSetting } from '../src/serviceSettings'
import type { ModelStoreModel, ModelStoreRoles } from '../src/types-model-store'
import { BRIDGE_PAGES, navEntriesFor } from '../src/pages'
import { DEFAULT_BRIDGE_ROUTES, type BridgeConfig } from '../src/context'

// Fixtures: the roles and models are made up here; the page reads them from
// model-store.
const roles: ModelStoreRoles = {
  canonical: ['best', 'default', 'balanced', 'efficient'],
  roles: { best: 'model-big', default: 'model-big', balanced: 'model-mid', efficient: 'model-small' },
  models: {
    best: ['model-big'], default: ['model-big'], balanced: ['model-mid', 'model-small'], efficient: ['model-small', 'model-mid', 'model-tiny'],
  },
}

const model = (over: Partial<ModelStoreModel>): ModelStoreModel => ({
  id: 'model-x', provider: 'acme', name: 'Model X', short_name: '', aliases: null, max_tokens: 1000,
  input_cost: 1, output_cost: 5, enabled: true, priority: 10, ...over,
})

const setting = (over: Partial<ServiceSetting>): ServiceSetting => ({
  key: 'turn_end_classifier.model_role', environment_variable: 'LLMBRIDGE_TURN_END_CLASSIFIER_MODEL_ROLE', kind: 'behavior',
  value_type: 'model_role', description: 'Which role classifies a turn end', value: 'balanced', is_set: true, source: 'stored',
  default_value: 'efficient', required: false, editable: true, notes: [], ...over,
})

const described = (service: string, settings: ServiceSetting[]): ServiceSettings => ({ service, settings, kinds: [], sources: [] })

describe('model-store paths', () => {
  it('asks under /api, which model-store roots its routes at', () => {
    expect(modelStoreRolesURL('/api/model-store')).toBe('/api/model-store/api/roles')
    expect(modelStoreModelsURL('/api/model-store')).toBe('/api/model-store/api/models')
  })
})

describe('roleModelsText', () => {
  it('names the first model, then each fallback in order', () => {
    expect(roleModelsText(['model-small', 'model-mid', 'model-tiny'])).toBe('model-small, then model-mid, then model-tiny')
    expect(roleModelsText(['model-big'])).toBe('model-big')
    expect(roleModelsText([])).toBe('')
  })
})

describe('roleOptions', () => {
  it('offers model-store canonical roles in its order, each with its models', () => {
    expect(roleOptions(roles, 'balanced')).toEqual([
      { role: 'best', label: 'best — model-big', unknown: false },
      { role: 'default', label: 'default — model-big', unknown: false },
      { role: 'balanced', label: 'balanced — model-mid, then model-small', unknown: false },
      { role: 'efficient', label: 'efficient — model-small, then model-mid, then model-tiny', unknown: false },
    ])
  })
  it('keeps a current value model-store does not know, marked, rather than showing another role', () => {
    const options = roleOptions(roles, 'cheap')
    expect(options.at(-1)).toEqual({ role: 'cheap', label: 'cheap — not a model-store role', unknown: true })
    expect(options).toHaveLength(5)
  })
  it('says when a canonical role lists no model', () => {
    expect(roleOptions({ ...roles, canonical: ['new'], models: {} }, '')[0].label).toBe('new — no models')
  })
})

describe('backgroundCallRowsOf', () => {
  it('takes only the model_role settings, with the service that holds them', () => {
    const answer = described('llm-bridge-server', [
      setting({}),
      setting({ key: 'session.idle_timeout', value_type: 'duration', value: '15m' }),
      setting({ key: 'renamer.model_role', value: 'efficient' }),
    ])
    const rows = backgroundCallRowsOf('basePath', '/api/bridge', answer)
    expect(rows.map(r => [r.service, r.basePath, r.setting.key])).toEqual([
      ['llm-bridge-server', '/api/bridge', 'turn_end_classifier.model_role'],
      ['llm-bridge-server', '/api/bridge', 'renamer.model_role'],
    ])
    expect(isModelRoleSetting(setting({ value_type: 'string' }))).toBe(false)
  })
})

describe('role list editing', () => {
  const list = ['a', 'b', 'c']
  it('moves a model up and down, and not past either end', () => {
    expect(moveModel(list, 2, -1)).toEqual(['a', 'c', 'b'])
    expect(moveModel(list, 0, 1)).toEqual(['b', 'a', 'c'])
    expect(moveModel(list, 0, -1)).toEqual(list)
    expect(moveModel(list, 2, 1)).toEqual(list)
  })
  it('removes by position and appends without duplicating', () => {
    expect(removeModel(list, 1)).toEqual(['a', 'c'])
    expect(appendModel(list, 'd')).toEqual(['a', 'b', 'c', 'd'])
    expect(appendModel(list, 'a')).toEqual(list)
  })
  it('compares lists by order', () => {
    expect(sameModelList(['a', 'b'], ['a', 'b'])).toBe(true)
    expect(sameModelList(['a', 'b'], ['b', 'a'])).toBe(false)
  })
  it('sends the whole list as model-store takes it', () => {
    expect(roleModelsSaveBody('balanced', ['model-mid', 'model-small'])).toEqual({ role: 'balanced', models: ['model-mid', 'model-small'] })
  })
})

describe('models to add', () => {
  const registry = [
    model({ id: 'z-one', provider: 'zeta' }), model({ id: 'a-off', provider: 'acme', enabled: false }),
    model({ id: 'a-two', provider: 'acme' }), model({ id: 'a-one', provider: 'acme' }),
  ]
  it('offers enabled models not already listed, by provider then id', () => {
    expect(addableModels(registry, ['a-two']).map(m => m.id)).toEqual(['a-one', 'z-one'])
  })
  it('flags a listed model that is disabled or gone', () => {
    expect(listedModelProblem(registry, 'a-off')).toBe('disabled')
    expect(listedModelProblem(registry, 'nowhere')).toBe('not in model-store')
    expect(listedModelProblem(registry, 'a-one')).toBeNull()
  })
  it('prices per million tokens', () => {
    expect(modelPriceText({ input_cost: 0.8, output_cost: 4 })).toBe('$0.80 in · $4.00 out per million tokens')
  })
})

describe('saving a role', () => {
  it('posts the list and returns model-store refusal text on a 400', async () => {
    const calls: { url: string; opts?: RequestInit }[] = []
    const fetchFn = async (url: string, opts?: RequestInit) => {
      calls.push({ url, opts })
      return new Response(JSON.stringify({ error: 'unknown model "nope"' }), { status: 400 })
    }
    const result = await saveRoleModels(fetchFn, '/api/model-store', 'balanced', ['nope'])
    expect(result).toEqual({ ok: false, error: 'POST /api/roles → 400: unknown model "nope"' })
    expect(calls[0].url).toBe('/api/model-store/api/roles')
    expect(calls[0].opts?.method).toBe('POST')
    expect(JSON.parse(String(calls[0].opts?.body))).toEqual({ role: 'balanced', models: ['nope'] })
  })
  it('keeps a body that is not JSON as it came', () => {
    expect(modelStoreErrorText(502, 'model-store unavailable\n')).toBe('502: model-store unavailable')
  })
})

describe('the shared service settings requests', () => {
  it('reads a description, and calls anything else silent', async () => {
    const ok = await fetchServiceSettings(async () => new Response(JSON.stringify(described('svc', [setting({})]))), '/api/x')
    expect(ok.phase).toBe('described')
    const notFound = await fetchServiceSettings(async () => new Response('no', { status: 404 }), '/api/x')
    expect(notFound).toEqual({ phase: 'silent', status: 404 })
    const other = await fetchServiceSettings(async () => new Response('{"hello":1}'), '/api/x')
    expect(other).toEqual({ phase: 'silent', status: 200 })
    const down = await fetchServiceSettings(async () => { throw new Error('offline') }, '/api/x')
    expect(down).toEqual({ phase: 'failed', message: 'offline' })
  })
  it('writes one setting and returns the refusal verbatim', async () => {
    const refused = await putServiceSetting(async () => new Response('not a role: cheap\n', { status: 400 }), '/api/x', 'renamer.model_role', 'cheap')
    expect(refused).toEqual({ ok: false, error: 'PUT /settings/renamer.model_role → 400: not a role: cheap' })
  })
})

describe('the Models page', () => {
  it('is in System, and listed only where the host proxies model-store', () => {
    const page = BRIDGE_PAGES.find(p => p.route === 'models')
    expect(page?.group).toBe('system')
    const flags = { showConformance: false, showServiceInventory: false }
    const base = { basePath: '/api/bridge', routes: DEFAULT_BRIDGE_ROUTES } as BridgeConfig
    expect(navEntriesFor(base, flags).some(e => e.to === '/models')).toBe(false)
    expect(navEntriesFor({ ...base, modelStoreBasePath: '/api/model-store' }, flags).some(e => e.to === '/models')).toBe(true)
  })
})
