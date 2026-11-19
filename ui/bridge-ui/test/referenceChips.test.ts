import { describe, expect, it } from 'vitest'
import type { ReferenceType } from '@kayushkin/chat-core'
import {
  isOffSiteHref,
  referenceChipLabel,
  referenceFieldAt,
  referencePageHref,
  referenceRecordFields,
  referenceSummaryRows,
  referenceValueText,
  splitReferenceLinks,
} from '../src/referenceChips'

const prediction: ReferenceType = {
  type: 'prediction', service: 'prediction-store', description: 'A prediction.', access: 'grant',
  label_field: 'claim',
  summary: [
    { field: 'probability', label: 'Odds', format: 'probability' },
    { field: 'status', label: 'Status', format: 'text' },
    { field: 'due_at', label: 'Due', format: 'unix_seconds' },
    { field: 'source', label: 'Source', format: 'url' },
  ],
}

describe('referenceFieldAt', () => {
  it('reads a dotted path through objects and arrays', () => {
    const data = { messages: [{ room_id: '!a:b', count: 3 }], open: false }
    expect(referenceFieldAt(data, 'messages.0.room_id')).toBe('!a:b')
    expect(referenceFieldAt(data, 'messages.0.count')).toBe(3)
    expect(referenceFieldAt(data, 'open')).toBe(false)
    expect(referenceFieldAt(data, 'messages.1.room_id')).toBeNull()
    expect(referenceFieldAt(data, 'messages')).toBeNull()
  })
})

describe('referenceValueText', () => {
  it('shows each format, and nothing for an empty value or a zero time', () => {
    expect(referenceValueText(0.9, 'probability')).toBe('90%')
    expect(referenceValueText(168850, 'cents')).toMatch(/1,688\.50/)
    expect(referenceValueText(0, 'unix_seconds')).toBeNull()
    expect(referenceValueText('', 'text')).toBeNull()
    expect(referenceValueText(true, 'boolean')).toBe('yes')
    expect(referenceValueText('2026-11-03', 'date')).toMatch(/2026/)
    expect(referenceValueText('not a time', 'iso_time')).toBeNull()
  })
})

describe('referenceSummaryRows', () => {
  it('reads the type\'s summary from the record and leaves out what is empty or unsafe', () => {
    const rows = referenceSummaryRows(prediction, { probability: 0.85, status: 'open', due_at: 0, source: 'javascript:alert(1)' })
    expect(rows).toEqual([
      { label: 'Odds', text: '85%', format: 'probability' },
      { label: 'Status', text: 'open', format: 'text' },
    ])
  })
  it('shows nothing for a type it does not know', () => {
    expect(referenceSummaryRows(undefined, { status: 'open' })).toEqual([])
  })
})

describe('the chip label and its link', () => {
  it('names the record by its label, or by its id when it has none', () => {
    expect(referenceChipLabel({ type: 'prediction', service: 'p', label: ' it rains ' }, 'prediction_000001')).toBe('it rains')
    expect(referenceChipLabel({ type: 'outfit', service: 'p', label: '' }, 'outfit_000001')).toBe('outfit_000001')
  })
  it('links to the record\'s own page, else the generic record page', () => {
    expect(referencePageHref({ type: 'p', service: 's', page_url: '/predictions?id=prediction_000001' }, 'prediction_000001', '/ref')).toBe('/predictions?id=prediction_000001')
    expect(referencePageHref({ type: 'p', service: 's' }, 'repo:dash', '/ref')).toBe('/ref/repo%3Adash')
    expect(referencePageHref({ type: 'p', service: 's' }, 'x', '')).toBe('')
    expect(isOffSiteHref('https://example.com/music/records/x')).toBe(true)
    expect(isOffSiteHref('/predictions?id=x')).toBe(false)
  })
})

describe('referenceRecordFields', () => {
  it('lists top-level values in order, leaving out empty and nested ones', () => {
    expect(referenceRecordFields({ id: 'x', name: '', count: 2, nested: { a: 1 }, ok: true })).toEqual([
      { field: 'id', text: 'x' },
      { field: 'count', text: '2' },
      { field: 'ok', text: 'true' },
    ])
  })
})

describe('splitReferenceLinks', () => {
  it('cuts text at each [label](ref:<id>) and keeps the id', () => {
    expect(splitReferenceLinks('in [a face group](ref:face_group_000054), twice')).toEqual([
      'in ',
      { refId: 'face_group_000054' },
      ', twice',
    ])
  })

  it('leaves other links and text alone', () => {
    expect(splitReferenceLinks('[site](https://example.com) and photo_000001')).toEqual([
      '[site](https://example.com) and photo_000001',
    ])
    expect(splitReferenceLinks('')).toEqual([])
  })
})
