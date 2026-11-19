import { describe, expect, it } from 'vitest'
import { drawerSwipeAction, touchMayMoveDrawer, type SwipeTargetElement } from '../src/drawerSwipe'

describe('drawerSwipeAction', () => {
  it('opens on a swipe to the right from anywhere', () => {
    expect(drawerSwipeAction({ x: 200, y: 400 }, { x: 300, y: 410 }, false)).toBe('open')
  })

  it('closes on a swipe to the left while open', () => {
    expect(drawerSwipeAction({ x: 300, y: 400 }, { x: 150, y: 390 }, true)).toBe('close')
  })

  it('ignores a swipe to the left while closed and to the right while open', () => {
    expect(drawerSwipeAction({ x: 300, y: 400 }, { x: 150, y: 400 }, false)).toBeNull()
    expect(drawerSwipeAction({ x: 100, y: 400 }, { x: 250, y: 400 }, true)).toBeNull()
  })

  it('ignores a short drag', () => {
    expect(drawerSwipeAction({ x: 100, y: 400 }, { x: 150, y: 400 }, false)).toBeNull()
  })

  it('ignores a scroll that drifts sideways', () => {
    expect(drawerSwipeAction({ x: 100, y: 400 }, { x: 180, y: 200 }, false)).toBeNull()
  })
})

function element(fields: Partial<SwipeTargetElement> & { parentElement?: SwipeTargetElement | null }): SwipeTargetElement {
  const attributes: Record<string, string> = {}
  return {
    tagName: 'DIV',
    isContentEditable: false,
    scrollLeft: 0,
    getAttribute: name => attributes[name] ?? null,
    parentElement: null,
    ...fields,
  }
}

describe('touchMayMoveDrawer', () => {
  it('allows a touch on ordinary page content', () => {
    const page = element({})
    expect(touchMayMoveDrawer(element({ parentElement: page }), null)).toBe(true)
  })

  it('refuses a touch in a text field', () => {
    expect(touchMayMoveDrawer(element({ tagName: 'TEXTAREA' }), null)).toBe(false)
    expect(touchMayMoveDrawer(element({ isContentEditable: true }), null)).toBe(false)
  })

  it('refuses a touch inside an element scrolled sideways', () => {
    const codeBlock = element({ tagName: 'PRE', scrollLeft: 40 })
    expect(touchMayMoveDrawer(element({ parentElement: codeBlock }), null)).toBe(false)
  })

  it('allows a touch inside a sideways scroller still at its left edge', () => {
    const codeBlock = element({ tagName: 'PRE', scrollLeft: 0 })
    expect(touchMayMoveDrawer(element({ parentElement: codeBlock }), null)).toBe(true)
  })

  it('refuses a touch inside another modal dialog', () => {
    const gallery = element({ getAttribute: name => (name === 'aria-modal' ? 'true' : null) })
    expect(touchMayMoveDrawer(element({ parentElement: gallery }), null)).toBe(false)
  })

  it('allows a touch inside the drawer whatever its ancestors are', () => {
    const scrolledAncestor = element({ scrollLeft: 10 })
    const drawer = element({ tagName: 'ASIDE', parentElement: scrolledAncestor })
    expect(touchMayMoveDrawer(element({ parentElement: drawer }), drawer)).toBe(true)
  })
})
