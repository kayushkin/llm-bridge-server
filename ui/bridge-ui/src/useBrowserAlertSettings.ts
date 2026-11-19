import { useCallback, useSyncExternalStore } from 'react'
import { MESSAGE_ALERT_BROWSER_SETTINGS_KEY, readBrowserAlertSettings, type MessageAlertBrowserSettings } from './awaitingReply'

// This browser's sound and pop-up settings for new messages, in localStorage.
// Every component that reads them sees a save at once, and so does another tab
// of the same browser, through the storage event.

const listeners = new Set<() => void>()
let cachedRaw: string | null | undefined
let cached: ReturnType<typeof readBrowserAlertSettings> | undefined

function savedRaw(): string | null {
  return typeof localStorage === 'undefined' ? null : localStorage.getItem(MESSAGE_ALERT_BROWSER_SETTINGS_KEY)
}

function snapshot(): ReturnType<typeof readBrowserAlertSettings> {
  const raw = savedRaw()
  if (cached === undefined || raw !== cachedRaw) {
    cachedRaw = raw
    cached = readBrowserAlertSettings(raw)
  }
  return cached
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  const onStorage = (event: StorageEvent) => { if (event.key === MESSAGE_ALERT_BROWSER_SETTINGS_KEY) listener() }
  window.addEventListener('storage', onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener('storage', onStorage)
  }
}

export function useBrowserAlertSettings() {
  const { settings, problem } = useSyncExternalStore(subscribe, snapshot, snapshot)
  const save = useCallback((next: MessageAlertBrowserSettings) => {
    localStorage.setItem(MESSAGE_ALERT_BROWSER_SETTINGS_KEY, JSON.stringify(next))
    for (const listener of [...listeners]) listener()
  }, [])
  return { settings, problem, save }
}
