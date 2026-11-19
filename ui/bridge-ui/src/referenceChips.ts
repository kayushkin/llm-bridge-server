// The generic reference chip's rules: what a record of any type shows, read
// from its type's row in reference-store's list. A type with a chip of its own
// (a session, a person, a photo…) is drawn by that chip; every other type is
// drawn from these, so a store added to reference-store's list gets a chip with
// a name, a few facts and a page link with no change here.

import type { ReferenceFieldFormat, ReferenceType, ResolvedRefMatch } from '@kayushkin/chat-core'

/** A value read from a record: a string, number or boolean, or null when the
 *  path names nothing of those. `field` is a dotted path, `messages.0.body`. */
export function referenceFieldAt(data: unknown, field: string): string | number | boolean | null {
  let value: unknown = data
  for (const step of field.split('.')) {
    if (Array.isArray(value)) {
      const index = Number(step)
      value = Number.isInteger(index) ? value[index] : undefined
    } else if (value !== null && typeof value === 'object') {
      value = (value as Record<string, unknown>)[step]
    } else {
      return null
    }
  }
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') return value
  return null
}

const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })
const dateOnly = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' })

/** One value as its format says to show it, or null when there is nothing to
 *  show: an empty string, a zero time (the stores' "never"), or a value whose
 *  shape the format does not fit. */
export function referenceValueText(value: string | number | boolean | null, format: ReferenceFieldFormat): string | null {
  if (value === null || value === '') return null
  switch (format) {
    case 'text':
    case 'reference':
    case 'url':
      return String(value)
    case 'number':
      return typeof value === 'number' ? value.toLocaleString() : String(value)
    case 'boolean':
      return value === true ? 'yes' : value === false ? 'no' : null
    case 'unix_seconds':
    case 'unix_milliseconds': {
      if (typeof value !== 'number' || value === 0) return null
      return dateTime.format(new Date(format === 'unix_seconds' ? value * 1000 : value))
    }
    case 'iso_time': {
      const time = Date.parse(String(value))
      return Number.isNaN(time) ? null : dateTime.format(new Date(time))
    }
    case 'date': {
      // A calendar day (2026-11-03) is no moment, so it is shown in UTC, where
      // its midnight lands on the same day.
      const day = Date.parse(`${String(value)}T00:00:00Z`)
      return Number.isNaN(day) ? String(value) : dateOnly.format(new Date(day))
    }
    case 'cents':
      return typeof value === 'number' ? (value / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : null
    case 'probability':
      return typeof value === 'number' ? `${Math.round(value * 100)}%` : null
  }
}

/** A summary row the chip's panel draws. A `reference` row's text is an id,
 *  drawn as a chip of its own; a `url` row's is a link. */
export interface ReferenceSummaryRow {
  label: string
  text: string
  format: ReferenceFieldFormat
}

/** The type's summary fields, read from the record, leaving out any with
 *  nothing to show. */
export function referenceSummaryRows(type: ReferenceType | undefined, data: unknown): ReferenceSummaryRow[] {
  const rows: ReferenceSummaryRow[] = []
  for (const field of type?.summary ?? []) {
    const text = referenceValueText(referenceFieldAt(data, field.field), field.format)
    if (text === null) continue
    if (field.format === 'url' && !/^https?:\/\//i.test(text)) continue
    rows.push({ label: field.label, text, format: field.format })
  }
  return rows
}

/** What the chip says: the record's name, or its id when it has none (an
 *  outfit nobody named, a trip with no title). */
export function referenceChipLabel(match: ResolvedRefMatch, refId: string): string {
  return match.label?.trim() ? match.label.trim() : refId
}

/** Where the chip's Open link goes: the record's own page, else the generic
 *  record page, else nowhere. */
export function referencePageHref(match: ResolvedRefMatch, refId: string, referenceRoute: string): string {
  if (match.page_url) return match.page_url
  if (referenceRoute) return `${referenceRoute}/${encodeURIComponent(refId)}`
  return ''
}

/** Whether an href leaves this site, so the link opens in a new tab. */
export function isOffSiteHref(href: string): boolean {
  return /^https?:\/\//i.test(href)
}

/** The record's own top-level values, for the generic record page: every
 *  string, number and boolean, in the store's order, empty ones left out. */
export function referenceRecordFields(data: unknown): Array<{ field: string; text: string }> {
  if (data === null || typeof data !== 'object' || Array.isArray(data)) return []
  const fields: Array<{ field: string; text: string }> = []
  for (const [field, value] of Object.entries(data as Record<string, unknown>)) {
    if (typeof value === 'string' && value !== '') fields.push({ field, text: value })
    else if (typeof value === 'number' || typeof value === 'boolean') fields.push({ field, text: String(value) })
  }
  return fields
}

// A markdown link to `ref:<id>`, the journal's mention form, in text that is
// not read as markdown: a note a model wrote, a photo's description.
const REFERENCE_LINK = /\[[^\]\n]*\]\(ref:([^)\s]+)\)/g

/** `text` cut at each `[label](ref:<id>)`: the text between, and each link's
 *  id. The label is dropped, as `ReferenceMarkdown` drops it: the chip names
 *  the record. */
export function splitReferenceLinks(text: string): Array<string | { refId: string }> {
  const pieces: Array<string | { refId: string }> = []
  let last = 0
  for (const found of text.matchAll(REFERENCE_LINK)) {
    if (found.index > last) pieces.push(text.slice(last, found.index))
    pieces.push({ refId: found[1] ?? '' })
    last = found.index + found[0].length
  }
  if (last < text.length) pieces.push(text.slice(last))
  return pieces
}
