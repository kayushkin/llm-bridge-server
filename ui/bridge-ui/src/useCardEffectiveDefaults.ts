import { useEffect, useState } from 'react'
import { useBridgeConfig } from './context'
import { getCardEffectiveDefaults } from './kanbanStoreClient'
import type { EffectiveDefaults } from '@kayushkin/kanban-store-types'

export interface CardEffectiveDefaultsRead {
  /** What kanban-store resolved for the card, each default with its source.
   *  Null while pending, for a card on no board, and after a failure. */
  effective: EffectiveDefaults | null
  error: string | null
  /** True until the store has answered for this board, card and tag set. A
   *  dispatch must not start while this is true: the instance and bundle it
   *  would use are not known yet. */
  pending: boolean
}

/**
 * One card's effective defaults on one board — the instance a dispatch would
 * run on, the bundle it is provisioned with, and where each came from (a tag
 * rule or the board) — read from kanban-store, which owns the precedence.
 *
 * `tagsKey` is any string that changes when the card's saved tags do (the
 * drawer passes them joined), so a save that retags the card re-reads what the
 * new tags select. The store reads the tags itself; the key only says when.
 *
 * A card on no board (`boardID` null) has no defaults and is never pending.
 */
export function useCardEffectiveDefaults(boardID: string | null, cardID: string, tagsKey: string): CardEffectiveDefaultsRead {
  const { fetch: fetchFn, kanbanStoreBasePath } = useBridgeConfig()
  const readKey = `${boardID ?? ''}\u0000${cardID}\u0000${tagsKey}`
  const [state, setState] = useState<{ effective: EffectiveDefaults | null; error: string | null; forReadKey: string | null }>(
    { effective: null, error: null, forReadKey: null },
  )
  useEffect(() => {
    if (!boardID) return
    if (!kanbanStoreBasePath) {
      setState({ effective: null, error: 'this host has no route to kanban-store, so the card’s defaults cannot be read', forReadKey: readKey })
      return
    }
    let canceled = false
    getCardEffectiveDefaults(fetchFn, kanbanStoreBasePath, boardID, cardID).then(result => {
      if (canceled) return
      setState(result.ok
        ? { effective: result.value, error: null, forReadKey: readKey }
        : { effective: null, error: result.error, forReadKey: readKey })
    })
    return () => { canceled = true }
  }, [fetchFn, kanbanStoreBasePath, boardID, cardID, readKey])
  if (!boardID) return { effective: null, error: null, pending: false }
  // An answer for another card or an older tag set must not stand in for this one's.
  if (state.forReadKey !== readKey) return { effective: null, error: null, pending: true }
  return { effective: state.effective, error: state.error, pending: false }
}
