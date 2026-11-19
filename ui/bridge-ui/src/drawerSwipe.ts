/**
 * Whether a one-finger swipe on a phone opens or closes the chat's session
 * drawer. A swipe to the right from anywhere on the page opens it; a swipe to
 * the left while it is open closes it. `SessionDrawer` reads the touch points
 * and asks this module what they mean.
 */

/** How far a finger must travel sideways to count as a swipe. */
export const DRAWER_SWIPE_MIN_PX = 60

/** How many times further sideways than up or down the finger must travel, so a
 *  scroll that drifts sideways does not open the drawer. */
export const DRAWER_SWIPE_SIDEWAYS_RATIO = 1.5

export interface TouchPoint {
  x: number
  y: number
}

export type DrawerSwipeAction = 'open' | 'close' | null

export function drawerSwipeAction(start: TouchPoint, end: TouchPoint, drawerOpen: boolean): DrawerSwipeAction {
  const dx = end.x - start.x
  const dy = end.y - start.y
  if (Math.abs(dx) < DRAWER_SWIPE_MIN_PX) return null
  if (Math.abs(dx) < Math.abs(dy) * DRAWER_SWIPE_SIDEWAYS_RATIO) return null
  if (dx > 0 && !drawerOpen) return 'open'
  if (dx < 0 && drawerOpen) return 'close'
  return null
}

/**
 * The parts of an element the swipe rule reads, so the rule can be tested
 * without a DOM. `scrollLeft` above zero means a swipe to the right would scroll
 * the element back, so that swipe belongs to it and not to the drawer.
 */
export interface SwipeTargetElement {
  tagName: string
  isContentEditable: boolean
  scrollLeft: number
  getAttribute(name: string): string | null
  parentElement: SwipeTargetElement | null
}

/**
 * Whether a touch that started on `target` may open or close the drawer. It may
 * not when it started in a text field (a drag there moves the cursor or selects
 * text), in an element scrolled sideways (the drag scrolls it), or in another
 * modal dialog such as the image viewer, whose own swipe steps between images.
 */
export function touchMayMoveDrawer(target: SwipeTargetElement | null, drawerElement: SwipeTargetElement | null): boolean {
  for (let element = target; element; element = element.parentElement) {
    if (element === drawerElement) return true
    const tag = element.tagName.toLowerCase()
    if (tag === 'input' || tag === 'textarea' || tag === 'select' || element.isContentEditable) return false
    if (element.scrollLeft > 0) return false
    if (element.getAttribute('aria-modal') === 'true') return false
  }
  return true
}
