import { SIGNAL_KIND_QUESTION, type Signal, type SignalAttentionGroup, type SignalRequest } from '@kayushkin/chat-core'

// What the "Needs you" inbox and the signals page show, from chat-core's split of
// the open signals into the operator's and background (`useOpenSignalsByAttention`).
// The split itself — which session types are the operator's — is chat-core's and
// is not restated here; this only orders and counts each group for display.

/** Whether a request group asks at least one question. */
export function requestHoldsQuestion(request: SignalRequest): boolean {
  return request.signals.some(signal => signal.kind === SIGNAL_KIND_QUESTION)
}

/** One group as the inbox draws it. */
export interface SignalSection {
  /** Question groups first, then notification groups, each in the server's order. A
   *  question blocks somebody and a notification only reports, and notifications
   *  outnumber questions here, so interleaving them by recency would bury the
   *  questions. Sorted, not filtered: an unacknowledged notification still waits. */
  requests: SignalRequest[]
  /** Question groups only, in the same order. */
  questionRequests: SignalRequest[]
  /** Notification-only groups, in the same order. */
  notificationRequests: SignalRequest[]
  /** How many SIGNALS the group holds — the count shown beside its title. Signals,
   *  not request groups: one AskUserQuestion call can carry several questions, and a
   *  "3" beside a panel holding 5 questions reads as a miscount. */
  signalCount: number
}

export function signalSection(group: SignalAttentionGroup): SignalSection {
  const questionRequests = group.requests.filter(requestHoldsQuestion)
  const notificationRequests = group.requests.filter(request => !requestHoldsQuestion(request))
  return {
    requests: [...questionRequests, ...notificationRequests],
    questionRequests,
    notificationRequests,
    signalCount: group.signals.length,
  }
}

/** The words for a background card's session type. An empty type is said to be
 *  unknown rather than given a guessed one; chat-core already filed it as
 *  background for the same reason. */
export function backgroundSessionTypeLabel(sessionType: string): string {
  return sessionType === '' ? 'unknown type' : sessionType
}

/** The session type of a request group, read off its first signal: every signal in
 *  a group comes from one session. */
export function sessionTypeOfRequest(
  request: SignalRequest,
  sessionTypeOf: (signal: Signal) => string,
): string {
  const [first] = request.signals
  return first ? sessionTypeOf(first) : ''
}
