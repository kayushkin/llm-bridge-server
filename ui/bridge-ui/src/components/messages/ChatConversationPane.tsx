import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../../context'
import { RoomThread, useClearOnOpen } from './BridgeMessagesWaiting'
import styles from './Messages.module.css'

/** A multichat conversation drawn in the chat's thread pane (`?view=conversation&room=`),
 *  opened from the sidebar's Waiting on you section or a pop-up. Opening a
 *  waiting room here clears it, as on the Waiting on you page. */
export function ChatConversationPane({ roomID, onClose }: { roomID: string; onClose: () => void }) {
  const { routes } = useBridgeConfig()
  const [clearError, setClearError] = useState<string | null>(null)
  const onClearError = useCallback((error: string | null) => setClearError(error), [])
  useClearOnOpen(roomID, onClearError)
  return (
    <div className={styles.chatConversationPane}>
      <div className={styles.chatConversationBar}>
        <button type="button" className={styles.olderButton} onClick={onClose}>← Back</button>
        <Link className={styles.muted} to={`${routes.messageWaiting}?room=${encodeURIComponent(roomID)}`}>Open in Messages ↗</Link>
      </div>
      {clearError && <pre className={styles.error}>{clearError}</pre>}
      <div className={`${styles.viewer} ${styles.chatConversationViewer}`}>
        <RoomThread roomID={roomID} />
      </div>
    </div>
  )
}
