import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  CLEAR_WEEK_PATCH, EMPTY_TIME_OFF_DRAFT, availabilityReasonLabel, describeTimeOff, describeWeek,
  timeOffDraftToWire, toggleWeekday, unwordedReasons, weekDirty, weekDraftIsEmpty, weekDraftOf, weekDraftToWire,
  type TimeOffDraft, type WeekDraft,
} from '../principalAvailability'
import {
  addTimeOff, getAvailability, listAvailabilityReasons, listTimeOff, listWeekdayCodes, removeTimeOff,
  type PatchPrincipalRequest, type PrincipalStoreResult,
} from '../principalStoreClient'
import type { AvailabilityAnswer, Principal, Principal as PrincipalDetail, TimeOff } from '@kayushkin/principal-store-types'
import type { FetchFn } from '../types'

/**
 * A human's declared working week and the absences beside it — what
 * principal-store has carried since 2026-09-11 and nothing could set.
 *
 * Three parts, each saving on its own: the week, the absences, and the store's
 * own answer for an instant you choose. That third part is the point of the
 * other two: the week and the time off are inputs, and what a dispatcher will
 * actually see is the reduced `{available, reason}` — so the page shows that
 * rather than leaving the reader to work it out from the form above.
 *
 * ⚠️ Groups never reach here. A group has no hours of its own; principal-store
 * answers 400 and points at `/members?available_at=`, and the caller renders
 * this only for a human.
 *
 * ⚠️ No zone is ever filled in for the reader. The store refuses to invent one
 * and so does this: an empty week means unknown, unknown is never available,
 * and a zone guessed from the browser would make an Amsterdam colleague look
 * free at 3am.
 */
export function PrincipalAvailabilitySection({ detail, fetchFn, base, save, onSaved }: {
  detail: PrincipalDetail
  fetchFn: FetchFn
  base: string
  save: (patch: PatchPrincipalRequest) => Promise<PrincipalStoreResult<Principal>>
  onSaved: () => Promise<void> | void
}) {
  return (
    <section className="bp-section" data-section="availability">
      <h4 className="bp-section-title">Availability</h4>
      <p className="bp-hint">
        The week this person declares they work, in their own zone, and the days they are away. kanban-store reads it
        to hand a card to someone who is actually working. There is no measured presence — no heartbeat, no last-seen —
        so what is declared here is the whole signal.
      </p>
      <WeekForm key={`week:${detail.id}:${detail.updated_at}`} detail={detail} fetchFn={fetchFn} base={base} save={save} onSaved={onSaved} />
      <TimeOffList key={`time-off:${detail.id}`} principalID={detail.id} fetchFn={fetchFn} base={base} />
      <AvailabilityProbe key={`probe:${detail.id}:${detail.updated_at}`} principalID={detail.id} fetchFn={fetchFn} base={base} />
    </section>
  )
}

// --- The week ----------------------------------------------------------------

/** The zones this browser knows, for the picker. Only a suggestion list: the
 *  store validates against ITS host's zone database, which is the one that
 *  counts, and its refusal names the zone it could not load. */
function supportedTimeZones(): string[] {
  const supported = (Intl as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf
  if (typeof supported !== 'function') return []
  try {
    return supported('timeZone')
  } catch {
    return []
  }
}

function WeekForm({ detail, fetchFn, base, save, onSaved }: {
  detail: PrincipalDetail
  fetchFn: FetchFn
  base: string
  save: (patch: PatchPrincipalRequest) => Promise<PrincipalStoreResult<Principal>>
  onSaved: () => Promise<void> | void
}) {
  const [draft, setDraft] = useState<WeekDraft>(() => weekDraftOf(detail))
  const [codes, setCodes] = useState<string[] | null>(null)
  const [codesError, setCodesError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // The day codes come from the store, never from a literal here: it owns the
  // vocabulary, and a form offering a code it does not accept would only
  // produce a 400 the reader cannot act on.
  useEffect(() => {
    let canceled = false
    void listWeekdayCodes(fetchFn, base).then(result => {
      if (canceled) return
      if (result.ok) { setCodes(result.value); setCodesError(null) } else setCodesError(result.error)
    })
    return () => { canceled = true }
  }, [fetchFn, base])

  const zones = useMemo(supportedTimeZones, [])
  const browserZone = useMemo(() => {
    try {
      return Intl.DateTimeFormat().resolvedOptions().timeZone || ''
    } catch {
      return ''
    }
  }, [])

  const stored = detail.availability
  const dirty = weekDirty(detail, draft)
  const empty = weekDraftIsEmpty(draft)

  const run = async (patch: PatchPrincipalRequest) => {
    setBusy(true)
    setError(null)
    const result = await save(patch)
    if (result.ok) await onSaved()
    else setError(result.error)
    setBusy(false)
  }

  return (
    <div className="bp-availability-week">
      <p className={`bp-week-current${stored ? '' : ' bp-week-none'}`}>{describeWeek(stored)}</p>

      <div className="bp-field">
        <label className="bp-field-label" htmlFor={`bp-tzid-${detail.id}`}>Time zone</label>
        {zones.length > 0 ? (
          <select
            id={`bp-tzid-${detail.id}`}
            className="bp-input"
            value={draft.tzid}
            onChange={e => setDraft({ ...draft, tzid: e.target.value })}
          >
            {/* Starts on nothing on purpose. The reader picks the person's zone;
                this browser's is offered as a labeled choice, never as the
                value already in the box. */}
            <option value="">— choose this person’s zone —</option>
            {browserZone && <option value={browserZone}>{browserZone} (this browser’s zone)</option>}
            {zones.filter(zone => zone !== browserZone).map(zone => <option key={zone} value={zone}>{zone}</option>)}
            {draft.tzid && !zones.includes(draft.tzid) && (
              <option value={draft.tzid}>{draft.tzid} (stored; this browser does not know it)</option>
            )}
          </select>
        ) : (
          <input
            id={`bp-tzid-${detail.id}`}
            className="bp-input"
            value={draft.tzid}
            placeholder="Europe/Amsterdam"
            onChange={e => setDraft({ ...draft, tzid: e.target.value })}
          />
        )}
        <p className="bp-hint">
          An IANA zone. Hours are read here, so a Los Angeles board can have an Amsterdam member and each is read where
          they are. principal-store checks the zone against its own host and names it if it cannot load it.
        </p>
      </div>

      <div className="bp-field">
        <span className="bp-field-label">Days</span>
        <div className="bp-weekdays">
          {codes === null
            ? <span className="bp-hint">{codesError ? '' : 'loading the day codes…'}</span>
            : codes.map(code => (
              <label key={code} className={`bp-weekday${draft.days.includes(code) ? ' bp-weekday-on' : ''}`}>
                <input
                  type="checkbox"
                  checked={draft.days.includes(code)}
                  onChange={e => setDraft({ ...draft, days: toggleWeekday(draft.days, code, e.target.checked, codes) })}
                />
                {code}
              </label>
            ))}
        </div>
        {codesError && <div className="bridge-error bp-error">Could not read the day codes: {codesError}</div>}
      </div>

      <div className="bp-field bp-hours-row">
        <div>
          <label className="bp-field-label" htmlFor={`bp-start-${detail.id}`}>Starts</label>
          <input
            id={`bp-start-${detail.id}`}
            type="time"
            className="bp-input bp-time-input"
            value={draft.start}
            onChange={e => setDraft({ ...draft, start: e.target.value })}
          />
        </div>
        <div>
          <label className="bp-field-label" htmlFor={`bp-end-${detail.id}`}>Ends</label>
          <input
            id={`bp-end-${detail.id}`}
            type="time"
            className="bp-input bp-time-input"
            value={draft.end}
            onChange={e => setDraft({ ...draft, end: e.target.value })}
          />
        </div>
      </div>
      <p className="bp-hint">
        The window is half-open: a week ending 17:00 is already off hours at 17:00 exactly.
      </p>

      <div className="bp-actions">
        <button
          type="button"
          className="bi-save-btn"
          disabled={busy || !dirty || empty}
          onClick={() => { void run({ availability: weekDraftToWire(draft) }) }}
        >
          {busy ? 'Saving…' : 'Save week'}
        </button>
        {stored && (
          <button
            type="button"
            className="bp-danger-btn"
            disabled={busy}
            onClick={() => { void run(CLEAR_WEEK_PATCH) }}
            title="Remove the declared week. Unknown is never available, so this makes them unavailable — it does not make them always available."
          >
            Clear week
          </button>
        )}
      </div>
      {empty && !stored && (
        <p className="bp-hint">Nothing is declared yet. Pick a zone, at least one day, and the hours.</p>
      )}
      {error && <div className="bridge-error bp-error">{error}</div>}
    </div>
  )
}

// --- Absences ----------------------------------------------------------------

function TimeOffList({ principalID, fetchFn, base }: { principalID: string; fetchFn: FetchFn; base: string }) {
  const [rows, setRows] = useState<TimeOff[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [draft, setDraft] = useState<TimeOffDraft>(EMPTY_TIME_OFF_DRAFT)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const reload = useCallback(async () => {
    const result = await listTimeOff(fetchFn, base, principalID)
    if (result.ok) { setRows(result.value); setLoadError(null) } else setLoadError(result.error)
  }, [fetchFn, base, principalID])

  useEffect(() => { void reload() }, [reload])

  const add = async () => {
    const wire = timeOffDraftToWire(draft)
    if (!wire.ok) { setError(wire.error); return }
    setBusy(true)
    setError(null)
    const result = await addTimeOff(fetchFn, base, principalID, wire.value)
    if (result.ok) {
      setDraft(EMPTY_TIME_OFF_DRAFT)
      await reload()
    } else setError(result.error)
    setBusy(false)
  }

  const remove = async (timeOffID: string) => {
    setBusy(true)
    setError(null)
    // The principal is in the path, so the store 404s a row that is not theirs
    // — one person's absence cannot be removed through another's page.
    const result = await removeTimeOff(fetchFn, base, principalID, timeOffID)
    if (result.ok) await reload()
    else setError(result.error)
    setBusy(false)
  }

  return (
    <div className="bp-availability-timeoff">
      <h5 className="bp-subsection-title">Time off</h5>
      {loadError && <div className="bridge-error bp-error">Could not read time off: {loadError}</div>}
      {rows === null
        ? !loadError && <p className="bp-hint">loading…</p>
        : rows.length === 0
          ? <p className="bp-hint">No time off booked.</p>
          : (
            <ul className="bp-timeoff-list">
              {rows.map(row => (
                <li key={row.id} className="bp-timeoff-row" data-time-off-id={row.id}>
                  <span className="bp-timeoff-range">{describeTimeOff(row)}</span>
                  {row.note && <span className="bp-timeoff-note">{row.note}</span>}
                  <code className="bp-id">{row.id}</code>
                  <button type="button" className="bp-cancel" disabled={busy} onClick={() => { void remove(row.id) }}>
                    remove
                  </button>
                </li>
              ))}
            </ul>
          )}

      <div className="bp-timeoff-add">
        <div className="bp-field">
          <label className="bp-field-label" htmlFor={`bp-off-start-${principalID}`}>Away from</label>
          <input
            id={`bp-off-start-${principalID}`}
            type="datetime-local"
            className="bp-input"
            value={draft.startsAt}
            onChange={e => setDraft({ ...draft, startsAt: e.target.value })}
          />
        </div>
        <div className="bp-field">
          <label className="bp-field-label" htmlFor={`bp-off-end-${principalID}`}>Back at</label>
          <input
            id={`bp-off-end-${principalID}`}
            type="datetime-local"
            className="bp-input"
            value={draft.endsAt}
            onChange={e => setDraft({ ...draft, endsAt: e.target.value })}
          />
        </div>
        <div className="bp-field">
          <label className="bp-field-label" htmlFor={`bp-off-note-${principalID}`}>Note</label>
          <input
            id={`bp-off-note-${principalID}`}
            className="bp-input"
            value={draft.note}
            placeholder="optional"
            onChange={e => setDraft({ ...draft, note: e.target.value })}
          />
        </div>
        <button type="button" className="bi-add-btn" disabled={busy} onClick={() => { void add() }}>+ Add time off</button>
      </div>
      <p className="bp-hint">
        Absolute instants, typed in this browser’s zone — an absence means the same moment wherever it is read. The
        range is half-open: they are back at work at “Back at”.
      </p>
      {error && <div className="bridge-error bp-error">{error}</div>}
    </div>
  )
}

// --- What the store answers --------------------------------------------------

/**
 * The reduced answer for one instant, straight from
 * `GET /principals/{id}/availability?at=` — the same call a dispatcher makes.
 *
 * Deliberately not computed here from the form above. The store applies the
 * five reasons in a fixed order, and a second implementation on this page would
 * be one to keep in step for no gain.
 */
function AvailabilityProbe({ principalID, fetchFn, base }: { principalID: string; fetchFn: FetchFn; base: string }) {
  const [at, setAt] = useState('')
  const [answer, setAnswer] = useState<AvailabilityAnswer | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [servedReasons, setServedReasons] = useState<string[] | null>(null)

  useEffect(() => {
    let canceled = false
    void listAvailabilityReasons(fetchFn, base).then(result => {
      if (!canceled && result.ok) setServedReasons(result.value)
    })
    return () => { canceled = true }
  }, [fetchFn, base])

  const ask = useCallback(async (seconds: number | undefined) => {
    const result = await getAvailability(fetchFn, base, principalID, seconds)
    if (result.ok) { setAnswer(result.value); setError(null) } else { setAnswer(null); setError(result.error) }
  }, [fetchFn, base, principalID])

  // Answer for "now" as soon as the section opens: the reader's first question
  // is almost always whether this person is working right now.
  useEffect(() => { void ask(undefined) }, [ask])

  const drift = servedReasons ? unwordedReasons(servedReasons) : []

  return (
    <div className="bp-availability-probe">
      <h5 className="bp-subsection-title">What a dispatcher sees</h5>
      {answer && (
        <p className={`bp-availability-answer${answer.available ? ' bp-available' : ' bp-unavailable'}`}>
          <strong>{answer.available ? 'Available' : 'Not available'}</strong>
          {' at '}
          {new Date(answer.at * 1000).toLocaleString()}
          {' — '}
          {availabilityReasonLabel(answer.reason)}
        </p>
      )}
      <div className="bp-probe-row">
        <input
          type="datetime-local"
          className="bp-input"
          value={at}
          aria-label="Check availability at a particular time"
          onChange={e => setAt(e.target.value)}
        />
        <button
          type="button"
          className="bi-add-btn"
          onClick={() => {
            const seconds = at.trim() ? Math.floor(new Date(at).getTime() / 1000) : undefined
            void ask(Number.isNaN(seconds) ? undefined : seconds)
          }}
        >
          Check
        </button>
        <button type="button" className="bp-cancel" onClick={() => { setAt(''); void ask(undefined) }}>now</button>
      </div>
      {drift.length > 0 && (
        <div className="bridge-error bp-error">
          principal-store now serves {drift.length === 1 ? 'a reason' : 'reasons'} this page has no wording for
          ({drift.join(', ')}) — it will be shown as unknown until bridge-ui catches up.
        </div>
      )}
      {error && <div className="bridge-error bp-error">{error}</div>}
    </div>
  )
}
