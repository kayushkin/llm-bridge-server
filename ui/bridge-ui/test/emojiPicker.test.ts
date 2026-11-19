import { describe, expect, it } from 'vitest'
import { EMOJI_GROUPS } from '../src/emojiData'
import { insertAtSelection } from '../src/emojiPicker'

describe('the emoji table', () => {
  it('has Unicode’s groups with names', () => {
    expect(EMOJI_GROUPS.map(g => g.name)).toContain('Smileys & Emotion')
    expect(EMOJI_GROUPS.flatMap(g => g.emojis).length).toBeGreaterThan(1500)
  })
})

describe('inserting', () => {
  it('puts the emoji at the cursor and moves the cursor after it', () => {
    expect(insertAtSelection('hello world', 5, 5, '\u{1F44B}')).toEqual({ text: 'hello\u{1F44B} world', cursor: 7 })
  })

  it('replaces a selection', () => {
    expect(insertAtSelection('hello world', 6, 11, '\u{1F30D}')).toEqual({ text: 'hello \u{1F30D}', cursor: 8 })
  })

  it('clamps a stale selection to the text', () => {
    expect(insertAtSelection('hi', 10, 12, '!')).toEqual({ text: 'hi!', cursor: 3 })
  })
})
