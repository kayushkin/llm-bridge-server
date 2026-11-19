import { createContext, useContext, useEffect, useState } from 'react'
import type { ReactNode } from 'react'

/** The three looks bridge-ui styles. `zine` is the risograph-print theme
 *  (zine.css: cream paper, navy ink, orange and pink, the crab mascot); `light`
 *  and `dark` are theme.css's plain ones. */
export type BridgeTheme = 'zine' | 'light' | 'dark'
export const BRIDGE_THEMES: readonly BridgeTheme[] = ['zine', 'light', 'dark']

interface BridgeThemeContextValue {
  theme: BridgeTheme
  /** The theme the switch in the top bar moves to next. */
  nextTheme: BridgeTheme
  cycleTheme: () => void
}

const BridgeThemeContext = createContext<BridgeThemeContextValue | null>(null)

function isBridgeTheme(value: string | null): value is BridgeTheme {
  return value !== null && (BRIDGE_THEMES as readonly string[]).includes(value)
}

export interface BridgeThemeProviderProps {
  /** The localStorage key that remembers a person's choice. Each host names
   *  its own, so two hosts on one origin do not share a choice by accident. */
  storageKey: string
  /** The look until someone picks one. */
  defaultTheme: BridgeTheme
  children: ReactNode
}

/** Sets `data-theme` on the document element, which is what theme.css and
 *  zine.css key on, and remembers the choice under `storageKey`. The key holds
 *  only a choice someone made, never the default, so changing a host's
 *  default reaches everyone who never picked. */
export function BridgeThemeProvider({ storageKey, defaultTheme, children }: BridgeThemeProviderProps) {
  const [theme, setTheme] = useState<BridgeTheme>(() => {
    const stored = localStorage.getItem(storageKey)
    return isBridgeTheme(stored) ? stored : defaultTheme
  })

  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])

  const nextTheme = BRIDGE_THEMES[(BRIDGE_THEMES.indexOf(theme) + 1) % BRIDGE_THEMES.length]
  const cycleTheme = () => {
    localStorage.setItem(storageKey, nextTheme)
    setTheme(nextTheme)
  }

  return (
    <BridgeThemeContext.Provider value={{ theme, nextTheme, cycleTheme }}>
      {children}
    </BridgeThemeContext.Provider>
  )
}

export function useBridgeTheme(): BridgeThemeContextValue {
  const context = useContext(BridgeThemeContext)
  if (!context) {
    throw new Error('useBridgeTheme must be used within a BridgeThemeProvider')
  }
  return context
}
