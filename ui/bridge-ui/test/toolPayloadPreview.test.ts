import { describe, expect, it } from 'vitest'
import { capText, formatBytes, shortenedPayloadLabel, DOM_TEXT_LIMIT } from '../src/toolPayloadPreview'

describe('capText', () => {
  it('passes short text through', () => {
    expect(capText('hello')).toEqual({ shown: 'hello', hiddenCharacters: 0 })
  })
  it('holds back what is over the limit and says how much', () => {
    const text = 'x'.repeat(DOM_TEXT_LIMIT + 123)
    const capped = capText(text)
    expect(capped.shown).toHaveLength(DOM_TEXT_LIMIT)
    expect(capped.hiddenCharacters).toBe(123)
  })
})

describe('formatBytes', () => {
  it('reads like a person would say it', () => {
    expect(formatBytes(900)).toBe('900 B')
    expect(formatBytes(2048)).toBe('2.0 KB')
    expect(formatBytes(586_317)).toBe('586 KB')
    expect(formatBytes(1_274_343)).toBe('1.3 MB')
  })
})

describe('shortenedPayloadLabel', () => {
  it('is null for an entry that carries its whole payload', () => {
    expect(shortenedPayloadLabel({})).toBeNull()
    expect(shortenedPayloadLabel({ toolResultBytes: 50 })).toBeNull()
  })
  it('names what was shortened and its full size', () => {
    expect(shortenedPayloadLabel({ toolResultTruncated: true, toolResultBytes: 586_317 })).toBe(
      'Preview only — full output 586 KB',
    )
    expect(
      shortenedPayloadLabel({ toolInputTruncated: true, toolInputBytes: 9000, toolResultTruncated: true }),
    ).toBe('Preview only — full input 9.0 KB, output (size unknown)')
  })
})
