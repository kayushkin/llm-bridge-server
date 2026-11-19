import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../../context'
import { newestPreviewText, sendersLabel, waitingAgeLabel } from '../../awaitingReply'
import { useAwaitingReply } from '../../useAwaitingReply'

const COLLAPSED_KEY = 'chat:awaiting-reply-collapsed'

/** The sidebar's Waiting on you section, under the Orchestrator row: one row
 *  per conversation where someone tagged wrote after your last word. A row
 *  opens the conversation in the thread pane, as a session row opens a
 *  session. Drawn only when something is waiting. */
export function AwaitingReplySidebarSection({ openRoomID, onOpenConversation }: {
  /** The conversation on screen, so its row reads as selected. */
  openRoomID: string | null
  onOpenConversation: (roomID: string) => void
}) {
  const { routes } = useBridgeConfig()
  const { overview, error, clear } = useAwaitingReply()
  const [collapsed, setCollapsed] = useState(() => localStorage.getItem(COLLAPSED_KEY) === '1')
  const [dismissError, setDismissError] = useState<string | null>(null)
  const waiting = overview?.waiting ?? []
  if (waiting.length === 0 && !error) return null
  const now = new Date()

  const toggle = () => {
    localStorage.setItem(COLLAPSED_KEY, collapsed ? '0' : '1')
    setCollapsed(!collapsed)
  }

  return (
    <div className="bc-awaiting-section" data-count={waiting.length}>
      <div className="bc-folder-header bc-awaiting-header">
        <button type="button" className="bc-awaiting-header-main" aria-expanded={!collapsed} onClick={toggle}>
          <span className="bc-folder-chevron">{collapsed ? '▸' : '▾'}</span>
          <span className="bc-folder-icon" aria-hidden>💬</span>
          <span className="bc-folder-name">Waiting on you</span>
          {waiting.length > 0 && <span className="bc-awaiting-count">{waiting.length}</span>}
        </button>
        <Link className="bc-awaiting-settings-link" to={`${routes.messageWaiting}?settings=1`} title="Alert settings">⚙</Link>
      </div>
      {error && <div className="bc-awaiting-error" role="status">{error}</div>}
      {dismissError && <div className="bc-awaiting-error" role="status">{dismissError}</div>}
      {!collapsed && waiting.map(entry => {
        const senders = sendersLabel(entry)
        return (
          <div key={entry.room_id} data-room-id={entry.room_id}
            className={`bc-session-item bc-session-row-grid bc-awaiting-row ${entry.room_id === openRoomID ? 'bc-session-item-selected' : ''}`}>
            <span className="bc-session-status"><span className="bc-awaiting-dot" aria-label="waiting" /></span>
            <button type="button" className="bc-session-item-main" onClick={() => onOpenConversation(entry.room_id)}
              title={`${entry.name} · ${entry.platform}`}>
              <span className="bc-session-label bc-awaiting-label">
                <span className="bc-awaiting-name">{entry.name}</span>
                <span className="bc-awaiting-platform">{entry.platform}</span>
                <span className="bc-awaiting-preview">
                  {senders && `${senders}: `}{newestPreviewText(entry)} · {waitingAgeLabel(entry.newest_message_at, now)}
                </span>
              </span>
            </button>
            <span className="bc-session-slot-empty" />
            <button type="button" className="bc-awaiting-dismiss" title="Dismiss: mark it seen without opening it" aria-label={`Dismiss ${entry.name}`}
              onClick={() => void clear(entry, 'dismissed').then(result => setDismissError(result.ok ? null : result.error))}>✕</button>
          </div>
        )
      })}
    </div>
  )
}
