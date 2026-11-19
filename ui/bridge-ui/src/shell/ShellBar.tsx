import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useBridgeTheme } from './theme'
import styles from './ShellBar.module.css'

export interface ShellBrandProps {
  /** The host's name, shown beside its mark (hidden on a phone). */
  name: string
  /** The mark in the light and dark themes, such as an emoji. */
  mark: ReactNode
  /** The mark in the zine theme. A host with no picture of its own leaves it
   *  out and gets `mark` there too. */
  zineMarkSrc?: string
}

/** A host's brand, for `<Bridge navStart>`: the start of the one top bar. It
 *  links to `/`, the chat. */
export function ShellBrand({ name, mark, zineMarkSrc }: ShellBrandProps) {
  const { theme } = useBridgeTheme()
  return (
    <Link to="/" className={styles.brand} draggable={false} aria-label={`${name} home`}>
      {theme === 'zine' && zineMarkSrc
        ? <img className={styles.brandCrab} src={zineMarkSrc} alt="" draggable={false} />
        : <span className={styles.brandMark} aria-hidden="true">{mark}</span>}
      <span className={styles.brandName}>{name}</span>
    </Link>
  )
}

export interface ShellControlsProps {
  /** Ends the sign-in. Its failure is the host's to show; the button only
   *  starts it. */
  onLogout: () => void
}

/** A host's controls, for `<Bridge navEnd>`: in the zine theme a "● LIVE"
 *  stamp with today's date, then the theme switch and log out. The switch
 *  shows the theme it moves to. */
export function ShellControls({ onLogout }: ShellControlsProps) {
  const { theme, nextTheme, cycleTheme } = useBridgeTheme()
  const themeLabel = `Switch to the ${nextTheme} theme`
  return (
    <>
      {theme === 'zine' && <LiveStamp />}
      <button type="button" className={styles.control} onClick={cycleTheme} aria-label={themeLabel} title={themeLabel}>
        {nextTheme === 'light' ? <SunIcon /> : nextTheme === 'dark' ? <MoonIcon /> : <StarIcon />}
      </button>
      <button type="button" className={`${styles.control} ${styles.logout}`} onClick={onLogout} title="Log out">
        <LogoutIcon />
        <span className={styles.controlLabel}>Log out</span>
      </button>
    </>
  )
}

/** The broadcast stamp: a blinking dot, LIVE, and the date as 2026.09.27. The
 *  date is read once per mount, which is often enough for a label. */
function LiveStamp() {
  const now = new Date()
  const date = `${now.getFullYear()}.${String(now.getMonth() + 1).padStart(2, '0')}.${String(now.getDate()).padStart(2, '0')}`
  return (
    <span className={styles.live} aria-hidden="true">
      <span className={styles.liveLabel}><span className={styles.liveDot} />LIVE</span>
      <span className={styles.liveDate}>{date}</span>
    </span>
  )
}

const iconProps = { viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', strokeWidth: 2, strokeLinecap: 'round', strokeLinejoin: 'round', 'aria-hidden': true } as const

function SunIcon() {
  return (
    <svg {...iconProps}>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" />
    </svg>
  )
}

function MoonIcon() {
  return (
    <svg {...iconProps}>
      <path d="M21 12.79A9 9 0 1 1 11.21 3a7 7 0 0 0 9.79 9.79z" />
    </svg>
  )
}

function StarIcon() {
  return (
    <svg {...iconProps}>
      <path d="M12 2.5l2.9 6.1 6.6.8-4.9 4.6 1.3 6.6L12 17.3l-5.9 3.3 1.3-6.6-4.9-4.6 6.6-.8z" />
    </svg>
  )
}

function LogoutIcon() {
  return (
    <svg {...iconProps}>
      <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
      <path d="M16 17l5-5-5-5" />
      <path d="M21 12H9" />
    </svg>
  )
}
