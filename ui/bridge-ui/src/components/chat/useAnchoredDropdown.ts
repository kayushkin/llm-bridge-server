import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { RefObject } from 'react'

/** Gap in px between the anchor and its panel, and the margin the panel keeps from the
 *  viewport edge when it has to be nudged back inside. Same numbers chat-core's
 *  `useAnchoredPanel` uses (RefChip.tsx), so a dropdown opened from the sidebar and one
 *  opened from a reference chip sit the same distance off their anchors. */
const PANEL_GAP = 4
const VIEWPORT_MARGIN = 8

/** Height/width assumed for the FIRST placement pass, before the panel exists to be
 *  measured. Only has to be close enough that the panel does not visibly jump once the
 *  real measurement lands — and it never paints at the guessed position anyway, because
 *  the caller hides the panel until `panelStyle` is non-null. */
const ASSUMED_PANEL_HEIGHT = 220
const ASSUMED_PANEL_WIDTH = 320

export interface AnchoredDropdown<A extends HTMLElement> {
  /** Put this on the element the panel should hang off. */
  anchorRef: RefObject<A | null>
  /** Put this on the portaled panel; it is what makes a click inside the panel count as
   *  "inside" rather than dismissing it, and what gets measured for the flip. */
  panelRef: RefObject<HTMLDivElement | null>
  /** Viewport coordinates for the `position: fixed` panel. Null until measured — render
   *  the panel hidden rather than at (0,0), or it flashes in the corner. */
  panelStyle: { top: number; left: number } | null
}

/**
 * Placement and dismissal for a dropdown that is PORTALED onto document.body.
 *
 * Ported from chat-core's `useAnchoredPanel` (`src/react/RefChip.tsx`), and portaled for
 * the same two reasons that hook documents, both of which apply verbatim to the session
 * list: the list is an inner scroller with `overflow` that would clip a panel opened on
 * the last visible row, and its rows are virtua list items whose wrapper carries
 * `contain: layout style` — a stacking context, inside which no z-index can lift a panel
 * above the rows painted after it. An inline panel is therefore both clipped and
 * unclickable; escaping to the body fixes both.
 *
 * The cost of the portal is that placement becomes manual, so the panel is anchored to
 * the anchor's measured rect and re-measured on scroll and resize. `capture: true` on the
 * scroll listener is required — the session list scrolls internally and its scroll events
 * never reach window on the bubble phase.
 *
 * `open` is a PARAMETER rather than state owned here: the sidebar allows one dropdown at a
 * time, and that rule can only be enforced by whoever knows about all of them. Dismissal
 * (Escape, click-outside) is reported through `onDismiss` for the same reason.
 */
export function useAnchoredDropdown<A extends HTMLElement>(
  open: boolean,
  onDismiss: () => void,
): AnchoredDropdown<A> {
  const [panelStyle, setPanelStyle] = useState<{ top: number; left: number } | null>(null)
  const anchorRef = useRef<A | null>(null)
  const panelRef = useRef<HTMLDivElement | null>(null)

  useLayoutEffect(() => {
    // Closed: leave the last measurement in place rather than clearing it. The panel is
    // unmounted anyway, and on the next open this effect re-measures in the SAME commit —
    // a layout effect runs before paint — so the stale coordinates are never painted.
    // Clearing here would be a setState in an effect body that buys nothing.
    if (!open) return
    const place = (): void => {
      const anchor = anchorRef.current
      if (!anchor) return
      const rect = anchor.getBoundingClientRect()
      const panelHeight = panelRef.current?.offsetHeight ?? ASSUMED_PANEL_HEIGHT
      const panelWidth = panelRef.current?.offsetWidth ?? ASSUMED_PANEL_WIDTH

      // Below the anchor, unless that would run off the bottom and there is more room
      // above — the usual dropdown flip. A `?` on the last row of a full-height sidebar
      // is the common case, so the flip is not an edge case here.
      const roomBelow = window.innerHeight - rect.bottom
      const roomAbove = rect.top
      const placeAbove = roomBelow < panelHeight + VIEWPORT_MARGIN && roomAbove > roomBelow
      const rawTop = placeAbove ? rect.top - panelHeight - PANEL_GAP : rect.bottom + PANEL_GAP

      const maxTop = window.innerHeight - panelHeight - VIEWPORT_MARGIN
      const maxLeft = window.innerWidth - panelWidth - VIEWPORT_MARGIN
      setPanelStyle({
        top: Math.max(VIEWPORT_MARGIN, Math.min(rawTop, Math.max(VIEWPORT_MARGIN, maxTop))),
        left: Math.max(VIEWPORT_MARGIN, Math.min(rect.left, Math.max(VIEWPORT_MARGIN, maxLeft))),
      })
    }

    place()
    window.addEventListener('scroll', place, true)
    window.addEventListener('resize', place)
    return () => {
      window.removeEventListener('scroll', place, true)
      window.removeEventListener('resize', place)
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const onDocPointerDown = (e: MouseEvent): void => {
      const target = e.target as Node
      // The panel is not a descendant of the anchor once portaled, so "outside" has to
      // clear BOTH — otherwise every click on an option inside the panel would close it
      // before the answer could be submitted.
      const insideAnchor = anchorRef.current?.contains(target) ?? false
      const insidePanel = panelRef.current?.contains(target) ?? false
      if (!insideAnchor && !insidePanel) onDismiss()
    }
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onDismiss()
    }
    // `mousedown`, not `click`: it fires before the click that opens ANOTHER marker's
    // dropdown, so the first is already dismissed by the time the second toggles itself
    // open — which is what keeps "one at a time" true without the markers knowing about
    // each other.
    document.addEventListener('mousedown', onDocPointerDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocPointerDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open, onDismiss])

  return { anchorRef, panelRef, panelStyle }
}
