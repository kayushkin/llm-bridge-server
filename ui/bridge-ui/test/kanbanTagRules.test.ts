import { describe, expect, it } from 'vitest'
import {
  emptyTagRuleDraft, moveTagRuleDown, moveTagRuleUp, parseRuleTags, removeTagRule, tagRulesDirty, tagRulesDraftOf,
  tagRulesDraftToWire, type TagRuleDraft,
} from '../src/kanbanTagRules'
import type { BoardTagRule } from '@kayushkin/kanban-store-types'

const storedRule = (id: string, position: number, tags: string[], over: Partial<BoardTagRule> = {}): BoardTagRule => ({
  id, board_id: 'b1', position, tags, created_at: '', updated_at: '', ...over,
})

const stored: BoardTagRule[] = [
  storedRule('rule-a', 0, ['cat:product', 'urgency:high'], { default_instance_id: 'inst-cc-local' }),
  storedRule('rule-b', 1, ['cat:product'], { default_bundle_id: '6', default_agent_id: '12' }),
]

describe('tag rules on the wire', () => {
  it('round-trips the stored list: same order, ids sent back, unset defaults omitted', () => {
    expect(tagRulesDraftToWire(tagRulesDraftOf(stored))).toEqual({ rules: [
      { id: 'rule-a', tags: ['cat:product', 'urgency:high'], default_instance_id: 'inst-cc-local' },
      { id: 'rule-b', tags: ['cat:product'], default_agent_id: '12', default_bundle_id: '6' },
    ] })
    expect(tagRulesDirty(stored, tagRulesDraftOf(stored))).toBe(false)
  })

  it('sends a new rule without an id, its tags parsed on commas and whitespace, blank defaults omitted and ids trimmed', () => {
    const fresh: TagRuleDraft = {
      ...emptyTagRuleDraft('new-rule-1'),
      tagsText: ' urgency:high,  cat:ops\nteam:infra ',
      defaults: { default_principal_id: '', default_agent_id: '   ', default_instance_id: ' inst-cc-local ', default_bundle_id: '' },
    }
    expect(tagRulesDraftToWire([fresh])).toEqual({ rules: [
      { tags: ['urgency:high', 'cat:ops', 'team:infra'], default_instance_id: 'inst-cc-local' },
    ] })
  })

  it('keeps a tag typed twice and a rule with no tags or defaults, so the store’s refusal names them', () => {
    expect(parseRuleTags('a, a')).toEqual(['a', 'a'])
    expect(tagRulesDraftToWire([emptyTagRuleDraft('new-rule-1')])).toEqual({ rules: [{ tags: [] }] })
  })

  it('an empty form is an empty list, which removes every rule', () => {
    expect(tagRulesDraftToWire([])).toEqual({ rules: [] })
    expect(tagRulesDirty(stored, [])).toBe(true)
  })
})

describe('moving and removing rows', () => {
  const drafts = tagRulesDraftOf([...stored, storedRule('rule-c', 2, ['team:infra'], { default_principal_id: 'principal_000001' })])
  const ids = (rows: TagRuleDraft[]) => tagRulesDraftToWire(rows).rules.map(rule => rule.id)

  it('moves a rule up and down, and the save sends the new order with the same ids', () => {
    expect(ids(moveTagRuleUp(drafts, 2))).toEqual(['rule-a', 'rule-c', 'rule-b'])
    expect(ids(moveTagRuleDown(drafts, 0))).toEqual(['rule-b', 'rule-a', 'rule-c'])
    expect(tagRulesDirty(stored, moveTagRuleDown(tagRulesDraftOf(stored), 0))).toBe(true)
  })

  it('leaves the first row first and the last row last, and does not mutate the form it is given', () => {
    expect(ids(moveTagRuleUp(drafts, 0))).toEqual(['rule-a', 'rule-b', 'rule-c'])
    expect(ids(moveTagRuleDown(drafts, 2))).toEqual(['rule-a', 'rule-b', 'rule-c'])
    moveTagRuleUp(drafts, 1)
    expect(ids(drafts)).toEqual(['rule-a', 'rule-b', 'rule-c'])
  })

  it('removes a rule', () => {
    expect(ids(removeTagRule(drafts, 1))).toEqual(['rule-a', 'rule-c'])
  })

  it('throws for a row that does not exist rather than quietly doing nothing', () => {
    expect(() => moveTagRuleUp(drafts, 3)).toThrow(RangeError)
    expect(() => removeTagRule(drafts, -1)).toThrow(RangeError)
  })
})
