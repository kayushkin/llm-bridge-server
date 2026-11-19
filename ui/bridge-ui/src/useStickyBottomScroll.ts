import { useCallback, useEffect, useRef, useState } from 'react'
import { useBeforePaintEffect } from './useBeforePaintEffect'

export interface StickyBottomScroll<T extends HTMLElement> {
  /**
   * Put this on the scrolling element: `<div ref={attachContainer}>`.
   *
   * A callback rather than a ref object because the hook has to *react* to
   * the element being swapped, and a ref object changing its `.current`
   * re-runs nothing. Thread swaps its container on every load transition —
   * its loading and empty states render a different div — so a hook that
   * only reads `.current` keeps its scroll listener and its ResizeObserver
   * bound to a node that has left the document.
   */
  attachContainer: (node: T | null) => void
  /** The attached element, for callers that need to read its scroll metrics. */
  containerRef: React.RefObject<T | null>
  endRef: React.RefObject<HTMLDivElement | null>
  isAtBottom: boolean
  scrollToBottom: (behavior?: ScrollBehavior) => void
}

export interface StickyBottomScrollOptions {
  /**
   * Identity of the log this container is showing — the session id, for the
   * chat panes. Required, and deliberately so: a pane that scrolls a log it
   * can swap has to say which log, because everything this hook remembers
   * about where the reader is belongs to one log and to no other. A container
   * that will only ever show one thing passes a constant.
   */
  logIdentity: string
  /** Distance from the bottom, in pixels, still counted as at the bottom. */
  threshold?: number
}

export function useStickyBottomScroll<T extends HTMLElement = HTMLDivElement>(
  { logIdentity, threshold = 40 }: StickyBottomScrollOptions,
): StickyBottomScroll<T> {
  const containerRef = useRef<T | null>(null)
  const [container, setContainer] = useState<T | null>(null)
  const endRef = useRef<HTMLDivElement | null>(null)
  const [isAtBottom, setIsAtBottom] = useState(true)
  const isAtBottomRef = useRef(true)
  const programmaticUntilRef = useRef(0)
  const lastScrollTopRef = useRef(0)
  const lastScrollHeightRef = useRef(0)

  // The ref keeps the element readable from callbacks that must not re-create
  // themselves; the state makes the same swap something effects can depend on.
  const attachContainer = useCallback((node: T | null) => {
    containerRef.current = node
    setContainer(node)
  }, [])

  const setAtBottom = useCallback((v: boolean) => {
    if (isAtBottomRef.current !== v) {
      isAtBottomRef.current = v
      setIsAtBottom(v)
    }
  }, [])

  const pinNow = useCallback(() => {
    const c = containerRef.current
    if (!c) return
    const target = c.scrollHeight - c.clientHeight
    if (target < 0) return
    if (Math.abs(c.scrollTop - target) < 1) return
    programmaticUntilRef.current = performance.now() + 120
    c.scrollTop = target
  }, [])

  // User-scroll tracking. Suppress while we're driving scrollTop ourselves.
  useEffect(() => {
    const c = container
    if (!c) return
    // A swapped-in container is a different element with its own metrics; the
    // last ones belong to a node that has left the document.
    lastScrollTopRef.current = c.scrollTop
    lastScrollHeightRef.current = c.scrollHeight
    const onScroll = () => {
      const scrollTop = c.scrollTop
      const scrollHeight = c.scrollHeight
      const readerScrolledUp = scrollTop < lastScrollTopRef.current
      const contentGrew = scrollHeight > lastScrollHeightRef.current
      lastScrollTopRef.current = scrollTop
      lastScrollHeightRef.current = scrollHeight
      if (performance.now() < programmaticUntilRef.current) return
      // Content growing is not the reader scrolling away from the bottom,
      // even though the browser fires a scroll event that looks exactly like
      // one. Showing another pane reflows this one narrower and therefore
      // taller; on the largest session on this host that landed the pane
      // 416px from the bottom after the first pane opened and 969px after the
      // second, on a pane the reader had not touched. Read as a scroll it
      // dropped stickiness, and dropping stickiness is what disables the
      // resize-pin path, so the pane never found the bottom again.
      //
      // Growth cannot be told from a scroll by `scrollTop` standing still:
      // the browser anchors the visible content, so growth above the viewport
      // moves `scrollTop` by nearly the same amount (64,823 -> 96,738 in that
      // measurement). It can be told by DIRECTION. Growth only ever pushes
      // `scrollTop` down the document; a reader leaving the bottom always
      // moves it back up. So an event that grew the content without moving
      // the reader up is layout, and layout does not get to decide where the
      // reader is: pinned stays pinned, and scrolled-up stays put.
      if (contentGrew && !readerScrolledUp) {
        if (isAtBottomRef.current) pinNow()
        return
      }
      setAtBottom(scrollHeight - scrollTop - c.clientHeight <= threshold)
    }
    c.addEventListener('scroll', onScroll, { passive: true })
    // A container this hook has not seen before starts at scrollTop 0. If we
    // are in sticky-bottom mode — the default, and where a load transition
    // leaves us — snap to the bottom before evaluating the reader's position.
    // Without this, fresh DOM with scrollTop=0 and overflowing content makes
    // onScroll flip isAtBottom to false, which then disables the resize-pin
    // path for good.
    if (isAtBottomRef.current) pinNow()
    onScroll()
    return () => c.removeEventListener('scroll', onScroll)
  }, [container, threshold, setAtBottom, pinNow])

  // A different log opens at its newest end, whatever the reader was doing in
  // the last one.
  //
  // The pane is NOT remounted when the chat switches session — `logIdentity`
  // is a prop and the rows are replaced under it — so nothing here used to run
  // a second time. Scrolling up sets `isAtBottomRef` false and nothing ever
  // set it back, which left every later session inheriting "the reader is not
  // at the bottom" from a log they had stopped looking at, and the resize-pin
  // path disabled along with it.
  //
  // Keyed on the identity of the log and on nothing else. Keying it on the
  // rows would fire while the reader is scrolled up in the log they are
  // reading — every arriving event, every filter toggle — and yank the pane to
  // the bottom under them, which is a worse bug than this one.
  useBeforePaintEffect(() => {
    setAtBottom(true)
    pinNow()
  }, [logIdentity, setAtBottom, pinNow])

  // Re-pin on container resize and content size changes (streaming text,
  // image loads, expanding rows, window/pane resize). When the reader is
  // sticky, any layout change snaps back to the bottom; otherwise leave them
  // be. Re-attached whenever the container is swapped, or it would go on
  // watching a detached node and no size change would ever be seen again.
  useEffect(() => {
    const c = container
    if (!c) return

    const ro = new ResizeObserver(() => {
      if (isAtBottomRef.current) pinNow()
    })
    ro.observe(c)

    const observed = new WeakSet<Element>()
    const observeChildren = () => {
      for (const child of Array.from(c.children)) {
        if (!observed.has(child)) {
          ro.observe(child)
          observed.add(child)
        }
      }
    }
    observeChildren()

    const mo = new MutationObserver(observeChildren)
    mo.observe(c, { childList: true })

    return () => {
      ro.disconnect()
      mo.disconnect()
    }
  }, [container, pinNow])

  const scrollToBottom = useCallback((behavior: ScrollBehavior = 'smooth') => {
    const c = containerRef.current
    if (!c) return
    setAtBottom(true)
    if (behavior === 'smooth') {
      programmaticUntilRef.current = performance.now() + 800
      const end = endRef.current
      if (end) end.scrollIntoView({ behavior: 'smooth' })
      else c.scrollTo({ top: c.scrollHeight, behavior: 'smooth' })
    } else {
      pinNow()
    }
  }, [pinNow, setAtBottom])

  return { attachContainer, containerRef, endRef, isAtBottom, scrollToBottom }
}
