import { openLooseEnds, type SessionBrief, type SignalRequest } from '@kayushkin/chat-core'

// How the signals page joins each chat's brief (llm-bridge-server), the
// orchestrator's remarks on it (orchestratord) and its open signals into one
// card per chat. The join is on the session id, the one id all three share.

/** One remark the orchestrator left on a session, as orchestratord answers
 *  GET /remarks. */
export interface OrchestratorRemark {
  id: string
  session_id: string
  text: string
  created_at: string
}

/** One chat's card: its brief, and what of it waits on the person. */
export interface ChatBriefCard {
  brief: SessionBrief
  /** The operator's open signal groups raised by this chat. */
  requests: SignalRequest[]
  remarks: OrchestratorRemark[]
  /** Open loose ends + remarks + signals in these groups. */
  waitingCount: number
}

export interface ChatBriefSections {
  /** Chats with something open on them, the most recently updated first. */
  waiting: ChatBriefCard[]
  /** Chats with nothing open, most recently updated first. */
  quiet: ChatBriefCard[]
  /** The operator's signal groups not drawn in a card: raised by a session
   *  with no brief. Same order as given. */
  requestsOutsideCards: SignalRequest[]
}

/**
 * Splits the briefs into chats waiting on the person and quiet ones, and takes
 * each waiting chat's signal groups into its card. A group is drawn exactly
 * once: in its chat's card, or in the page's own lists when its session has no
 * brief. Briefs come most recently updated first and keep that order.
 */
export function chatBriefSections(
  briefs: readonly SessionBrief[],
  operatorRequests: readonly SignalRequest[],
  remarks: readonly OrchestratorRemark[],
): ChatBriefSections {
  const requestsBySession = new Map<string, SignalRequest[]>()
  for (const request of operatorRequests) {
    const list = requestsBySession.get(request.sessionId) ?? []
    list.push(request)
    requestsBySession.set(request.sessionId, list)
  }
  const remarksBySession = new Map<string, OrchestratorRemark[]>()
  for (const remark of remarks) {
    const list = remarksBySession.get(remark.session_id) ?? []
    list.push(remark)
    remarksBySession.set(remark.session_id, list)
  }

  const waiting: ChatBriefCard[] = []
  const quiet: ChatBriefCard[] = []
  const drawnInCards = new Set<string>()
  for (const brief of briefs) {
    const requests = requestsBySession.get(brief.sessionId) ?? []
    const cardRemarks = remarksBySession.get(brief.sessionId) ?? []
    const signalCount = requests.reduce((sum, request) => sum + request.signals.length, 0)
    const waitingCount = openLooseEnds(brief).length + cardRemarks.length + signalCount
    const card: ChatBriefCard = { brief, requests, remarks: cardRemarks, waitingCount }
    drawnInCards.add(brief.sessionId)
    if (waitingCount > 0) waiting.push(card)
    else quiet.push(card)
  }
  return {
    waiting,
    quiet,
    requestsOutsideCards: operatorRequests.filter(request => !drawnInCards.has(request.sessionId)),
  }
}
