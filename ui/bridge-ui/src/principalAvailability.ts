// The pure rules behind the Principals page's Availability section: what a
// stored week becomes on the form, what the form becomes on the wire, and how
// an absence typed in a browser becomes the absolute range principal-store
// stores. No fetch, no React, so the PATCH body is pinned by tests.
//
// The wire contract is principal-store's `PATCH /principals/{id}` (CONTRACT.md
// "Availability"): `availability` is an object to replace the week, `{}` to
// clear it, absent to leave it alone.
//
// ⚠️ Whether someone is available is NOT decided here. principal-store reduces
// the week and the absences to one answer with one reason
// (`GET /principals/{id}/availability?at=`), and this page asks it. A client
// copy of that reduction would drift from the store's, and would have to invent
// the one thing the store refuses to invent — a zone for someone who has
// declared none.

import type { Availability, Principal, TimeOff } from '@kayushkin/principal-store-types'

/** The working week as the form holds it. `tzid` is '' until a zone is chosen;
 *  it is never filled in from the browser, because a guessed zone makes someone
 *  look free at 3am. */
export interface WeekDraft {
  tzid: string
  days: string[]
  /** `HH:MM`, as an `<input type="time">` gives it. */
  start: string
  end: string
}

/** An empty week: no zone, no days, no hours. What the form shows for a human
 *  who has declared nothing — which the store reads as unknown, not as always. */
export const EMPTY_WEEK_DRAFT: WeekDraft = { tzid: '', days: [], start: '', end: '' }

export function weekDraftOf(principal: Pick<Principal, 'availability'>): WeekDraft {
  const week = principal.availability
  if (!week) return { ...EMPTY_WEEK_DRAFT }
  return { tzid: week.tzid, days: [...week.days], start: week.start, end: week.end }
}

/**
 * The `availability` object for a `PATCH`, fields trimmed and days left in the
 * order the form holds them (which is the order the store serves its codes in).
 *
 * Nothing is refused here beyond what cannot be sent at all: a week with no
 * zone, no days or no hours goes to the store as it is, and the store's refusal
 * — which names the field and the vocabulary — is what the page shows. Checking
 * it twice would mean wording it twice, and the second wording is the one that
 * goes stale.
 */
export function weekDraftToWire(draft: WeekDraft): Availability {
  return {
    tzid: draft.tzid.trim(),
    days: [...draft.days],
    start: draft.start.trim(),
    end: draft.end.trim(),
  }
}

/** The patch that removes a declared week. `{}` and not `null`: both clear it,
 *  and an empty object cannot be mistaken for "leave it alone" by a reader. */
export const CLEAR_WEEK_PATCH: { availability: Record<string, never> } = { availability: {} }

/** True when the form holds a week with nothing in it at all — the state a
 *  human with no declared hours starts in, where "Save" would only send four
 *  empty fields for the store to refuse. */
export function weekDraftIsEmpty(draft: WeekDraft): boolean {
  return !draft.tzid.trim() && draft.days.length === 0 && !draft.start.trim() && !draft.end.trim()
}

/** True when saving would send something other than what the store holds. */
export function weekDirty(principal: Pick<Principal, 'availability'>, draft: WeekDraft): boolean {
  return JSON.stringify(weekDraftToWire(weekDraftOf(principal))) !== JSON.stringify(weekDraftToWire(draft))
}

/**
 * A day added or removed, kept in `order` — the codes as `GET /weekday-codes`
 * serves them, which is week order. Sorting by the served order rather than by
 * click order means the form always reads Mon-to-Sun however it was filled in,
 * and a code the store adds later sorts itself.
 *
 * A code absent from `order` is a bug — the form only ever offers what the
 * store served — and throws rather than landing at the end of the week.
 */
export function toggleWeekday(days: readonly string[], code: string, on: boolean, order: readonly string[]): string[] {
  if (!order.includes(code)) {
    throw new RangeError(`weekday ${code} is not one of the codes principal-store serves (${order.join(', ')})`)
  }
  const next = on ? [...new Set([...days, code])] : days.filter(day => day !== code)
  return next.slice().sort((a, b) => order.indexOf(a) - order.indexOf(b))
}

/** A stored week in one line: "MO, TU, WE, TH, FR · 09:00–17:00 · Europe/Amsterdam".
 *  For a human with none, the sentence that says what that means — because
 *  "no hours" and "always available" look alike on a screen and are opposites. */
export function describeWeek(week: Availability | undefined): string {
  if (!week) return 'No declared week — unknown, and unknown is never available.'
  return `${week.days.join(', ')} · ${week.start}–${week.end} · ${week.tzid}`
}

// --- What the store answered -------------------------------------------------

/** Wording for each reason principal-store can give, in its own order of
 *  precedence. The store owns the vocabulary; this owns only the English. */
const REASON_WORDING: Record<string, string> = {
  disabled: 'disabled, whatever the hours say',
  no_schedule: 'no declared week — unknown is never available',
  time_off: 'on time off',
  off_hours: 'outside their working week',
  in_hours: 'inside their working week',
}

/** A reason in words. A reason the store serves but this page has no wording
 *  for is named as unknown rather than dressed up as one of the five — the same
 *  rule the kanban page follows for a default's source. */
export function availabilityReasonLabel(reason: string): string {
  return REASON_WORDING[reason] ?? `unknown reason "${reason}"`
}

/**
 * Reasons the store serves that this page has no wording for. Empty is the
 * expected answer; anything else means principal-store grew a reason and this
 * file did not follow, which is worth saying on screen rather than discovering
 * as a stray string in a tooltip.
 */
export function unwordedReasons(served: readonly string[]): string[] {
  return served.filter(reason => !(reason in REASON_WORDING))
}

// --- Absences ----------------------------------------------------------------

/** One absence as the form holds it: two `datetime-local` values, which are
 *  wall-clock in whatever zone the browser is in, and a note. */
export interface TimeOffDraft {
  /** `YYYY-MM-DDTHH:MM`, as `<input type="datetime-local">` gives it. */
  startsAt: string
  endsAt: string
  note: string
}

export const EMPTY_TIME_OFF_DRAFT: TimeOffDraft = { startsAt: '', endsAt: '', note: '' }

/** Unix seconds for a `datetime-local` value, read in the browser's own zone —
 *  which is what the control means — or null when it is blank or unparseable.
 *  An absence is an absolute range, so this is the one place a browser zone is
 *  the right zone: "away from the 14th to the 21st" is the same instants
 *  wherever it is typed. */
export function localInputToEpochSeconds(value: string): number | null {
  const trimmed = value.trim()
  if (!trimmed) return null
  const ms = new Date(trimmed).getTime()
  return Number.isNaN(ms) ? null : Math.floor(ms / 1000)
}

/** Unix seconds as a `datetime-local` value in the browser's zone. */
export function epochSecondsToLocalInput(seconds: number): string {
  const date = new Date(seconds * 1000)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
    + `T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export type TimeOffWireResult =
  | { ok: true; value: { starts_at: number; ends_at: number; note?: string } }
  | { ok: false; error: string }

/**
 * The `POST /time-off` body. Only what cannot be sent at all is refused here —
 * a blank or unreadable date has no number to put on the wire. Whether the
 * range runs backwards is the store's to say, and it says it well
 * ("starts_at … must be before ends_at …"), so it is sent and its answer shown.
 */
export function timeOffDraftToWire(draft: TimeOffDraft): TimeOffWireResult {
  const startsAt = localInputToEpochSeconds(draft.startsAt)
  if (startsAt === null) return { ok: false, error: 'Give a date and time for the first day away.' }
  const endsAt = localInputToEpochSeconds(draft.endsAt)
  if (endsAt === null) return { ok: false, error: 'Give a date and time for the return.' }
  // Spread rather than `note: note || undefined`: that leaves the key present
  // holding undefined, which survives every check but JSON.stringify.
  const note = draft.note.trim()
  return { ok: true, value: { starts_at: startsAt, ends_at: endsAt, ...(note ? { note } : {}) } }
}

/** An absence in one line, in the reader's own zone. The range is half-open —
 *  the return instant is already back at work — and the wording says so. */
export function describeTimeOff(timeOff: Pick<TimeOff, 'starts_at' | 'ends_at'>): string {
  const format = (seconds: number) => new Date(seconds * 1000).toLocaleString(undefined, {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
  return `${format(timeOff.starts_at)} until ${format(timeOff.ends_at)}`
}
