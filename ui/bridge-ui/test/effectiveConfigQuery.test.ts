import { describe, expect, it } from 'vitest'
import { EMPTY_EFFECTIVE_CONFIG_INPUTS, effectiveConfigInputsAreAskable, effectiveConfigInputsFromParams, effectiveConfigQuery } from '../src/effectiveConfigQuery'

describe('the effective-config dry-run query', () => {
  it("spells the inputs the way the server's GET /effective-config reads them", () => {
    const q = effectiveConfigQuery({ ...EMPTY_EFFECTIVE_CONFIG_INPUTS, harness: 'claude_code', instanceId: 'inst-1', boardId: 'b', tags: ['urgent', ' work '] })
    expect(q).toBe('harness=claude_code&instance_id=inst-1&board_id=b&tag=urgent&tag=work')
  })
  it('omits what is empty rather than sending blanks', () => {
    expect(effectiveConfigQuery(EMPTY_EFFECTIVE_CONFIG_INPUTS)).toBe('')
  })
  it('round-trips through URLSearchParams', () => {
    const inputs = { ...EMPTY_EFFECTIVE_CONFIG_INPUTS, harness: 'codex', principalId: 'principal_000003', bundleId: '6', cardId: 'c1', tags: ['a', 'b'] }
    expect(effectiveConfigInputsFromParams(new URLSearchParams(effectiveConfigQuery(inputs)))).toEqual(inputs)
  })
  it('is askable with a harness, an instance or a board, and not otherwise', () => {
    expect(effectiveConfigInputsAreAskable(EMPTY_EFFECTIVE_CONFIG_INPUTS)).toBe(false)
    expect(effectiveConfigInputsAreAskable({ ...EMPTY_EFFECTIVE_CONFIG_INPUTS, boardId: 'b' })).toBe(true)
    expect(effectiveConfigInputsAreAskable({ ...EMPTY_EFFECTIVE_CONFIG_INPUTS, principalId: 'p' })).toBe(false)
  })
})
