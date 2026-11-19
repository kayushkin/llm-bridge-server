import { describe, expect, it } from 'vitest'
import { sentBubblePlacement, sentBubbleText } from '../src/composerSentBubble'

describe('sentBubblePlacement', () => {
  it('places the bubble over the text box, relative to the composer wrapper', () => {
    expect(sentBubblePlacement({ left: 330.4, top: 834, width: 837, height: 46.2 }, { left: 264, top: 824 })).toEqual({
      '--sent-left': '66px',
      '--sent-top': '10px',
      '--sent-width': '837px',
      '--sent-height': '46px',
    })
  })
})

describe('sentBubbleText', () => {
  it('says the typed text, trimmed', () => {
    expect(sentBubbleText('  hello crab \n', ['a.png'])).toBe('hello crab')
  })
  it('names the files when only files were sent', () => {
    expect(sentBubbleText('  ', ['a.png', 'notes.txt'])).toBe('a.png, notes.txt')
  })
})
