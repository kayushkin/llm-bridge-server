import { describe, expect, it } from 'vitest'
import { answerTextOf, signalFromWire, type Signal } from '@kayushkin/chat-core'
import { answersWithComposerText } from '../src/signalComposerText'

function question(id: string, options: string[] = ['x', 'y']): Signal {
  return signalFromWire({
    options: options.map((value) => ({ label: value, value })),
    id,
    session_id: 'br_a',
    kind: 'question',
    source: 'tool',
    surface: 'chat',
    title: `Question ${id}`,
    state: 'open',
    created_at: '2026-08-17T10:00:00Z',
    request_id: 'req-1',
    allow_multiple_options: true,
  })
}

describe('answersWithComposerText', () => {
  it('sends the composer text with the picked options', () => {
    const one = question('sig-1')
    const composed = answersWithComposerText(
      [one],
      { 'sig-1': { pickedOptionValues: ['a.go', 'b.go'] } },
      '  but staging first ',
    )
    expect(answerTextOf(composed['sig-1'])).toBe('a.go, b.go\n\nbut staging first')
  })

  it('adds the text to the last question only', () => {
    const first = question('sig-1')
    const last = question('sig-2')
    const composed = answersWithComposerText(
      [first, last],
      { 'sig-1': { pickedOptionValues: ['x'] }, 'sig-2': { pickedOptionValues: ['y'] } },
      'note',
    )
    expect(answerTextOf(composed['sig-1'])).toBe('x')
    expect(answerTextOf(composed['sig-2'])).toBe('y\n\nnote')
  })

  it('keeps text already typed for that question first', () => {
    const one = question('sig-1')
    const composed = answersWithComposerText([one], { 'sig-1': { text: 'mine' } }, 'composer')
    expect(answerTextOf(composed['sig-1'])).toBe('mine\n\ncomposer')
  })

  it('answers an unanswered last question with the text alone', () => {
    const one = question('sig-1')
    expect(answerTextOf(answersWithComposerText([one], {}, 'typed')['sig-1'])).toBe('typed')
  })

  it('changes nothing when the composer is empty', () => {
    const one = question('sig-1')
    const answers = { 'sig-1': { pickedOptionValues: ['a'] } }
    expect(answersWithComposerText([one], answers, '   ')).toEqual(answers)
  })

  it('answers the one question without options, even when it is not the last', () => {
    const browser = question('sig-1', [])
    const selfCheck = question('sig-2')
    const composed = answersWithComposerText(
      [browser, selfCheck],
      { 'sig-2': { pickedOptionValues: ['yes'] } },
      'Windows, Chrome',
    )
    expect(answerTextOf(composed['sig-1'])).toBe('Windows, Chrome')
    expect(answerTextOf(composed['sig-2'])).toBe('yes')
  })

  it('joins the last question when more than one question has no options', () => {
    const first = question('sig-1', [])
    const last = question('sig-2', [])
    const composed = answersWithComposerText([first, last], {}, 'typed')
    expect(composed['sig-1']).toBeUndefined()
    expect(answerTextOf(composed['sig-2'])).toBe('typed')
  })
})
