// The pure rules behind the board settings page and the board-default chips on
// a card: what a board's record becomes on the form, what a form becomes on the
// wire, and which instance a dispatch picks when the card has no say.
//
// Everything here is a function of its arguments — no fetch, no React — so the
// wire bodies the page sends are pinned by tests without a DOM. The wire
// contract is kanban-store's `PATCH /api/boards/{id}` and
// `PUT /api/boards/{id}/priority-levels` (README "Board settings", "Priority
// ladders", "Business hours"): omit a field to leave it alone, `""` clears an
// id, `{"classifier":{}}` clears the classifier, `{"business_hours":{}}`
// clears the hours, `{"assignment_pool":{}}` clears the pool.

import type { Instance } from '@kayushkin/llm-bridge-types'
import type { Machine } from './types'
import type { Bundle } from '@kayushkin/bundle-store-types'
import type { AssignmentPool, Board, BoardPriorityLevel, BusinessHours, CardEvent, ClassifierConfig, DefaultSource, EffectiveDefault, PriorityLadder, UpdateBoardRequest } from '@kayushkin/kanban-store-types'
import { machineLabel } from './grantResources'

/** A `PATCH /api/boards/{id}` body. Every key is optional because an omitted
 *  key is "leave it alone"; an empty object clears the three object-valued
 *  settings and an empty string clears an id. */
export type BoardSettingsPatch = Omit<UpdateBoardRequest, 'business_hours' | 'classifier' | 'assignment_pool'> & {
  business_hours?: BusinessHours | Record<string, never>
  classifier?: ClassifierConfig | Record<string, never>
  assignment_pool?: AssignmentPool | Record<string, never>
}

export type BoardDefaultField = 'default_principal_id' | 'default_agent_id' | 'default_instance_id' | 'default_bundle_id'

export const BOARD_DEFAULT_FIELDS: readonly BoardDefaultField[] = [
  'default_principal_id', 'default_agent_id', 'default_instance_id', 'default_bundle_id',
]

// --- General -----------------------------------------------------------------

export interface GeneralDraft {
  name: string
  description: string
  archived: boolean
}

export function generalDraftOf(board: Board): GeneralDraft {
  return { name: board.name, description: board.description, archived: board.archived }
}

/** Only what differs from the stored board, so a save never re-sends a field
 *  the user did not touch — the store re-checks nothing it is not sent. */
export function generalPatchOf(board: Board, draft: GeneralDraft): BoardSettingsPatch {
  const patch: BoardSettingsPatch = {}
  if (draft.name !== board.name) patch.name = draft.name
  if (draft.description !== board.description) patch.description = draft.description
  if (draft.archived !== board.archived) patch.archived = draft.archived
  return patch
}

// --- Business hours ----------------------------------------------------------

/** RFC 5545 weekday codes, in the order the checkboxes are drawn. */
export const WEEKDAY_CODES = ['MO', 'TU', 'WE', 'TH', 'FR', 'SA', 'SU'] as const

export interface BusinessHoursDraft {
  tzid: string
  days: string[]
  start: string
  end: string
}

export function businessHoursDraftOf(board: Board): BusinessHoursDraft {
  const hours = board.business_hours
  return hours
    ? { tzid: hours.tzid, days: [...hours.days], start: hours.start, end: hours.end }
    : { tzid: '', days: [], start: '', end: '' }
}

/** The whole object, every time: the store replaces the working week outright.
 *  Days are sent in weekday order whatever order they were ticked in. */
export function businessHoursPatchOf(draft: BusinessHoursDraft): BoardSettingsPatch {
  const days = WEEKDAY_CODES.filter(code => draft.days.includes(code))
  return { business_hours: { tzid: draft.tzid.trim(), days, start: draft.start.trim(), end: draft.end.trim() } }
}

export const CLEAR_BUSINESS_HOURS_PATCH: BoardSettingsPatch = { business_hours: {} }

// --- Defaults ----------------------------------------------------------------

/** The four id-valued defaults as the form holds them: the stored id, or ''
 *  for unset — which is also what "clear" puts here, because '' is the wire's
 *  own word for clearing. */
export type DefaultsDraft = Record<BoardDefaultField, string>

export function defaultsDraftOf(board: Pick<Board, BoardDefaultField>): DefaultsDraft {
  return {
    default_principal_id: board.default_principal_id ?? '',
    default_agent_id: board.default_agent_id ?? '',
    default_instance_id: board.default_instance_id ?? '',
    default_bundle_id: board.default_bundle_id ?? '',
  }
}

/** Only the defaults that differ from the stored board. A field cleared on the
 *  form goes out as '' (the store's clear), one left alone is not sent at all —
 *  the store re-checks every id it is sent with that id's owner, and an
 *  untouched id should not be refused because its owner is down today. */
export function defaultsPatchOf(board: Pick<Board, BoardDefaultField>, draft: DefaultsDraft): BoardSettingsPatch {
  const stored = defaultsDraftOf(board)
  const patch: BoardSettingsPatch = {}
  for (const field of BOARD_DEFAULT_FIELDS) {
    const value = draft[field].trim()
    if (value !== stored[field]) patch[field] = value
  }
  return patch
}

// --- Assignment pool ---------------------------------------------------------
//
// A card that arrives with no assignee goes to one available member of a
// principal-store group, chosen by a strategy kanban-store serves at
// `GET /api/assignment-strategies`. kanban-store decides who; these helpers only
// carry the form to the wire and word what the store recorded.

export interface AssignmentPoolDraft {
  /** principal-store group id; '' means no pool. */
  principalID: string
  /** One of the served strategies; '' until one is chosen. */
  strategy: string
}

export function assignmentPoolDraftOf(board: Board): AssignmentPoolDraft {
  const pool = board.assignment_pool
  return pool ? { principalID: pool.principal_id, strategy: pool.strategy } : { principalID: '', strategy: '' }
}

export const CLEAR_ASSIGNMENT_POOL_PATCH: BoardSettingsPatch = { assignment_pool: {} }

/** The whole pool, as the store replaces it outright; no group clears it. A
 *  group with no strategy is sent as it stands, so the store's refusal — which
 *  names the strategies it runs — is what the page shows. */
export function assignmentPoolPatchOf(draft: AssignmentPoolDraft): BoardSettingsPatch {
  const principalID = draft.principalID.trim()
  if (!principalID) return CLEAR_ASSIGNMENT_POOL_PATCH
  return { assignment_pool: { principal_id: principalID, strategy: draft.strategy } }
}

/** A strategy in words for the select. A strategy the store serves that this
 *  list does not know is shown as its own name, not hidden. */
const ASSIGNMENT_STRATEGY_LABELS: Record<string, string> = {
  least_open_cards: 'least open cards — the member with the fewest unfinished cards here',
  round_robin: 'round robin — the member whose last assignment here is oldest',
}

export function assignmentStrategyLabel(strategy: string): string {
  return ASSIGNMENT_STRATEGY_LABELS[strategy] ?? strategy
}

/** The parts of an `assigned` or `assignment_skipped` event's detail that
 *  kanban-store writes when it picks an assignee on arrival. Every field is
 *  optional: an assignment made by hand carries no detail, and detail is
 *  free-form JSON, so each field is checked rather than trusted. */
export interface ArrivalAssignmentDetail {
  source?: string
  strategy?: string
  candidates?: number
  poolPrincipalID?: string
  poolEmpty?: boolean
  ruleTags?: string[]
  ruleID?: string
  reason?: string
}

export function arrivalAssignmentDetailOf(event: Pick<CardEvent, 'detail'>): ArrivalAssignmentDetail | null {
  const detail = event.detail
  if (!detail || typeof detail !== 'object' || Array.isArray(detail)) return null
  const fields = detail as Record<string, unknown>
  const text = (key: string) => (typeof fields[key] === 'string' && fields[key] !== '' ? fields[key] as string : undefined)
  const parsed: ArrivalAssignmentDetail = {
    source: text('source'),
    strategy: text('strategy'),
    candidates: typeof fields.candidates === 'number' ? fields.candidates : undefined,
    poolPrincipalID: text('pool_principal_id'),
    poolEmpty: fields.pool_empty === true ? true : undefined,
    ruleTags: Array.isArray(fields.rule_tags) && fields.rule_tags.every(tag => typeof tag === 'string') ? fields.rule_tags as string[] : undefined,
    ruleID: text('rule_id'),
    reason: text('reason'),
  }
  if (!parsed.source && !parsed.reason) return null
  return parsed
}

/**
 * Why a card arriving on the board got the assignee it did, or none, in one
 * line: "from the pool principal_000030 by round robin, 3 available",
 * "board default — nobody in the pool principal_000030 was available". Null
 * for an event that says nothing about arrival (an assignment by hand). A
 * source or reason kanban-store adds later is named as it was recorded.
 * `principalName` turns an id into what principal-store calls it, for display.
 */
export function describeArrivalAssignment(
  kind: string, detail: ArrivalAssignmentDetail | null, principalName: (id: string) => string = id => id,
): string | null {
  if (!detail) return null
  const pool = detail.poolPrincipalID ? `the pool ${principalName(detail.poolPrincipalID)}` : 'the pool'
  if (kind === 'assignment_skipped') {
    if (detail.reason === 'pool_empty') return `nobody in ${pool} was available and the board has no default principal, so the card is unassigned`
    return `left unassigned: ${detail.reason ?? 'kanban-store gave no reason'}`
  }
  switch (detail.source) {
    case 'pool': {
      const how = detail.strategy ? ` by ${detail.strategy.replace(/_/g, ' ')}` : ''
      const count = detail.candidates !== undefined ? `, ${detail.candidates} available` : ''
      return `from ${pool}${how}${count}`
    }
    case 'board_default':
      return detail.poolEmpty ? `board default — nobody in ${pool} was available` : 'board default'
    case 'tag_rule':
      return detail.ruleTags && detail.ruleTags.length > 0
        ? `from tag rule ${detail.ruleTags.join(' + ')}`
        : `from tag rule ${detail.ruleID ?? '(the store named no rule)'}`
    case undefined:
      return null
    default:
      return `from unknown source "${detail.source}"`
  }
}

// --- Priority ladder ---------------------------------------------------------

export type BudgetUnit = 'hours' | 'days'

export const SECONDS_PER_HOUR = 3600
export const SECONDS_PER_DAY = 86400

export interface LadderRowDraft {
  label: string
  /** Kept as text while editing so a half-typed number is not coerced to 0 —
   *  0 is the one value the store refuses outright. */
  priorityValue: string
  /** Empty means a rung that is named but not timed. */
  budgetAmount: string
  budgetUnit: BudgetUnit
  /** Dollars a card starts with as its auto-hold ceiling at this rung. Empty
   *  means no default; "0" is a real default. */
  defaultAutoHoldAtUSD: string
}

/** A stored budget as a human amount: whole days when it is a whole number of
 *  days, hours otherwise (fractional hours allowed — 90 minutes is 1.5 h). */
export function budgetDraftOfSeconds(seconds: number | null): { budgetAmount: string; budgetUnit: BudgetUnit } {
  if (seconds === null) return { budgetAmount: '', budgetUnit: 'hours' }
  if (seconds >= SECONDS_PER_DAY && seconds % SECONDS_PER_DAY === 0) {
    return { budgetAmount: String(seconds / SECONDS_PER_DAY), budgetUnit: 'days' }
  }
  return { budgetAmount: String(seconds / SECONDS_PER_HOUR), budgetUnit: 'hours' }
}

export function ladderRowDraftOf(level: BoardPriorityLevel): LadderRowDraft {
  return {
    label: level.label,
    priorityValue: String(level.priority_value),
    ...budgetDraftOfSeconds(level.budget_seconds),
    defaultAutoHoldAtUSD: level.default_auto_hold_at_usd === null || level.default_auto_hold_at_usd === undefined ? '' : String(level.default_auto_hold_at_usd),
  }
}

export function ladderDraftOf(ladder: PriorityLadder | null): LadderRowDraft[] {
  return (ladder?.levels ?? []).map(ladderRowDraftOf)
}

/** A fresh rung one below the lowest on the form, or 1 for an empty ladder. */
export function emptyLadderRow(rows: readonly LadderRowDraft[]): LadderRowDraft {
  const values = rows.map(row => Number(row.priorityValue)).filter(value => Number.isInteger(value) && value > 0)
  const lowest = values.length > 0 ? Math.min(...values) : 2
  return { label: '', priorityValue: String(Math.max(1, lowest - 1)), budgetAmount: '', budgetUnit: 'hours', defaultAutoHoldAtUSD: '' }
}

/** One rung as `PUT /api/boards/{id}/priority-ladder` takes it: kanban-store's
 *  BoardPriorityLevel without `board_id`, which the route supplies. */
export type LadderWireLevel = Omit<BoardPriorityLevel, 'board_id'>

export type LadderDraftResult = { ok: true; value: { levels: LadderWireLevel[] } } | { ok: false; error: string }

/** The `PUT /priority-levels` body. Budgets are converted to seconds here and
 *  nowhere else. Only what cannot be sent at all is refused on the client — a
 *  number that is not one — and the store's own refusals (a zero rung, a
 *  duplicate value or label) are left to the store, whose wording is the point. */
export function ladderDraftToWire(rows: readonly LadderRowDraft[]): LadderDraftResult {
  const levels: LadderWireLevel[] = []
  for (const [index, row] of rows.entries()) {
    const nth = `row ${index + 1}${row.label.trim() ? ` ("${row.label.trim()}")` : ''}`
    const priorityValue = Number(row.priorityValue.trim())
    if (row.priorityValue.trim() === '' || !Number.isInteger(priorityValue)) {
      return { ok: false, error: `${nth}: priority_value must be a whole number, got "${row.priorityValue}"` }
    }
    let budgetSeconds: number | null = null
    if (row.budgetAmount.trim() !== '') {
      const amount = Number(row.budgetAmount.trim())
      if (!Number.isFinite(amount)) {
        return { ok: false, error: `${nth}: budget must be a number of ${row.budgetUnit}, got "${row.budgetAmount}"` }
      }
      budgetSeconds = Math.round(amount * (row.budgetUnit === 'days' ? SECONDS_PER_DAY : SECONDS_PER_HOUR))
    }
    let defaultAutoHoldAtUSD: number | null = null
    if (row.defaultAutoHoldAtUSD.trim() !== '') {
      const amount = Number(row.defaultAutoHoldAtUSD.trim())
      if (!Number.isFinite(amount)) {
        return { ok: false, error: `${nth}: default cost must be a number of dollars, got "${row.defaultAutoHoldAtUSD}"` }
      }
      defaultAutoHoldAtUSD = amount
    }
    levels.push({ priority_value: priorityValue, label: row.label.trim(), budget_seconds: budgetSeconds, default_auto_hold_at_usd: defaultAutoHoldAtUSD })
  }
  return { ok: true, value: { levels } }
}

// --- Classifier --------------------------------------------------------------

export interface ClassifierDraft {
  /** Off means the save clears the classifier (`{"classifier":{}}`). */
  enabled: boolean
  vocabulary: string
  mailAccountIDs: string[]
  holdNewCards: boolean
  /** principal-store group id the classifier's model calls run for; empty when unset. */
  organizationID: string
}

export function classifierDraftOf(board: Board): ClassifierDraft {
  const classifier = board.classifier
  return classifier
    ? {
        enabled: true, vocabulary: classifier.vocabulary, mailAccountIDs: [...classifier.mail_account_ids],
        holdNewCards: classifier.hold_new_cards, organizationID: classifier.organization_id ?? '',
      }
    : { enabled: false, vocabulary: '', mailAccountIDs: [], holdNewCards: false, organizationID: '' }
}

/** A free-text account list: comma or whitespace separated, trimmed, deduped. */
export function parseMailAccountIDs(text: string): string[] {
  const seen = new Set<string>()
  for (const part of text.split(/[\s,]+/)) {
    const id = part.trim()
    if (id) seen.add(id)
  }
  return [...seen]
}

export const CLEAR_CLASSIFIER_PATCH: BoardSettingsPatch = { classifier: {} }

export function classifierPatchOf(draft: ClassifierDraft): BoardSettingsPatch {
  if (!draft.enabled) return CLEAR_CLASSIFIER_PATCH
  return {
    classifier: {
      vocabulary: draft.vocabulary.trim(),
      mail_account_ids: draft.mailAccountIDs.map(id => id.trim()).filter(Boolean),
      hold_new_cards: draft.holdNewCards,
      // Sent only when set: the classifier object is replaced whole on save,
      // and an empty id would be refused rather than read as "none".
      ...(draft.organizationID.trim() ? { organization_id: draft.organizationID.trim() } : {}),
    },
  }
}

// --- Effective defaults on a card --------------------------------------------
//
// A card's defaults are not the board's: kanban-store resolves the board's tag
// rules against the card's tags (`GET …/cards/{id}/effective-defaults`) and
// says where each value came from. These helpers only word and apply what the
// store answered; none of them decides which rule wins.

/** How a bundle is named on a card or in a picker: the store's `name`, with its
 *  `display_name` beside it when it has one — the Bundles page's own wording. */
export function bundleLabel(bundle: Pick<Bundle, 'name' | 'display_name'>): string {
  return bundle.display_name ? `${bundle.name} — ${bundle.display_name}` : bundle.name
}

/** A bundle by bundle-store id, or the raw id when the list lacks it — never
 *  nothing: the stored id is true even when the name is missing. */
export function bundleLabelForID(bundleID: string, bundles: readonly Bundle[] | null): string {
  const bundle = bundles?.find(candidate => String(candidate.id) === bundleID)
  return bundle ? bundleLabel(bundle) : bundleID
}

/** What the board's own bundle does, for the settings page: a dispatch sends
 *  it as `bundle_id` and the spawn provisions its tools. */
export const BOARD_BUNDLE_HELP_TEXT =
  'Sessions dispatched from this board are provisioned with this bundle’s tools.'

/** What a card's effective bundle does, for the card: the same, from wherever
 *  the store resolved it (a tag rule or the board). */
export const CARD_BUNDLE_HELP_TEXT =
  'A session dispatched from this card is provisioned with this bundle’s tools.'

/**
 * Where a resolved default came from, in words: "from tag rule cat:product +
 * urgency:high" or "board default". A source kind kanban-store adds later is
 * named as unknown rather than passed off as one of these.
 */
export function defaultSourceLabel(source: DefaultSource): string {
  if (source.kind === 'board') return 'board default'
  if (source.kind === 'tag_rule') {
    const tags = source.rule_tags ?? []
    return tags.length > 0 ? `from tag rule ${tags.join(' + ')}` : `from tag rule ${source.rule_id ?? '(the store named no rule)'}`
  }
  return `from unknown source kind "${source.kind}"`
}

export type DispatchTarget =
  | { kind: 'last-session'; instance: Instance }
  | { kind: 'default-instance'; instance: Instance; source: DefaultSource; lastSessionNote: string | null }
  | { kind: 'none'; why: string | null }

export interface DispatchTargetArgs {
  /** Where the card's last session ran, or null for a card that has had none. */
  lastSessionInstanceID: string | null
  /** The card's effective `default_instance_id` exactly as kanban-store
   *  resolved it, source included, or null when nothing sets one (or the card
   *  is on no board). */
  defaultInstance: EffectiveDefault | null
  instances: readonly Instance[]
}

function unusableInstanceNote(instanceID: string, instances: readonly Instance[]): string | null {
  const instance = instances.find(candidate => candidate.id === instanceID)
  if (!instance) return `instance ${instanceID}, which the bridge does not list`
  if (!instance.enabled) return `${instance.name}, which is disabled`
  return null
}

/** "default instance (from tag rule cat:product)" / "default instance (board default)". */
export function defaultInstanceLabel(source: DefaultSource): string {
  return `default instance (${defaultSourceLabel(source)})`
}

/**
 * Where a dispatch runs when nobody chooses: the card's last session's
 * instance, else the card's effective default instance, else nothing. Only an
 * instance the bridge lists AND has enabled counts at either step — the server
 * answers 503 for a disabled one, so choosing it would only defer the refusal.
 *
 * `why` on `none` names what was tried and found unusable, or is null when
 * there was nothing to try; the caller opens the picker either way.
 */
export function dispatchTargetForCard({ lastSessionInstanceID, defaultInstance, instances }: DispatchTargetArgs): DispatchTarget {
  let lastSessionNote: string | null = null
  if (lastSessionInstanceID) {
    const last = instances.find(candidate => candidate.id === lastSessionInstanceID)
    if (last?.enabled) return { kind: 'last-session', instance: last }
    lastSessionNote = `this card’s last session ran on ${unusableInstanceNote(lastSessionInstanceID, instances)}`
  }
  if (defaultInstance) {
    const preset = instances.find(candidate => candidate.id === defaultInstance.value)
    if (preset?.enabled) return { kind: 'default-instance', instance: preset, source: defaultInstance.source, lastSessionNote }
    const defaultNote = `the ${defaultInstanceLabel(defaultInstance.source)} is ${unusableInstanceNote(defaultInstance.value, instances)}`
    return { kind: 'none', why: lastSessionNote ? `${lastSessionNote}; ${defaultNote}` : defaultNote }
  }
  return { kind: 'none', why: lastSessionNote }
}

/**
 * Why a dispatch target was chosen, in the words both the drawer and the
 * tile's confirmation use: "where this card’s last session ran", or "the
 * default instance (from tag rule …)" with the reason the last session's
 * instance was passed over, or the `none` explanation. Null for a `none` with
 * nothing to explain.
 */
export function dispatchTargetNote(target: DispatchTarget): string | null {
  if (target.kind === 'last-session') return 'where this card’s last session ran'
  if (target.kind === 'default-instance') {
    return `the ${defaultInstanceLabel(target.source)}${target.lastSessionNote ? ` — ${target.lastSessionNote}` : ''}`
  }
  return target.why
}

/** One line naming where an instance runs, for a confirmation or a chip. */
export function describeInstanceLocation(instance: Instance, machines: readonly Machine[]): string {
  const machine = machines.find(candidate => candidate.id === instance.machine_id)
  return `${instance.name} (${instance.harness_type}) in ${machine ? machineLabel(machine) : instance.machine_id}`
}
