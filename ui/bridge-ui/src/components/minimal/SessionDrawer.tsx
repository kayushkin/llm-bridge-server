import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'
import { NavLink } from 'react-router-dom'
import { useMinimalChrome } from './MinimalChromeContext'
import { useShellNavigation } from '../shellNavigation'
import { drawerSwipeAction, touchMayMoveDrawer, type TouchPoint } from '../../drawerSwipe'

export function SessionDrawer({ children }: { children: ReactNode }) {
  const { drawerOpen, setDrawerOpen } = useMinimalChrome()
  const navigation = useShellNavigation()
  const drawerRef = useRef<HTMLElement | null>(null)

  // A swipe to the right anywhere on the page opens the drawer, and a swipe to
  // the left closes it. The rule is in `drawerSwipe.ts`.
  useEffect(() => {
    let start: TouchPoint | null = null
    const onTouchStart = (e: TouchEvent) => {
      start = null
      if (e.touches.length !== 1) return
      if (!touchMayMoveDrawer(e.target instanceof Element ? e.target as HTMLElement : null, drawerRef.current)) return
      start = { x: e.touches[0].clientX, y: e.touches[0].clientY }
    }
    const onTouchEnd = (e: TouchEvent) => {
      if (!start || e.changedTouches.length !== 1) return
      const touch = e.changedTouches[0]
      const action = drawerSwipeAction(start, { x: touch.clientX, y: touch.clientY }, drawerOpen)
      start = null
      // A drag that selected text was for the text, not the drawer.
      if (!action) return
      const selection = window.getSelection()
      if (selection && !selection.isCollapsed) return
      setDrawerOpen(action === 'open')
    }
    const onTouchCancel = () => { start = null }
    document.addEventListener('touchstart', onTouchStart, { passive: true })
    document.addEventListener('touchend', onTouchEnd, { passive: true })
    document.addEventListener('touchcancel', onTouchCancel, { passive: true })
    return () => {
      document.removeEventListener('touchstart', onTouchStart)
      document.removeEventListener('touchend', onTouchEnd)
      document.removeEventListener('touchcancel', onTouchCancel)
    }
  }, [drawerOpen, setDrawerOpen])

  useEffect(() => {
    if (!drawerOpen) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setDrawerOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [drawerOpen, setDrawerOpen])

  return (
    <>
      <div
        className={`bc-mc-scrim ${drawerOpen ? 'bc-mc-scrim-open' : ''}`}
        onClick={() => setDrawerOpen(false)}
        aria-hidden={!drawerOpen}
      />
      <aside
        ref={drawerRef}
        className={`bc-mc-drawer ${drawerOpen ? 'bc-mc-drawer-open' : ''}`}
        aria-hidden={!drawerOpen}
        role="dialog"
        aria-label="Sessions"
      >
        <div className="bc-mc-drawer-header">
          <span className="bc-mc-drawer-title">Sessions</span>
          <button
            type="button"
            className="bc-mc-close"
            onClick={() => setDrawerOpen(false)}
            aria-label="Close"
          >×</button>
        </div>
        <div className="bc-mc-drawer-body">
          {children}
        </div>
        {/* The minimal chrome hides the shell's two navigation rows, so without this
            the chat was a dead end on a phone: the only way out was "Show full
            layout" in the controls sheet. */}
        {navigation && (
          <details className="bc-mc-drawer-pages">
            <summary className="bc-mc-drawer-pages-summary">Pages</summary>
            <nav className="bc-mc-drawer-pages-body" aria-label="Pages">
              {navigation.groups.map(group => (
                <div key={group.key} className="bc-mc-drawer-pages-group">
                  <span className="bc-mc-drawer-pages-group-label">{group.label}</span>
                  <div className="bc-mc-drawer-pages-links">
                    {navigation.entries.filter(entry => entry.group === group.key).map(entry => (
                      <NavLink
                        key={entry.to}
                        to={entry.to}
                        end={entry.end}
                        draggable={false}
                        onClick={() => setDrawerOpen(false)}
                        className={({ isActive }) => `bc-mc-drawer-page ${isActive ? 'bc-mc-drawer-page-active' : ''}`}
                      >
                        {entry.label}
                      </NavLink>
                    ))}
                  </div>
                </div>
              ))}
            </nav>
          </details>
        )}
      </aside>
    </>
  )
}
