import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import type { CardEvent, CardNote, CardTimeSummary, CardTimeline, ClockState, TimelineEntry } from '@kayushkin/kanban-store-types'
import { entityTarget } from '../entityLinks'
import { arrivalAssignmentDetailOf, describeArrivalAssignment } from '../kanbanBoardSettings'
import { usePrincipals } from '../usePrincipals'
import { formatDurationCompact, formatDurationProse } from '../utils'

/**
 * A card's clock, as a badge.
 *
 * It answers the question a board is actually asked — is this late? — which
 * needs two numbers, not one. A card that arrived yesterday and spent all of it
 * waiting on somebody else's reply has burned none of its limit; a card opened
 * an hour ago and worked the whole hour has burned half a two-hour one. Reporting
 * age alone cannot tell those apart, so the badge leads with the budget clock and
 * keeps elapsed in the tooltip.
 *
 * A card with no limit still gets the badge, showing what it has spent with
 * nothing to spend it against.
 *
 * A card with a limit but no recorded actions is the live majority: every card
 * that existed before the event log did. Its limit is real and worth showing,
 * and its spend is not zero but unknown — so the badge states the rung and the
 * limit and puts a dash where the figure would go. Printing 0s there would be a
 * measurement the data never supported, and on a card that has sat for months it
 * would be the most reassuring number on the board.
 */
export function CardBudgetBadge({ time }: { time: CardTimeSummary | undefined }) {
  if (!hasClockData(time)) return null

  const limit = formatDurationCompact(time.budget_seconds)
  const measured = time.event_count > 0
  const spent = measured ? formatDurationCompact(time.budget_clock_seconds) : null
  if (!measured && !limit) return null

  const over = time.over_budget === true
  const nearly = !over && time.budget_seconds !== undefined
    && time.budget_clock_seconds >= time.budget_seconds * 0.75

  const classes = ['bk-card-budget']
  if (!measured) classes.push('bk-card-budget-unmeasured')
  if (over) classes.push('bk-card-budget-over')
  else if (nearly) classes.push('bk-card-budget-close')
  if (time.clock_state === 'paused') classes.push('bk-card-budget-paused')
  if (time.clock_state === 'stopped') classes.push('bk-card-budget-stopped')

  return (
    <span className={classes.join(' ')} title={describeCardTime(time)}>
      {time.priority_label && <b className="bk-card-budget-rung">{time.priority_label}</b>}
      <span className="bk-card-budget-clock">
        {measured ? `${clockStateMark(time.clock_state)} ${spent}` : '—'}
      </span>
      {limit && <span className="bk-card-budget-limit">/ {limit}</span>}
    </span>
  )
}

/** Whether a card has anything to say about its clock. Cards whose whole history
 * predates the event log have no events and no rung, and asking them for a
 * budget badge would draw a zero that means "we never saw it", not "no time has
 * been spent". */
export function hasClockData(time: CardTimeSummary | undefined): time is CardTimeSummary {
  if (!time) return false
  return time.event_count > 0 || time.budget_seconds !== undefined
}

/** The mark says which clock is running, because the same number means different
 * things when it is still counting and when it has stopped. */
function clockStateMark(state: ClockState | ''): string {
  if (state === 'paused') return '⏸'
  if (state === 'stopped') return '✓'
  return '▶'
}

/** The tooltip carries every figure the badge had to leave out, including the
 * working-week ones when the board declares hours. */
export function describeCardTime(time: CardTimeSummary): string {
  const lines: string[] = []
  if (time.event_count === 0) {
    if (time.priority_label && time.budget_seconds !== undefined) {
      lines.push(`${time.priority_label}: ${formatDurationProse(time.budget_seconds)} allowed`)
    }
    lines.push('Nothing has been recorded for this card, so how much of its limit is spent is unknown — not zero. Cards carry a clock from the first action taken on them.')
    return lines.join('\n')
  }
  if (time.priority_label && time.budget_seconds !== undefined) {
    lines.push(`${time.priority_label}: ${formatDurationProse(time.budget_seconds)} allowed`)
  } else if (time.priority_label) {
    lines.push(`${time.priority_label}: no time limit set`)
  }
  lines.push(`Worked: ${formatDurationProse(time.budget_clock_seconds)}`)
  lines.push(`Waiting on someone else: ${formatDurationProse(time.waiting_seconds)}`)
  lines.push(`Elapsed in total: ${formatDurationProse(time.elapsed_seconds)}`)
  if (time.business_hours_elapsed_seconds !== undefined) {
    lines.push(`Within business hours — worked ${formatDurationProse(time.business_hours_budget_clock_seconds)}, elapsed ${formatDurationProse(time.business_hours_elapsed_seconds)}`)
  }
  if (time.over_budget === true && time.budget_remaining_seconds !== undefined) {
    lines.push(`Over by ${formatDurationProse(Math.abs(time.budget_remaining_seconds))}`)
  } else if (time.budget_remaining_seconds !== undefined) {
    lines.push(`${formatDurationProse(time.budget_remaining_seconds)} left`)
  }
  lines.push(time.clock_state === 'paused'
    ? 'The clock is paused: the ball is with someone else.'
    : time.clock_state === 'stopped'
      ? 'Finished — the clock has stopped.'
      : 'The clock is running.')
  return lines.join('\n')
}

/**
 * The card's history: what happened, in what order, and how long each step took.
 *
 * Every figure on screen is computed by kanban-store from the same event log
 * this list renders, so a reader can check the totals against the steps rather
 * than taking them on trust.
 */
export function CardTimelinePanel({ cardID, boardID }: { cardID: string; boardID?: string }) {
  const { fetch: fetchFn, kanbanStoreBasePath } = useBridgeConfig()
  const [timeline, setTimeline] = useState<CardTimeline | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [noteBody, setNoteBody] = useState('')
  const [handingOver, setHandingOver] = useState(false)
  const [saving, setSaving] = useState(false)

  const load = useCallback(async () => {
    if (!kanbanStoreBasePath) return
    const query = boardID ? `?board_id=${encodeURIComponent(boardID)}` : ''
    try {
      const res = await fetchFn(`${kanbanStoreBasePath}/api/cards/${encodeURIComponent(cardID)}/timeline${query}`)
      if (!res.ok) throw new Error(`timeline: ${res.status}`)
      setTimeline(await res.json())
      setError(null)
    } catch (e) {
      // A timeline that failed to load says so. Rendering an empty history
      // instead would read as "nothing has happened to this card", which is a
      // different and much more reassuring claim than "we could not find out".
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [cardID, boardID, fetchFn, kanbanStoreBasePath])

  useEffect(() => { void load() }, [load])

  const addNote = async () => {
    if (!noteBody.trim() || !kanbanStoreBasePath) return
    setSaving(true)
    try {
      const res = await fetchFn(`${kanbanStoreBasePath}/api/cards/${encodeURIComponent(cardID)}/notes`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          body: noteBody,
          board_id: boardID ?? '',
          // A note that hands the ball over is the moment the clock pauses, which
          // is how "replied, waiting on them" is one action rather than two.
          clock_state: handingOver ? 'paused' : undefined,
        }),
      })
      if (!res.ok) throw new Error(`add note: ${res.status}`)
      setNoteBody('')
      setHandingOver(false)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  if (error) return <div className="bridge-error">Could not read this card's history: {error}</div>
  if (!timeline) return <div className="bi-empty">Reading this card's history…</div>

  return (
    <div className="bk-timeline">
      <CardTimeTotals time={timeline.summary} />

      <div className="bk-timeline-compose">
        <textarea
          rows={2}
          value={noteBody}
          placeholder="What happened? A status update or a summary — it lands on the timeline."
          onChange={e => setNoteBody(e.target.value)}
        />
        <div className="bk-timeline-compose-actions">
          <label title="Record this note as the moment the work stopped being yours to do. The clock pauses until something starts it again.">
            <input type="checkbox" checked={handingOver} onChange={e => setHandingOver(e.target.checked)} />
            Waiting on someone else after this
          </label>
          <button type="button" className="bi-add-btn" disabled={!noteBody.trim() || saving} onClick={addNote}>
            Add note
          </button>
        </div>
      </div>

      {timeline.entries.length === 0 ? (
        <div className="bi-empty">
          Nothing recorded yet. Cards carry a history from the moment something happens to them — mail
          arriving, an agent picking it up, a move, a note.
        </div>
      ) : (
        <ol className="bk-timeline-list">
          {timeline.entries.map(entry => <TimelineRow key={entry.event.id} entry={entry} />)}
        </ol>
      )}
    </div>
  )
}

/** The three numbers, side by side, with the working-week pair alongside them
 * when the board declares hours. */
function CardTimeTotals({ time }: { time: CardTimeSummary }) {
  const hasBusinessHours = time.business_hours_elapsed_seconds !== undefined
  return (
    <div className="bk-timeline-totals">
      <Figure
        label="Worked"
        value={formatDurationCompact(time.budget_clock_seconds)}
        business={hasBusinessHours ? formatDurationCompact(time.business_hours_budget_clock_seconds) : null}
        hint="Time the work was actually available to be done. This is what the limit is measured against."
        emphasis={time.over_budget === true ? 'over' : undefined}
      />
      <Figure
        label="Waiting"
        value={formatDurationCompact(time.waiting_seconds)}
        business={null}
        hint="Time the ball was with someone else. It does not count against the limit."
      />
      <Figure
        label="Elapsed"
        value={formatDurationCompact(time.elapsed_seconds)}
        business={hasBusinessHours ? formatDurationCompact(time.business_hours_elapsed_seconds) : null}
        hint="Wall clock, from the first thing that happened to the last."
      />
      <Figure
        label={time.priority_label ? `${time.priority_label} limit` : 'Limit'}
        value={formatDurationCompact(time.budget_seconds) ?? 'none'}
        business={null}
        hint={time.budget_seconds === undefined
          ? 'This card carries no limit — either its board ignores priorities, or nobody has ranked it.'
          : 'The time work at this priority is meant to take.'}
      />
    </div>
  )
}

/** One figure, with its business-hours counterpart underneath rather than
 * instead of it — the working week is an extra way of reading the same stretch
 * of time, not a replacement for the wall clock. */
function Figure({
  label, value, business, hint, emphasis,
}: {
  label: string
  value: string | null
  business: string | null
  hint: string
  emphasis?: 'over'
}) {
  return (
    <div className={`bk-timeline-figure${emphasis === 'over' ? ' bk-timeline-figure-over' : ''}`} title={hint}>
      <span className="bk-timeline-figure-label">{label}</span>
      <span className="bk-timeline-figure-value">{value ?? '—'}</span>
      {business !== null && (
        <span className="bk-timeline-figure-business" title="Counting only the board's business hours">
          {business} in hours
        </span>
      )}
    </div>
  )
}

function TimelineRow({ entry }: { entry: TimelineEntry }) {
  const gap = formatDurationCompact(entry.seconds_since_previous_event)
  const held = formatDurationCompact(entry.segment_seconds)
  const when = new Date(entry.event.occurred_at)
  return (
    <li className={`bk-timeline-entry bk-timeline-${entry.clock_state}`}>
      {gap && entry.seconds_since_previous_event > 0 && (
        <div className="bk-timeline-gap">↓ {gap} later</div>
      )}
      <div className="bk-timeline-head">
        <span className="bk-timeline-kind">{describeEventKind(entry.event.kind)}</span>
        <span className="bk-timeline-when" title={when.toLocaleString()}>{when.toLocaleString()}</span>
        {entry.event.actor && <span className="bk-timeline-actor">{entry.event.actor}</span>}
      </div>
      {entry.event.summary && <div className="bk-timeline-summary">{entry.event.summary}</div>}
      <TimelineSubject event={entry.event} />
      <TimelineArrivalAssignment event={entry.event} />
      {entry.note && <NoteBody note={entry.note} />}
      {held && entry.segment_seconds > 0 && (
        <div className="bk-timeline-segment">
          {entry.counts_against_budget ? 'counted' : entry.clock_state === 'paused' ? 'waiting' : 'after this'}
          {' '}{held}{entry.segment_open ? ' and running' : ''}
        </div>
      )}
    </li>
  )
}

/** What an event was ABOUT, when it was about something addressable.
 *
 * kanban-store records the entity on the event's `detail` whenever a link
 * creates one: the mail that arrived, the session an agent was handed. The
 * timeline had that all along and rendered none of it, so "Handed to an agent"
 * named no agent and "Email arrived" named no email — the reader had to go
 * looking in the links list for something the row already knew.
 *
 * Anything without a resolvable subject renders nothing rather than an empty
 * chip. */
function TimelineSubject({ event }: { event: CardEvent }) {
  const { routes, mailPagePath } = useBridgeConfig()
  const subject = eventSubject(event)
  if (!subject) return null

  const { entityType, entityRef } = subject
  const target = entityTarget(entityType, entityRef)

  if (entityType === 'session' && routes.chat) {
    return (
      <div className="bk-timeline-subject">
        <span className="bk-link-type">session</span>
        <Link className="bk-link-ref-action" to={`${routes.chat}?session=${encodeURIComponent(entityRef)}`}
          title={entityRef}>{shortRef(entityRef)} ↗</Link>
      </div>
    )
  }
  if (entityType === 'email' && mailPagePath) {
    // The locator is "account:message id"; the Mail page wants them apart.
    const cut = entityRef.indexOf(':')
    if (cut > 0) {
      const account = entityRef.slice(0, cut)
      const message = entityRef.slice(cut + 1)
      return (
        <div className="bk-timeline-subject">
          <span className="bk-link-type">email</span>
          <Link className="bk-link-ref-action" title={entityRef}
            to={`${mailPagePath}?account=${encodeURIComponent(account)}&message=${encodeURIComponent(message)}`}
          >open in Mail ↗</Link>
        </div>
      )
    }
  }
  if (target) {
    return (
      <div className="bk-timeline-subject">
        <span className="bk-link-type">{entityType}</span>
        <a className="bk-link-ref-action" href={target.href} target="_blank" rel="noopener noreferrer"
          title={entityRef}>{target.label} ↗</a>
      </div>
    )
  }
  return (
    <div className="bk-timeline-subject">
      <span className="bk-link-type">{entityType}</span>
      <span className="bk-link-ref">{shortRef(entityRef)}</span>
    </div>
  )
}

/** Why the card got the assignee it did when it arrived — the pool, the board
 * default, a tag rule — or why it got nobody. kanban-store writes that on the
 * event's detail; an assignment made by hand has none and renders nothing. */
function TimelineArrivalAssignment({ event }: { event: CardEvent }) {
  const principals = usePrincipals()
  const detail = arrivalAssignmentDetailOf(event)
  if (!detail) return null
  const principalName = (id: string) => {
    const principal = principals.byId.get(id)
    return principal ? `${principal.display_name} (${id})` : id
  }
  const assignee = typeof (event.detail as Record<string, unknown>).principal_id === 'string'
    ? (event.detail as Record<string, string>).principal_id
    : ''
  const why = describeArrivalAssignment(event.kind, detail, principalName)
  if (!why) return null
  return (
    <div className="bk-timeline-subject">
      {assignee && <span className="bk-link-ref">{principalName(assignee)}</span>}
      <span className="bk-timeline-assignment-source">{why}</span>
    </div>
  )
}

/** Reads the entity off an event's detail.
 *
 * `detail` is free-form JSON by design — kanban-store keeps whatever a caller
 * recorded — so this checks the shape rather than trusting it. A detail that is
 * not an object with two string fields is not a subject, and saying so quietly
 * is right: detail is also where callers put things that are not entities. */
function eventSubject(event: CardEvent): { entityType: string; entityRef: string } | null {
  const detail = event.detail
  if (!detail || typeof detail !== 'object') return null
  const entityType = (detail as Record<string, unknown>).entity_type
  const entityRef = (detail as Record<string, unknown>).entity_ref
  if (typeof entityType !== 'string' || typeof entityRef !== 'string') return null
  if (!entityType || !entityRef) return null
  return { entityType, entityRef }
}

/** Session ids and locators are long and mostly prefix. Show the tail, which is
 * the part that differs, and keep the whole thing on hover. */
function shortRef(ref: string): string {
  return ref.length <= 28 ? ref : '…' + ref.slice(-24)
}

function NoteBody({ note }: { note: CardNote }) {
  return (
    <div className="bk-timeline-note">
      {note.kind !== 'note' && <span className="bk-tag">{note.kind}</span>}
      {note.body}
    </div>
  )
}

/** Event kinds are stored as they were recorded, including kinds this UI has
 * never heard of. An unknown one reads as its own name with the underscores
 * taken out rather than being dropped or labeled "unknown" — the log is the
 * record, and a reader is better served by the raw kind than by nothing. */
const EVENT_KIND_LABELS: Record<string, string> = {
  card_created: 'Card created',
  card_attached: 'Added to the board',
  card_detached: 'Taken off the board',
  card_moved: 'Moved',
  card_held: 'Held',
  card_unheld: 'Released',
  card_completed: 'Completed',
  email_received: 'Email arrived',
  agent_dispatched: 'Handed to an agent',
  agent_finished: 'Agent finished',
  note_added: 'Note',
  waiting_started: 'Waiting on someone else',
  waiting_ended: 'Ball back with us',
  assigned: 'Assigned',
  unassigned: 'Unassigned',
  assignment_skipped: 'Nobody assigned',
  // Kinds this store has never defined. kind is deliberately open — an action
  // it has never heard of is kept — and these are the two the demo team
  // records: work handed to a reviewer, and the verdict coming back.
  pull_request_opened: 'Pull request opened',
  pull_request_reviewed: 'Pull request reviewed',
}

export function describeEventKind(kind: string): string {
  return EVENT_KIND_LABELS[kind] ?? kind.replace(/_/g, ' ')
}
