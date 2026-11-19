import type { CardView } from '@kayushkin/kanban-store-types'
import type { Item as NoteboardItem } from '@kayushkin/noteboard-types'

/**
 * Card axes — the classification a card carries in its noteboard tags.
 *
 * A board can only have one axis as its columns. The Email board spends that
 * slot on action state (No action / Action needed / Action completed), so the
 * other two axes ride along as prefixed tags and this module is what turns them
 * back into something you can filter, group and edit.
 *
 * Nothing here is email-specific by construction: a board whose cards carry no
 * prefixed tags reports no axes and the UI leaves it exactly as it was.
 */

export interface AxisDefinition {
  /** Tag prefix, including the colon. */
  prefix: string
  /** Short label for the control. */
  label: string
  /**
   * Known values, in the order they should be offered. A value found on a card
   * but missing from this list is still shown — the list orders the vocabulary,
   * it does not police it, and hiding an unexpected value would hide exactly the
   * classifier bug worth seeing.
   */
  values: string[]
}

/**
 * The axes email-classifier writes. Kept in the same order the controls should
 * appear: what it is, what it needs, how soon.
 */
export const CARD_AXES: AxisDefinition[] = [
  {
    prefix: 'cat:',
    label: 'Category',
    values: [
      'finance', 'housing', 'health', 'commerce', 'work-career', 'dev-infra',
      'social-events', 'government-legal', 'media-subs', 'politics', 'promotions', 'personal',
    ],
  },
  {
    prefix: 'action:',
    label: 'Action',
    values: ['reply', 'pay', 'decide', 'schedule', 'verify', 'fix', 'read', 'file', 'none'],
  },
  {
    prefix: 'urgency:',
    label: 'Urgency',
    // Ordered by pressure rather than alphabetically, so the control reads as a
    // scale. 'expired' sits last because a passed deadline is not urgency, it is
    // history — but it must stay visible rather than collapse into 'low'.
    values: ['critical', 'high', 'normal', 'low', 'expired'],
  },
  {
    prefix: 'work:',
    label: 'Work',
    values: ['not-started', 'in-progress', 'blocked', 'done'],
  },
]

export const WORK_AXIS_PREFIX = 'work:'
export const WORK_NOT_STARTED = 'not-started'
export const WORK_IN_PROGRESS = 'in-progress'

/** Rank used to sort urgency descending; unknown values sort last. */
const URGENCY_RANK: Record<string, number> = {
  critical: 0, high: 1, normal: 2, low: 3, expired: 4,
}

export function tagsOf(card: CardView): string[] {
  const item = card.item as NoteboardItem | null
  return item?.tags ?? []
}

/** Returns the value of the axis tag on a card, or '' when it carries none. */
export function axisValue(card: CardView, prefix: string): string {
  const found = tagsOf(card).find(t => t.startsWith(prefix))
  return found ? found.slice(prefix.length) : ''
}

/**
 * Replaces (or adds) one axis tag, leaving every other tag untouched.
 * Passing an empty value removes the axis tag entirely.
 */
export function withAxisValue(tags: string[], prefix: string, value: string): string[] {
  const rest = tags.filter(t => !t.startsWith(prefix))
  return value ? [...rest, prefix + value] : rest
}

/**
 * The axes actually present on a board, each with the values in use and how many
 * cards carry them. An axis nobody uses is omitted, so the toolbar shows only
 * controls that can do something.
 */
export interface AxisUsage {
  axis: AxisDefinition
  /** Values present on this board, ordered by the axis vocabulary then alphabetically. */
  values: { value: string; count: number }[]
}

export function axisUsage(cards: CardView[]): AxisUsage[] {
  const out: AxisUsage[] = []
  for (const axis of CARD_AXES) {
    const counts = new Map<string, number>()
    for (const card of cards) {
      const v = axisValue(card, axis.prefix)
      if (!v) continue
      counts.set(v, (counts.get(v) ?? 0) + 1)
    }
    if (counts.size === 0) continue
    const values = [...counts.entries()]
      .map(([value, count]) => ({ value, count }))
      .sort((a, b) => {
        const ia = axis.values.indexOf(a.value)
        const ib = axis.values.indexOf(b.value)
        // Values outside the vocabulary sort after known ones rather than being
        // dropped, so a classifier writing an unexpected value is visible.
        if (ia !== ib) return (ia < 0 ? Number.MAX_SAFE_INTEGER : ia) - (ib < 0 ? Number.MAX_SAFE_INTEGER : ib)
        return a.value.localeCompare(b.value)
      })
    out.push({ axis, values })
  }
  return out
}

export function allCardsOf(columns: { cards: CardView[] | null }[]): CardView[] {
  // kanban-store serializes an empty column as `cards: null`, not `[]`.
  return columns.flatMap(c => c.cards ?? [])
}

/** An active filter: axis prefix → the set of values to keep. */
export type AxisFilter = Record<string, string[]>

export function matchesFilter(card: CardView, filter: AxisFilter): boolean {
  for (const [prefix, wanted] of Object.entries(filter)) {
    if (wanted.length === 0) continue
    if (!wanted.includes(axisValue(card, prefix))) return false
  }
  return true
}

export function filterIsActive(filter: AxisFilter): boolean {
  return Object.values(filter).some(v => v.length > 0)
}

export type SortKey = 'priority' | 'stored' | 'urgency' | 'title' | 'newest'

/**
 * Compares two cards as a triage queue: what is worth the most first, then
 * whichever of those is due soonest, then whichever moved most recently.
 *
 * Priority is descending because higher means more urgent. Due date is ascending
 * because sooner means more pressing — and a card with no deadline sorts after
 * every card that has one at the same priority, rather than ahead of them, which
 * is what sorting an empty string would do.
 */
function compareForTriage(a: CardView, b: CardView): number {
  const byPriority = priorityOf(b) - priorityOf(a)
  if (byPriority !== 0) return byPriority

  const dueA = dueAtOf(a)
  const dueB = dueAtOf(b)
  if (dueA !== dueB) {
    if (!dueA) return 1
    if (!dueB) return -1
    return dueA.localeCompare(dueB)
  }
  return updatedAtOf(b).localeCompare(updatedAtOf(a))
}

/**
 * Sorts a column's cards. 'stored' returns them untouched, preserving whatever
 * order the board view gave us — worth keeping as an option because every writer
 * on this host passes position 0, so the stored order is arbitrary and a user may
 * still want to see it as-is.
 */
export function sortCards(cards: CardView[], key: SortKey): CardView[] {
  if (key === 'stored') return cards
  const copy = [...cards]
  switch (key) {
    case 'priority':
      copy.sort(compareForTriage)
      break
    case 'urgency':
      copy.sort((a, b) => {
        const ra = URGENCY_RANK[axisValue(a, 'urgency:')] ?? 99
        const rb = URGENCY_RANK[axisValue(b, 'urgency:')] ?? 99
        if (ra !== rb) return ra - rb
        return titleOf(a).localeCompare(titleOf(b))
      })
      break
    case 'title':
      copy.sort((a, b) => titleOf(a).localeCompare(titleOf(b)))
      break
    case 'newest':
      copy.sort((a, b) => updatedAtOf(b).localeCompare(updatedAtOf(a)))
      break
  }
  return copy
}

function titleOf(card: CardView): string {
  return (card.item as NoteboardItem | null)?.title ?? ''
}

function updatedAtOf(card: CardView): string {
  return (card.item as NoteboardItem | null)?.updated_at ?? ''
}

/**
 * Higher is more urgent, and 0 means nothing is owed. Cards written before
 * email-classifier scored anything also read as 0, so they sink rather than
 * float — the safe direction for a card nobody has ranked yet.
 */
function priorityOf(card: CardView): number {
  return (card.item as NoteboardItem | null)?.priority ?? 0
}

function dueAtOf(card: CardView): string {
  return (card.item as NoteboardItem | null)?.due_at ?? ''
}

/**
 * Groups a column's cards by an axis, for the "group by" control. Returns a
 * single unnamed group when grouping is off, so callers render one code path.
 */
export function groupCards(cards: CardView[], prefix: string | null): { name: string; cards: CardView[] }[] {
  if (!prefix) return [{ name: '', cards }]
  const groups = new Map<string, CardView[]>()
  for (const card of cards) {
    const key = axisValue(card, prefix) || '(unset)'
    const bucket = groups.get(key)
    if (bucket) bucket.push(card)
    else groups.set(key, [card])
  }
  const axis = CARD_AXES.find(a => a.prefix === prefix)
  return [...groups.entries()]
    .map(([name, cards]) => ({ name, cards }))
    .sort((a, b) => {
      const order = axis?.values ?? []
      const ia = order.indexOf(a.name)
      const ib = order.indexOf(b.name)
      if (ia !== ib) return (ia < 0 ? Number.MAX_SAFE_INTEGER : ia) - (ib < 0 ? Number.MAX_SAFE_INTEGER : ib)
      return a.name.localeCompare(b.name)
    })
}

/**
 * Splits an email link's entity_ref back into the two parts email-classifier
 * joined: the mailstack account id and the provider message id.
 *
 * The account id is everything before the FIRST colon, because a Gmail message
 * id never contains one but an account id could not be recovered otherwise.
 */
export function parseEmailLocator(ref: string): { accountID: string; messageID: string } | null {
  const i = ref.indexOf(':')
  if (i <= 0 || i === ref.length - 1) return null
  return { accountID: ref.slice(0, i), messageID: ref.slice(i + 1) }
}
