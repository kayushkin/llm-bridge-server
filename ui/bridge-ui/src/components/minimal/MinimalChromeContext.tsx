import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'

export type ChromeOverride = 'minimal' | 'full' | null

const STORAGE_KEY = 'bridge-chrome-override'
/**
 * The viewport width below which minimal mode engages on its own.
 *
 * Exported because it is a JS comparison and not a media query, so a host that draws
 * its own minimal chrome cannot reach it from CSS. The one question that needs it is
 * the re-engage pill's: `minimal` folds `override` in, so it reads false whether the
 * viewport is wide or the user forced the full layout on a narrow one, and telling
 * those apart is the whole condition for offering the way back.
 */
export const MOBILE_BREAKPOINT = 640

function loadOverride(): ChromeOverride {
  if (typeof window === 'undefined') return null
  const v = window.localStorage.getItem(STORAGE_KEY)
  if (v === 'minimal' || v === 'full') return v
  return null
}

function saveOverride(value: ChromeOverride) {
  if (typeof window === 'undefined') return
  if (value === null) window.localStorage.removeItem(STORAGE_KEY)
  else window.localStorage.setItem(STORAGE_KEY, value)
}

interface MinimalChromeValue {
  minimal: boolean
  /**
   * True only while some mounted surface is actually DRAWING the minimal chrome —
   * the top bar and the session drawer that replace the navigation a narrow
   * viewport hides.
   *
   * `minimal` alone says the viewport is narrow. It does NOT say anyone answered,
   * and the two are not the same fact: `MinimalChromeProvider` is nested inside
   * every `BridgeProvider`, so `minimal` goes true on every page a host mounts
   * under one — the instance list, the settings page, the host's chat — while only
   * the host's chat page renders `MinimalTopBar` and `SessionDrawer`.
   * Anything that HIDES navigation on the strength of a narrow viewport has to
   * gate on this instead, or it takes the navigation away and puts nothing back.
   */
  minimalChromeMounted: boolean
  /**
   * Called by a surface that draws the minimal chrome, for as long as it draws it.
   * Returns the unregister. Prefer the `useRegisterMinimalChrome` hook below;
   * registrations are counted, so several surfaces may overlap during a route change.
   */
  registerMinimalChrome: () => () => void
  override: ChromeOverride
  setOverride: (v: ChromeOverride) => void
  drawerOpen: boolean
  setDrawerOpen: (v: boolean) => void
  sheetOpen: boolean
  setSheetOpen: (v: boolean) => void
  controlsSlot: HTMLElement | null
  registerControlsSlot: (el: HTMLElement | null) => void
}

const MinimalChromeContext = createContext<MinimalChromeValue | null>(null)

export function useMinimalChrome(): MinimalChromeValue {
  const ctx = useContext(MinimalChromeContext)
  if (!ctx) {
    return {
      minimal: false,
      minimalChromeMounted: false,
      registerMinimalChrome: () => () => {},
      override: null,
      setOverride: () => {},
      drawerOpen: false,
      setDrawerOpen: () => {},
      sheetOpen: false,
      setSheetOpen: () => {},
      controlsSlot: null,
      registerControlsSlot: () => {},
    }
  }
  return ctx
}

export function MinimalChromeProvider({ children }: { children: ReactNode }) {
  const [override, setOverrideState] = useState<ChromeOverride>(() => loadOverride())
  const [vw, setVw] = useState<number>(() => typeof window === 'undefined' ? 1024 : window.innerWidth)
  const [drawerOpen, setDrawerOpenState] = useState(false)
  const [sheetOpen, setSheetOpenState] = useState(false)
  const [controlsSlot, setControlsSlot] = useState<HTMLElement | null>(null)
  const slotRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    if (typeof window === 'undefined') return
    const onResize = () => setVw(window.innerWidth)
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  const minimal = useMemo(() => {
    if (override === 'minimal') return true
    if (override === 'full') return false
    return vw < MOBILE_BREAKPOINT
  }, [override, vw])

  const setOverride = useCallback((next: ChromeOverride) => {
    setOverrideState(next)
    saveOverride(next)
  }, [])

  // Counted rather than a plain boolean: during a route change React mounts the
  // incoming surface before it unmounts the outgoing one, so a boolean would be
  // cleared by the departing chrome right after the arriving chrome set it.
  const minimalChromeCountRef = useRef(0)
  const [minimalChromeMounted, setMinimalChromeMounted] = useState(false)
  const registerMinimalChrome = useCallback(() => {
    minimalChromeCountRef.current += 1
    setMinimalChromeMounted(true)
    return () => {
      minimalChromeCountRef.current -= 1
      if (minimalChromeCountRef.current <= 0) {
        minimalChromeCountRef.current = 0
        setMinimalChromeMounted(false)
      }
    }
  }, [])

  useEffect(() => {
    if (!minimal) {
      setDrawerOpenState(false)
      setSheetOpenState(false)
    }
  }, [minimal])

  // The body class is the signal a HOST reads to hide its own site header, so it
  // has to mean "the minimal chrome has taken the navigation over", not merely
  // "the window is narrow". Gate it on a surface having registered: a page that
  // is under a `BridgeProvider` but draws no minimal chrome would otherwise hide
  // the host's header and leave the user with no way out of the page.
  const showMinimalChrome = minimal && minimalChromeMounted
  useEffect(() => {
    if (typeof document === 'undefined') return
    if (showMinimalChrome) document.body.classList.add('bridge-minimal-chrome')
    else document.body.classList.remove('bridge-minimal-chrome')
    return () => { document.body.classList.remove('bridge-minimal-chrome') }
  }, [showMinimalChrome])

  const registerControlsSlot = useCallback((el: HTMLElement | null) => {
    slotRef.current = el
    setControlsSlot(el)
  }, [])

  const setDrawerOpen = useCallback((v: boolean) => setDrawerOpenState(v), [])
  const setSheetOpen = useCallback((v: boolean) => setSheetOpenState(v), [])

  const value = useMemo<MinimalChromeValue>(() => ({
    minimal,
    minimalChromeMounted,
    registerMinimalChrome,
    override,
    setOverride,
    drawerOpen,
    setDrawerOpen,
    sheetOpen,
    setSheetOpen,
    controlsSlot,
    registerControlsSlot,
  }), [minimal, minimalChromeMounted, registerMinimalChrome, override, setOverride, drawerOpen, setDrawerOpen, sheetOpen, setSheetOpen, controlsSlot, registerControlsSlot])

  return (
    <MinimalChromeContext.Provider value={value}>
      {children}
    </MinimalChromeContext.Provider>
  )
}

/**
 * Declare that this surface draws the minimal chrome while `active` is true.
 *
 * Call it from the component that renders `MinimalTopBar` / `SessionDrawer`, with
 * the same condition it renders them under. Until something calls this, the
 * library treats a narrow viewport as unanswered and leaves every navigation in
 * place — which is the safe direction, because the alternative hides the host's
 * chrome for a replacement that was never drawn.
 *
 * A layout effect, not a plain one: `BridgeLayout` renders the nav a frame before
 * its routed child registers, so an ordinary effect would show the tab row and
 * then snatch it away on the next paint.
 */
export function useRegisterMinimalChrome(active: boolean) {
  const { registerMinimalChrome } = useMinimalChrome()
  useLayoutEffect(() => {
    if (!active) return
    return registerMinimalChrome()
  }, [active, registerMinimalChrome])
}
