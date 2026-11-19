import { describe, expect, it } from 'vitest'
import type { Instance } from '@kayushkin/llm-bridge-types'
import {
  CLEAR_ASSIGNMENT_POOL_PATCH, arrivalAssignmentDetailOf, assignmentPoolDraftOf, assignmentPoolPatchOf, assignmentStrategyLabel,
  describeArrivalAssignment,
  CLEAR_BUSINESS_HOURS_PATCH, CLEAR_CLASSIFIER_PATCH, budgetDraftOfSeconds, bundleLabelForID, businessHoursPatchOf,
  classifierDraftOf, classifierPatchOf, defaultSourceLabel, defaultsDraftOf, defaultsPatchOf, dispatchTargetForCard,
  dispatchTargetNote, generalDraftOf,
  generalPatchOf, ladderDraftOf, ladderDraftToWire, parseMailAccountIDs,
} from '../src/kanbanBoardSettings'
import type { Board, PriorityLadder } from '@kayushkin/kanban-store-types'
import type { Bundle } from '@kayushkin/bundle-store-types'

const board = (over: Partial<Board> = {}): Board => ({
  id: 'b1', name: 'Email', description: '', archived: false, created_at: '', updated_at: '', ...over,
})
const instance = (id: string, enabled = true): Instance =>
  ({ id, name: `name-${id}`, harness_type: 'claude_code', machine_id: 'm1', enabled }) as unknown as Instance

describe('a save sends only what changed', () => {
  it('general: an untouched field is not sent', () => {
    const stored = board({ name: 'Email', description: 'mail' })
    expect(generalPatchOf(stored, generalDraftOf(stored))).toEqual({})
    expect(generalPatchOf(stored, { ...generalDraftOf(stored), name: 'Inbox' })).toEqual({ name: 'Inbox' })
  })

  it('defaults: only the changed id goes out, so an untouched id is not re-checked with its owner', () => {
    const stored = board({ default_instance_id: 'inst-cc-local', default_bundle_id: '6' })
    const draft = defaultsDraftOf(stored)
    expect(defaultsPatchOf(stored, draft)).toEqual({})
    expect(defaultsPatchOf(stored, { ...draft, default_bundle_id: '7' })).toEqual({ default_bundle_id: '7' })
  })
})

describe('clearing', () => {
  it('a cleared default goes out as the empty string, the store’s own word for clear', () => {
    const stored = board({ default_principal_id: 'principal_000004', default_instance_id: 'inst-cc-local' })
    expect(defaultsPatchOf(stored, { ...defaultsDraftOf(stored), default_instance_id: '' })).toEqual({ default_instance_id: '' })
  })

  it('a switched-off classifier goes out as an empty object', () => {
    const stored = board({ classifier: { vocabulary: 'work', mail_account_ids: ['demo-work'], hold_new_cards: true } })
    expect(classifierPatchOf({ ...classifierDraftOf(stored), enabled: false })).toEqual({ classifier: {} })
    expect(CLEAR_CLASSIFIER_PATCH).toEqual({ classifier: {} })
  })

  it('cleared business hours go out as an empty object', () => {
    expect(CLEAR_BUSINESS_HOURS_PATCH).toEqual({ business_hours: {} })
  })
})

describe('classifier and business hours', () => {
  it('a classifier saved from the form keeps the organization it was stored with', () => {
    const stored = board({ classifier: { vocabulary: 'personal', mail_account_ids: ['gmail-personal'], hold_new_cards: false, organization_id: 'principal_000023' } })
    expect(classifierPatchOf(classifierDraftOf(stored))).toEqual({
      classifier: { vocabulary: 'personal', mail_account_ids: ['gmail-personal'], hold_new_cards: false, organization_id: 'principal_000023' },
    })
  })
  it('trims the vocabulary and sends the account list and hold flag as the store names them', () => {
    expect(classifierPatchOf({ enabled: true, vocabulary: ' work ', mailAccountIDs: [' demo-work ', ''], holdNewCards: true, organizationID: '' }))
      .toEqual({ classifier: { vocabulary: 'work', mail_account_ids: ['demo-work'], hold_new_cards: true } })
  })

  it('parses a typed account list on commas and whitespace, without repeats', () => {
    expect(parseMailAccountIDs('gmail-personal, imap-pretender\ngmail-personal')).toEqual(['gmail-personal', 'imap-pretender'])
  })

  it('sends days in weekday order whatever order they were ticked in', () => {
    expect(businessHoursPatchOf({ tzid: 'America/Los_Angeles', days: ['FR', 'MO', 'WE'], start: '09:00', end: '17:00' }))
      .toEqual({ business_hours: { tzid: 'America/Los_Angeles', days: ['MO', 'WE', 'FR'], start: '09:00', end: '17:00' } })
  })
})

describe('priority ladder', () => {
  it('converts hours and days to budget_seconds, and an empty budget to null', () => {
    const result = ladderDraftToWire([
      { label: 'P0', priorityValue: '5', budgetAmount: '2', budgetUnit: 'hours', defaultAutoHoldAtUSD: '' },
      { label: 'P3', priorityValue: '2', budgetAmount: '7', budgetUnit: 'days', defaultAutoHoldAtUSD: '' },
      { label: 'P4', priorityValue: '1', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: '' },
    ])
    expect(result).toEqual({ ok: true, value: { levels: [
      { priority_value: 5, label: 'P0', budget_seconds: 7200, default_auto_hold_at_usd: null },
      { priority_value: 2, label: 'P3', budget_seconds: 604800, default_auto_hold_at_usd: null },
      { priority_value: 1, label: 'P4', budget_seconds: null, default_auto_hold_at_usd: null },
    ] } })
  })

  it('shows a stored budget in whole days when it is one, in hours otherwise', () => {
    expect(budgetDraftOfSeconds(86400)).toEqual({ budgetAmount: '1', budgetUnit: 'days' })
    expect(budgetDraftOfSeconds(5400)).toEqual({ budgetAmount: '1.5', budgetUnit: 'hours' })
    expect(budgetDraftOfSeconds(null)).toEqual({ budgetAmount: '', budgetUnit: 'hours' })
  })

  it('round-trips the live ladder unchanged', () => {
    const ladder: PriorityLadder = { board_id: 'b1', levels: [
      { board_id: 'b1', priority_value: 5, label: 'P0', budget_seconds: 7200, default_auto_hold_at_usd: 20 },
      { board_id: 'b1', priority_value: 4, label: 'P1', budget_seconds: 28800, default_auto_hold_at_usd: 0 },
      { board_id: 'b1', priority_value: 1, label: 'P4', budget_seconds: 2592000, default_auto_hold_at_usd: null },
    ] }
    const result = ladderDraftToWire(ladderDraftOf(ladder))
    expect(result.ok && result.value.levels).toEqual(ladder.levels.map(({ board_id: _board, ...level }) => level))
  })

  it('sends a default cost in dollars, keeps zero as a real default and an empty box as none', () => {
    const result = ladderDraftToWire([
      { label: 'P0', priorityValue: '5', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: '12.5' },
      { label: 'P1', priorityValue: '4', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: '0' },
      { label: 'P4', priorityValue: '1', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: ' ' },
    ])
    expect(result.ok && result.value.levels.map(level => level.default_auto_hold_at_usd)).toEqual([12.5, 0, null])
    const bad = ladderDraftToWire([{ label: 'P0', priorityValue: '5', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: 'lots' }])
    expect(bad.ok).toBe(false)
    if (!bad.ok) expect(bad.error).toMatch(/default cost/)
  })

  it('refuses a priority that is not a whole number, naming the row, and leaves a zero rung to the store', () => {
    const bad = ladderDraftToWire([{ label: 'P0', priorityValue: 'high', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: '' }])
    expect(bad.ok).toBe(false)
    if (!bad.ok) expect(bad.error).toMatch(/row 1 \("P0"\)/)
    expect(ladderDraftToWire([{ label: 'P0', priorityValue: '0', budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: '' }]).ok).toBe(true)
  })
})

describe('where a dispatch runs when nobody chooses', () => {
  const instances = [instance('inst-a'), instance('inst-b'), instance('inst-off', false)]
  // What kanban-store answers for the card: the resolved value and which rule
  // or board set it. This module never decides that — it only words it.
  const fromBoard = (value: string) => ({ value, source: { kind: 'board' } })
  const fromRule = (value: string) => ({
    value,
    source: { kind: 'tag_rule', rule_id: 'rule_1', rule_tags: ['cat:product', 'urgency:high'] },
  })

  it('prefers the card’s last session’s instance over the effective default', () => {
    const target = dispatchTargetForCard({ lastSessionInstanceID: 'inst-a', defaultInstance: fromBoard('inst-b'), instances })
    expect(target.kind).toBe('last-session')
    if (target.kind === 'last-session') expect(target.instance.id).toBe('inst-a')
  })

  it('uses the effective default instance for a card with no session, keeping its source', () => {
    const target = dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: fromBoard('inst-b'), instances })
    expect(target).toMatchObject({ kind: 'default-instance', lastSessionNote: null })
    if (target.kind === 'default-instance') {
      expect(target.instance.id).toBe('inst-b')
      expect(target.source.kind).toBe('board')
    }
  })

  it('carries a tag rule’s source through, so the confirmation can name the rule', () => {
    const target = dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: fromRule('inst-b'), instances })
    expect(target.kind).toBe('default-instance')
    if (target.kind === 'default-instance') expect(target.source.rule_tags).toEqual(['cat:product', 'urgency:high'])
  })

  it('falls to the effective default when the last session’s instance is gone, and says why', () => {
    const target = dispatchTargetForCard({ lastSessionInstanceID: 'inst-gone', defaultInstance: fromBoard('inst-b'), instances })
    expect(target.kind).toBe('default-instance')
    if (target.kind === 'default-instance') expect(target.lastSessionNote).toMatch(/does not list/)
  })

  it('picks nothing when the effective default is disabled, and says so', () => {
    const target = dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: fromBoard('inst-off'), instances })
    expect(target.kind).toBe('none')
    if (target.kind === 'none') expect(target.why).toMatch(/disabled/)
  })

  it('names the rule in the refusal when a tag rule chose the unusable instance', () => {
    const target = dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: fromRule('inst-off'), instances })
    expect(target.kind).toBe('none')
    if (target.kind === 'none') expect(target.why).toMatch(/from tag rule cat:product \+ urgency:high/)
  })

  it('picks nothing, with nothing to explain, when there is neither', () => {
    expect(dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: null, instances }))
      .toEqual({ kind: 'none', why: null })
  })

  it('notes where the target came from in the words both surfaces use', () => {
    expect(dispatchTargetNote(dispatchTargetForCard({ lastSessionInstanceID: 'inst-a', defaultInstance: null, instances })))
      .toBe('where this card\u2019s last session ran')
    expect(dispatchTargetNote(dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: fromRule('inst-b'), instances })))
      .toBe('the default instance (from tag rule cat:product + urgency:high)')
    expect(dispatchTargetNote(dispatchTargetForCard({ lastSessionInstanceID: null, defaultInstance: null, instances }))).toBeNull()
  })
})

describe('where a resolved default came from, in words', () => {
  it('names the board and the rule that set it', () => {
    expect(defaultSourceLabel({ kind: 'board' })).toBe('board default')
    expect(defaultSourceLabel({ kind: 'tag_rule', rule_id: 'r1', rule_tags: ['a', 'b'] })).toBe('from tag rule a + b')
  })

  it('falls back to the rule id when the store named no tags', () => {
    expect(defaultSourceLabel({ kind: 'tag_rule', rule_id: 'r1' })).toBe('from tag rule r1')
  })

  it('says a source kind it does not know is unknown, rather than passing it off as a board default', () => {
    expect(defaultSourceLabel({ kind: 'column_rule' })).toBe('from unknown source kind "column_rule"')
  })
})

describe('a bundle named by its bundle-store id', () => {
  const bundles = [{ id: 6, name: 'docker', display_name: 'Docker', enabled: true }] as unknown as Bundle[]

  it('is named from the bundle list, joined on the id', () => {
    expect(bundleLabelForID('6', bundles)).toBe('docker — Docker')
  })

  it('shows the raw id, never nothing, when the list lacks it or has not loaded', () => {
    expect(bundleLabelForID('9', bundles)).toBe('9')
    expect(bundleLabelForID('6', null)).toBe('6')
  })
})

describe('assignment pool', () => {
  it('a board with no pool reads as an empty form, and saving that form sends the clear', () => {
    const stored = board()
    expect(assignmentPoolDraftOf(stored)).toEqual({ principalID: '', strategy: '' })
    expect(assignmentPoolPatchOf(assignmentPoolDraftOf(stored))).toEqual(CLEAR_ASSIGNMENT_POOL_PATCH)
    expect(CLEAR_ASSIGNMENT_POOL_PATCH).toEqual({ assignment_pool: {} })
  })

  it('a stored pool round-trips to the same wire object', () => {
    const stored = board({ assignment_pool: { principal_id: 'principal_000030', strategy: 'round_robin' } })
    expect(assignmentPoolPatchOf(assignmentPoolDraftOf(stored))).toEqual({
      assignment_pool: { principal_id: 'principal_000030', strategy: 'round_robin' },
    })
  })

  it('choosing no group clears the pool whatever strategy is left on the form', () => {
    expect(assignmentPoolPatchOf({ principalID: '  ', strategy: 'round_robin' })).toEqual({ assignment_pool: {} })
  })

  it('a group with no strategy is sent as it stands, so the store’s refusal names the strategies', () => {
    expect(assignmentPoolPatchOf({ principalID: 'principal_000030', strategy: '' })).toEqual({
      assignment_pool: { principal_id: 'principal_000030', strategy: '' },
    })
  })

  it('a strategy this page has no words for is shown as its own name', () => {
    expect(assignmentStrategyLabel('least_open_cards')).toContain('fewest')
    expect(assignmentStrategyLabel('by_skill')).toBe('by_skill')
  })
})

describe('why a card got its assignee on arrival, in words', () => {
  const words = (kind: string, detail: unknown, name?: (id: string) => string) =>
    describeArrivalAssignment(kind, arrivalAssignmentDetailOf({ detail }), name)

  it('the pool, with its strategy and how many were available', () => {
    expect(words('assigned', {
      principal_id: 'principal_000011', source: 'pool', strategy: 'least_open_cards', pool_principal_id: 'principal_000030', candidates: 3,
    }, id => (id === 'principal_000030' ? 'Northwind Eng' : id))).toBe('from the pool Northwind Eng by least open cards, 3 available')
  })

  it('the board default, and whether the pool was empty', () => {
    expect(words('assigned', { principal_id: 'principal_000024', source: 'board_default' })).toBe('board default')
    expect(words('assigned', { principal_id: 'principal_000024', source: 'board_default', pool_empty: true, pool_principal_id: 'principal_000030' }))
      .toBe('board default — nobody in the pool principal_000030 was available')
  })

  it('a tag rule, by its tags', () => {
    expect(words('assigned', { principal_id: 'p', source: 'tag_rule', rule_id: 'r1', rule_tags: ['cat:product', 'urgency:high'] }))
      .toBe('from tag rule cat:product + urgency:high')
  })

  it('a skipped assignment says the card is unassigned and why', () => {
    expect(words('assignment_skipped', { reason: 'pool_empty', pool_principal_id: 'principal_000030' }))
      .toBe('nobody in the pool principal_000030 was available and the board has no default principal, so the card is unassigned')
  })

  it('a source kanban-store adds later is named, not passed off as a known one', () => {
    expect(words('assigned', { principal_id: 'p', source: 'skills_match' })).toBe('from unknown source "skills_match"')
  })

  it('an assignment made by hand, or a detail that is not an object, says nothing', () => {
    expect(words('assigned', undefined)).toBeNull()
    expect(words('assigned', { principal_id: 'p' })).toBeNull()
    expect(words('assigned', ['source', 'pool'])).toBeNull()
    expect(words('assigned', 'pool')).toBeNull()
  })
})
