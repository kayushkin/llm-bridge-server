import { describe, expect, it } from 'vitest'
import type { SessionBrief, Signal, SignalRequest } from '@kayushkin/chat-core'
import { chatBriefSections, type OrchestratorRemark } from '../src/chatBriefSections'

function brief(sessionId: string, openLooseEndIds: string[] = []): SessionBrief {
  return {
    sessionId,
    overallGoal: `goal of ${sessionId}`,
    currentStep: '',
    revisionCount: 1,
    createdAt: '2026-11-02T18:00:00Z',
    updatedAt: '2026-11-02T18:00:00Z',
    revisions: [],
    looseEnds: openLooseEndIds.map(id => ({
      id, sessionId, kind: 'question', text: id, context: '', state: 'open', resolution: '',
      createdAt: '2026-11-02T18:00:00Z', resolvedAt: null,
    })),
  }
}

function request(sessionId: string, signalCount = 1): SignalRequest {
  return { sessionId, requestId: '', signals: Array.from({ length: signalCount }, () => ({ sessionId }) as Signal) } as SignalRequest
}

const remark = (sessionId: string): OrchestratorRemark => ({ id: `remark_${sessionId}`, session_id: sessionId, text: 't', created_at: '' })

describe('chatBriefSections', () => {
  it('puts a chat with anything open in waiting and draws its signals only in its card', () => {
    const sections = chatBriefSections(
      [brief('br_loose', ['loose_1']), brief('br_quiet'), brief('br_signal'), brief('br_remark')],
      [request('br_signal', 2), request('br_no_brief')],
      [remark('br_remark')],
    )
    expect(sections.waiting.map(card => [card.brief.sessionId, card.waitingCount])).toEqual([
      ['br_loose', 1],
      ['br_signal', 2],
      ['br_remark', 1],
    ])
    expect(sections.quiet.map(card => card.brief.sessionId)).toEqual(['br_quiet'])
    expect(sections.requestsOutsideCards.map(r => r.sessionId)).toEqual(['br_no_brief'])
  })
})
