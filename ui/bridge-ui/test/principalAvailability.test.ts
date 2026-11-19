import { describe, expect, it } from 'vitest'
import {
  CLEAR_WEEK_PATCH, availabilityReasonLabel, describeTimeOff, describeWeek, epochSecondsToLocalInput,
  localInputToEpochSeconds, timeOffDraftToWire, toggleWeekday, unwordedReasons, weekDirty, weekDraftIsEmpty,
  weekDraftOf, weekDraftToWire,
} from '../src/principalAvailability'
import type { Availability } from '@kayushkin/principal-store-types'

// The codes as principal-store serves them, week order. The form only ever
// offers what the store served, so every test passes this rather than a literal.
const SERVED_CODES = ['MO', 'TU', 'WE', 'TH', 'FR', 'SA', 'SU']

const week: Availability = { tzid: 'Europe/Amsterdam', days: ['MO', 'TU', 'WE', 'TH', 'FR'], start: '09:00', end: '17:00' }

describe('a human’s declared week on the form', () => {
  it('reads a stored week onto the form', () => {
    expect(weekDraftOf({ availability: week })).toEqual({
      tzid: 'Europe/Amsterdam', days: ['MO', 'TU', 'WE', 'TH', 'FR'], start: '09:00', end: '17:00',
    })
  })

  it('shows a human with no week an empty form, and never a zone borrowed from the browser', () => {
    const draft = weekDraftOf({ availability: undefined })
    expect(draft).toEqual({ tzid: '', days: [], start: '', end: '' })
    expect(weekDraftIsEmpty(draft)).toBe(true)
  })

  it('does not share the stored arrays, so editing the form cannot mutate the row', () => {
    const draft = weekDraftOf({ availability: week })
    draft.days.push('SA')
    expect(week.days).toEqual(['MO', 'TU', 'WE', 'TH', 'FR'])
  })

  it('sends the week trimmed', () => {
    expect(weekDraftToWire({ tzid: '  UTC  ', days: ['MO'], start: ' 09:00 ', end: '17:00 ' }))
      .toEqual({ tzid: 'UTC', days: ['MO'], start: '09:00', end: '17:00' })
  })

  it('sends an incomplete week as it is, and leaves the refusal to the store', () => {
    // principal-store names the field and the vocabulary ("availability.days
    // must name at least one of MO, TU, …"). Wording it here too would mean
    // maintaining two wordings, and the copy is the one that goes stale.
    expect(weekDraftToWire({ tzid: '', days: [], start: '', end: '' }))
      .toEqual({ tzid: '', days: [], start: '', end: '' })
  })

  it('clears a week with an empty object, which cannot be misread as "leave it alone"', () => {
    expect(CLEAR_WEEK_PATCH).toEqual({ availability: {} })
  })

  it('is dirty only when saving would send something different', () => {
    expect(weekDirty({ availability: week }, weekDraftOf({ availability: week }))).toBe(false)
    expect(weekDirty({ availability: week }, { ...weekDraftOf({ availability: week }), end: '18:00' })).toBe(true)
    expect(weekDirty({ availability: undefined }, { tzid: '', days: [], start: '', end: '' })).toBe(false)
  })
})

describe('picking the days of the week', () => {
  it('keeps the days in the order the store serves them, however they were clicked', () => {
    let days = toggleWeekday([], 'FR', true, SERVED_CODES)
    days = toggleWeekday(days, 'MO', true, SERVED_CODES)
    days = toggleWeekday(days, 'WE', true, SERVED_CODES)
    expect(days).toEqual(['MO', 'WE', 'FR'])
  })

  it('removes a day, and adding one twice does not double it', () => {
    expect(toggleWeekday(['MO', 'TU'], 'MO', false, SERVED_CODES)).toEqual(['TU'])
    expect(toggleWeekday(['MO'], 'MO', true, SERVED_CODES)).toEqual(['MO'])
  })

  it('throws on a code the store did not serve, rather than sorting it to the end of the week', () => {
    expect(() => toggleWeekday(['MO'], 'FUNDAY', true, SERVED_CODES)).toThrow(/principal-store serves/)
  })
})

describe('saying what a week means', () => {
  it('reads a week in one line', () => {
    expect(describeWeek(week)).toBe('MO, TU, WE, TH, FR · 09:00–17:00 · Europe/Amsterdam')
  })

  it('says what no week means, because "none" and "always" look alike and are opposites', () => {
    expect(describeWeek(undefined)).toMatch(/never available/)
  })
})

describe('the reason the store gave', () => {
  it('words each reason principal-store serves', () => {
    for (const reason of ['in_hours', 'off_hours', 'time_off', 'no_schedule', 'disabled']) {
      expect(availabilityReasonLabel(reason)).not.toMatch(/unknown reason/)
    }
    expect(availabilityReasonLabel('no_schedule')).toMatch(/never available/)
  })

  it('names a reason it has no wording for, rather than passing it off as one of the five', () => {
    expect(availabilityReasonLabel('on_call')).toBe('unknown reason "on_call"')
  })

  it('reports which served reasons this page has not caught up with', () => {
    expect(unwordedReasons(['in_hours', 'off_hours', 'time_off', 'no_schedule', 'disabled'])).toEqual([])
    expect(unwordedReasons(['in_hours', 'on_call'])).toEqual(['on_call'])
  })
})

describe('an absence typed in a browser', () => {
  it('round-trips an instant through the local input', () => {
    const seconds = Math.floor(new Date(2026, 8, 14, 9, 30).getTime() / 1000)
    expect(localInputToEpochSeconds(epochSecondsToLocalInput(seconds))).toBe(seconds)
  })

  it('pads every field, so the input parses', () => {
    expect(epochSecondsToLocalInput(Math.floor(new Date(2026, 0, 2, 3, 4).getTime() / 1000)))
      .toBe('2026-01-02T03:04')
  })

  it('reads a blank or unparseable value as no instant at all', () => {
    expect(localInputToEpochSeconds('')).toBeNull()
    expect(localInputToEpochSeconds('   ')).toBeNull()
    expect(localInputToEpochSeconds('not a date')).toBeNull()
  })

  it('refuses only what cannot be sent — a missing date has no number for the wire', () => {
    expect(timeOffDraftToWire({ startsAt: '', endsAt: '2026-09-21T09:00', note: '' }))
      .toEqual({ ok: false, error: 'Give a date and time for the first day away.' })
    expect(timeOffDraftToWire({ startsAt: '2026-09-14T09:00', endsAt: '', note: '' }))
      .toEqual({ ok: false, error: 'Give a date and time for the return.' })
  })

  it('sends a backwards range for the store to refuse, rather than wording the refusal twice', () => {
    // principal-store answers "starts_at … must be before ends_at …" and that
    // is what the page shows.
    const wire = timeOffDraftToWire({ startsAt: '2026-09-21T09:00', endsAt: '2026-09-14T09:00', note: '' })
    expect(wire.ok).toBe(true)
    if (wire.ok) expect(wire.value.starts_at).toBeGreaterThan(wire.value.ends_at)
  })

  it('omits an empty note rather than sending a blank one', () => {
    const blank = timeOffDraftToWire({ startsAt: '2026-09-14T09:00', endsAt: '2026-09-21T09:00', note: '   ' })
    expect(blank.ok && 'note' in blank.value).toBe(false)
    const written = timeOffDraftToWire({ startsAt: '2026-09-14T09:00', endsAt: '2026-09-21T09:00', note: ' Lisbon ' })
    expect(written.ok && written.value.note).toBe('Lisbon')
  })

  it('reads an absence as a range that ends when they are back', () => {
    const starts = Math.floor(new Date(2026, 8, 14, 9, 0).getTime() / 1000)
    const ends = Math.floor(new Date(2026, 8, 21, 9, 0).getTime() / 1000)
    expect(describeTimeOff({ starts_at: starts, ends_at: ends })).toMatch(/ until /)
  })
})
