import { useEffect, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import type { Overview, WaitingConversation } from '@kayushkin/multichat-types'
import { useBridgeConfig } from '../../context'
import { alertActions, arrivalText, arrivalsToAnnounce, newlyArrived, roomOnScreen } from '../../awaitingReply'
import { useAwaitingReply } from '../../useAwaitingReply'
import { useBrowserAlertSettings } from '../../useBrowserAlertSettings'
import { playAlertSound } from '../../alertSound'
import { openConversationInChat } from '../../chatConversationEvents'

/** How long a pop-up stays before it goes by itself. */
const POP_UP_MS = 15_000

interface PopUp {
  key: string
  entry: WaitingConversation
}

/**
 * The sound, pop-up and desktop notification for a new message from someone
 * the operator tagged, on every page of the bridge. It compares each answer
 * of the shared awaiting-reply poll with the one before, so the first answer
 * after opening dash is never news.
 */
export function AwaitingReplyAlerts() {
  const { overview } = useAwaitingReply()
  const { settings } = useBrowserAlertSettings()
  const { routes } = useBridgeConfig()
  const { pathname, search } = useLocation()
  const onScreen = roomOnScreen(pathname, search, routes)
  const onScreenRef = useRef(onScreen)
  onScreenRef.current = onScreen
  const navigate = useNavigate()
  const previous = useRef<Overview | null>(null)
  const [popUps, setPopUps] = useState<PopUp[]>([])
  const [soundError, setSoundError] = useState<string | null>(null)
  const inChat = pathname === routes.chat

  const open = (entry: WaitingConversation) => {
    setPopUps(current => current.filter(popUp => popUp.entry.room_id !== entry.room_id))
    if (inChat) openConversationInChat(entry.room_id)
    else navigate(`${routes.messageWaiting}?room=${encodeURIComponent(entry.room_id)}`)
  }
  const openRef = useRef(open)
  openRef.current = open

  useEffect(() => {
    if (!overview) return
    const arrived = arrivalsToAnnounce(newlyArrived(previous.current, overview), onScreenRef.current)
    previous.current = overview
    if (arrived.length === 0) return
    const actions = alertActions(settings, new Date(), document.hidden)
    if (actions.playSound) {
      playAlertSound(settings.sound, settings.volume).then(() => setSoundError(null)).catch(err => setSoundError(err instanceof Error ? err.message : String(err)))
    }
    if (actions.popUp) {
      setPopUps(current => [
        ...current.filter(popUp => !arrived.some(entry => entry.room_id === popUp.entry.room_id)),
        ...arrived.map(entry => ({ key: `${entry.room_id} ${entry.newest_message_at}`, entry })),
      ])
    }
    if (actions.desktopNotification && typeof Notification !== 'undefined' && Notification.permission === 'granted') {
      for (const entry of arrived) {
        const { title, line } = arrivalText(entry)
        const notification = new Notification(title, { body: line, tag: entry.room_id })
        notification.onclick = () => { window.focus(); openRef.current(entry) }
      }
    }
  }, [overview, settings])

  useEffect(() => {
    if (popUps.length === 0) return
    const timer = window.setTimeout(() => setPopUps(current => current.slice(1)), POP_UP_MS)
    return () => window.clearTimeout(timer)
  }, [popUps])

  if (popUps.length === 0) return null
  return (
    <div className="bc-awaiting-popups" role="status" aria-live="polite">
      {popUps.map(({ key, entry }) => {
        const { title, line } = arrivalText(entry)
        return (
          <div key={key} className="bc-awaiting-popup">
            <button type="button" className="bc-awaiting-popup-open" onClick={() => open(entry)}
              title={inChat ? 'Open it in the chat' : 'Open it on Waiting on you'}>
              <span className="bc-awaiting-popup-title">{title}</span>
              <span className="bc-awaiting-popup-line">{line}</span>
              <span className="bc-awaiting-popup-meta">
                {entry.platform}{soundError ? ` · no sound: ${soundError}` : ''}
              </span>
            </button>
            <button type="button" className="bc-awaiting-popup-close" aria-label="Close"
              onClick={() => setPopUps(current => current.filter(popUp => popUp.key !== key))}>✕</button>
          </div>
        )
      })}
    </div>
  )
}
