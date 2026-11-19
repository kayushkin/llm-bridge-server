import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from 'react'
import { createPortal } from 'react-dom'
import { formatBytes } from '../../toolPayloadPreview'

/**
 * The full-window image viewer, and the list of a session's images it steps through.
 *
 * TurnList gathers every image in the loaded transcript — files shared into the
 * session and images a tool result carried — in transcript order, and provides them
 * through {@link SessionImagesContext}. A thumbnail, a grid tile or an image chip in
 * prose opens the viewer at its own image, and the viewer then moves through the rest:
 * arrow keys, the wheel, the side buttons, a swipe or the strip of thumbnails along the
 * bottom. It zooms (ctrl+wheel or a pinch at the pointer, double-click, `+` `-`, `0` to
 * fit, `W` to fit the width — which a very tall image opens with), pans by dragging or
 * the wheel once zoomed, and splits into two panes to compare images side by side,
 * with zoom and pan shared between them so the same spot shows in both.
 *
 * Outside a provider — a message in the inbox, say — the viewer shows the one image it
 * was opened on.
 */

/** One image the viewer can show. `key` is stable for the life of the transcript. */
export interface GalleryImage {
  key: string
  src: string
  name: string
  /** Where the original downloads from; absent for bytes that have no address. */
  downloadHref?: string
  sizeBytes?: number
  /** file-store's id for a shared file (`file_000305`); absent for a tool's image. */
  fileId?: string
  /** RFC 3339, when it was shared into the session; absent for a tool's image. */
  sharedAt?: string
}

export interface SessionImages {
  images: readonly GalleryImage[]
  /** Open the viewer at the image with this key. */
  open: (key: string) => void
}

export const SessionImagesContext = createContext<SessionImages | null>(null)

export function useSessionImages(): SessionImages | null {
  return useContext(SessionImagesContext)
}

/** The gallery key of a file shared into the session. */
export function sharedFileGalleryKey(fileId: string): string {
  return `file:${fileId}`
}

/** The gallery key of an image a tool result carried. The entry id, not the event id:
 *  an image folded live has no event id yet, and the key must not change when the
 *  settled copy replaces it. */
export function toolImageGalleryKey(entryId: string, index: number): string {
  return `tool:${entryId}:${index}`
}

/** Provides a session's images and draws the viewer when one is opened. */
export function SessionImagesProvider({ images, children }: { images: readonly GalleryImage[]; children: ReactNode }) {
  const [openKey, setOpenKey] = useState<string | null>(null)
  const value = useMemo<SessionImages>(() => ({ images, open: setOpenKey }), [images])
  const startIndex = openKey === null ? -1 : images.findIndex((image) => image.key === openKey)
  return (
    <SessionImagesContext.Provider value={value}>
      {children}
      {startIndex >= 0 && (
        <ImageGalleryViewer images={images} startIndex={startIndex} onClose={() => setOpenKey(null)} />
      )}
    </SessionImagesContext.Provider>
  )
}

/**
 * Opens the viewer for one image: through the session's gallery when the image is in
 * it, otherwise on that image alone. Returns the open function and the element to
 * render (null unless a lone viewer is showing).
 */
export function useOpenImage(image: GalleryImage): { open: () => void; viewer: ReactNode } {
  const session = useSessionImages()
  const [alone, setAlone] = useState(false)
  const inGallery = !!session && session.images.some((candidate) => candidate.key === image.key)
  const open = useCallback(() => {
    if (inGallery) session!.open(image.key)
    else setAlone(true)
  }, [inGallery, session, image.key])
  const viewer = alone ? <ImageGalleryViewer images={[image]} startIndex={0} onClose={() => setAlone(false)} /> : null
  return { open, viewer }
}

const MIN_SCALE = 1
/** The most zoom over the fitted size; a tall image may go further — see `maxScaleOf`. */
const MAX_SCALE = 16
/** How far past its own pixels an image may be zoomed. */
const MAX_PIXEL_ZOOM = 4
const ZOOM_STEP = 1.25
/** The least time between two wheel turns of the page, so one flick of a trackpad,
 *  which sends dozens of wheel events, moves one image rather than ten. */
const WHEEL_STEP_MS = 280
/** How far a finger must travel sideways, unzoomed, to count as a swipe. */
const SWIPE_PX = 50
/** Space kept around an image fitted to its pane, in px. */
const PANE_MARGIN = 12
/** An image this many times taller than wide — a full-page phone screenshot — opens
 *  fitted to the pane's width at its top, since fitted whole it is a sliver. */
const TALL_RATIO = 2.5

interface View {
  scale: number
  /** Pan in screen pixels, from the center of the pane. */
  x: number
  y: number
}

const FITTED: View = { scale: 1, x: 0, y: 0 }

function clamp(value: number, low: number, high: number): number {
  return Math.min(high, Math.max(low, value))
}

interface Size {
  width: number
  height: number
}

/** Where an image sits in its pane: its own size, the pane's, and the size it is drawn
 *  at fitted whole (never larger than its own pixels). Zoom multiplies the fitted size,
 *  so the browser redraws the picture at each zoom rather than stretching a small copy. */
interface PaneGeometry {
  natural: Size
  pane: Size
  fitted: Size
}

function geometryOf(natural: Size, pane: Size): PaneGeometry {
  const fit = Math.min(
    1,
    (pane.width - 2 * PANE_MARGIN) / natural.width,
    (pane.height - 2 * PANE_MARGIN) / natural.height,
  )
  return { natural, pane, fitted: { width: natural.width * fit, height: natural.height * fit } }
}

/** The most an image may be zoomed: 16 times its fitted size, or enough to show its
 *  own pixels four times over, whichever is more — a 20,000-pixel page fitted whole
 *  needs far more than 16 to read. */
function maxScaleOf(geometry: PaneGeometry | undefined): number {
  if (!geometry || geometry.fitted.width === 0) return MAX_SCALE
  return Math.max(MAX_SCALE, (MAX_PIXEL_ZOOM * geometry.natural.width) / geometry.fitted.width)
}

/** The view after zooming to `scale` about a point given relative to the pane's
 *  center, so the spot under the pointer stays under it. */
function zoomAbout(view: View, scale: number, pointX: number, pointY: number, maxScale: number): View {
  const next = clamp(scale, MIN_SCALE, maxScale)
  if (next === MIN_SCALE) return FITTED
  const ratio = next / view.scale
  return { scale: next, x: pointX - (pointX - view.x) * ratio, y: pointY - (pointY - view.y) * ratio }
}

type Pane = 'left' | 'right'

/** The view that fills the pane's width with the image and shows its top. */
function fitWidthView(geometry: PaneGeometry): View {
  const { fitted, pane } = geometry
  if (fitted.width === 0) return FITTED
  const scale = clamp((pane.width - 2 * PANE_MARGIN) / fitted.width, MIN_SCALE, maxScaleOf(geometry))
  if (scale === MIN_SCALE) return FITTED
  return { scale, x: 0, y: PANE_MARGIN - pane.height / 2 + (fitted.height * scale) / 2 }
}

export function ImageGalleryViewer({
  images,
  startIndex,
  onClose,
}: {
  images: readonly GalleryImage[]
  startIndex: number
  onClose: () => void
}) {
  const count = images.length
  const [index, setIndex] = useState(startIndex)
  // The right pane while comparing; null when showing one image.
  const [compareIndex, setCompareIndex] = useState<number | null>(null)
  const [focusedPane, setFocusedPane] = useState<Pane>('left')
  const [view, setView] = useState<View>(FITTED)
  const lastWheelStep = useRef(0)
  const paneGeometries = useRef(new Map<Pane, PaneGeometry>())

  const comparing = compareIndex !== null
  const focusedIndex = comparing && focusedPane === 'right' ? compareIndex : index
  const current = images[clamp(focusedIndex, 0, count - 1)]!

  const show = useCallback(
    (target: number) => {
      const wrapped = ((target % count) + count) % count
      if (comparing && focusedPane === 'right') setCompareIndex(wrapped)
      else setIndex(wrapped)
      // Comparing keeps the zoom, so stepping the other pane through a series shows
      // the same spot in each; a single image starts fitted.
      if (!comparing) setView(FITTED)
    },
    [count, comparing, focusedPane],
  )
  const step = useCallback((delta: number) => show(focusedIndex + delta), [show, focusedIndex])

  const toggleCompare = useCallback(() => {
    if (comparing) {
      setIndex(focusedIndex)
      setCompareIndex(null)
      setFocusedPane('left')
    } else if (count > 1) {
      setCompareIndex((index + 1) % count)
      setFocusedPane('right')
    }
    setView(FITTED)
  }, [comparing, count, focusedIndex, index])

  const maxScale = () => maxScaleOf(paneGeometries.current.get(comparing ? focusedPane : 'left'))
  const zoomBy = (factor: number) => {
    const limit = maxScale()
    setView((v) => zoomAbout(v, v.scale * factor, 0, 0, limit))
  }
  const fitWidth = () => {
    const geometry = paneGeometries.current.get(comparing ? focusedPane : 'left')
    if (geometry) setView(fitWidthView(geometry))
  }
  // A single tall image opens fitted to the width, so a long page reads from its top.
  const onImageLoaded = (pane: Pane, geometry: PaneGeometry) => {
    paneGeometries.current.set(pane, geometry)
    if (!comparing && geometry.natural.height > geometry.natural.width * TALL_RATIO) setView(fitWidthView(geometry))
  }
  const onPaneResized = (pane: Pane, geometry: PaneGeometry) => paneGeometries.current.set(pane, geometry)

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.ctrlKey || event.metaKey || event.altKey) return
      let handled = true
      switch (event.key) {
        case 'Escape':
          if (view.scale > 1) setView(FITTED)
          else onClose()
          break
        case 'ArrowLeft':
        case 'ArrowUp':
        case 'PageUp':
          step(-1)
          break
        case 'ArrowRight':
        case 'ArrowDown':
        case 'PageDown':
        case ' ':
          step(1)
          break
        case 'Home':
          show(0)
          break
        case 'End':
          show(count - 1)
          break
        case '+':
        case '=':
          zoomBy(ZOOM_STEP)
          break
        case '-':
          zoomBy(1 / ZOOM_STEP)
          break
        case '0':
          setView(FITTED)
          break
        case 'w':
        case 'W':
          fitWidth()
          break
        case 'c':
        case 'C':
          toggleCompare()
          break
        case 'Tab':
          if (comparing) setFocusedPane((pane) => (pane === 'left' ? 'right' : 'left'))
          else handled = false
          break
        default:
          handled = false
      }
      if (handled) {
        event.preventDefault()
        event.stopPropagation()
      }
    }
    // Capture, so the chat's own shortcuts underneath never see a key meant here.
    window.addEventListener('keydown', onKeyDown, true)
    return () => window.removeEventListener('keydown', onKeyDown, true)
  })

  // The page under the viewer must not scroll while it is open.
  useEffect(() => {
    const previous = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.body.style.overflow = previous
    }
  }, [])

  const onStageWheel = useCallback(
    (event: WheelEvent, paneRect: DOMRect) => {
      event.preventDefault()
      const pointX = event.clientX - (paneRect.left + paneRect.width / 2)
      const pointY = event.clientY - (paneRect.top + paneRect.height / 2)
      // A trackpad pinch arrives as a wheel event with ctrlKey set.
      if (event.ctrlKey || event.metaKey) {
        // A mouse notch sends ~100 and a pinch a few per event; clamping keeps a notch
        // to one step of about 1.3x while a pinch stays smooth.
        const factor = Math.exp(-clamp(event.deltaY, -25, 25) * 0.01)
        const limit = maxScale()
        setView((v) => zoomAbout(v, v.scale * factor, pointX, pointY, limit))
        return
      }
      if (view.scale > 1) {
        setView((v) => ({ ...v, x: v.x - event.deltaX, y: v.y - event.deltaY }))
        return
      }
      const now = Date.now()
      if (now - lastWheelStep.current < WHEEL_STEP_MS) return
      const delta = Math.abs(event.deltaY) >= Math.abs(event.deltaX) ? event.deltaY : event.deltaX
      if (Math.abs(delta) < 4) return
      lastWheelStep.current = now
      step(delta > 0 ? 1 : -1)
    },
    // maxScale reads the geometry ref of the focused pane, which these name.
    [step, view.scale, comparing, focusedPane],
  )

  // Zoom as a share of the image's own pixels, so 100% is its actual size.
  const focusedGeometry = paneGeometries.current.get(comparing ? focusedPane : 'left')
  const zoomPercent = focusedGeometry
    ? Math.round((100 * focusedGeometry.fitted.width * view.scale) / focusedGeometry.natural.width)
    : Math.round(view.scale * 100)
  const openable = !current.src.startsWith('data:')
  const panes: { pane: Pane; image: GalleryImage }[] = comparing
    ? [
        { pane: 'left', image: images[index]! },
        { pane: 'right', image: images[compareIndex]! },
      ]
    : [{ pane: 'left', image: images[index]! }]

  return createPortal(
    <div className="bc-gallery" role="dialog" aria-modal="true" aria-label={`Image viewer: ${current.name}`}>
      <div className="bc-gallery-bar">
        <span className="bc-gallery-count">
          {focusedIndex + 1} / {count}
        </span>
        <span className="bc-gallery-name" title={current.name}>
          {current.name}
        </span>
        {current.sizeBytes !== undefined && <span className="bc-gallery-size">{formatBytes(current.sizeBytes)}</span>}
        <span className="bc-gallery-spacer" />
        <div className="bc-gallery-group" role="group" aria-label="Zoom">
          <button type="button" className="bc-gallery-btn" onClick={() => zoomBy(1 / ZOOM_STEP)} title="Zoom out (-)" disabled={view.scale <= MIN_SCALE}>
            −
          </button>
          <button type="button" className="bc-gallery-btn bc-gallery-zoom" onClick={() => setView(FITTED)} title="Zoom against the image's own pixels; click to fit the window (0)">
            {view.scale === MIN_SCALE ? 'Fit' : `${zoomPercent}%`}
          </button>
          <button type="button" className="bc-gallery-btn" onClick={() => zoomBy(ZOOM_STEP)} title="Zoom in (+)">
            +
          </button>
          <button type="button" className="bc-gallery-btn" onClick={fitWidth} title="Fit to width, from the top (W)">
            ↔
          </button>
        </div>
        {count > 1 && (
          <button
            type="button"
            className={`bc-gallery-btn${comparing ? ' bc-gallery-btn-on' : ''}`}
            onClick={toggleCompare}
            aria-pressed={comparing}
            title="Compare two images side by side (C)"
          >
            ◫ Compare
          </button>
        )}
        {openable && (
          <a className="bc-gallery-btn" href={current.src} target="_blank" rel="noreferrer" title="Open the original in a new tab">
            ↗
          </a>
        )}
        {current.downloadHref && (
          <a className="bc-gallery-btn" href={current.downloadHref} download={current.name} title="Download">
            ⤓
          </a>
        )}
        <button type="button" className="bc-gallery-btn" onClick={onClose} aria-label="Close" title="Close (Esc)">
          ✕
        </button>
      </div>

      <div className={`bc-gallery-stage${comparing ? ' bc-gallery-stage-split' : ''}`}>
        {panes.map(({ pane, image }) => (
          <GalleryPane
            key={pane}
            image={image}
            view={view}
            setView={setView}
            focused={comparing && focusedPane === pane}
            onFocus={() => setFocusedPane(pane)}
            onWheel={onStageWheel}
            onSwipe={step}
            onBackdropClick={comparing ? undefined : onClose}
            onLoaded={(geometry) => onImageLoaded(pane, geometry)}
            onResized={(geometry) => onPaneResized(pane, geometry)}
          />
        ))}
        {count > 1 && (
          <>
            <button type="button" className="bc-gallery-nav bc-gallery-nav-prev" onClick={() => step(-1)} aria-label="Previous image" title="Previous (←)">
              ‹
            </button>
            <button type="button" className="bc-gallery-nav bc-gallery-nav-next" onClick={() => step(1)} aria-label="Next image" title="Next (→)">
              ›
            </button>
          </>
        )}
      </div>

      {count > 1 && (
        <GalleryStrip
          images={images}
          index={index}
          compareIndex={compareIndex}
          focusedIndex={focusedIndex}
          onPick={show}
        />
      )}
    </div>,
    document.body,
  )
}

function GalleryPane({
  image,
  view,
  setView,
  focused,
  onFocus,
  onWheel,
  onSwipe,
  onBackdropClick,
  onLoaded,
  onResized,
}: {
  image: GalleryImage
  view: View
  setView: React.Dispatch<React.SetStateAction<View>>
  focused: boolean
  onFocus: () => void
  onWheel: (event: WheelEvent, paneRect: DOMRect) => void
  onSwipe: (delta: number) => void
  onBackdropClick?: () => void
  onLoaded: (geometry: PaneGeometry) => void
  onResized: (geometry: PaneGeometry) => void
}) {
  const paneRef = useRef<HTMLDivElement>(null)
  const [failed, setFailed] = useState(false)
  const [natural, setNatural] = useState<Size | null>(null)
  const [paneSize, setPaneSize] = useState<Size | null>(null)
  useEffect(() => {
    setFailed(false)
    setNatural(null)
  }, [image.src])
  useEffect(() => {
    const element = paneRef.current
    if (!element) return
    const observer = new ResizeObserver(() => setPaneSize({ width: element.clientWidth, height: element.clientHeight }))
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  const geometry = natural && paneSize ? geometryOf(natural, paneSize) : null
  const onResizedRef = useRef(onResized)
  onResizedRef.current = onResized
  useEffect(() => {
    if (geometry) onResizedRef.current(geometry)
  }, [natural, paneSize])

  // React attaches wheel listeners as passive, and a passive listener cannot stop the
  // page zooming on ctrl+wheel; this one is attached by hand so it can.
  const onWheelRef = useRef(onWheel)
  onWheelRef.current = onWheel
  useEffect(() => {
    const element = paneRef.current
    if (!element) return
    const listener = (event: WheelEvent) => onWheelRef.current(event, element.getBoundingClientRect())
    element.addEventListener('wheel', listener, { passive: false })
    return () => element.removeEventListener('wheel', listener)
  }, [])

  // Pointers down on this pane, for dragging with one and pinching with two.
  const pointers = useRef(new Map<number, { x: number; y: number }>())
  const gesture = useRef<{ startX: number; startY: number; moved: boolean; pinchDistance: number | null }>({
    startX: 0,
    startY: 0,
    moved: false,
    pinchDistance: null,
  })

  const centerOf = () => {
    const rect = paneRef.current!.getBoundingClientRect()
    return { cx: rect.left + rect.width / 2, cy: rect.top + rect.height / 2 }
  }

  const onPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0) return
    onFocus()
    pointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY })
    event.currentTarget.setPointerCapture(event.pointerId)
    if (pointers.current.size === 1) {
      gesture.current = { startX: event.clientX, startY: event.clientY, moved: false, pinchDistance: null }
    } else if (pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      gesture.current.pinchDistance = Math.hypot(a!.x - b!.x, a!.y - b!.y)
      gesture.current.moved = true
    }
  }

  const onPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    const previous = pointers.current.get(event.pointerId)
    if (!previous) return
    pointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY })
    if (pointers.current.size === 2 && gesture.current.pinchDistance) {
      const [a, b] = [...pointers.current.values()]
      const distance = Math.hypot(a!.x - b!.x, a!.y - b!.y)
      const factor = distance / gesture.current.pinchDistance
      gesture.current.pinchDistance = distance
      const { cx, cy } = centerOf()
      const limit = maxScaleOf(geometry ?? undefined)
      setView((v) => zoomAbout(v, v.scale * factor, (a!.x + b!.x) / 2 - cx, (a!.y + b!.y) / 2 - cy, limit))
      return
    }
    if (Math.hypot(event.clientX - gesture.current.startX, event.clientY - gesture.current.startY) > 4) {
      gesture.current.moved = true
    }
    if (view.scale > 1) {
      const dx = event.clientX - previous.x
      const dy = event.clientY - previous.y
      setView((v) => ({ ...v, x: v.x + dx, y: v.y + dy }))
    }
  }

  const onPointerUp = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!pointers.current.has(event.pointerId)) return
    pointers.current.delete(event.pointerId)
    if (pointers.current.size > 0) return
    const dx = event.clientX - gesture.current.startX
    const dy = event.clientY - gesture.current.startY
    if (view.scale === 1 && gesture.current.pinchDistance === null && Math.abs(dx) > SWIPE_PX && Math.abs(dx) > Math.abs(dy) * 1.5) {
      onSwipe(dx < 0 ? 1 : -1)
      return
    }
    // A plain click on the dark space around the image closes a single-image view.
    if (!gesture.current.moved && onBackdropClick && event.target === event.currentTarget) onBackdropClick()
  }

  const onDoubleClick = (event: React.MouseEvent<HTMLDivElement>) => {
    const { cx, cy } = centerOf()
    const limit = maxScaleOf(geometry ?? undefined)
    setView((v) => (v.scale > 1 ? FITTED : zoomAbout(v, 2.5, event.clientX - cx, event.clientY - cy, limit)))
  }

  return (
    <div
      ref={paneRef}
      className={`bc-gallery-pane${focused ? ' bc-gallery-pane-focused' : ''}${view.scale > 1 ? ' bc-gallery-pane-zoomed' : ''}`}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
      onDoubleClick={onDoubleClick}
    >
      {failed ? (
        <div className="bc-gallery-failed">
          This browser could not draw {image.name}.{' '}
          {image.downloadHref && (
            <a href={image.downloadHref} download={image.name}>
              Download it
            </a>
          )}
        </div>
      ) : (
        <img
          className={`bc-gallery-image${geometry ? '' : ' bc-gallery-image-loading'}`}
          src={image.src}
          alt={image.name}
          draggable={false}
          onLoad={(event) => {
            const size = { width: event.currentTarget.naturalWidth, height: event.currentTarget.naturalHeight }
            setNatural(size)
            const pane = paneRef.current!
            onLoaded(geometryOf(size, { width: pane.clientWidth, height: pane.clientHeight }))
          }}
          onError={() => setFailed(true)}
          style={
            geometry
              ? {
                  width: geometry.fitted.width * view.scale,
                  height: geometry.fitted.height * view.scale,
                  transform: `translate(-50%, -50%) translate(${view.x}px, ${view.y}px)`,
                }
              : undefined
          }
        />
      )}
      <span className="bc-gallery-pane-label">{image.name}</span>
    </div>
  )
}

/** The thumbnails along the bottom. The one showing scrolls into view as it changes. */
function GalleryStrip({
  images,
  index,
  compareIndex,
  focusedIndex,
  onPick,
}: {
  images: readonly GalleryImage[]
  index: number
  compareIndex: number | null
  focusedIndex: number
  onPick: (index: number) => void
}) {
  const stripRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const tile = stripRef.current?.querySelector<HTMLElement>(`[data-gallery-index="${focusedIndex}"]`)
    tile?.scrollIntoView({ block: 'nearest', inline: 'center', behavior: 'smooth' })
  }, [focusedIndex])
  return (
    <div className="bc-gallery-strip" ref={stripRef}>
      {images.map((image, position) => {
        const marks = [
          position === index ? 'bc-gallery-tile-current' : '',
          position === compareIndex ? 'bc-gallery-tile-compare' : '',
          position === focusedIndex ? 'bc-gallery-tile-focused' : '',
        ]
          .filter(Boolean)
          .join(' ')
        return (
          <button
            key={image.key}
            type="button"
            className={`bc-gallery-tile ${marks}`}
            data-gallery-index={position}
            onClick={() => onPick(position)}
            title={image.name}
            aria-label={`Show ${image.name}`}
            aria-current={position === focusedIndex}
          >
            <img src={image.src} alt="" loading="lazy" draggable={false} />
          </button>
        )
      })}
    </div>
  )
}
