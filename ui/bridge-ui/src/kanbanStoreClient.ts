import type { FetchFn } from './types'
import type { AssignmentStrategy, Board, BoardTagRuleInput, BoardTagRules, EffectiveDefaults, EntityCardView, PriorityLadder, TicketList, TicketView } from '@kayushkin/kanban-store-types'
import type { BoardSettingsPatch, LadderWireLevel } from './kanbanBoardSettings'
import { readErrorText } from './useKanban'

// The board-level reads and writes behind the board settings page, as typed
// functions over the host's authenticated fetch. `useKanban` carries the
// card-level verbs; these are the ones the board's own record needs.
//
// Every call answers a `KanbanStoreResult`: the stored record on success, and
// on failure kanban-store's own `{"error":"…"}` text, verbatim — its refusals
// name the field and the owner that said no (an unknown or disabled principal,
// a slug where a numeric agent id belongs, a bundle name instead of its id,
// half a classifier, a zero rung), so the page shows them as they arrive.

export type KanbanStoreResult<T> = { ok: true; value: T } | { ok: false; error: string }

async function request<T>(fetchFn: FetchFn, verb: string, url: string, init?: RequestInit): Promise<KanbanStoreResult<T>> {
  let res: Response
  try {
    res = await fetchFn(url, init)
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}` }
  }
  if (!res.ok) return { ok: false, error: await readErrorText(res, verb) }
  try {
    return { ok: true, value: (await res.json()) as T }
  } catch (err) {
    return { ok: false, error: `${verb}: response was not JSON (${err instanceof Error ? err.message : String(err)})` }
  }
}

function boardURL(kanbanStoreBasePath: string, boardID: string): string {
  return `${kanbanStoreBasePath}/api/boards/${encodeURIComponent(boardID)}`
}

/** `GET /api/boards/{id}` — the board with its settings. A cleared setting is
 *  absent on the wire, never an empty string. */
export function getBoard(fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string): Promise<KanbanStoreResult<Board>> {
  return request(fetchFn, 'read board', boardURL(kanbanStoreBasePath, boardID))
}

/** `PATCH /api/boards/{id}` — any subset of the settings. Omitted keys are left
 *  alone; the store checks each id with its owner before writing anything, so
 *  a refusal means nothing changed. Answers the stored board. */
export function patchBoard(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, patch: BoardSettingsPatch,
): Promise<KanbanStoreResult<Board>> {
  return request(fetchFn, 'save board', boardURL(kanbanStoreBasePath, boardID), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
}

/** `GET /api/assignment-strategies` — the strategies an assignment pool may
 *  name. Served so no page keeps its own copy of the list. */
export function listAssignmentStrategies(fetchFn: FetchFn, kanbanStoreBasePath: string): Promise<KanbanStoreResult<AssignmentStrategy[]>> {
  return request(fetchFn, 'list assignment strategies', `${kanbanStoreBasePath}/api/assignment-strategies`)
}

export function getPriorityLadder(fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string): Promise<KanbanStoreResult<PriorityLadder>> {
  return request(fetchFn, 'read priority ladder', `${boardURL(kanbanStoreBasePath, boardID)}/priority-levels`)
}

/** `PUT /api/boards/{id}/priority-levels` — the whole ladder, replaced. */
export function putPriorityLadder(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, body: { levels: LadderWireLevel[] },
): Promise<KanbanStoreResult<PriorityLadder>> {
  return request(fetchFn, 'save priority ladder', `${boardURL(kanbanStoreBasePath, boardID)}/priority-levels`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** `GET /api/boards/{id}/tag-rules` — the board's tag rules, in order. */
export function getTagRules(fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string): Promise<KanbanStoreResult<BoardTagRules>> {
  return request(fetchFn, 'read tag rules', `${boardURL(kanbanStoreBasePath, boardID)}/tag-rules`)
}

/** `PUT /api/boards/{id}/tag-rules` — the whole list, replaced in the order
 *  sent; a rule sent with its `id` is kept, `[]` removes every rule. The store
 *  refuses the whole list (400 naming `rules[i]` and its tags, 502 when an
 *  owner cannot answer) and writes nothing on any refusal. */
export function putTagRules(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, body: { rules: BoardTagRuleInput[] },
): Promise<KanbanStoreResult<BoardTagRules>> {
  return request(fetchFn, 'save tag rules', `${boardURL(kanbanStoreBasePath, boardID)}/tag-rules`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** `GET /api/boards/{id}/cards/{card_id}/effective-defaults` — what this card
 *  gets on this board, with the card's tags read from noteboard by the store.
 *  The precedence lives in kanban-store and nowhere else. */
export function getCardEffectiveDefaults(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, cardID: string,
): Promise<KanbanStoreResult<EffectiveDefaults>> {
  return request(
    fetchFn, 'read card effective defaults',
    `${boardURL(kanbanStoreBasePath, boardID)}/cards/${encodeURIComponent(cardID)}/effective-defaults`,
  )
}

/** `GET /api/boards/{id}/effective-defaults?tag=…&tag=…` — what a card carrying
 *  exactly these tags would get on this board. No tags asks for the board's own. */
export function getEffectiveDefaultsForTags(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, tags: readonly string[],
): Promise<KanbanStoreResult<EffectiveDefaults>> {
  const query = new URLSearchParams(tags.map(tag => ['tag', tag])).toString()
  return request(
    fetchFn, 'read effective defaults',
    `${boardURL(kanbanStoreBasePath, boardID)}/effective-defaults${query ? `?${query}` : ''}`,
  )
}

/** `GET /api/tickets` — tickets the caller can view, newest first, across
 *  boards. `before` is the previous page's `next_before`. */
export function listTickets(
  fetchFn: FetchFn, kanbanStoreBasePath: string, query: { channel?: string; before?: string; limit?: number },
): Promise<KanbanStoreResult<TicketList>> {
  const params = new URLSearchParams()
  if (query.channel) params.set('channel', query.channel)
  if (query.before) params.set('before', query.before)
  if (query.limit) params.set('limit', String(query.limit))
  const suffix = params.toString()
  return request(fetchFn, 'list tickets', `${kanbanStoreBasePath}/api/tickets${suffix ? `?${suffix}` : ''}`)
}

/** `GET /api/entities/{type}/{ref}/cards` — the cards that link one entity,
 *  such as every card an email was filed onto. */
export function listCardsForEntity(
  fetchFn: FetchFn, kanbanStoreBasePath: string, entityType: string, entityRef: string,
): Promise<KanbanStoreResult<EntityCardView[]>> {
  return request(fetchFn, 'list cards for entity',
    `${kanbanStoreBasePath}/api/entities/${encodeURIComponent(entityType)}/${encodeURIComponent(entityRef)}/cards`)
}

/** `GET /api/cards/{id}/ticket` — the card's ticket, or null when the card is
 *  not one (the store answers 404 for that, which is not a failure here). */
export async function getCardTicket(
  fetchFn: FetchFn, kanbanStoreBasePath: string, cardID: string,
): Promise<KanbanStoreResult<TicketView | null>> {
  const url = `${kanbanStoreBasePath}/api/cards/${encodeURIComponent(cardID)}/ticket`
  let res: Response
  try {
    res = await fetchFn(url)
  } catch (err) {
    return { ok: false, error: `read ticket: ${err instanceof Error ? err.message : String(err)}` }
  }
  if (res.status === 404) return { ok: true, value: null }
  if (!res.ok) return { ok: false, error: await readErrorText(res, 'read ticket') }
  return { ok: true, value: (await res.json()) as TicketView }
}
