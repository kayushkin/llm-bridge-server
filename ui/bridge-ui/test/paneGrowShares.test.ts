import { describe, expect, it } from 'vitest'
import {
  DEFAULT_PANE_SIZES,
  VISIBLE_PANES_GROW_TOTAL,
  growSharesOfVisiblePanes,
  type PaneKey,
} from '../src/components/chat/panePersistence'

const sumOf = (shares: Partial<Record<PaneKey, number>>) =>
  Object.values(shares).reduce((sum, share) => sum + share, 0)

describe('growSharesOfVisiblePanes', () => {
  it('gives a lone pane the whole row when its stored size is below 1', () => {
    // What a drag writes: the pair's total of 2 is conserved, so Turns dragged narrow
    // lands below 1. Handed to CSS as it is, 0.4 claims 40% of the free space and no more.
    const shares = growSharesOfVisiblePanes({ ...DEFAULT_PANE_SIZES, turns: 0.4, timeline: 1.6 }, ['turns'])
    expect(shares).toEqual({ turns: VISIBLE_PANES_GROW_TOTAL })
  })

  it('keeps the stored ratio between the visible panes', () => {
    const shares = growSharesOfVisiblePanes({ ...DEFAULT_PANE_SIZES, turns: 0.4, timeline: 1.6 }, ['turns', 'timeline'])
    expect(shares.timeline! / shares.turns!).toBeCloseTo(4)
    expect(sumOf(shares)).toBeCloseTo(VISIBLE_PANES_GROW_TOTAL)
  })

  it('fills the row when two survivors of several drags add up to less than 1', () => {
    const shares = growSharesOfVisiblePanes({ ...DEFAULT_PANE_SIZES, turns: 0.3, kanban: 0.5, timeline: 2.2 }, ['turns', 'kanban'])
    expect(sumOf(shares)).toBeCloseTo(VISIBLE_PANES_GROW_TOTAL)
    expect(shares.timeline).toBeUndefined()
  })
})
