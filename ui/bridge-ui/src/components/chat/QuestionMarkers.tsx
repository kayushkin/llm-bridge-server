import { createPortal } from 'react-dom'
import { StatusDot } from './StatusDot'
import { useOpenSignals } from '@kayushkin/chat-core'
import { SignalRequestList } from './SessionSignals'
import { useAnchoredDropdown } from './useAnchoredDropdown'
import { humanAskedFor } from './sessionAwaitingHuman'
import styles from './Chat.module.css'

interface SessionQuestionMarkerProps {
  sessionId: string
  /** For the accessible name and the panel heading. The row already resolved it, so it is
   *  passed in rather than re-resolved here — one session, one display name. */
  sessionName: string
  /** The row's effective state, which decides the wording. NOT necessarily an
   *  awaiting-human state any more: a row also carries the marker when an open question
   *  signal is recorded against it, and such a session is usually `completed` or
   *  `aborted` by the time anyone looks. `humanAskedFor` says "approval" only for
   *  `awaiting_permission` and "question" for everything else, which is right for both. */
  displayState: string
  /** True when this row is the session the chat pane is showing. That pane's own
   *  `AwaitingYouBanner` renders every open signal this session has, so the panel points
   *  at it instead of drawing a second copy of the same answer form. */
  isActiveSession: boolean
  open: boolean
  onToggle: (sessionId: string) => void
  onDismiss: () => void
}

/**
 * The `?` on a session row: the row's status indicator, made into a disclosure control.
 *
 * Seeing the marker is how the user learns a session is blocked on them — there is
 * deliberately no global "needs you" count anywhere in the sidebar, because a count tells
 * you a number and the marker tells you WHICH ROW, which is the thing you have to act on.
 *
 * It is a real `<button>` and a SIBLING of `bc-session-item-main`, never a child of it:
 * nesting one button inside another is invalid HTML and leaves the inner control out of
 * the accessibility tree. Being a sibling is also what keeps it from swallowing the row's
 * click-to-select — the two controls simply do not overlap.
 */
export function SessionQuestionMarker({
  sessionId,
  sessionName,
  displayState,
  isActiveSession,
  open,
  onToggle,
  onDismiss,
}: SessionQuestionMarkerProps) {
  const { anchorRef, panelRef, panelStyle } = useAnchoredDropdown<HTMLSpanElement>(
    open,
    onDismiss,
  )
  const label = `Open the ${humanAskedFor(displayState)} waiting in ${sessionName}`

  return (
    <span className={styles.questionMarkerAnchor} ref={anchorRef}>
      <button
        type="button"
        className={`${styles.questionMarker} ${open ? styles.questionMarkerOpen : ''}`}
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={label}
        title={label}
        onClick={() => onToggle(sessionId)}
      >
        {/* bridge-ui's own dot, so the glyph inside the marker is the SAME one every
            other surface draws for this state (amber `?` for awaiting_user, red `!` for
            awaiting_permission) rather than a second opinion about what waiting looks
            like. Both states have a `.bc-status-dot-*` rule in bridge-ui's stylesheet —
            checked, because a state without one renders a laid-out, invisible dot. The
            caret beside it is drawn from this module's own CSS, so the control carries a
            visible mark of its own either way. */}
        <StatusDot state={displayState} title={displayState} />
        <span className={styles.questionMarkerCaret} aria-hidden>
          ▾
        </span>
      </button>
      {open &&
        createPortal(
          <div
            ref={panelRef}
            className={styles.questionPanel}
            role="dialog"
            aria-label={`${humanAskedFor(displayState)} — ${sessionName}`}
            // The portal puts this at the end of <body>, far from the row it belongs to,
            // so it carries the id of the session it is answering. Without it the DOM
            // cannot say which session an open panel is for.
            data-session-id={sessionId}
            // Placement is inline because it is measured, not themeable; `visibility`
            // hides the first, unmeasured paint rather than flashing it at (0,0).
            style={{
              top: panelStyle?.top ?? 0,
              left: panelStyle?.left ?? 0,
              visibility: panelStyle ? 'visible' : 'hidden',
            }}
          >
            <div className={styles.questionPanelTitle}>{sessionName}</div>
            {/* The open session answers its questions in the chat pane, which renders
                every one of them. Two live answer forms for the same question is worse
                than one anywhere else: both accept input, only one submit lands, and the
                loser sits there looking answerable.

                Branched HERE rather than inside the body so the body never MOUNTS for the
                active session — its `useOpenSignals(sessionId)` would otherwise put a
                per-session read on the wire to build a list nothing renders.

                This replaced a per-request-id exclusion against the chat pane's parked
                hooks. That test could only ever hide the PARKED questions, so a derived
                one — or one whose park had died — was still drawn twice, and it needed
                the panel to know what another component was rendering. */}
            {isActiveSession ? (
              <p className={styles.questionPanelNote}>
                You’re looking at this session — answer it in the chat, below the
                transcript.
              </p>
            ) : (
              <SessionQuestionPanelBody sessionId={sessionId} />
            )}
          </div>,
          document.body,
        )}
    </span>
  )
}

/**
 * The open signals of one session the chat pane is NOT showing, answerable in place.
 *
 * Mounted only while its dropdown is open, which is what gates the PER-SESSION read. The
 * sidebar separately holds one cross-session read to decide which rows get a marker at all
 * (`useSessionsWithOpenOperatorQuestion`); both go through the same 30s cache in `useOpenSignals`,
 * so opening a panel for a session the list already covered costs no second request.
 *
 * Uses `useOpenSignals` + `SignalRequestList` rather than `SessionSignals` for the empty
 * case: `SessionSignals` renders nothing when there is nothing to show, which is right
 * inside a bigger surface and wrong here — the user clicked a control and an empty box
 * would be the panel refusing to say why.
 */
function SessionQuestionPanelBody({ sessionId }: { sessionId: string }) {
  const { requests, available, loading, error, reload } = useOpenSignals(sessionId)

  // Said out loud rather than swallowed — the session is still blocked, and a panel that
  // silently showed nothing would look like the question had gone away.
  if (error !== null) {
    return <p className={styles.questionPanelError}>Couldn’t load the question: {error}</p>
  }
  if (loading && requests.length === 0) {
    return <p className={styles.questionPanelNote}>Loading…</p>
  }
  // `available === false` is the server answering 404 for /signals: this bridge-server
  // predates the route. Not an error and not "nothing is waiting" — the row's state says
  // something IS. Nothing here throws, so the sidebar renders on regardless.
  if (!available) {
    return (
      <p className={styles.questionPanelNote}>
        This bridge-server doesn’t record signals, so the question can only be answered in
        the session itself — open the row to see it.
      </p>
    )
  }
  if (requests.length === 0) {
    return (
      <p className={styles.questionPanelNote}>
        No open question is recorded for this session yet — open the row to see what it is
        waiting on.
      </p>
    )
  }
  return <SignalRequestList requests={requests} compact onResolved={reload} />
}

interface FolderQuestionRollupMarkerProps {
  /** How many sessions in this folder are waiting on a human. Zero renders nothing. */
  waitingCount: number
  collapsed: boolean
  /** The folder's label, for the accessible name only. */
  folderLabel: string
  /** Expand the folder. Never collapse it: the marker exists because collapsing hid the
   *  rows that carry the real, answerable `?`. */
  onReveal: () => void
}

/**
 * The folder header's rollup `?`.
 *
 * A collapsed folder hides its rows, and with them every marker beneath it — so the count
 * is the only thing that says a folder is sitting on questions. It deliberately does NOT
 * open a dropdown: answering here would mean picking one of several sessions on the user's
 * behalf, and the row that owns the question is one click away.
 *
 * When the folder is already open the rollup is a plain `<span>`, not a button. A control
 * whose only act is "reveal" has nothing to do on a folder that is already revealed, and a
 * button that does nothing is worse than a label.
 */
export function FolderQuestionRollupMarker({
  waitingCount,
  collapsed,
  folderLabel,
  onReveal,
}: FolderQuestionRollupMarkerProps) {
  if (waitingCount === 0) return null
  const what = `${waitingCount} session${waitingCount === 1 ? '' : 's'} in ${folderLabel} waiting on you`

  if (!collapsed) {
    return (
      <span className={styles.folderQuestionRollupStatic} title={what}>
        <span className={styles.folderQuestionGlyph} aria-hidden>
          ?
        </span>
        <span className={styles.folderQuestionCount}>{waitingCount}</span>
      </span>
    )
  }
  return (
    <button
      type="button"
      className={styles.folderQuestionRollup}
      aria-label={`${what} — expand the folder to answer`}
      title={`${what} — expand to answer`}
      onClick={onReveal}
    >
      <span className={styles.folderQuestionGlyph} aria-hidden>
        ?
      </span>
      <span className={styles.folderQuestionCount}>{waitingCount}</span>
    </button>
  )
}
