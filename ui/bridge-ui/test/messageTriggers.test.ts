import { describe, expect, it } from 'vitest'
import {
  createBodyOfMessageTriggerDraft, createMessageTrigger, describeMessageTrigger, draftOfMessageTrigger,
  emptyMessageTriggerDraft, patchBodyOfMessageTriggerDraft, type MessageTrigger,
} from '../src/messageTriggers'
import type { FetchFn } from '../src/types'
import type { Column } from '@kayushkin/kanban-store-types'

const COLUMN_FILTER_KINDS = ['card_moved']

const storedP0Trigger: MessageTrigger = {
  id: 't1', board_id: 'b1', name: 'P0 created', event_kind: 'card_created', priority_value: 5, priority_label: 'P0',
  recipient_user_id: '@whatsapp_1:chat.test', recipient_display_name: 'On-call', message_template: '{{.Title}}',
  enabled: true, created_at: '2026-09-11T00:00:00Z', updated_at: '2026-09-11T00:00:00Z',
}

const doneColumn = { id: 'c-done', board_id: 'b1', name: 'Done' } as Column

describe('message trigger form rules', () => {
  it('sends a column only for a kind that takes one, and a priority as the rung value', () => {
    const draft = {
      ...emptyMessageTriggerDraft(['card_created', 'card_moved']),
      name: 'x', toColumnID: 'c-done', priorityValue: '5', recipientUserID: ' @telegram_1:chat.test ', messageTemplate: '{{.Title}}',
    }
    expect(draft.eventKind).toBe('card_created')
    expect(createBodyOfMessageTriggerDraft(draft, COLUMN_FILTER_KINDS)).toEqual({
      name: 'x', event_kind: 'card_created', recipient_user_id: '@telegram_1:chat.test', message_template: '{{.Title}}', priority_value: 5,
    })
    expect(createBodyOfMessageTriggerDraft({ ...draft, eventKind: 'card_moved' }, COLUMN_FILTER_KINDS)).toMatchObject({
      event_kind: 'card_moved', to_column_id: 'c-done',
    })
  })

  it('patches nothing for an untouched trigger', () => {
    expect(patchBodyOfMessageTriggerDraft(storedP0Trigger, draftOfMessageTrigger(storedP0Trigger), COLUMN_FILTER_KINDS)).toEqual({})
  })

  it('drops the priority filter with clear_priority rather than a sentinel value', () => {
    const draft = { ...draftOfMessageTrigger(storedP0Trigger), priorityValue: '' }
    expect(patchBodyOfMessageTriggerDraft(storedP0Trigger, draft, COLUMN_FILTER_KINDS)).toEqual({ clear_priority: true })
  })

  it('clears the column when the kind no longer takes one', () => {
    const moved: MessageTrigger = { ...storedP0Trigger, event_kind: 'card_moved', to_column_id: 'c-done' }
    const draft = { ...draftOfMessageTrigger(moved), eventKind: 'card_created' }
    expect(patchBodyOfMessageTriggerDraft(moved, draft, COLUMN_FILTER_KINDS)).toEqual({ event_kind: 'card_created', to_column_id: '' })
  })

  it('clears the display name with an empty string', () => {
    const draft = { ...draftOfMessageTrigger(storedP0Trigger), recipientDisplayName: '  ' }
    expect(patchBodyOfMessageTriggerDraft(storedP0Trigger, draft, COLUMN_FILTER_KINDS)).toEqual({ recipient_display_name: '' })
  })

  it('describes a trigger by its column and rung, and says when either is gone', () => {
    const moved: MessageTrigger = { ...storedP0Trigger, event_kind: 'card_moved', to_column_id: 'c-done' }
    expect(describeMessageTrigger(moved, [doneColumn])).toBe('card_moved into Done at P0 → On-call (@whatsapp_1:chat.test)')
    const orphaned: MessageTrigger = { ...moved, priority_label: undefined, recipient_display_name: undefined }
    expect(describeMessageTrigger(orphaned, [])).toBe(
      'card_moved into c-done (no longer on this board) at priority 5 (no longer a rung on the ladder) → @whatsapp_1:chat.test',
    )
  })
})

describe('message trigger calls', () => {
  it('POSTs the body to the board’s trigger list', async () => {
    let seen: { url: string; method?: string; body?: unknown } | null = null
    const fetchFn = (async (url: string, init?: RequestInit) => {
      seen = { url, method: init?.method, body: JSON.parse(String(init?.body)) }
      return new Response(JSON.stringify(storedP0Trigger), { status: 201 })
    }) as unknown as FetchFn
    const result = await createMessageTrigger(fetchFn, '/api/kanban', 'b1', { name: 'P0 created', event_kind: 'card_created' })
    expect(result.ok).toBe(true)
    expect(seen).toEqual({ url: '/api/kanban/api/boards/b1/message-triggers', method: 'POST', body: { name: 'P0 created', event_kind: 'card_created' } })
  })

  it('returns kanban-store’s refusal verbatim', async () => {
    const refusal = 'priority_value 4 is not a rung on this board\'s ladder (P0=5, P2=3)'
    const fetchFn = (async () => new Response(JSON.stringify({ error: refusal }), { status: 400 })) as unknown as FetchFn
    const result = await createMessageTrigger(fetchFn, '/api/kanban', 'b1', { priority_value: 4 })
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toBe(refusal)
  })
})
