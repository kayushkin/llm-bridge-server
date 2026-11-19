import { useEffect, useRef, useState, type CSSProperties } from 'react'
import {
  isRunningState,
  useConnState,
  useInProgressTodo,
  useSessionStatus,
  type InProgressTodo,
  type SessionStatus,
  type SessionStatusSubagent,
} from '@kayushkin/chat-core'
import { formatHMS } from './bridgeAdapters'
import { statusActivitySince, statusActivityText, statusRateLimitText } from './sessionStatusWords'
import styles from './Chat.module.css'

// The turns pane's ONE status slot: a fixed-height line under the scroller that
// renders whichever status is current. One slot, one height; only its content swaps.
//
// What it shows comes from two places and no more:
//
//  1. Facts only this browser knows — the connection is down, a Stop or Resume the
//     user just clicked was refused, a Compact was just asked for. These outrank
//     everything, because nothing below them can be trusted (or has caught up) yet.
//  2. The session's `SessionStatus`, which llm-bridge-server decides and chat-core
//     keeps newest-`as_of`-first. State, tool in flight, thinking or text, running
//     subagents, since when. It rides the session row, so it is right for a session
//     in the same commit as switching to it.
//
// Until 2026-09-17 tier 2 was rebuilt here from the transcript and a live activity
// fold, and a ladder of six kinds papered over the two disagreeing. There is nothing
// left to disagree.
//
// The subagents chip sits at a fixed place at the right of the slot whatever the slot
// is saying, so it neither slides as the activity text changes width nor vanishes
// (closing its popover) when the line briefly says "compacting" or "disconnected".

/** The slice of Chat's one `useComposer` instance the status slot renders.
 *  `error` is hook-local state, which is why this arrives as a prop rather than
 *  from a second hook call that would never see it. */
export interface ComposerStatus {
  paused: boolean
  resumable: boolean
  error: string | null
  /** Which action `error` describes, for phrasing — set by the Composer's handlers. */
  failedAction: 'stop' | 'resume' | null
}

/** What the slot says. null = nothing to say (the slot disappears, its spacer may
 *  linger). `status` rides along on every kind so the subagents chip can be drawn
 *  beside any of them. */
export type StatusSlotContent =
  | ({ status: SessionStatus | null; todo?: InProgressTodo } & (
      | { kind: 'error'; text: string }
      | { kind: 'disconnected'; text: string }
      | { kind: 'stopped' }
      | { kind: 'paused' }
      | { kind: 'compacting' }
      | { kind: 'live' }
      | { kind: 'rate_limited'; text: string }
      /** The turn is over but tasks it started still run — the server's
       *  `background_tasks_running`, or a status that still lists subagents. The
       *  chip is most of the content. */
      | { kind: 'tasks' }
    ))
  | null

/** States in which the session is working and the live row is what to draw.
 *  `awaiting_permission` is not one of chat-core's RUNNING states — the turn is
 *  parked on a person — but it is mid-turn, and saying so beats going blank. */
function isLiveState(state: string): boolean {
  return isRunningState(state) || state === 'awaiting_permission'
}

/** How long a page's FIRST connection may take before the slot says "connecting".
 *
 *  Every page load starts unconnected, and the stream usually opens within half a
 *  second. Saying so drew the slot and took it away again before the session's own
 *  status arrived to fill it — measured 2026-09-28 on 9 of 9 reloads: "connecting"
 *  at ~430ms, gone at ~460ms, the real status back at ~670ms, each one a jump in the
 *  transcript above it. A first connection slower than this is worth reporting; a
 *  lost connection (after one was open) is reported at once. */
const FIRST_CONNECTION_GRACE_MS = 2000

/** Whether an unopened connection should be reported: always once this page has had
 *  an open connection, and before that only after `FIRST_CONNECTION_GRACE_MS`. */
function useConnectionWaitIsWorthReporting(connectionIsOpen: boolean): boolean {
  const [connectionHasBeenOpen, setConnectionHasBeenOpen] = useState(connectionIsOpen)
  // State adjusted during render, React's pattern for state derived from a prop's
  // history: it re-renders once, before anything is painted.
  if (connectionIsOpen && !connectionHasBeenOpen) setConnectionHasBeenOpen(true)
  const [firstConnectionGraceIsOver, setFirstConnectionGraceIsOver] = useState(false)
  useEffect(() => {
    const timer = setTimeout(() => setFirstConnectionGraceIsOver(true), FIRST_CONNECTION_GRACE_MS)
    return () => clearTimeout(timer)
  }, [])
  return connectionHasBeenOpen || firstConnectionGraceIsOver
}

/**
 * Decide the slot's content. Browser-local facts first (see the header), then the
 * server's status read straight through.
 *
 * `compactRequested` is chat-core's hook-local flag, true from the Compact click until
 * the boundary event. It stays because a click should answer at once; the server's own
 * `compacting` state — which also covers a compaction the harness started by itself —
 * draws the same row.
 */
export function useStatusSlotContent(
  sessionId: string | null,
  compactRequested: boolean,
  composerStatus: ComposerStatus,
): StatusSlotContent {
  const status = useSessionStatus(sessionId)
  const todo = useInProgressTodo(sessionId)
  const connState = useConnState()
  const connectionWaitIsWorthReporting = useConnectionWaitIsWorthReporting(connState === 'open')
  if (composerStatus.error) {
    const prefix =
      composerStatus.failedAction === 'stop'
        ? "couldn't stop — still running: "
        : composerStatus.failedAction === 'resume'
          ? "couldn't resume — still stopped: "
          : ''
    return { kind: 'error', text: `${prefix}${composerStatus.error}`, status }
  }
  // 'open' is the only state in which updates are actually flowing (the Composer's
  // own send gate uses the same test).
  if (connState !== 'open') {
    // Nothing to say yet: the page has only just started connecting.
    if (!connectionWaitIsWorthReporting) return null
    return {
      kind: 'disconnected',
      text:
        connState === 'closed'
          ? 'disconnected — nothing can be sent'
          : 'connecting — nothing can be sent yet',
      status,
    }
  }
  if (compactRequested) return { kind: 'compacting', status }
  if (!status) return null
  if (composerStatus.resumable) return { kind: 'stopped', status }
  if (status.state === 'paused') return { kind: 'paused', status }
  if (status.state === 'compacting') return { kind: 'compacting', status }
  if (isLiveState(status.state)) return { kind: 'live', status, todo }
  const rateLimit = statusRateLimitText(status, (unixSeconds) =>
    formatHMS(new Date(unixSeconds * 1000).toISOString()),
  )
  if (rateLimit) return { kind: 'rate_limited', text: rateLimit, status }
  if (status.state === 'background_tasks_running' || (status.subagents?.length ?? 0) > 0) {
    return { kind: 'tasks', status }
  }
  return null
}

/** Compact elapsed readout since an RFC3339 timestamp: `42s`, `3m 07s`, `1h 12m`. */
function formatElapsed(sinceTs: string, nowMs: number): string {
  const startedMs = Date.parse(sinceTs)
  if (Number.isNaN(startedMs)) return ''
  const totalSeconds = Math.max(0, Math.floor((nowMs - startedMs) / 1000))
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, '0')}m`
  if (minutes > 0) return `${minutes}m ${String(seconds).padStart(2, '0')}s`
  return `${seconds}s`
}

/** What kind of worker a task row is: the agent role when reported, else an honest
 *  generic — a backgrounded shell is not an agent and must not read as one. */
function subagentKindLabel(task: SessionStatusSubagent): string {
  if (task.subagent_type) return task.subagent_type
  return task.task_type === 'local_bash' ? 'background shell' : 'subagent'
}

/** One row of the popover: a task still running, or one that finished while the
 *  popover was open. */
interface SubagentRow {
  task: SessionStatusSubagent
  /** Set when the task left the status while the popover was open (RFC3339-less:
   *  the ms clock reading it was last seen running at). */
  finishedAtMs?: number
}

/**
 * The popover's rows: every task running now, plus every task that was running at
 * some point since the popover opened, in the order first seen.
 *
 * A finished task's row used to be removed the instant it finished, and every row
 * below it jumped up a line under the reader's eyes. It now stays where it is,
 * marked done with its clock stopped, until the popover closes — the list only ever
 * grows while it is being read.
 */
function useStableSubagentRows(
  running: readonly SessionStatusSubagent[],
  open: boolean,
): SubagentRow[] {
  const seen = useRef(new Map<string, SubagentRow>())
  if (!open) {
    seen.current = new Map()
    return running.map((task) => ({ task }))
  }
  const runningIds = new Set<string>()
  for (const task of running) {
    runningIds.add(task.task_id)
    seen.current.set(task.task_id, { task })
  }
  for (const [taskId, row] of seen.current) {
    if (!runningIds.has(taskId) && row.finishedAtMs === undefined) {
      seen.current.set(taskId, { ...row, finishedAtMs: Date.now() })
    }
  }
  return [...seen.current.values()]
}

/**
 * The `⑂ N agents` chip and the popover behind it: one row per subagent, with its
 * kind, description, last tool, elapsed, and a link to its own promoted session.
 *
 * Mounted by the status line and by the Timeline pane — the status line lives in the
 * Turns pane alone, so hiding that pane would otherwise take the only view of a
 * session's running subagents with it.
 *
 * Pure: it takes the subagents rather than subscribing for them.
 */
export function SubagentsChip({
  subagents,
  onOpenSession,
}: {
  subagents: readonly SessionStatusSubagent[]
  onOpenSession: (sessionId: string) => void
}) {
  const [open, setOpen] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)
  const rows = useStableSubagentRows(subagents, open)

  // The clock ticks only while the popover is OPEN: the elapsed readouts it drives
  // are inside the popover and are the only thing here that changes per second.
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    if (!open) return
    setNowMs(Date.now())
    const timer = setInterval(() => setNowMs(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [open])

  useEffect(() => {
    if (!open) return
    const onMouseDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false)
    }
    const onKeyDown = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onMouseDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onMouseDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  // Nothing running and nothing being read: no chip. While the popover is open the
  // chip stays even at zero, so the rows the reader is looking at are not pulled away.
  const count = subagents.length
  if (count === 0 && !open) return null

  return (
    <div ref={rootRef} className={styles.subagentsAnchor}>
      <button
        className={styles.statusAgentsChip}
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        title="Running subagents"
      >
        ⑂ {count} {count === 1 ? 'agent' : 'agents'}
      </button>
      {open && (
        <div className={styles.statusPopover}>
          {rows.map(({ task, finishedAtMs }) => (
            <div
              key={task.task_id}
              className={`${styles.statusAgentRow} ${finishedAtMs !== undefined ? styles.statusAgentRowDone : ''}`}
            >
              <span className={styles.statusAgentKind} title={subagentKindLabel(task)}>
                {subagentKindLabel(task)}
              </span>
              <span className={styles.statusAgentDescription} title={task.description}>
                {task.description ?? ''}
              </span>
              <span className={styles.statusAgentTool} title={task.last_tool_name}>
                {finishedAtMs !== undefined ? 'done' : (task.last_tool_name ?? '')}
              </span>
              <span className={styles.statusAgentElapsed}>
                {formatElapsed(task.started_at, finishedAtMs ?? nowMs)}
              </span>
              {task.session_id ? (
                <button
                  className={styles.statusOpen}
                  onClick={() => onOpenSession(task.session_id!)}
                  title={`Open subagent session ${task.session_id}`}
                >
                  open ↗
                </button>
              ) : (
                <span aria-hidden />
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

interface SessionStatusLineProps {
  /** The decided content. Never null — the parent renders nothing (or the spacer)
   *  instead of this component when there is nothing to say. */
  status: NonNullable<StatusSlotContent>
  /** Opens a subagent's promoted session — TurnList's `select`. */
  onOpenSession: (sessionId: string) => void
}

export default function SessionStatusLine({ status: content, onOpenSession }: SessionStatusLineProps) {
  // One 1s clock for the elapsed readout, ticking only while the live row is shown —
  // no other kind draws a clock.
  const isLive = content.kind === 'live'
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    if (!isLive) return
    setNowMs(Date.now())
    const timer = setInterval(() => setNowMs(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [isLive])

  const activityText = isLive ? statusActivityText(content.status) : ''
  const since = isLive ? statusActivitySince(content.status) : undefined

  return (
    <div
      className={`bc-status-slot ${styles.statusSlot}`}
      role="status"
      aria-live="polite"
      data-mood={statusMood(content)}
    >
      {/* A slot for the host's mascot. bridge-ui draws nothing in it (the rule hides it);
          a host theme that has a character can show one per `data-mood`. */}
      <span className="bc-status-mascot" aria-hidden />
      {/* LEFT: whatever the slot is saying. The one part that may shrink. */}
      <div className={styles.statusMain}>
        {content.kind === 'error' && (
          <span className={`${styles.statusText} ${styles.statusError}`} title={content.text}>
            ✗ {content.text}
          </span>
        )}
        {content.kind === 'disconnected' && (
          <span className={`${styles.statusText} ${styles.statusWarn}`}>{content.text}</span>
        )}
        {content.kind === 'rate_limited' && (
          <span className={`${styles.statusText} ${styles.statusWarn}`} title={content.text}>
            {content.text}
          </span>
        )}
        {content.kind === 'stopped' && (
          <span className={styles.statusText}>⏸ stopped — its harness process is gone</span>
        )}
        {content.kind === 'paused' && <span className={styles.statusText}>⏸ paused</span>}
        {content.kind === 'tasks' && (
          <>
            <span className={styles.statusPulse} aria-hidden />
            <span className={styles.statusText}>turn over — background tasks still running</span>
          </>
        )}
        {content.kind === 'compacting' && (
          <>
            <span className={`${styles.statusPulse} ${styles.statusPulseViolet}`} aria-hidden />
            <span className={styles.statusCompacting}>Compacting context…</span>
          </>
        )}
        {content.kind === 'live' && (
          <>
            <span className={styles.statusPulse} aria-hidden />
            {content.todo && (
              <>
                <span className={styles.statusTodo} title={content.todo.text}>
                  {content.todo.text}
                </span>
                <span className={styles.statusSep} aria-hidden>
                  ·
                </span>
              </>
            )}
            <Ticker text={activityText} />
          </>
        )}
      </div>
      {/* RIGHT: fixed places. The chip is drawn beside EVERY kind, from the same spot
          in the tree, so its popover survives the slot changing what it says. */}
      <SubagentsChip subagents={content.status?.subagents ?? NO_SUBAGENTS} onOpenSession={onOpenSession} />
      <span className={styles.statusTime}>
        {since ? `${formatHMS(since)} · ${formatElapsed(since, nowMs)}` : ''}
      </span>
    </div>
  )
}

const NO_SUBAGENTS: readonly SessionStatusSubagent[] = []

/** What mood the slot is in, for a host theme to draw by (`data-mood` on the slot):
 *  - `thinking`: the model is generating, or the context is being compacted
 *  - `working`: a tool is running, or background tasks are
 *  - `waiting`: parked on a person, for permission
 *  - `resting`: paused, or stopped
 *  - `trouble`: an error, a dropped connection, a rate limit */
export type StatusMood = 'thinking' | 'working' | 'waiting' | 'resting' | 'trouble'

export function statusMood(content: NonNullable<StatusSlotContent>): StatusMood {
  switch (content.kind) {
    case 'error':
    case 'disconnected':
    case 'rate_limited':
      return 'trouble'
    case 'stopped':
    case 'paused':
      return 'resting'
    case 'compacting':
      return 'thinking'
    case 'tasks':
      return 'working'
    case 'live': {
      const status = content.status
      if ((status?.tools ?? []).length > 0) return 'working'
      if (status?.state === 'awaiting_permission' || status?.state === 'waiting_on_approval') return 'waiting'
      if (status?.state === 'model_generating') return 'thinking'
      return 'working'
    }
  }
}

/** The live activity text as a ticker tape: when it is wider than its room it scrolls
 *  slowly to its end and back, instead of being cut off with an ellipsis, so the part
 *  that says what the command is doing can be read. It holds still while hovered, and
 *  for anyone who asks the system for reduced motion (the CSS drops the animation, and
 *  the ellipsis comes back). */
function Ticker({ text }: { text: string }) {
  const outerRef = useRef<HTMLSpanElement | null>(null)
  const innerRef = useRef<HTMLSpanElement | null>(null)
  const [shift, setShift] = useState(0)
  useEffect(() => {
    const outer = outerRef.current
    const inner = innerRef.current
    if (!outer || !inner) return
    const measure = () => setShift(Math.max(0, inner.scrollWidth - outer.clientWidth))
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(outer)
    return () => observer.disconnect()
  }, [text])
  // Roughly 40px a second, so a long command reads rather than whips past.
  const seconds = Math.max(4, shift / 40)
  const tickerStyle = shift > 0
    ? ({ ['--ticker-shift']: `-${shift}px`, ['--ticker-seconds']: `${seconds}s` } as CSSProperties)
    : undefined
  return (
    <span
      ref={outerRef}
      className={`${styles.statusText} ${styles.ticker}`}
      data-ticker={shift > 0 ? 'on' : 'off'}
      title={text}
    >
      <span ref={innerRef} className={styles.tickerTape} style={tickerStyle}>
        {text}
      </span>
    </span>
  )
}

/** The empty spacer TurnList renders while lingering after the slot empties: the
 *  same box as the slot with no content and a transparent border, so the transcript
 *  holds its position until the user scrolls. Exported from here so the slot and
 *  its stand-in can never drift apart in height. */
export const STATUS_SPACER_CLASSNAME = `${styles.statusSlot} ${styles.statusSpacer}`
