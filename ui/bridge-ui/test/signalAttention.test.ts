import { describe, expect, it } from 'vitest'
import {
  groupSignalsByRequest,
  signalFromWire,
  splitSignalsByAttention,
  type Signal,
  type SignalAttentionGroup,
  type SignalWire,
} from '@kayushkin/chat-core'
import {
  backgroundSessionTypeLabel,
  sessionTypeOfRequest,
  signalSection,
} from '../src/signalAttentionSections'
import { sessionChipSignalShares } from '../src/sessionOpenQuestions'

// The inbox, the signals page and every session chip split the open signals the same
// way — chat-core's `splitSignalsByAttention` — and these pin what each draws from the
// two groups: what counts as needing the operator, and where a background agent's
// questions and notifications go instead.

function signal(overrides: Partial<SignalWire>): Signal {
  return signalFromWire({
    id: 'sig-1',
    session_id: 'br_a',
    session_type: 'interactive',
    kind: 'question',
    source: 'tool',
    surface: 'chat',
    title: 'Which database?',
    state: 'open',
    created_at: '2026-10-19T10:00:00Z',
    ...overrides,
  })
}

const openSignals: Signal[] = [
  // Newest first, as the server lists them.
  signal({ id: 'auto-note-1', session_id: 'autoworker-1', session_type: 'autonomous', surface: 'kanban', kind: 'notification' }),
  signal({ id: 'chat-note', session_id: 'br_chat', kind: 'notification' }),
  signal({ id: 'auto-q', session_id: 'autoworker-1', session_type: 'autonomous', surface: 'kanban', request_id: 'req-auto' }),
  signal({ id: 'chat-q1', session_id: 'br_chat', request_id: 'req-chat' }),
  signal({ id: 'chat-q2', session_id: 'br_chat', request_id: 'req-chat' }),
  signal({ id: 'herald-q', session_id: 'herald-1', session_type: 'herald', request_id: 'req-herald' }),
  signal({ id: 'gone-q', session_id: 'br_gone', session_type: undefined, request_id: 'req-gone' }),
  signal({ id: 'sub-note', session_id: 'br_sub', session_type: 'system', kind: 'notification' }),
]

function groups(): { operator: SignalAttentionGroup; background: SignalAttentionGroup } {
  const { operatorSignals, backgroundSignals } = splitSignalsByAttention(openSignals, () => undefined)
  return {
    operator: { signals: operatorSignals, requests: groupSignalsByRequest(operatorSignals) },
    background: { signals: backgroundSignals, requests: groupSignalsByRequest(backgroundSignals) },
  }
}

describe('signalSection — Needs you', () => {
  it('counts only interactive and herald signals, questions first', () => {
    const needsYou = signalSection(groups().operator)
    // chat-q1 and chat-q2 are one request; the count is of signals, not groups.
    expect(needsYou.signalCount).toBe(4)
    expect(needsYou.requests.map(request => request.signals[0]!.id)).toEqual(['chat-q1', 'herald-q', 'chat-note'])
    expect(needsYou.questionRequests.map(request => request.requestId)).toEqual(['req-chat', 'req-herald'])
    expect(needsYou.notificationRequests.map(request => request.signals[0]!.id)).toEqual(['chat-note'])
  })
})

describe('signalSection — Background agents', () => {
  it('holds every other signal, including an unknown type, with its own count', () => {
    const background = signalSection(groups().background)
    expect(background.signalCount).toBe(4)
    expect(background.questionRequests.map(request => request.signals[0]!.id)).toEqual(['auto-q', 'gone-q'])
    expect(background.notificationRequests.map(request => request.signals[0]!.id)).toEqual(['auto-note-1', 'sub-note'])
  })

  it('together with Needs you covers every open signal exactly once', () => {
    const { operator, background } = groups()
    const ids = [...operator.signals, ...background.signals].map(s => s.id).sort()
    expect(ids).toEqual(openSignals.map(s => s.id).sort())
  })

  it('names an unknown session type as unknown, never a guessed one', () => {
    const background = signalSection(groups().background)
    const gone = background.requests.find(request => request.sessionId === 'br_gone')!
    const typeOf = (s: Signal): string => s.sessionType
    expect(backgroundSessionTypeLabel(sessionTypeOfRequest(gone, typeOf))).toBe('unknown type')
    const auto = background.requests.find(request => request.sessionId === 'autoworker-1')!
    expect(backgroundSessionTypeLabel(sessionTypeOfRequest(auto, typeOf))).toBe('autonomous')
  })
})

describe('sessionChipSignalShares', () => {
  it('gives an interactive session’s questions the chip’s ? and its notifications to the panel', () => {
    const { operator, background } = groups()
    const shares = sessionChipSignalShares(operator.requests, background.requests, 'br_chat')
    expect(shares.questionRequests.map(request => request.requestId)).toEqual(['req-chat'])
    expect(shares.panelRequests.map(request => request.signals[0]!.id)).toEqual(['chat-note'])
  })

  it('never gives a background session’s question the ? — it goes in the panel', () => {
    const { operator, background } = groups()
    const shares = sessionChipSignalShares(operator.requests, background.requests, 'autoworker-1')
    expect(shares.questionRequests).toEqual([])
    expect(shares.panelRequests.map(request => request.signals[0]!.id)).toEqual(['auto-note-1', 'auto-q'])
  })
})
