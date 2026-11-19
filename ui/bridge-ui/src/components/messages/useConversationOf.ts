import { useCallback, useEffect, useState } from 'react'
import type { MultichatConversation } from '../../types-multichat'
import { errorText, useMultichat } from './useMultichat'

/** One room of multichat's conversation list, for drawing its thread outside
 *  the Conversations page. multichat serves the list from memory, so reading
 *  it whole for one room is cheap. `conversation` is null until read, and when
 *  multichat lists no such room `missing` says so. */
export function useConversationOf(roomID: string) {
  const { read, configured } = useMultichat()
  const [conversation, setConversation] = useState<MultichatConversation | null>(null)
  const [missing, setMissing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const reload = useCallback(async () => {
    try {
      const conversations = await read<MultichatConversation[] | null>('/conversations') ?? []
      const found = conversations.find(candidate => candidate.room_id === roomID) ?? null
      setConversation(found)
      setMissing(!found)
      setError(null)
    } catch (err) {
      setError(errorText(err))
    }
  }, [read, roomID])

  useEffect(() => {
    setConversation(null)
    setMissing(false)
    if (configured && roomID) void reload()
  }, [configured, roomID, reload])

  return { conversation, missing, error, reload }
}
