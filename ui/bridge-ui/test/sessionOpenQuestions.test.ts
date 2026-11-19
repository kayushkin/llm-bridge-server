import { createElement, type ReactElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import {
  ApiClient,
  ChatContext,
  groupSignalsByRequest,
  signalFromWire,
  type ChatContextValue,
  type Signal,
  type SignalWire,
} from '@kayushkin/chat-core'
import {
  openQuestionPreview,
  questionRequestsOfSession,
  requestsOfSessionWithoutQuestion,
} from '../src/sessionOpenQuestions'
import { SessionChipOpenQuestions } from '../src/components/chat/SessionChipOpenQuestions'

// What a session chip shows about its session's open signals. Every chip reads one
// cross-session list, so these pin how a chip takes its own session's share of it,
// and what it draws before and after the Answer button is pressed.

function signal(overrides: Partial<SignalWire>): Signal {
  return signalFromWire({
    id: 'sig-1',
    session_id: 'br_a',
    kind: 'question',
    source: 'tool',
    surface: 'chat',
    title: 'Which database?',
    state: 'open',
    created_at: '2026-10-15T10:00:00Z',
    ...overrides,
  })
}

/** The answer cards read the ApiClient from context and nothing else; these cases
 *  never let them reach the wire. */
function inChatContext(node: ReactElement): ReactElement {
  const fetch = (async () => {
    throw new Error('no request expected')
  }) as unknown as typeof globalThis.fetch
  const api = new ApiClient({ fetch, basePath: '/api/bridge' })
  return createElement(ChatContext.Provider, { value: { api } as ChatContextValue }, node)
}

const crossSessionList = groupSignalsByRequest([
  signal({ id: 'q-a1', session_id: 'br_a', request_id: 'req-a', title: 'Which database?' }),
  signal({ id: 'q-a2', session_id: 'br_a', request_id: 'req-a', title: 'Which port?' }),
  signal({ id: 'n-a', session_id: 'br_a', kind: 'notification', title: 'Build finished' }),
  signal({ id: 'q-b', session_id: 'br_b', title: 'Deploy now?' }),
  signal({ id: 'n-c', session_id: 'br_c', kind: 'notification', title: 'Tests passed' }),
])

describe("a chip's share of the cross-session list", () => {
  it('keeps only its own session, and only groups that ask a question', () => {
    const forA = questionRequestsOfSession(crossSessionList, 'br_a')
    expect(forA.map((r) => r.signals.map((s) => s.id))).toEqual([['q-a1', 'q-a2']])
    expect(questionRequestsOfSession(crossSessionList, 'br_b').map((r) => r.sessionId)).toEqual(['br_b'])
    // A notification is not a question: a session with only notifications has
    // nothing waiting on an answer, so its chip carries no marker.
    expect(questionRequestsOfSession(crossSessionList, 'br_c')).toEqual([])
    expect(questionRequestsOfSession(crossSessionList, 'br_unknown')).toEqual([])
  })

  it('keeps a mixed group whole, notification included', () => {
    const mixed = groupSignalsByRequest([
      signal({ id: 'q', request_id: 'req-m' }),
      signal({ id: 'n', request_id: 'req-m', kind: 'notification', title: 'FYI' }),
    ])
    const [group] = questionRequestsOfSession(mixed, 'br_a')
    expect(group?.signals.map((s) => s.id)).toEqual(['q', 'n'])
    expect(requestsOfSessionWithoutQuestion(mixed, 'br_a')).toEqual([])
  })

  it('splits every open group of the session exactly once between chip and panel', () => {
    const questions = questionRequestsOfSession(crossSessionList, 'br_a')
    const rest = requestsOfSessionWithoutQuestion(crossSessionList, 'br_a')
    expect(rest.map((r) => r.signals.map((s) => s.id))).toEqual([['n-a']])
    const ofSession = crossSessionList.filter((r) => r.sessionId === 'br_a')
    expect(questions.length + rest.length).toBe(ofSession.length)
  })
})

describe('the question preview', () => {
  it('names the first question and counts all of them', () => {
    expect(openQuestionPreview(questionRequestsOfSession(crossSessionList, 'br_a'))).toEqual({
      firstQuestionText: 'Which database?',
      questionCount: 2,
    })
  })

  it('does not count a notification riding in a question group', () => {
    const mixed = groupSignalsByRequest([
      signal({ id: 'q', request_id: 'req-m', title: 'Ship it?' }),
      signal({ id: 'n', request_id: 'req-m', kind: 'notification', title: 'FYI' }),
    ])
    expect(openQuestionPreview(mixed)).toEqual({ firstQuestionText: 'Ship it?', questionCount: 1 })
  })

  it('is null when nothing asks a question', () => {
    expect(openQuestionPreview([])).toBeNull()
    expect(openQuestionPreview(requestsOfSessionWithoutQuestion(crossSessionList, 'br_c'))).toBeNull()
  })
})

describe('what the chip draws after itself', () => {
  const props = {
    sessionId: 'br_a',
    questionRequests: questionRequestsOfSession(crossSessionList, 'br_a'),
    onToggleAnswerForm: () => {},
    onResolved: () => {},
  }

  it('shows the question and an Answer button, with the form closed', () => {
    const html = renderToStaticMarkup(
      inChatContext(createElement(SessionChipOpenQuestions, { ...props, answerFormOpen: false })),
    )
    expect(html).toContain('Which database?')
    expect(html).toContain('and 1 more question')
    expect(html).toContain('aria-expanded="false"')
    expect(html).toContain('>Answer<')
    expect(html).not.toContain('signal-card')
  })

  it('draws the shared answer cards once opened', () => {
    const html = renderToStaticMarkup(
      inChatContext(createElement(SessionChipOpenQuestions, { ...props, answerFormOpen: true })),
    )
    expect(html).toContain('aria-expanded="true"')
    expect(html).toContain('ref-chip-answer-form')
    // The same cards every other surface draws, both questions of the group in one.
    expect(html).toContain('signal-card')
    expect(html).toContain('Which port?')
  })

  it('draws nothing for a session with no question', () => {
    const html = renderToStaticMarkup(
      inChatContext(
        createElement(SessionChipOpenQuestions, { ...props, questionRequests: [], answerFormOpen: true }),
      ),
    )
    expect(html).toBe('')
  })
})
