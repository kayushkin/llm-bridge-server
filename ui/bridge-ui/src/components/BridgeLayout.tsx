import { useEffect, useMemo, useRef } from 'react'
import type { ReactNode } from 'react'
import { Link, Outlet, useLocation } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import type { BridgeRoutes } from '../context'
import { PAGE_GROUPS, activeNavEntry, groupForPath, navEntriesFor, type HostPage, type NavEntry, type PageGroupKey } from '../pages'
import { useMinimalChrome } from './minimal/MinimalChromeContext'
import { ShellNavigationContext } from './shellNavigation'
import { AwaitingReplyAlerts } from './messages/AwaitingReplyAlerts'

interface BridgeLayoutProps {
  /** If true, include the Conformance page. Default: true. */
  showConformance?: boolean
  /** If true, include the Service inventory page. Default: true. */
  showServiceInventory?: boolean
  /** Pages the host brings into this navigation. */
  hostPages?: readonly HostPage[]
  /** Only these of this library's pages are listed. Absent means every page. */
  onlyBridgePages?: readonly (keyof BridgeRoutes)[]
  /** What the host puts at the start of the top bar: its brand. */
  navStart?: ReactNode
  /** What the host puts at the end of the top bar: its own controls (theme,
   *  logout). With both slots a host needs no header of its own, so the page
   *  has one bar rather than an empty one over a full one. */
  navEnd?: ReactNode
}

/** The shell: two navigation rows and the page. The first row is the groups
 *  (Work, Agents, Access, System, Personal); the second is the pages of the
 *  group the current path belongs to. Both rows come from `navEntriesFor`, so a
 *  host that proxies no store for a page sees no tab for it, exactly as before —
 *  what changed on 2026-09-18 is that twenty-odd tabs became five groups.
 *
 *  The tabs are not draggable. A link is draggable by default, so a click made
 *  while the mouse is moving starts a link drag; choosing a group replaces the
 *  second row, and a tab removed mid-drag never gets its `dragend` (measured:
 *  `dragstart`, then nothing). While the browser believes a drag is on, the
 *  cursor is pinned and the page takes no clicks. A tab has no use for a drag. */
export function BridgeLayout({ showConformance = true, showServiceInventory = true, hostPages = [], onlyBridgePages, navStart, navEnd }: BridgeLayoutProps) {
  const config = useBridgeConfig()
  const { pathname } = useLocation()
  // Gated on the chrome being DRAWN, not on the viewport being narrow. These rows
  // are the only navigation on every page here except the host's chat, and the
  // routed child that replaces them exists on that chat alone — so dropping them
  // for a narrow window used to strand the user on every other page, with the
  // host's own header hidden by the same mistaken signal.
  const { minimal, minimalChromeMounted } = useMinimalChrome()
  const chromeTakenOver = minimal && minimalChromeMounted

  const entries = navEntriesFor(config, { showConformance, showServiceInventory, onlyBridgePages }, hostPages)
  const groups = PAGE_GROUPS.filter(g => entries.some(e => e.group === g.key))
  const matched = groupForPath(pathname, config.routes, hostPages)
  const activeGroup: PageGroupKey | null = matched && groups.some(g => g.key === matched) ? matched : (groups[0]?.key ?? null)
  const pages: NavEntry[] = entries.filter(e => e.group === activeGroup)
  const activePage = activeNavEntry(pathname, pages)
  const entriesKey = entries.map(e => e.to).join('|')
  // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on the routes, not on the arrays built each render
  const shellNavigation = useMemo(() => ({ groups, entries }), [entriesKey])

  // Both rows scroll sideways when their tabs do not fit, which on a phone is
  // always. Without this the active tab can sit past the right edge — a Personal
  // page showed "Work Agents Acc…" with nothing marked — so the row is scrolled to
  // show it. Only the row moves: `scrollIntoView` would scroll the page too.
  const groupRow = useRef<HTMLElement>(null)
  const pageRow = useRef<HTMLElement>(null)
  useEffect(() => {
    for (const row of [groupRow.current, pageRow.current]) {
      const active = row?.querySelector<HTMLElement>('.bridge-tab-active')
      if (!row || !active) continue
      const left = active.getBoundingClientRect().left - row.getBoundingClientRect().left + row.scrollLeft
      if (left < row.scrollLeft || left + active.offsetWidth > row.scrollLeft + row.clientWidth) {
        row.scrollLeft = left - (row.clientWidth - active.offsetWidth) / 2
      }
    }
  }, [pathname, chromeTakenOver])

  return (
    <ShellNavigationContext.Provider value={shellNavigation}>
    <div className={`bridge-layout ${chromeTakenOver ? 'bridge-layout-minimal' : ''}`}>
      {!chromeTakenOver && <>
        <header className="bridge-shell-bar">
        {navStart && <div className="bridge-shell-slot bridge-shell-start">{navStart}</div>}
        <nav ref={groupRow} className="bridge-nav bridge-nav-groups" aria-label="Sections">
          {groups.map(g => {
            const first = entries.find(e => e.group === g.key)!
            return (
              <Link
                key={g.key}
                to={first.to}
                draggable={false}
                className={`bridge-tab bridge-group-tab ${g.key === activeGroup ? 'bridge-tab-active' : ''}`}
                aria-current={g.key === activeGroup ? 'true' : undefined}
              >
                {g.label}
                {entries.filter(e => e.group === g.key && e.badge).map(e => { const Badge = e.badge!; return <Badge key={e.to} /> })}
              </Link>
            )
          })}
        </nav>
        {navEnd && <div className="bridge-shell-slot bridge-shell-end">{navEnd}</div>}
        </header>
        <nav ref={pageRow} className="bridge-nav bridge-nav-pages" aria-label={`${groups.find(g => g.key === activeGroup)?.label ?? ''} pages`}>
          {pages.map(t => (
            <Link
              key={t.to}
              to={t.to}
              draggable={false}
              className={`bridge-tab ${t === activePage ? 'bridge-tab-active' : ''}`}
              aria-current={t === activePage ? 'page' : undefined}
            >
              {t.label}
              {t.badge && <t.badge />}
            </Link>
          ))}
        </nav>
      </>}
      <div className="bridge-content">
        <Outlet />
      </div>
      {config.multichatBasePath && <AwaitingReplyAlerts />}
    </div>
    </ShellNavigationContext.Provider>
  )
}
