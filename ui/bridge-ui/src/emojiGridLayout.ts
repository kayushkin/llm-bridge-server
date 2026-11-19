// Where each row of an emoji grid sits, so a picker draws only the rows in
// view instead of ~2,000 buttons. A section is a heading row and then its
// emoji, `columns` to a row. Covered by test/emojiGridLayout.test.ts.

export type EmojiGridRow =
  | { kind: 'heading'; sectionIndex: number; top: number; height: number }
  | { kind: 'emoji'; sectionIndex: number; top: number; height: number; start: number; end: number }

export interface EmojiGridLayout {
  rows: EmojiGridRow[]
  /** Where each section's heading row starts, for jumping to it. */
  sectionTops: number[]
  height: number
}

/** The rows for sections of the given sizes. An empty section gets no rows. */
export function emojiGridLayout(sectionSizes: readonly number[], columns: number, emojiRowHeight: number,
  headingHeight: number): EmojiGridLayout {
  const perRow = Math.max(1, Math.floor(columns))
  const rows: EmojiGridRow[] = []
  const sectionTops: number[] = []
  let top = 0
  sectionSizes.forEach((size, sectionIndex) => {
    sectionTops.push(top)
    if (size === 0) return
    rows.push({ kind: 'heading', sectionIndex, top, height: headingHeight })
    top += headingHeight
    for (let start = 0; start < size; start += perRow) {
      rows.push({ kind: 'emoji', sectionIndex, top, height: emojiRowHeight, start, end: Math.min(size, start + perRow) })
      top += emojiRowHeight
    }
  })
  return { rows, sectionTops, height: top }
}

/** The rows any part of which is within `overscan` pixels of the view. Rows
 *  are in order of `top`, so this finds the first by bisection. */
export function rowsInView(rows: readonly EmojiGridRow[], scrollTop: number, viewportHeight: number,
  overscan: number): EmojiGridRow[] {
  const from = scrollTop - overscan
  const to = scrollTop + viewportHeight + overscan
  let low = 0
  let high = rows.length
  while (low < high) {
    const middle = (low + high) >> 1
    if (rows[middle].top + rows[middle].height <= from) low = middle + 1
    else high = middle
  }
  const shown: EmojiGridRow[] = []
  for (let i = low; i < rows.length && rows[i].top < to; i++) shown.push(rows[i])
  return shown
}

/** The section the view's top edge is in, for marking it in the jump bar. */
export function sectionAt(sectionTops: readonly number[], scrollTop: number): number {
  let current = 0
  sectionTops.forEach((top, index) => { if (top <= scrollTop + 1) current = index })
  return current
}
