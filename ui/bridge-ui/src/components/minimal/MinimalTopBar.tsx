import { useMinimalChrome } from './MinimalChromeContext'

export interface MinimalTopBarProps {
  title: string
  subtitle?: string
}

export function MinimalTopBar({ title, subtitle }: MinimalTopBarProps) {
  const { setDrawerOpen, setSheetOpen } = useMinimalChrome()
  return (
    <div className="bc-mc-topbar" role="toolbar">
      <button
        type="button"
        className="bc-mc-topbar-btn bc-mc-topbar-drawer"
        aria-label="Show sessions"
        onClick={() => setDrawerOpen(true)}
      >
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <line x1="3" y1="6" x2="21" y2="6" />
          <line x1="3" y1="12" x2="21" y2="12" />
          <line x1="3" y1="18" x2="21" y2="18" />
        </svg>
      </button>
      <button
        type="button"
        className="bc-mc-topbar-title"
        onClick={() => setDrawerOpen(true)}
        title={title || 'No session'}
      >
        <span className="bc-mc-topbar-title-text">{title || 'No session'}</span>
        {subtitle && <span className="bc-mc-topbar-subtitle">{subtitle}</span>}
      </button>
      <button
        type="button"
        className="bc-mc-topbar-btn bc-mc-topbar-menu"
        aria-label="Show controls"
        onClick={() => setSheetOpen(true)}
      >
        <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <circle cx="5" cy="12" r="1.7" />
          <circle cx="12" cy="12" r="1.7" />
          <circle cx="19" cy="12" r="1.7" />
        </svg>
      </button>
    </div>
  )
}
