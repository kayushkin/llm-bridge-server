import { describe, expect, it } from 'vitest'
import { bundleDraftOf, bundleDraftToWire, emptyBundleDraft, parseTagList } from '../src/bundleDraft'
import type { Bundle } from '@kayushkin/bundle-store-types'

describe('parseTagList', () => {
  it('splits on commas, trims, drops empties and repeats', () => {
    expect(parseTagList(' react, frontend ,,react, ')).toEqual(['react', 'frontend'])
    expect(parseTagList('')).toEqual([])
  })
})

describe('bundleDraftToWire', () => {
  it('refuses a draft with no name', () => {
    const result = bundleDraftToWire(emptyBundleDraft())
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toMatch(/needs a name/)
  })

  it('refuses a name with whitespace', () => {
    const result = bundleDraftToWire({ ...emptyBundleDraft(), name: 'go service' })
    expect(result.ok).toBe(false)
  })

  it('refuses a member without a positive id, naming it', () => {
    const draft = { ...emptyBundleDraft(), name: 'x', members: [{ kind: 'skill' as const, id: '', name: 'sql-pro', condition: '', effect: '' }] }
    const result = bundleDraftToWire(draft)
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toMatch(/"sql-pro".*no id/)
  })

  it('refuses the same member twice — the store keys members on (kind, id)', () => {
    const member = { kind: 'tool' as const, id: '13', name: 'playwright', condition: '', effect: '' }
    const result = bundleDraftToWire({ ...emptyBundleDraft(), name: 'x', members: [member, { ...member, name: 'other label' }] })
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toMatch(/tool #13 is on the list twice/)
  })

  it('builds the POST /bundles body, with extends_id and never extends', () => {
    const result = bundleDraftToWire({
      name: ' e2e-testing ', displayName: 'E2E', description: 'd', extendsID: '1',
      matchTagsText: 'e2e, playwright', model: '', effort: 'high', enabled: true, deniedReadPaths: [],
      members: [
        { kind: 'skill', id: '1283', name: 'test-automator', condition: '', effect: '' },
        { kind: 'tool', id: '14', name: 'chrome-devtools', condition: ' perf ', effect: '' },
      ],
    })
    expect(result).toEqual({
      ok: true,
      value: {
        name: 'e2e-testing', display_name: 'E2E', description: 'd', extends_id: 1,
        match_tags: ['e2e', 'playwright'], denied_read_paths: [], model: '', effort: 'high', enabled: true,
        members: [
          { kind: 'skill', id: 1283, name: 'test-automator', effect: '' },
          { kind: 'tool', id: 14, name: 'chrome-devtools', condition: 'perf', effect: '' },
        ],
      },
    })
    if (result.ok) expect('extends' in result.value).toBe(false)
  })

  it('leaves extends_id out for a root bundle', () => {
    const result = bundleDraftToWire({ ...emptyBundleDraft(), name: 'root' })
    expect(result.ok && 'extends_id' in result.value).toBe(false)
  })
})

describe('bundleDraftOf', () => {
  it('round-trips a stored bundle through the draft and back', () => {
    const stored: Bundle = {
      id: 2, name: 'react', display_name: 'React frontend', description: 'Frontend web work',
      extends_id: 1, extends: 'base', match_tags: ['react', 'frontend'],
      denied_read_paths: ['~/.ssh/', '/etc/shadow'],
      members: [
        { kind: 'skill', id: 1446, name: 'browser-automation', effect: 'include' },
        { kind: 'tool', id: 14, name: 'chrome-devtools', condition: 'perf', effect: 'include' },
        // The form cannot set a deny, so it must hand one back exactly as it came:
        // an empty effect is stored as include.
        { kind: 'tool', id: 13, name: 'playwright', effect: 'deny' },
      ],
      enabled: true, created_at: 1, updated_at: 2,
    }
    const draft = bundleDraftOf(stored)
    expect(draft.extendsID).toBe('1')
    expect(draft.matchTagsText).toBe('react, frontend')
    const wire = bundleDraftToWire(draft)
    expect(wire.ok).toBe(true)
    if (wire.ok) {
      expect(wire.value.members).toEqual(stored.members)
      expect(wire.value.extends_id).toBe(1)
      expect(wire.value.match_tags).toEqual(stored.match_tags)
      expect(wire.value.denied_read_paths).toEqual(stored.denied_read_paths)
    }
  })

  it('reads a bundle with null members and tags as empty', () => {
    const draft = bundleDraftOf({ id: 1, name: 'base', match_tags: null, members: null, enabled: true, created_at: 0, updated_at: 0 })
    expect(draft.members).toEqual([])
    expect(draft.matchTagsText).toBe('')
    expect(draft.extendsID).toBe('')
  })
})
