import { memo, useCallback, useMemo, useRef } from 'react'
import { VList } from 'virtua'
import { useTurns, selectTimeline, useSessionStatus } from '@kayushkin/chat-core'
import { RefChip } from './RefChip'
import type { TurnModel, TimelineItem } from '@kayushkin/chat-core'
import { formatHMS } from './bridgeAdapters'
import { useSelectSession } from './useSelectSession'
import { SubagentsChip } from './SessionStatusLine'
import styles from './Chat.module.css'

interface TimelineProps {
  sessionId: string | null
  /** The pane's share of the split row, as `flex: <grow> 1 0`. Owned by the parent —
   *  see `TurnListProps.style`, which this mirrors. */
  style?: React.CSSProperties
}

/** The Timeline pane — a third view alongside Turns/Raw. Presentation-only: it
 *  consumes `selectTimeline(model)`'s flat, chronological rows and renders them
 *  in bridge-ui's timeline DOM (bc-timeline / bc-timeline-body / bc-tl-item /
 *  bc-tl-<tone>) so it inherits the shared stylesheet. It never decides what a
 *  row is (chat-core owns the derivation, memoized on model identity).
 *
 *  It shows what THIS session did, and nothing a subagent did. A subagent's work
 *  is a session of its own, so a task contributes a spawn row and a finish row,
 *  each carrying a chip that opens the subagent's session — the same chip a
 *  session reference in the chat gets. The pane used to nest a subagent's rows
 *  under a task header instead, which is where the reported bug lived: with more
 *  than one subagent running, rows landed under the wrong header and then fell
 *  out from under it as soon as any task finished.
 *
 *  We feed selectTimeline a TurnModel assembled from useTurns' already-materialized
 *  `entries` (the whole entry dict — selectTimeline reads only `model.entries`,
 *  ordered by eventId). The wrapper object is memoized on `entries` identity so the
 *  selector's identity-memo stays warm across unrelated re-renders. */
export default function Timeline({ sessionId, style }: TimelineProps) {
  // 'raw' guarantees the full entry dict is loaded/tailed; the entries record is the
  // same either way, but this keeps intent explicit (Timeline is the audit surface).
  const { turns, entries, loading, more, loadOlder } = useTurns(sessionId, 'raw')
  // Clicking a subagent chip switches the pane to that session, the same as
  // clicking a session reference in the chat.
  //
  // Held through a ref so this callback's identity NEVER changes. useSelectSession
  // rebuilds on every change to the session list (it reads `groups` to resolve the
  // instance id), and the list churns constantly on a live dashboard. Passed
  // straight down, that invalidated the memo on every TimelineTurn on every poll,
  // so every turn group re-rendered and virtua re-measured the entire list —
  // which it does against absolutely-positioned items, so the visible symptom was
  // rows drawn over each other at stale offsets.
  const select = useSelectSession()
  const selectRef = useRef(select)
  selectRef.current = select
  const onActivateSubagent = useCallback((_kind: string, refId: string) => {
    selectRef.current(refId)
  }, [])
  // Same ref, same reason: handed to a leaf that must not re-render on every
  // change to the session list.
  const onActivateSubagentSession = useCallback((refId: string) => {
    selectRef.current(refId)
  }, [])

  // Applied to every `bc-timeline` root below — the main render and three early
  // returns. One object for the reason `TurnList`'s copy states: the resizer finds
  // this pane by `[data-pane="timeline"]`, and an attribute on the loaded root alone
  // would leave the boundary dead while the pane is still empty.
  const rootProps = { className: 'bc-timeline', style, 'data-pane': 'timeline' }

  const model = useMemo<TurnModel | undefined>(() => {
    if (!sessionId) return undefined
    return {
      sessionId,
      turns,
      entries,
      validator: { maxEventId: 0, eventCount: 0, updatedAt: '' },
      more,
    }
  }, [sessionId, turns, entries, more])

  const timeline = selectTimeline(model)

  // One list child per ROW, not per turn.
  //
  // A turn used to be one child holding all of its rows, and on a long session
  // that child is tens of thousands of pixels tall. virtua sizes a child it has
  // not yet mounted by estimate and corrects when it measures — measured here,
  // scrollHeight swung 78,860 → 92,388 → 84,919 → 55,713 during a single scroll,
  // and a scroll target moved backwards mid-sequence. Rows are near-uniform, so
  // the same correction is worth pixels.
  const rows = useMemo(() => {
    const out: React.ReactNode[] = []
    for (const group of timeline.turns) {
      out.push(
        <TimelineItemRow
          key={group.header.key}
          item={group.header}
          isTurnHeader
          onActivateSubagent={onActivateSubagent}
        />,
      )
      for (const item of group.children) {
        out.push(
          <TimelineItemRow key={item.key} item={item} onActivateSubagent={onActivateSubagent} />,
        )
      }
    }
    return out
  }, [timeline.turns, onActivateSubagent])

  if (!sessionId) {
    return (
      <div {...rootProps}>
        <div className="bc-timeline-body">
          <div className="bc-timeline-empty">Select a session to see its timeline.</div>
        </div>
      </div>
    )
  }

  if (loading && timeline.count === 0) {
    return (
      <div {...rootProps}>
        <div className="bc-timeline-body">
          <div className="bc-timeline-empty">Loading…</div>
        </div>
      </div>
    )
  }

  if (timeline.count === 0) {
    return (
      <div {...rootProps}>
        <div className="bc-timeline-body">
          <div className="bc-timeline-empty">No events yet</div>
        </div>
      </div>
    )
  }

  return (
    <div {...rootProps}>
      <VList className="bc-timeline-body">
        {more ? (
          <button key="__older__" className={styles.loadOlder} onClick={loadOlder}>
            ↑ Load older events
          </button>
        ) : null}
        {rows}
      </VList>
      {/* The running-subagents chip, a SIBLING of the scroller and never a child of
          it. A VList child would be virtualized away when scrolled out of range, and
          would shift the index the sticky-bottom scroll reads — the rule `TurnList`
          already records for its compacting strip and status slot.

          Below the list rather than above it because the popover opens upward
          (`bottom: calc(100% + 6px)`), so anchored at the top it would open off the
          pane. This also puts it where the Turns pane keeps the same chip. */}
      <TimelineSubagents sessionId={sessionId} onOpenSession={onActivateSubagentSession} />
    </div>
  )
}

/**
 * The subscription for the chip above, deliberately its own component.
 *
 * `useLiveStatus` reads the turn model, the activity map AND `s.sessions`, so it
 * re-renders its caller on every sidebar poll. Called in `Timeline` itself that
 * would re-run the row derivation and hand virtua a new element array on every
 * poll, which it measures against absolutely-positioned items — the same failure
 * the `selectRef` dance above exists to prevent. As a leaf, the poll re-renders
 * one chip and nothing else.
 */
function TimelineSubagents({
  sessionId,
  onOpenSession,
}: {
  sessionId: string | null
  onOpenSession: (sessionId: string) => void
}) {
  const status = useSessionStatus(sessionId)
  return <SubagentsChip subagents={status?.subagents ?? []} onOpenSession={onOpenSession} />
}

interface TimelineItemRowProps {
  item: TimelineItem
  /** The row that opens a turn. It carries the turn's banding; every other row
   *  is indented under it. There is no wrapper element to say so any more. */
  isTurnHeader?: boolean
  onActivateSubagent: (kind: string, refId: string) => void
}

const TimelineItemRow = memo(function TimelineItemRow({
  item,
  isTurnHeader,
  onActivateSubagent,
}: TimelineItemRowProps) {
  const rowClass = isTurnHeader ? 'bc-tl-row-turn-header' : 'bc-tl-row-in-turn'
  return (
    <div
      className={`bc-tl-item bc-tl-row ${rowClass} bc-tl-${item.tone}`}
      title={hoverText(item.fullText || item.detail || item.label)}
    >
      <span className="bc-tl-ts">{formatHMS(item.ts)}</span>
      <span className="bc-tl-icon">{item.icon}</span>
      <span className="bc-tl-label">{item.label}</span>
      {item.subagentType && <span className="bc-tl-role">{item.subagentType}</span>}
      {item.detail && <span className="bc-tl-detail">{item.detail}</span>}
      {/* Only a task that got a session has anything to link to. A backgrounded
          shell gets the same task rows and deliberately never does, so the chip
          is absent rather than dead. */}
      {item.subagentSessionId && (
        <RefChip
          kind="session"
          refId={item.subagentSessionId}
          className={styles.refChip}
          onActivate={onActivateSubagent}
        />
      )}
    </div>
  )
})

/** How much of a row's text its hover title carries. `fullText` is the tool's whole
 *  input and output, and a title attribute holding a 586 KB grep result is still 586 KB
 *  in the DOM for every mounted row. The Raw view shows the rest. */
const HOVER_TEXT_LIMIT = 2000

function hoverText(text: string): string {
  return text.length > HOVER_TEXT_LIMIT ? `${text.slice(0, HOVER_TEXT_LIMIT)}…` : text
}
