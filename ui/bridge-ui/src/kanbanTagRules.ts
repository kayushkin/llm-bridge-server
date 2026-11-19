// The pure rules behind the board settings page's Tag rules section: what the
// board's stored rules become on the form, what the form becomes on the wire,
// and how rows move. No fetch, no React, so the PUT body is pinned by tests.
//
// The wire contract is kanban-store's `PUT /api/boards/{id}/tag-rules` (README
// "Tag rules"): the whole list replaces the stored one in the order sent, a
// rule sent with its `id` is kept, an omitted default is unset.
//
// ⚠️ How rules combine — which rule's default a card gets — is deliberately NOT
// here. kanban-store resolves it (`GET …/effective-defaults`) and every surface
// asks it; a client copy of the precedence would drift from the store's.

import type { BoardTagRule, BoardTagRuleInput } from '@kayushkin/kanban-store-types'
import { BOARD_DEFAULT_FIELDS, defaultsDraftOf, type DefaultsDraft } from './kanbanBoardSettings'

export interface TagRuleDraft {
  /** A stable React key for the row across moves: the stored rule's id, or a
   *  key the caller mints for a rule not yet saved. Never sent. */
  draftKey: string
  /** The stored rule's id, or null for a rule the next save creates. */
  id: string | null
  /** The tags as typed: comma or whitespace separated. Parsed on the wire and
   *  for the chips, never per keystroke into state — that would eat the
   *  separator just typed. */
  tagsText: string
  /** The four defaults, '' for unset. */
  defaults: DefaultsDraft
}

export function tagRuleDraftOf(rule: BoardTagRule): TagRuleDraft {
  return { draftKey: rule.id, id: rule.id, tagsText: rule.tags.join(', '), defaults: defaultsDraftOf(rule) }
}

export function tagRulesDraftOf(rules: readonly BoardTagRule[]): TagRuleDraft[] {
  return rules.map(tagRuleDraftOf)
}

export function emptyTagRuleDraft(draftKey: string): TagRuleDraft {
  return { draftKey, id: null, tagsText: '', defaults: defaultsDraftOf({}) }
}

/**
 * A typed tag list: split on commas and whitespace, empty pieces dropped. A tag
 * typed twice is kept twice — kanban-store refuses a rule naming a tag twice,
 * and its refusal says which rule, where a silent dedupe here would save
 * something other than what was typed.
 */
export function parseRuleTags(text: string): string[] {
  return text.split(/[\s,]+/).filter(Boolean)
}

/**
 * The `PUT /tag-rules` body, rules in form order. A stored rule's id goes back
 * so the store keeps it; each default is trimmed and a blank one is omitted.
 * Nothing is refused here: a rule with no tags or no defaults is sent as it is
 * and the store's refusal, which names the rule, is what the page shows.
 */
export function tagRulesDraftToWire(drafts: readonly TagRuleDraft[]): { rules: BoardTagRuleInput[] } {
  return {
    rules: drafts.map(draft => {
      const rule: BoardTagRuleInput = { ...(draft.id ? { id: draft.id } : {}), tags: parseRuleTags(draft.tagsText) }
      for (const field of BOARD_DEFAULT_FIELDS) {
        const value = draft.defaults[field].trim()
        if (value) rule[field] = value
      }
      return rule
    }),
  }
}

/** True when saving the form would send something other than the stored list. */
export function tagRulesDirty(stored: readonly BoardTagRule[], drafts: readonly TagRuleDraft[]): boolean {
  return JSON.stringify(tagRulesDraftToWire(tagRulesDraftOf(stored))) !== JSON.stringify(tagRulesDraftToWire(drafts))
}

function swap<T>(items: readonly T[], from: number, to: number): T[] {
  const next = [...items]
  ;[next[from], next[to]] = [next[to], next[from]]
  return next
}

function assertRowIndex(drafts: readonly unknown[], index: number): void {
  if (!Number.isInteger(index) || index < 0 || index >= drafts.length) {
    throw new RangeError(`tag rule row ${index} does not exist (the form has ${drafts.length})`)
  }
}

/** The rule at `index` one place earlier. The first row stays first — its
 *  button is disabled — but an index off the list is a bug and throws. */
export function moveTagRuleUp(drafts: readonly TagRuleDraft[], index: number): TagRuleDraft[] {
  assertRowIndex(drafts, index)
  return index === 0 ? [...drafts] : swap(drafts, index, index - 1)
}

/** The rule at `index` one place later; the last row stays last. */
export function moveTagRuleDown(drafts: readonly TagRuleDraft[], index: number): TagRuleDraft[] {
  assertRowIndex(drafts, index)
  return index === drafts.length - 1 ? [...drafts] : swap(drafts, index, index + 1)
}

export function removeTagRule(drafts: readonly TagRuleDraft[], index: number): TagRuleDraft[] {
  assertRowIndex(drafts, index)
  return drafts.filter((_, at) => at !== index)
}

/** The precedence, stated once for the settings page. It describes what
 *  kanban-store does; it is not an implementation of it. */
export const TAG_RULES_PRECEDENCE_HELP_TEXT =
  'Rules are checked top to bottom. A card matches a rule when it carries all of the rule’s tags. ' +
  'For each default, the first matching rule that sets it wins; a default a rule leaves blank falls through to the next ' +
  'matching rule, and past the last one to the board’s own default. A rule’s principal is only applied when a card ' +
  'arrives on the board with no assignee — adding a tag later assigns nobody.'
