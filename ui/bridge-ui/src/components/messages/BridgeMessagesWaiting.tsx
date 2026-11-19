import { useEffect, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import type { ClearedConversation, WaitingConversation } from '@kayushkin/multichat-types'
import { useBridgeConfig } from '../../context'
import {
  clearedByLabel, entryToClearOnOpen, newestPreviewText, previewMessageText, sendersLabel, waitingAgeLabel,
} from '../../awaitingReply'
import { useAwaitingReply } from '../../useAwaitingReply'
import { MultichatNotConfigured } from './messagesShared'
import { ConversationThread } from './BridgeMessageConversations'
import { AwaitingReplySettingsSections } from './AwaitingReplySettingsSections'
import { useConversationOf } from './useConversationOf'
import { useDailyNitroCheck } from './useEmojiCatalog'
import styles from './Messages.module.css'

/**
 * Conversations where someone the operator tagged wrote after the operator's
 * last word, from multichat's GET /awaiting-reply, beside the thread of the
 * one picked (`?room=`). Opening a waiting room clears it when the settings
 * say so; `?settings=1` shows the alert settings above the list.
 */
export function BridgeMessagesWaiting() {
  useDailyNitroCheck()
  const { enabled, overview, loading, error, clear } = useAwaitingReply()
  const [searchParams, setSearchParams] = useSearchParams()
  const roomID = searchParams.get('room') ?? ''
  const showSettings = searchParams.get('settings') === '1'
  const [clearError, setClearError] = useState<string | null>(null)
  const now = new Date()

  useClearOnOpen(roomID, setClearError)

  const setParam = (key: string, value: string | null) => {
    const next = new URLSearchParams(searchParams)
    if (value === null) next.delete(key)
    else next.set(key, value)
    setSearchParams(next, { replace: true })
  }

  const dismiss = async (entry: WaitingConversation) => {
    const result = await clear(entry, 'dismissed')
    setClearError(result.ok ? null : result.error)
  }

  if (!enabled) return <MultichatNotConfigured page="Waiting on you" />

  return (
    <div className={`${styles.page} ${roomID ? styles.pageRoomOpen : ''}`}>
      <header className={styles.header}>
        <h2 className={styles.title}>
          Waiting on you
          <button type="button" className={styles.awaitingSettingsToggle} aria-expanded={showSettings}
            onClick={() => setParam('settings', showSettings ? null : '1')}>⚙ Alert settings</button>
        </h2>
        <p className={styles.subtitle}>
          People you tagged, in direct messages and small groups, whose message came after your last one.
          Opening a chat here or replying from any app clears it.
        </p>
      </header>
      {showSettings && <AwaitingReplySettingsSections />}
      {error && <pre className={styles.error}>{error}</pre>}
      {clearError && <pre className={styles.error}>{clearError}</pre>}
      {overview && overview.settings.tag_ids.length === 0 && (
        <div className={styles.empty}>No tag is chosen, so nobody counts yet. Open ⚙ Alert settings and pick one.</div>
      )}
      {!overview ? (loading ? <div className={styles.empty}>Loading…</div> : null) : (
        <div className={`${styles.columns} ${roomID ? styles.columnsRoomOpen : ''}`}>
          <div className={styles.listColumn}>
            {overview.waiting.length === 0 && <div className={styles.empty}>Nobody is waiting on you.</div>}
            <ul className={styles.list}>
              {overview.waiting.map(entry => (
                <li key={entry.room_id}>
                  <WaitingRow entry={entry} selected={entry.room_id === roomID} now={now}
                    onPick={() => setParam('room', entry.room_id)} onDismiss={() => void dismiss(entry)} />
                </li>
              ))}
            </ul>
            {overview.cleared.length > 0 && (
              <>
                <h3 className={styles.awaitingSectionHeading}>Cleared · last 7 days</h3>
                <ul className={styles.list}>
                  {overview.cleared.map(cleared => (
                    <li key={cleared.room_id}>
                      <ClearedRow cleared={cleared} selected={cleared.room_id === roomID} onPick={() => setParam('room', cleared.room_id)} />
                    </li>
                  ))}
                </ul>
              </>
            )}
          </div>
          <div className={styles.viewer}>
            <button type="button" className={styles.backToList} onClick={() => setParam('room', null)}>← Waiting on you</button>
            {roomID ? <RoomThread roomID={roomID} /> : <div className={styles.empty}>Pick a conversation to read and answer it here.</div>}
          </div>
        </div>
      )}
    </div>
  )
}

/** Clears the open room when it is waiting and the settings say opening
 *  clears: on opening it, and again when a message arrives while it is open,
 *  since the operator is looking at it. */
export function useClearOnOpen(roomID: string, onError: (error: string | null) => void) {
  const { overview, clear } = useAwaitingReply()
  const attempted = useRef('')
  useEffect(() => {
    const entry = entryToClearOnOpen(overview, roomID)
    if (!entry) return
    const key = `${entry.room_id} ${entry.newest_message_at}`
    if (attempted.current === key) return
    attempted.current = key
    void clear(entry, 'opened').then(result => onError(result.ok ? null : result.error))
  }, [overview, roomID, clear, onError])
}

function WaitingRow({ entry, selected, now, onPick, onDismiss }: {
  entry: WaitingConversation
  selected: boolean
  now: Date
  onPick: () => void
  onDismiss: () => void
}) {
  const senders = sendersLabel(entry)
  return (
    <div className={styles.awaitingRow}>
      <button type="button" data-room-id={entry.room_id}
        className={`${styles.row} ${styles.rowPick} ${selected ? styles.rowSelected : ''}`} onClick={onPick}>
        <span className={styles.rowHead}>
          <span className={styles.name} title={entry.name}><span className={styles.waitingDot} aria-label="waiting" />{entry.name}</span>
          <span className={styles.time} title={new Date(entry.newest_message_at).toLocaleString()}>{waitingAgeLabel(entry.newest_message_at, now)}</span>
        </span>
        <span className={styles.rowMeta}>
          <span className={styles.platform}>{entry.platform}</span>
          <span className={styles.muted}>{entry.direct_message ? 'direct message' : `group · ${entry.member_count}`}</span>
          {senders && <span className={styles.muted}>{senders}</span>}
          {entry.unanswered_message_count > 1 && <span className={styles.muted}>{entry.unanswered_message_count} messages</span>}
        </span>
        <span className={styles.preview}>{newestPreviewText(entry)}</span>
      </button>
      <button type="button" className={styles.awaitingDismiss} title="Dismiss: mark it seen without opening it" onClick={onDismiss}>✕</button>
    </div>
  )
}

function ClearedRow({ cleared, selected, onPick }: { cleared: ClearedConversation; selected: boolean; onPick: () => void }) {
  return (
    <button type="button" data-room-id={cleared.room_id}
      className={`${styles.row} ${styles.rowPick} ${styles.rowHidden} ${selected ? styles.rowSelected : ''}`} onClick={onPick}>
      <span className={styles.rowHead}>
        <span className={styles.name} title={cleared.name}>{cleared.name}</span>
        <span className={styles.time}>{new Date(cleared.cleared_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}</span>
      </span>
      <span className={styles.rowMeta}>
        <span className={styles.platform}>{cleared.platform}</span>
        <span className={styles.muted}>{clearedByLabel(cleared)}</span>
      </span>
      <span className={styles.preview}>{previewMessageText(cleared.last_message)}</span>
    </button>
  )
}

/** The thread of one room, with its composer, as on Conversations. */
export function RoomThread({ roomID }: { roomID: string }) {
  const { routes } = useBridgeConfig()
  const { conversation, missing, error, reload } = useConversationOf(roomID)
  if (error) return <pre className={styles.error}>{error}</pre>
  if (missing) return <div className={styles.empty}>multichat lists no conversation {roomID}.</div>
  if (!conversation) return <div className={styles.empty}>Loading…</div>
  return (
    <ConversationThread key={conversation.room_id} conversations={[conversation]} onSent={() => { void reload() }} shortName={null}
      tagEditor={<Link className={styles.muted} to={`${routes.messageConversations}?room=${encodeURIComponent(roomID)}`}>Tags, links and visibility on Conversations ↗</Link>} />
  )
}
