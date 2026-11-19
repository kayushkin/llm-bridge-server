import { useCallback, useEffect, useId, useState } from 'react'
import { useBridgeConfig } from '../context'
import { getPriorityLadder, type KanbanStoreResult } from '../kanbanStoreClient'
import {
  createBodyOfMessageTriggerDraft, createMessageTrigger, deleteMessageTrigger, describeMessageTrigger, draftOfMessageTrigger,
  emptyMessageTriggerDraft, getMessageTriggerOptions, listBoardColumns, listMessageDeliveries, listMessageTriggers,
  patchBodyOfMessageTriggerDraft, patchMessageTrigger,
  type MessageDelivery, type MessageTrigger, type MessageTriggerDraft, type MessageTriggerOptions,
} from '../messageTriggers'
import { SettingsSection } from './settings/SettingsSection'
import type { Board, Column, PriorityLadder } from '@kayushkin/kanban-store-types'

const DELIVERIES_SHOWN = 20
const NEW_TRIGGER = 'new'

/**
 * Message triggers: text someone through multichat when something happens to a
 * card on this board — a P0 created, a card moved into Done, a card resolved.
 * Each trigger saves on its own and the list is re-read from kanban-store after
 * every write; a refusal is shown in the store's words next to what caused it.
 *
 * Below the triggers, what they did: the newest deliveries with the message as
 * it was rendered, so a trigger set up before kanban-store is pointed at
 * multichat shows it firing (`not_configured`) instead of looking dead.
 */
export function BoardMessageTriggersSection({ board }: { board: Board }) {
  const { fetch: fetchFn, kanbanStoreBasePath: base } = useBridgeConfig()
  const [options, setOptions] = useState<MessageTriggerOptions | null>(null)
  const [triggers, setTriggers] = useState<MessageTrigger[] | null>(null)
  const [deliveries, setDeliveries] = useState<MessageDelivery[] | null>(null)
  const [columns, setColumns] = useState<Column[]>([])
  const [ladder, setLadder] = useState<PriorityLadder | null>(null)
  const [loadErrors, setLoadErrors] = useState<string[]>([])
  const [editing, setEditing] = useState<string | null>(null)
  const [rowError, setRowError] = useState<{ triggerID: string; error: string } | null>(null)

  const reload = useCallback(async () => {
    const [optionsResult, triggersResult, deliveriesResult, columnsResult, ladderResult] = await Promise.all([
      getMessageTriggerOptions(fetchFn, base),
      listMessageTriggers(fetchFn, base, board.id),
      listMessageDeliveries(fetchFn, base, board.id, DELIVERIES_SHOWN),
      listBoardColumns(fetchFn, base, board.id),
      getPriorityLadder(fetchFn, base, board.id),
    ])
    const errors: string[] = []
    if (optionsResult.ok) setOptions(optionsResult.value); else errors.push(optionsResult.error)
    if (triggersResult.ok) setTriggers(triggersResult.value); else errors.push(triggersResult.error)
    if (deliveriesResult.ok) setDeliveries(deliveriesResult.value); else errors.push(deliveriesResult.error)
    if (columnsResult.ok) setColumns(columnsResult.value); else errors.push(columnsResult.error)
    if (ladderResult.ok) setLadder(ladderResult.value); else errors.push(ladderResult.error)
    setLoadErrors(errors)
  }, [fetchFn, base, board.id])

  useEffect(() => { void reload() }, [reload])

  const toggleEnabled = async (trigger: MessageTrigger) => {
    const result = await patchMessageTrigger(fetchFn, base, trigger.id, { enabled: !trigger.enabled })
    if (!result.ok) {
      setRowError({ triggerID: trigger.id, error: result.error })
      return
    }
    setRowError(null)
    await reload()
  }

  const remove = async (trigger: MessageTrigger) => {
    if (typeof window !== 'undefined' && !window.confirm(`Delete the trigger "${trigger.name}"? Its delivery history is deleted with it; switching it off keeps the history.`)) return
    const result = await deleteMessageTrigger(fetchFn, base, trigger.id)
    if (!result.ok) {
      setRowError({ triggerID: trigger.id, error: result.error })
      return
    }
    setRowError(null)
    if (editing === trigger.id) setEditing(null)
    await reload()
  }

  const create = async (draft: MessageTriggerDraft): Promise<KanbanStoreResult<unknown>> => {
    if (!options) return { ok: false, error: 'the trigger options have not loaded' }
    const result = await createMessageTrigger(fetchFn, base, board.id, createBodyOfMessageTriggerDraft(draft, options.column_filter_event_kinds))
    if (result.ok) {
      setEditing(null)
      await reload()
    }
    return result
  }

  const update = async (trigger: MessageTrigger, draft: MessageTriggerDraft): Promise<KanbanStoreResult<unknown>> => {
    if (!options) return { ok: false, error: 'the trigger options have not loaded' }
    const patch = patchBodyOfMessageTriggerDraft(trigger, draft, options.column_filter_event_kinds)
    if (Object.keys(patch).length === 0) {
      setEditing(null)
      return { ok: true, value: trigger }
    }
    const result = await patchMessageTrigger(fetchFn, base, trigger.id, patch)
    if (result.ok) {
      setEditing(null)
      await reload()
    }
    return result
  }

  const triggerName = (triggerID: string) => triggers?.find(trigger => trigger.id === triggerID)?.name ?? triggerID

  return (
    <SettingsSection id="message-triggers" title="Message triggers" scope="board" storedBy="kanban-store · board message triggers"
      precedence="Each trigger saves on its own; delivery is through multichat and is recorded per firing, on or off.">
      <p className="bks-help">
        Send a text message through multichat when something happens to a card on this board. A human writes the
        message and picks who gets it, so it goes out without an agent or a permission prompt.
      </p>
      {options && !options.delivery_configured && (
        <div className="bks-notice" data-notice="delivery-not-configured">
          kanban-store is not connected to multichat (no <code>MULTICHAT_URL</code>). Triggers still match and render,
          and each is recorded below as <code>not_configured</code> with the message it would have sent — nothing is sent.
        </div>
      )}
      {loadErrors.map(error => <div key={error} className="bridge-error bks-error">{error}</div>)}

      {triggers === null ? (
        loadErrors.length === 0 && <div className="bi-loading">Loading…</div>
      ) : (
        <div className="bks-triggers">
          {triggers.length === 0 && <span className="bks-help">No triggers on this board.</span>}
          {triggers.map(trigger => (
            <div key={trigger.id} className="bks-trigger" data-trigger-id={trigger.id}>
              <div className={`bks-trigger-row${trigger.enabled ? '' : ' is-disabled'}`}>
                <label className="bks-check" title={trigger.enabled ? 'Switch off; its history is kept' : 'Switch on'}>
                  <input type="checkbox" checked={trigger.enabled} onChange={() => { void toggleEnabled(trigger) }} />
                  <span className="bks-trigger-name">{trigger.name}</span>
                </label>
                <span className="bks-trigger-summary">{describeMessageTrigger(trigger, columns)}</span>
                <button type="button" className="bp-cancel" onClick={() => setEditing(editing === trigger.id ? null : trigger.id)}>
                  {editing === trigger.id ? 'close' : 'edit'}
                </button>
                <button type="button" className="bp-cancel" onClick={() => { void remove(trigger) }}>delete</button>
              </div>
              {rowError?.triggerID === trigger.id && <div className="bridge-error bks-error">{rowError.error}</div>}
              {editing === trigger.id && options && (
                <MessageTriggerEditor
                  initial={draftOfMessageTrigger(trigger)}
                  options={options}
                  columns={columns}
                  ladder={ladder}
                  submitLabel="Save trigger"
                  onSubmit={draft => update(trigger, draft)}
                  onCancel={() => setEditing(null)}
                />
              )}
            </div>
          ))}
        </div>
      )}

      {editing === NEW_TRIGGER && options ? (
        <MessageTriggerEditor
          initial={emptyMessageTriggerDraft(options.event_kinds)}
          options={options}
          columns={columns}
          ladder={ladder}
          submitLabel="Create trigger"
          onSubmit={create}
          onCancel={() => setEditing(null)}
        />
      ) : (
        <div className="bks-actions">
          <button type="button" className="bi-save-btn" disabled={!options} onClick={() => setEditing(NEW_TRIGGER)}>New trigger</button>
        </div>
      )}

      <div className="bks-field">
        <div className="bks-actions">
          <span className="bks-field-label">Recent deliveries</span>
          <button type="button" className="bp-cancel" onClick={() => { void reload() }}>refresh</button>
        </div>
        {deliveries !== null && deliveries.length === 0 && <span className="bks-help">No trigger has fired on this board yet.</span>}
        <div className="bks-deliveries">
          {(deliveries ?? []).map(delivery => (
            <div key={delivery.id} className="bks-delivery" data-status={delivery.status}>
              <span className="bks-delivery-time">{new Date(delivery.created_at).toLocaleString()}</span>
              <span className="bks-delivery-status" data-status={delivery.status}>{delivery.status}</span>
              <div className="bks-delivery-body">
                <div className="bks-help">
                  {triggerName(delivery.trigger_id)} → {delivery.recipient_user_id} · card <span className="bp-id">{delivery.card_id}</span>
                </div>
                {delivery.rendered_message && <div className="bks-delivery-message">{delivery.rendered_message}</div>}
                {delivery.error && <div className="bks-error">{delivery.error}</div>}
              </div>
            </div>
          ))}
        </div>
      </div>
    </SettingsSection>
  )
}

function MessageTriggerEditor({ initial, options, columns, ladder, submitLabel, onSubmit, onCancel }: {
  initial: MessageTriggerDraft
  options: MessageTriggerOptions
  columns: Column[]
  /** Null when the ladder could not be read; the load error is already shown. */
  ladder: PriorityLadder | null
  submitLabel: string
  onSubmit: (draft: MessageTriggerDraft) => Promise<KanbanStoreResult<unknown>>
  onCancel: () => void
}) {
  const id = useId()
  const [draft, setDraft] = useState(initial)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const takesColumn = options.column_filter_event_kinds.includes(draft.eventKind)
  const levels = ladder ? ladder.levels : []

  const submit = async () => {
    setSaving(true)
    const result = await onSubmit(draft)
    setSaving(false)
    setError(result.ok ? null : result.error)
  }

  return (
    <div className="bks-trigger-editor">
      <label className="bks-field" htmlFor={`${id}-name`}>
        <span className="bks-field-label">Name</span>
        <input id={`${id}-name`} value={draft.name} placeholder="P0 created → on-call" onChange={e => setDraft({ ...draft, name: e.target.value })} />
      </label>
      <div className="bks-row">
        <label className="bks-field" htmlFor={`${id}-kind`}>
          <span className="bks-field-label">When</span>
          <select id={`${id}-kind`} value={draft.eventKind} onChange={e => setDraft({ ...draft, eventKind: e.target.value })}>
            {options.event_kinds.map(kind => <option key={kind} value={kind}>{kind}</option>)}
          </select>
        </label>
        {takesColumn && (
          <label className="bks-field" htmlFor={`${id}-column`}>
            <span className="bks-field-label">Into column</span>
            <select id={`${id}-column`} value={draft.toColumnID} onChange={e => setDraft({ ...draft, toColumnID: e.target.value })}>
              <option value="">any column</option>
              {columns.map(column => <option key={column.id} value={column.id}>{column.name}</option>)}
            </select>
          </label>
        )}
        <label className="bks-field" htmlFor={`${id}-priority`}>
          <span className="bks-field-label">Priority</span>
          <select id={`${id}-priority`} value={draft.priorityValue} onChange={e => setDraft({ ...draft, priorityValue: e.target.value })} disabled={levels.length === 0 && draft.priorityValue === ''}>
            <option value="">any priority</option>
            {levels.map(level => <option key={level.priority_value} value={String(level.priority_value)}>{level.label}</option>)}
          </select>
        </label>
      </div>
      {levels.length === 0 && <span className="bks-help">This board has no priority ladder, so a trigger cannot filter by priority.</span>}
      <div className="bks-row">
        <label className="bks-field" htmlFor={`${id}-recipient`}>
          <span className="bks-field-label">Send to (multichat user id)</span>
          <input
            id={`${id}-recipient`}
            className="bks-id-input"
            value={draft.recipientUserID}
            placeholder="@whatsapp_15551234567:chat.kayushkin.com"
            onChange={e => setDraft({ ...draft, recipientUserID: e.target.value })}
          />
        </label>
        <label className="bks-field" htmlFor={`${id}-recipient-name`}>
          <span className="bks-field-label">Their name (display only)</span>
          <input id={`${id}-recipient-name`} value={draft.recipientDisplayName} onChange={e => setDraft({ ...draft, recipientDisplayName: e.target.value })} />
        </label>
      </div>
      <span className="bks-help">
        The bridge puppet id of the person on the app to use, from multichat's contacts (<code>GET /api/contacts/unified</code>,
        an identity's <code>user_id</code>). The id picks the app: <code>@whatsapp_…</code>, <code>@telegram_…</code>,
        <code>@signal_…</code>, <code>@meta_…</code>.
      </span>
      <label className="bks-field" htmlFor={`${id}-template`}>
        <span className="bks-field-label">Message</span>
        <textarea
          id={`${id}-template`}
          className="bks-trigger-template"
          value={draft.messageTemplate}
          placeholder="{{.PriorityLabel}} on {{.BoardName}}: {{.Title}} ({{.ColumnName}})"
          onChange={e => setDraft({ ...draft, messageTemplate: e.target.value })}
        />
        <span className="bks-help">
          A Go template. Fields: {options.template_fields.map(field => `{{.${field}}}`).join(', ')}.
        </span>
      </label>
      <div className="bks-actions">
        <button type="button" className="bi-save-btn" disabled={saving} onClick={() => { void submit() }}>
          {saving ? 'Saving…' : submitLabel}
        </button>
        <button type="button" className="bp-cancel" onClick={onCancel}>cancel</button>
        {error && <div className="bridge-error bks-error">{error}</div>}
      </div>
    </div>
  )
}
