// A board's message triggers: when a card on the board is created, moved,
// completed, assigned or held, kanban-store renders a template and sends it to
// one person through multichat. This file is the wire contract (kanban-store
// README "Message triggers") as types, typed calls over the host's fetch, and
// the pure rules that turn a form into a request body — no React, so the
// bodies are pinned by tests without a DOM.

import type { FetchFn } from './types'
import type { Column, MessageTrigger, MessageDelivery, MessageTriggerOptions, UpsertMessageTriggerRequest } from '@kayushkin/kanban-store-types'
// Re-exported so callers that took these from here keep working; the
// definitions are kanban-store's, rendered from its Go types.
export type { MessageTrigger, MessageDelivery, MessageDeliveryStatus, MessageTriggerOptions, UpsertMessageTriggerRequest } from '@kayushkin/kanban-store-types'
import type { KanbanStoreResult } from './kanbanStoreClient'
import { readErrorText } from './useKanban'

/** `pending`: handed to multichat, no answer recorded. `not_configured`: kanban-store has no MULTICHAT_URL. */

/** `GET /api/message-trigger-options` — everything the form offers, served rather than hardcoded. */

/** The body of `POST /api/boards/{id}/message-triggers` and `PATCH /api/message-triggers/{id}`.
 *  On PATCH an omitted key is left alone, `""` clears a string, and
 *  `clear_priority` drops the priority filter. */

// --- calls -------------------------------------------------------------------

async function send<T>(fetchFn: FetchFn, verb: string, url: string, init?: RequestInit): Promise<KanbanStoreResult<T>> {
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

function jsonInit(method: string, body: unknown): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
}

function boardURL(kanbanStoreBasePath: string, boardID: string): string {
  return `${kanbanStoreBasePath}/api/boards/${encodeURIComponent(boardID)}`
}

export function getMessageTriggerOptions(fetchFn: FetchFn, kanbanStoreBasePath: string): Promise<KanbanStoreResult<MessageTriggerOptions>> {
  return send(fetchFn, 'read message trigger options', `${kanbanStoreBasePath}/api/message-trigger-options`)
}

export function listMessageTriggers(fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string): Promise<KanbanStoreResult<MessageTrigger[]>> {
  return send(fetchFn, 'read message triggers', `${boardURL(kanbanStoreBasePath, boardID)}/message-triggers`)
}

export function createMessageTrigger(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, body: UpsertMessageTriggerRequest,
): Promise<KanbanStoreResult<MessageTrigger>> {
  return send(fetchFn, 'create message trigger', `${boardURL(kanbanStoreBasePath, boardID)}/message-triggers`, jsonInit('POST', body))
}

export function patchMessageTrigger(
  fetchFn: FetchFn, kanbanStoreBasePath: string, triggerID: string, body: UpsertMessageTriggerRequest,
): Promise<KanbanStoreResult<MessageTrigger>> {
  return send(fetchFn, 'save message trigger', `${kanbanStoreBasePath}/api/message-triggers/${encodeURIComponent(triggerID)}`, jsonInit('PATCH', body))
}

export function deleteMessageTrigger(fetchFn: FetchFn, kanbanStoreBasePath: string, triggerID: string): Promise<KanbanStoreResult<unknown>> {
  return send(fetchFn, 'delete message trigger', `${kanbanStoreBasePath}/api/message-triggers/${encodeURIComponent(triggerID)}`, { method: 'DELETE' })
}

export function listMessageDeliveries(
  fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string, limit: number,
): Promise<KanbanStoreResult<MessageDelivery[]>> {
  return send(fetchFn, 'read message deliveries', `${boardURL(kanbanStoreBasePath, boardID)}/message-deliveries?limit=${limit}`)
}

export function listBoardColumns(fetchFn: FetchFn, kanbanStoreBasePath: string, boardID: string): Promise<KanbanStoreResult<Column[]>> {
  return send(fetchFn, 'read columns', `${boardURL(kanbanStoreBasePath, boardID)}/columns`)
}

// --- form rules --------------------------------------------------------------

export interface MessageTriggerDraft {
  name: string
  eventKind: string
  /** '' means any column. */
  toColumnID: string
  /** '' means any priority; otherwise the rung's value as text, as a select holds it. */
  priorityValue: string
  recipientUserID: string
  recipientDisplayName: string
  messageTemplate: string
}

/** A blank form, with the first served event kind selected. */
export function emptyMessageTriggerDraft(eventKinds: readonly string[]): MessageTriggerDraft {
  return {
    name: '', eventKind: eventKinds.length > 0 ? eventKinds[0] : '', toColumnID: '', priorityValue: '',
    recipientUserID: '', recipientDisplayName: '', messageTemplate: '',
  }
}

export function draftOfMessageTrigger(trigger: MessageTrigger): MessageTriggerDraft {
  return {
    name: trigger.name,
    eventKind: trigger.event_kind,
    toColumnID: trigger.to_column_id ?? '',
    priorityValue: trigger.priority_value === undefined ? '' : String(trigger.priority_value),
    recipientUserID: trigger.recipient_user_id,
    recipientDisplayName: trigger.recipient_display_name ?? '',
    messageTemplate: trigger.message_template,
  }
}

/** The POST body. A column is sent only for a kind that takes one, so switching
 *  a draft away from `card_moved` cannot leave a column the store would refuse. */
export function createBodyOfMessageTriggerDraft(draft: MessageTriggerDraft, columnFilterEventKinds: readonly string[]): UpsertMessageTriggerRequest {
  const body: UpsertMessageTriggerRequest = {
    name: draft.name,
    event_kind: draft.eventKind,
    recipient_user_id: draft.recipientUserID.trim(),
    message_template: draft.messageTemplate,
  }
  const displayName = draft.recipientDisplayName.trim()
  if (displayName) body.recipient_display_name = displayName
  if (draft.toColumnID && columnFilterEventKinds.includes(draft.eventKind)) body.to_column_id = draft.toColumnID
  if (draft.priorityValue !== '') body.priority_value = Number(draft.priorityValue)
  return body
}

/** The PATCH body: only what differs from the stored trigger. */
export function patchBodyOfMessageTriggerDraft(
  trigger: MessageTrigger, draft: MessageTriggerDraft, columnFilterEventKinds: readonly string[],
): UpsertMessageTriggerRequest {
  const next = createBodyOfMessageTriggerDraft(draft, columnFilterEventKinds)
  const patch: UpsertMessageTriggerRequest = {}
  if (next.name !== trigger.name) patch.name = next.name
  if (next.event_kind !== trigger.event_kind) patch.event_kind = next.event_kind
  if ((next.to_column_id ?? '') !== (trigger.to_column_id ?? '')) patch.to_column_id = next.to_column_id ?? ''
  if (next.priority_value === undefined) {
    if (trigger.priority_value !== undefined) patch.clear_priority = true
  } else if (next.priority_value !== trigger.priority_value) {
    patch.priority_value = next.priority_value
  }
  if (next.recipient_user_id !== trigger.recipient_user_id) patch.recipient_user_id = next.recipient_user_id
  if ((next.recipient_display_name ?? '') !== (trigger.recipient_display_name ?? '')) patch.recipient_display_name = next.recipient_display_name ?? ''
  if (next.message_template !== trigger.message_template) patch.message_template = next.message_template
  return patch
}

/** One line for the trigger list: what fires it and who hears. A column or
 *  rung that is gone from the board is said so, since such a trigger can no
 *  longer fire. */
export function describeMessageTrigger(trigger: MessageTrigger, columns: readonly Column[]): string {
  const parts = [trigger.event_kind]
  if (trigger.to_column_id) {
    const column = columns.find(candidate => candidate.id === trigger.to_column_id)
    parts.push(column ? `into ${column.name}` : `into ${trigger.to_column_id} (no longer on this board)`)
  }
  if (trigger.priority_value !== undefined) {
    parts.push(trigger.priority_label ? `at ${trigger.priority_label}` : `at priority ${trigger.priority_value} (no longer a rung on the ladder)`)
  }
  const recipient = trigger.recipient_display_name
    ? `${trigger.recipient_display_name} (${trigger.recipient_user_id})`
    : trigger.recipient_user_id
  return `${parts.join(' ')} → ${recipient}`
}
