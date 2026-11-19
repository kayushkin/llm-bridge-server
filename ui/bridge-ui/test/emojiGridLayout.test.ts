import { describe, expect, it } from 'vitest'
import { emojiGridLayout, rowsInView, sectionAt } from '../src/emojiGridLayout'

describe('the emoji grid layout', () => {
  const layout = emojiGridLayout([3, 0, 9], 4, 36, 24)

  it('puts a heading and then rows of `columns` emoji per section, and skips an empty one', () => {
    expect(layout.rows.map(r => (r.kind === 'heading' ? `h${r.sectionIndex}@${r.top}` : `${r.start}-${r.end}@${r.top}`))).toEqual([
      'h0@0', '0-3@24', 'h2@60', '0-4@84', '4-8@120', '8-9@156',
    ])
    expect(layout.sectionTops).toEqual([0, 60, 60])
    expect(layout.height).toBe(192)
  })

  it('draws only the rows near the view', () => {
    const shown = rowsInView(layout.rows, 100, 30, 0)
    expect(shown.map(r => r.top)).toEqual([84, 120])
    expect(rowsInView(layout.rows, 0, 1000, 0)).toHaveLength(6)
    expect(rowsInView(layout.rows, 500, 100, 0)).toEqual([])
  })

  it('knows which section the view starts in', () => {
    expect(sectionAt(layout.sectionTops, 0)).toBe(0)
    expect(sectionAt(layout.sectionTops, 70)).toBe(2)
  })

  it('never makes a row of zero columns', () => {
    expect(emojiGridLayout([2], 0, 36, 24).rows).toHaveLength(3)
  })
})
