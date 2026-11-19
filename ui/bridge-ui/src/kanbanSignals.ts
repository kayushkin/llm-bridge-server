import { useCallback, useEffect, useState } from 'react'
import {
  SIGNAL_STATE_OPEN,
  signalFromWire,
  subscribeToSignalChanges,
  type Signal,
  type SignalWire,
} from '@kayushkin/chat-core'
import { useBridgeConfig } from './context'
import type { FetchFn } from './types'

// The kanban's two signal reads: which todos have open signals (a badge per
// card) and the open signals against one todo (the drawer's "Needs you"). Both
// on chat-core's Signal model and its resolve announcements, so answering a
// question in the chat refreshes the board's badge and vice versa — the fork
// this replaced kept two listener sets that never heard each other.
//
// chat-core itself carries only the per-session reads: a todo is the kanban's
// unit, not the chat's, so the queries keyed on one live here.

/** A 404 from the signals route means this bridge-server predates the signals
 *  API. Reported as `null` rather than an error: a server without the feature
 *  is not a failure to show the user, and the surfaces render nothing. Any
 *  other failure is a real error and is surfaced. */
type SignalsResult = Signal[] | null

async function getOpenSignals(fetchFn: FetchFn, url: string): Promise<SignalsResult> {
  const res = await fetchFn(url)
  if (res.status === 404) return null
  if (!res.ok) throw new Error(`HTTP ${res.status} ${await res.text()}`)
  const body: unknown = await res.json()
  if (!Array.isArray(body)) throw new Error('signals response was not an array')
  return (body as SignalWire[]).map(signalFromWire)
}

/** Every open signal that names a todo, grouped by that todo — the board's read.
 *
 *  Surface is deliberately not filtered. A todo is worked by chat sessions and
 *  by autonomous workers alike, and the badge answers "does this piece of work
 *  need me?", which is true of a signal on either surface. */
export async function fetchOpenSignalsByTodo(fetchFn: FetchFn, basePath: string): Promise<Map<string, Signal[]> | null> {
  const params = new URLSearchParams({ state: SIGNAL_STATE_OPEN })
  const signals = await getOpenSignals(fetchFn, `${basePath}/signals?${params.toString()}`)
  if (signals === null) return null
  const byTodo = new Map<string, Signal[]>()
  for (const signal of signals) {
    // An unlinked signal belongs to no todo. It is not a signal on "the todo
    // with an empty id".
    if (!signal.linkedTodoId) continue
    const existing = byTodo.get(signal.linkedTodoId)
    if (existing) existing.push(signal); else byTodo.set(signal.linkedTodoId, [signal])
  }
  return byTodo
}

/** Open signals against exactly one todo — the drawer's read. Narrowed
 *  server-side, so a view that knows its todo fetches only its own rows. An
 *  empty todo id is a 400 from the server, so callers must not pass one. */
export function fetchOpenSignalsForTodo(fetchFn: FetchFn, basePath: string, todoID: string): Promise<SignalsResult> {
  const params = new URLSearchParams({ state: SIGNAL_STATE_OPEN, linked_todo_id: todoID })
  return getOpenSignals(fetchFn, `${basePath}/signals?${params.toString()}`)
}

/** Re-run on every resolve announced in this tab, from any surface. */
function useReloadOnSignalChange(): number {
  const [reloadToken, setReloadToken] = useState(0)
  const reload = useCallback(() => setReloadToken(t => t + 1), [])
  useEffect(() => subscribeToSignalChanges(reload), [reload])
  return reloadToken
}

/** Open signals grouped by the todo they propagate to, for a view that shows
 *  many todos at once. Empty until the first fetch lands, and empty forever
 *  against a bridge-server with no signals route — a board full of todos must
 *  render either way.
 *
 *  Takes no refresh key. The query has no dimension to key on: it asks for
 *  every open signal that names a todo, whatever board the caller is looking
 *  at. Keying it to the board id refetched an identical list on every
 *  selection change. Resolves still refresh it, through chat-core's announce. */
export function useOpenSignalsByTodo(): Map<string, Signal[]> {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [byTodo, setByTodo] = useState<Map<string, Signal[]>>(new Map())
  const reloadToken = useReloadOnSignalChange()

  useEffect(() => {
    let live = true
    fetchOpenSignalsByTodo(fetchFn, basePath)
      .then(result => { if (live) setByTodo(result ?? new Map()) })
      .catch(() => {
        // A board is not a signal surface. Failing to learn which todos have
        // open signals costs a badge; it must not cost the board.
        if (live) setByTodo(new Map())
      })
    return () => { live = false }
  }, [fetchFn, basePath, reloadToken])

  return byTodo
}

/** Open signals against exactly one todo, for a view already looking at that
 *  todo alone — the card drawer.
 *
 *  Deliberately not a lookup into useOpenSignalsByTodo's map. That map is the
 *  board's read; a drawer knows its own todo id and asks the server for its own
 *  rows, so it is right whether or not a board-wide read ever ran.
 *
 *  Empty against a bridge-server with no signals route, and empty when the read
 *  fails: a drawer is a card editor first, and it must open either way. */
export function useOpenSignalsForTodo(todoID: string): Signal[] {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [signals, setSignals] = useState<Signal[]>([])
  const reloadToken = useReloadOnSignalChange()

  useEffect(() => {
    // An empty todo id is a 400 from the server, not "every signal" — so it is
    // never asked. A drawer on a placement whose noteboard item is gone has no
    // todo to ask about.
    if (!todoID) { setSignals([]); return }
    let live = true
    fetchOpenSignalsForTodo(fetchFn, basePath, todoID)
      .then(result => { if (live) setSignals(result ?? []) })
      .catch(() => { if (live) setSignals([]) })
    return () => { live = false }
  }, [fetchFn, basePath, todoID, reloadToken])

  return signals
}
