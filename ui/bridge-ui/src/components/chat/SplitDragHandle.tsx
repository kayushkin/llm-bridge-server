import { useCallback, useState } from 'react'
import {
  EVEN_SPLIT_GROW_UNITS, measureSplitDragGeometry, splitGrowUnitsAfterDrag,
} from './splitDragGeometry'
import type { SplitGrowUnits } from './splitDragGeometry'

/**
 * The one draggable boundary between two panes of a split.
 *
 * It owns the pointer handling and the body-level drag state; where the two
 * panes live and where their sizes are stored is the caller's business, passed
 * in as `resolveDraggedPair` and `commitGrowUnits`. That is the whole reason
 * this is parameterized: the outer workspace split keys its panes by index into
 * a sizes array on a layout tree, the inner view split keys them by `PaneKey`
 * into a record, and neither needs its own copy of the arithmetic.
 */
export interface DraggedSplitPair {
  /** The pane on the left of a horizontal split, or above a vertical one. */
  elementBefore: HTMLElement
  elementAfter: HTMLElement
  growUnitsBefore: number
  growUnitsAfter: number
}

export interface SplitDragHandleProps {
  /** How the split lays its children out: side by side, or stacked. */
  axis: 'horizontal' | 'vertical'
  /** Class for the handle element. The two splits are styled separately. */
  className: string
  /** Read the pair at the moment the drag starts; null if it cannot be found. */
  resolveDraggedPair: () => DraggedSplitPair | null
  /** Write new sizes back wherever the caller keeps them. Called per move. */
  commitGrowUnits: (growUnits: SplitGrowUnits) => void
}

export function SplitDragHandle({
  axis,
  className,
  resolveDraggedPair,
  commitGrowUnits,
}: SplitDragHandleProps) {
  const [dragging, setDragging] = useState(false)
  const laidOutSideBySide = axis === 'horizontal'

  const onPointerDown = useCallback((event: React.PointerEvent<HTMLDivElement>) => {
    event.preventDefault()
    const pair = resolveDraggedPair()
    if (!pair) return

    const startPointerPixels = laidOutSideBySide ? event.clientX : event.clientY
    const rectBefore = pair.elementBefore.getBoundingClientRect()
    const rectAfter = pair.elementAfter.getBoundingClientRect()
    const pairExtentPixels = laidOutSideBySide
      ? rectBefore.width + rectAfter.width
      : rectBefore.height + rectAfter.height
    const geometry = measureSplitDragGeometry(
      pairExtentPixels, pair.growUnitsBefore, pair.growUnitsAfter,
    )
    if (!geometry) return

    setDragging(true)
    document.body.style.cursor = laidOutSideBySide ? 'col-resize' : 'row-resize'
    document.body.style.userSelect = 'none'

    const onPointerMove = (moveEvent: PointerEvent) => {
      const pointerPixels = laidOutSideBySide ? moveEvent.clientX : moveEvent.clientY
      commitGrowUnits(splitGrowUnitsAfterDrag(geometry, pointerPixels - startPointerPixels))
    }
    const onPointerRelease = () => {
      window.removeEventListener('pointermove', onPointerMove)
      window.removeEventListener('pointerup', onPointerRelease)
      window.removeEventListener('pointercancel', onPointerRelease)
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      setDragging(false)
    }
    window.addEventListener('pointermove', onPointerMove)
    window.addEventListener('pointerup', onPointerRelease)
    window.addEventListener('pointercancel', onPointerRelease)
  }, [laidOutSideBySide, resolveDraggedPair, commitGrowUnits])

  const onDoubleClick = useCallback(() => {
    commitGrowUnits(EVEN_SPLIT_GROW_UNITS)
  }, [commitGrowUnits])

  return (
    <div
      className={`${className}${dragging ? ' is-dragging' : ''}`}
      onPointerDown={onPointerDown}
      onDoubleClick={onDoubleClick}
      role="separator"
      // A separator between side-by-side panes is itself a vertical line.
      aria-orientation={laidOutSideBySide ? 'vertical' : 'horizontal'}
      title="Drag to resize — double-click to reset"
    />
  )
}
