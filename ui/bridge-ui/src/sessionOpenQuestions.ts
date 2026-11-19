import { SIGNAL_KIND_QUESTION, type SignalRequest } from '@kayushkin/chat-core'

// What a session chip shows about the signals open against its session.
//
// Every chip reads the same cross-session list (`useOpenSignals()` with no session
// id), so these rules pick one session's share out of it. The server puts no cap on
// that list unless asked for one, so the share is the session's whole open set, not
// a page of it.

/** A request group belonging to `sessionId` that asks at least one question.
 *
 *  The whole group is kept once it qualifies, notifications included: one tool call
 *  resolves as one group, so dropping a notification out of a mixed group would ask
 *  half a question. */
export function questionRequestsOfSession(
  requests: readonly SignalRequest[],
  sessionId: string,
): SignalRequest[] {
  return requests.filter(
    (request) =>
      request.sessionId === sessionId &&
      request.signals.some((signal) => signal.kind === SIGNAL_KIND_QUESTION),
  )
}

/** The rest of the session's open groups: notifications, which are acknowledged
 *  rather than answered and so are left to the chip's panel. Together with
 *  {@link questionRequestsOfSession} this covers every open group of the session
 *  exactly once, so no signal is drawn twice and none is left out. */
export function requestsOfSessionWithoutQuestion(
  requests: readonly SignalRequest[],
  sessionId: string,
): SignalRequest[] {
  return requests.filter(
    (request) =>
      request.sessionId === sessionId &&
      !request.signals.some((signal) => signal.kind === SIGNAL_KIND_QUESTION),
  )
}

/** A session chip's share of the open signals, split by chat-core's attention rule.
 *
 *  Only the operator's question groups get the chip's `?` and the Answer button
 *  beside it. Everything else of the session — its notifications, and every group
 *  of a background session, questions included — goes in the chip's panel, which
 *  opens only on a click. So an autoworker's question stays answerable from its chip
 *  without the chip claiming it needs the operator. Each group lands in exactly one
 *  of the two. */
export interface SessionChipSignalShares {
  /** Drawn beside the chip with a `?`: the operator's question groups. */
  questionRequests: SignalRequest[]
  /** Listed in the chip's panel. */
  panelRequests: SignalRequest[]
}

export function sessionChipSignalShares(
  operatorRequests: readonly SignalRequest[],
  backgroundRequests: readonly SignalRequest[],
  sessionId: string,
): SessionChipSignalShares {
  return {
    questionRequests: questionRequestsOfSession(operatorRequests, sessionId),
    panelRequests: [
      ...requestsOfSessionWithoutQuestion(operatorRequests, sessionId),
      ...backgroundRequests.filter(request => request.sessionId === sessionId),
    ],
  }
}

/** What a chip says about its session's questions before anyone clicks: the first
 *  question's text, and how many questions there are in all. */
export interface OpenQuestionPreview {
  firstQuestionText: string
  questionCount: number
}

/** The preview for a session's question groups, or null when there is no question.
 *
 *  The first question is the first one the server listed, which is newest first.
 *  Its title is the question as the session asked it. */
export function openQuestionPreview(questionRequests: readonly SignalRequest[]): OpenQuestionPreview | null {
  const questions = questionRequests.flatMap((request) =>
    request.signals.filter((signal) => signal.kind === SIGNAL_KIND_QUESTION),
  )
  const [first] = questions
  if (!first) return null
  return {
    firstQuestionText: first.title.trim(),
    questionCount: questions.length,
  }
}
