import { useMemo } from 'react'
import { SIGNAL_KIND_QUESTION, useOpenSignalsByAttention, type SignalRequest } from '@kayushkin/chat-core'
import { sessionChipSignalShares } from '../../sessionOpenQuestions'

/**
 * The sessions with an open question THE OPERATOR is to answer, across EVERY session
 * rather than the sidebar's loaded page.
 *
 * Only the operator's group of chat-core's split (`useOpenSignalsByAttention`:
 * interactive and herald sessions). A background session's question — an autoworker,
 * a delegate, a scheduled task — never puts a `?` on a row; it is listed in the
 * inbox's Background agents group instead.
 *
 * Why signals at all, rather than session state: a signal outlives the state that
 * produced it. The agent asks, the turn ends, the session settles to `completed` or
 * is `aborted` — and the question is still open. Measured 2026-08-18: of 19 open chat
 * signals, ZERO belonged to a session in `awaiting_user`/`awaiting_permission`.
 *
 * One request for the whole sidebar, shared through chat-core's 30s cache and resolve
 * announcement with every other signals surface, so answering a question anywhere
 * clears the marker here without a refetch.
 *
 * `available` is false when the server answers 404 — a bridge-server predating the
 * signals route. Callers must fall back to session state rather than concluding
 * nothing is waiting.
 *
 * A notification is not a question: it is acknowledged, never answered, and badging a
 * row for one would say work is waiting when none is.
 */
export function useSessionsWithOpenOperatorQuestion(): {
  sessionIds: ReadonlySet<string>
  available: boolean
} {
  const { operator, available } = useOpenSignalsByAttention()
  const sessionIds = useMemo(() => {
    const ids = new Set<string>()
    for (const signal of operator.signals) {
      if (signal.kind === SIGNAL_KIND_QUESTION) ids.add(signal.sessionId)
    }
    return ids
  }, [operator.signals])
  return { sessionIds, available }
}

export interface OpenSignalRequestsOfSession {
  /** The operator's question groups of this session — drawn beside its chip with a
   *  `?`. Always empty for a background session. */
  questionRequests: SignalRequest[]
  /** Every other open group of this session, listed in the chip's panel: its
   *  notifications, and all of a background session's groups. */
  panelRequests: SignalRequest[]
  /** False when this bridge-server has no signals route (404). */
  available: boolean
  /** The cross-session read failed; nothing here says whether a question waits. */
  error: string | null
  /** Re-read the cross-session list. */
  reload: () => void
}

/**
 * One session's open signals, taken from the SAME cross-session read as
 * {@link useSessionsWithOpenOperatorQuestion} rather than from a read of its own, and
 * split the same way — see `sessionChipSignalShares`.
 *
 * Forty chips in a transcript cost the one `/signals` request the sidebar already
 * makes, and a resolve anywhere in the tab re-reads it for all of them at once.
 */
export function useOpenSignalRequestsOfSession(sessionId: string): OpenSignalRequestsOfSession {
  const { operator, background, available, error, reload } = useOpenSignalsByAttention()
  const shares = useMemo(
    () => sessionChipSignalShares(operator.requests, background.requests, sessionId),
    [operator.requests, background.requests, sessionId],
  )
  return { ...shares, available, error, reload }
}
