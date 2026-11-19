import { describe, expect, it } from 'vitest'
import { DEFAULT_BRIDGE_ROUTES } from '../src/context'
import { SETTINGS_SCOPE_INDEX, SETTINGS_SCOPE_LABEL, saveResultOf, type SettingsScope } from '../src/components/settings/SettingsSection'

describe('saveResultOf', () => {
  it('reads a settled write as ok', async () => {
    expect(await saveResultOf(Promise.resolve())).toEqual({ ok: true })
  })
  it("keeps the store's own words when the write is refused", async () => {
    const result = await saveResultOf(Promise.reject(new Error('PUT /api/bridge/bridge-prefs → 400: unknown field "defaults2"')))
    expect(result).toEqual({ ok: false, error: 'PUT /api/bridge/bridge-prefs → 400: unknown field "defaults2"' })
  })
  it('does not lose a rejection that is not an Error', async () => {
    expect(await saveResultOf(Promise.reject('refused'))).toEqual({ ok: false, error: 'refused' })
  })
})

describe('the scope index', () => {
  const scopes: SettingsScope[] = ['global', 'harness', 'instance', 'board', 'principal', 'session', 'browser']
  it('names every scope exactly once, in override order (widest first)', () => {
    expect(SETTINGS_SCOPE_INDEX.map(r => r.scope)).toEqual(scopes)
  })
  it('points every row at a route this library ships', () => {
    for (const row of SETTINGS_SCOPE_INDEX) expect(DEFAULT_BRIDGE_ROUTES[row.route], row.scope).toBeTruthy()
  })
  it('has a label for every scope', () => {
    for (const scope of scopes) expect(SETTINGS_SCOPE_LABEL[scope]).toBeTruthy()
  })
})
