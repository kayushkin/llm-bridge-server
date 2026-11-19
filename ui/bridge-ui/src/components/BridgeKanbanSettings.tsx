import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import { KANBAN_LAST_BOARD_STORAGE_KEY } from '../constants'
import { useKanban } from '../useKanban'
import { getBoard, getPriorityLadder, listAssignmentStrategies, patchBoard, putPriorityLadder, type KanbanStoreResult } from '../kanbanStoreClient'
import { listMailAccounts, type MailAccount } from '../mailstackClient'
import { BoardMessageTriggersSection } from './BoardMessageTriggersSection'
import { BoardTagRulesSection } from './BoardTagRulesSection'
import { IdField, useDefaultPickers } from './BoardDefaultIdFields'
import { SettingsSection, useSectionSave } from './settings/SettingsSection'
import { pickablePrincipals, usePrincipals } from '../usePrincipals'
import {
  BOARD_BUNDLE_HELP_TEXT, CLEAR_ASSIGNMENT_POOL_PATCH, CLEAR_BUSINESS_HOURS_PATCH, WEEKDAY_CODES,
  assignmentPoolDraftOf, assignmentPoolPatchOf, assignmentStrategyLabel, businessHoursDraftOf, businessHoursPatchOf, classifierDraftOf, classifierPatchOf,
  defaultsDraftOf, defaultsPatchOf, emptyLadderRow, generalDraftOf, generalPatchOf, ladderDraftOf,
  ladderDraftToWire, parseMailAccountIDs,
  type BoardSettingsPatch, type BudgetUnit, type LadderRowDraft, type LadderWireLevel,
} from '../kanbanBoardSettings'
import type { FetchFn } from '../types'
import type { Board, PriorityLadder } from '@kayushkin/kanban-store-types'

/**
 * Board settings page: everything kanban-store keeps on a board besides its
 * columns — name and description, the working week, the priority ladder, the
 * defaults a dispatcher or classifier used to take as flags on a cron job
 * (principal, agent, instance, bundle), the assignment pool that hands a new
 * card to someone who is working, the classifier that files mail onto it, and the message triggers that text someone when a card changes. The board owns what a job runs with; the scheduler owns when.
 *
 * The board is `?board=<id>`; without one, the board the kanban page last
 * opened. Each section saves on its own and sends only what it changed, so an
 * untouched id is never re-checked with an owner that is down today. Nothing is
 * updated optimistically: after a save the board is re-read from the store and
 * the section redrawn from the record, and a refusal is shown in place in the
 * store's own words — it names the field and what its owner said.
 *
 * Renders nothing when the host passed no `kanbanStoreBasePath`, which is also
 * when `BridgeLayout` shows no Kanban tab.
 */
export function BridgeKanbanSettings() {
  const { fetch: fetchFn, kanbanStoreBasePath } = useBridgeConfig()
  if (!kanbanStoreBasePath) return null
  return <KanbanSettingsPage fetchFn={fetchFn} base={kanbanStoreBasePath} />
}

function readLastBoardID(): string | null {
  if (typeof localStorage === 'undefined') return null
  return localStorage.getItem(KANBAN_LAST_BOARD_STORAGE_KEY)
}

function KanbanSettingsPage({ fetchFn, base }: { fetchFn: FetchFn; base: string }) {
  const { routes, mailBasePath } = useBridgeConfig()
  // The board id lives in the URL and nowhere else, as the kanban page's open
  // card does; the last-opened board is only ever copied INTO the URL.
  const [searchParams, setSearchParams] = useSearchParams()
  const boardID = searchParams.get('board')
  const setBoardID = useCallback((id: string) => {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev)
      next.set('board', id)
      return next
    }, { replace: true })
  }, [setSearchParams])
  useEffect(() => {
    if (boardID) return
    const last = readLastBoardID()
    if (last) setBoardID(last)
  }, [boardID, setBoardID])
  // So "← Back to board" opens the board whose settings these are.
  useEffect(() => {
    if (boardID && typeof localStorage !== 'undefined') localStorage.setItem(KANBAN_LAST_BOARD_STORAGE_KEY, boardID)
  }, [boardID])

  // The board list, for switching. No board view: this page edits the record,
  // not the cards.
  const k = useKanban(null, { loadEntityTypes: false })

  const [board, setBoard] = useState<Board | null>(null)
  const [ladder, setLadder] = useState<PriorityLadder | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const ticket = useRef(0)

  const reload = useCallback(async () => {
    const mine = ++ticket.current
    if (!boardID) {
      setBoard(null)
      setLadder(null)
      setLoadError(null)
      return
    }
    const [boardResult, ladderResult] = await Promise.all([
      getBoard(fetchFn, base, boardID),
      getPriorityLadder(fetchFn, base, boardID),
    ])
    if (mine !== ticket.current) return
    if (!boardResult.ok) {
      setBoard(null)
      setLadder(null)
      setLoadError(boardResult.error)
      return
    }
    setBoard(boardResult.value)
    // The ladder is a second record; a board still renders when only it failed.
    setLadder(ladderResult.ok ? ladderResult.value : null)
    setLoadError(ladderResult.ok ? null : ladderResult.error)
  }, [fetchFn, base, boardID])

  useEffect(() => { void reload() }, [reload])

  // Every write re-reads the record on success and hands the refusal back
  // otherwise; the section that wrote shows it.
  const savePatch = useCallback(async (patch: BoardSettingsPatch): Promise<KanbanStoreResult<Board>> => {
    if (!boardID) return { ok: false, error: 'no board is selected' }
    const result = await patchBoard(fetchFn, base, boardID, patch)
    if (result.ok) await reload()
    return result
  }, [fetchFn, base, boardID, reload])

  const saveLadder = useCallback(async (body: { levels: LadderWireLevel[] }): Promise<KanbanStoreResult<PriorityLadder>> => {
    if (!boardID) return { ok: false, error: 'no board is selected' }
    const result = await putPriorityLadder(fetchFn, base, boardID, body)
    if (result.ok) await reload()
    return result
  }, [fetchFn, base, boardID, reload])

  return (
    <div className="bks-container">
      <header className="bks-header">
        <h2 className="bks-title">Board settings</h2>
        <select
          className="bk-board-select"
          aria-label="Board"
          value={boardID ?? ''}
          onChange={e => { if (e.target.value) setBoardID(e.target.value) }}
        >
          {!boardID && <option value="">— select board —</option>}
          {k.boards.map(b => (
            <option key={b.id} value={b.id}>{b.name}{b.archived ? ' (archived)' : ''}</option>
          ))}
        </select>
        <Link className="bks-back" to={routes.kanban} title="Back to the board">← Back to board</Link>
        {board && <span className="bp-id">{board.id}</span>}
        {board && routes.effectiveConfig && (
          <Link className="bks-back" to={`${routes.effectiveConfig}?board_id=${encodeURIComponent(board.id)}`} title="What a session dispatched from this board would be given, and which of these settings decides each">
            Effective config for this board →
          </Link>
        )}
      </header>
      {k.error && <div className="bridge-error">{k.error}</div>}
      {loadError && <div className="bridge-error">{loadError}</div>}

      {!boardID ? (
        <div className="bi-empty">{k.boards.length === 0 && !k.loading ? 'No boards.' : 'Select a board.'}</div>
      ) : !board ? (
        loadError ? null : <div className="bi-loading">Loading…</div>
      ) : (
        <div className="bss-sections">
          <GeneralSection key={`general:${board.id}:${JSON.stringify(generalDraftOf(board))}`} board={board} onSave={savePatch} />
          <BusinessHoursSection key={`hours:${board.id}:${JSON.stringify(board.business_hours ?? null)}`} board={board} onSave={savePatch} />
          <PriorityLadderSection key={`ladder:${board.id}:${JSON.stringify(ladder?.levels ?? null)}`} ladder={ladder} onSave={saveLadder} />
          <DefaultsSection key={`defaults:${board.id}:${JSON.stringify(defaultsDraftOf(board))}`} board={board} onSave={savePatch} />
          <AssignmentPoolSection
            key={`assignment-pool:${board.id}:${JSON.stringify(board.assignment_pool ?? null)}`}
            board={board}
            onSave={savePatch}
            fetchFn={fetchFn}
            base={base}
          />
          <BoardTagRulesSection key={`tag-rules:${board.id}`} board={board} />
          <ClassifierSection
            key={`classifier:${board.id}:${JSON.stringify(board.classifier ?? null)}`}
            board={board}
            onSave={savePatch}
            fetchFn={fetchFn}
            mailBasePath={mailBasePath}
          />
          <BoardMessageTriggersSection key={`message-triggers:${board.id}`} board={board} />
        </div>
      )}
    </div>
  )
}

// --- General -----------------------------------------------------------------

function GeneralSection({ board, onSave }: { board: Board; onSave: (patch: BoardSettingsPatch) => Promise<KanbanStoreResult<Board>> }) {
  const [draft, setDraft] = useState(() => generalDraftOf(board))
  const patch = useMemo(() => generalPatchOf(board, draft), [board, draft])
  const dirty = Object.keys(patch).length > 0
  const save = useSectionSave()
  return (
    <SettingsSection id="general" title="General" scope="board" storedBy="kanban-store · board"
      save={{ dirty, state: save, onSave: () => save.run(() => onSave(patch)) }}>
      <label className="bks-field">
        <span className="bks-field-label">Name</span>
        <input value={draft.name} onChange={e => setDraft({ ...draft, name: e.target.value })} />
      </label>
      <label className="bks-field">
        <span className="bks-field-label">Description</span>
        <textarea rows={3} value={draft.description} onChange={e => setDraft({ ...draft, description: e.target.value })} />
      </label>
      <label className="bks-check">
        <input type="checkbox" checked={draft.archived} onChange={e => setDraft({ ...draft, archived: e.target.checked })} />
        Archived
      </label>
    </SettingsSection>
  )
}

// --- Business hours ----------------------------------------------------------

function BusinessHoursSection({ board, onSave }: { board: Board; onSave: (patch: BoardSettingsPatch) => Promise<KanbanStoreResult<Board>> }) {
  const [draft, setDraft] = useState(() => businessHoursDraftOf(board))
  const stored = useMemo(() => JSON.stringify(businessHoursPatchOf(businessHoursDraftOf(board))), [board])
  const dirty = JSON.stringify(businessHoursPatchOf(draft)) !== stored
  const save = useSectionSave()
  const toggleDay = (code: string, on: boolean) => {
    setDraft({ ...draft, days: on ? [...draft.days, code] : draft.days.filter(day => day !== code) })
  }
  return (
    <SettingsSection id="business-hours" title="Business hours" scope="board" storedBy="kanban-store · board"
      save={{ dirty, state: save, onSave: () => save.run(() => onSave(businessHoursPatchOf(draft))), extra: (
        <>
        {board.business_hours && (
          <button
            type="button"
            className="bp-cancel"
            disabled={save.saving}
            title="Remove the working week. The board then reports wall-clock figures only."
            onClick={() => save.run(() => onSave(CLEAR_BUSINESS_HOURS_PATCH))}
          >Clear hours</button>
        )}
      
        </>
      ) }}>
      <p className="bks-help">
        A working week the board reports elapsed time against, beside the wall-clock figures — never instead of them.
        The zone is mandatory and never defaulted: an offset is not a zone. A board without hours reports no business figures at all.
      </p>
      <div className="bks-row">
        <label className="bks-field">
          <span className="bks-field-label">Time zone (tzid)</span>
          <input value={draft.tzid} placeholder="America/Los_Angeles" onChange={e => setDraft({ ...draft, tzid: e.target.value })} />
        </label>
        <label className="bks-field">
          <span className="bks-field-label">Start</span>
          <input type="time" value={draft.start} onChange={e => setDraft({ ...draft, start: e.target.value })} />
        </label>
        <label className="bks-field">
          <span className="bks-field-label">End</span>
          <input type="time" value={draft.end} onChange={e => setDraft({ ...draft, end: e.target.value })} />
        </label>
      </div>
      <div className="bks-days">
        {WEEKDAY_CODES.map(code => (
          <label key={code} className="bks-check">
            <input type="checkbox" checked={draft.days.includes(code)} onChange={e => toggleDay(code, e.target.checked)} />
            {code}
          </label>
        ))}
      </div>
    </SettingsSection>
  )
}

// --- Priority ladder ---------------------------------------------------------

function PriorityLadderSection({ ladder, onSave }: { ladder: PriorityLadder | null; onSave: (body: { levels: LadderWireLevel[] }) => Promise<KanbanStoreResult<PriorityLadder>> }) {
  const [rows, setRows] = useState<LadderRowDraft[]>(() => ladderDraftOf(ladder))
  const stored = useMemo(() => JSON.stringify(ladderDraftOf(ladder)), [ladder])
  const dirty = JSON.stringify(rows) !== stored
  const save = useSectionSave()
  const setRow = (index: number, next: Partial<LadderRowDraft>) => {
    setRows(rows.map((row, at) => (at === index ? { ...row, ...next } : row)))
  }
  const submit = () => {
    const wire = ladderDraftToWire(rows)
    if (!wire.ok) {
      save.setError(wire.error)
      return
    }
    void save.run(() => onSave(wire.value))
  }
  return (
    <SettingsSection id="priority-ladder" title="Priority ladder" scope="board" storedBy="kanban-store · board"
      save={{ dirty, state: save, onSave: submit, label: 'Save ladder', extra: (
        <>
        <button type="button" className="bi-add-btn" onClick={() => setRows([...rows, emptyLadderRow(rows)])}>+ Rung</button>
        </>
      ) }}>
      <p className="bks-help">
        Each rung names a stored noteboard priority and how long work at that rung should take.
        The top rung is the HIGHEST priority value — noteboard sorts priority descending — so P0 is a label on the
        highest number, not the number itself. Value 0 is reserved for unranked cards and refused. A board with no rungs
        ignores priorities entirely: its cards carry no limit. Default cost is the auto-hold dollar ceiling a card starts
        with when it is created, attached or re-prioritized at that rung; it stays editable on each card, a ceiling set by
        hand is never overwritten, and existing cards keep theirs when the ladder changes. The whole ladder is replaced on save.
      </p>
      {rows.length > 0 && (
        <table className="bks-ladder">
          <thead>
            <tr><th>Label</th><th>Priority value</th><th>Budget</th><th>Default cost</th><th></th></tr>
          </thead>
          <tbody>
            {rows.map((row, index) => (
              <tr key={index} className="bks-ladder-row">
                <td><input aria-label={`Rung ${index + 1} label`} value={row.label} placeholder="P0" onChange={e => setRow(index, { label: e.target.value })} /></td>
                <td><input aria-label={`Rung ${index + 1} priority value`} className="bks-ladder-value" inputMode="numeric" value={row.priorityValue} onChange={e => setRow(index, { priorityValue: e.target.value })} /></td>
                <td className="bks-ladder-budget">
                  <input aria-label={`Rung ${index + 1} budget`} className="bks-ladder-amount" inputMode="decimal" value={row.budgetAmount} placeholder="none" onChange={e => setRow(index, { budgetAmount: e.target.value })} />
                  <select aria-label={`Rung ${index + 1} budget unit`} value={row.budgetUnit} onChange={e => setRow(index, { budgetUnit: e.target.value as BudgetUnit })}>
                    <option value="hours">hours</option>
                    <option value="days">days</option>
                  </select>
                </td>
                <td className="bks-ladder-default-cost">
                  $<input aria-label={`Rung ${index + 1} default cost`} className="bks-ladder-amount" inputMode="decimal" value={row.defaultAutoHoldAtUSD} placeholder="none" title="Auto-hold ceiling a card starts with at this priority" onChange={e => setRow(index, { defaultAutoHoldAtUSD: e.target.value })} />
                </td>
                <td>
                  <button type="button" className="bp-cancel" title="Remove this rung" onClick={() => setRows(rows.filter((_, at) => at !== index))}>remove</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </SettingsSection>
  )
}

// --- Defaults ----------------------------------------------------------------

function DefaultsSection({ board, onSave }: { board: Board; onSave: (patch: BoardSettingsPatch) => Promise<KanbanStoreResult<Board>> }) {
  const [draft, setDraft] = useState(() => defaultsDraftOf(board))
  const patch = useMemo(() => defaultsPatchOf(board, draft), [board, draft])
  const dirty = Object.keys(patch).length > 0
  const save = useSectionSave()

  const pickers = useDefaultPickers()

  return (
    <SettingsSection id="defaults" title="Defaults" scope="board" storedBy="kanban-store · board"
      precedence="Per card, a matching tag rule below outranks these. For a card dispatched from this board they outrank the global default principal on the Settings page, and the bundle's tools are what get provisioned, not the instance's opt-ins."
      save={{ dirty, state: save, onSave: () => save.run(() => onSave(patch)), label: 'Save defaults' }}>
      <p className="bks-help">
        What a job runs with when it works this board. Each id is checked with its owner when saved; the store refuses
        one the owner does not know and writes nothing.
      </p>
      <IdField
        id="bks-default-principal"
        label="Default principal"
        help="Applied by kanban-store itself: a card created on or attached to this board with no assignee is assigned to this principal. A card that already has someone on it is left alone. Disabled principals are not offered."
        value={draft.default_principal_id}
        onChange={next => setDraft({ ...draft, default_principal_id: next })}
        {...pickers.default_principal_id}
      />
      <IdField
        id="bks-default-agent"
        label="Default agent"
        help="The agent a dispatcher runs this board's cards as — agent-store's numeric id, never the slug, which is renameable."
        value={draft.default_agent_id}
        onChange={next => setDraft({ ...draft, default_agent_id: next })}
        {...pickers.default_agent_id}
      />
      <IdField
        id="bks-default-instance"
        label="Default instance"
        help="Where a session dispatched from this board runs when the card has had no session yet; a card's last session's instance wins when there is one. Only enabled instances are offered."
        value={draft.default_instance_id}
        onChange={next => setDraft({ ...draft, default_instance_id: next })}
        {...pickers.default_instance_id}
      />
      <IdField
        id="bks-default-bundle"
        label="Default bundle"
        help={`${BOARD_BUNDLE_HELP_TEXT} bundle-store's numeric id, never the bundle's name. Only enabled bundles are offered.`}
        value={draft.default_bundle_id}
        onChange={next => setDraft({ ...draft, default_bundle_id: next })}
        {...pickers.default_bundle_id}
      />
    </SettingsSection>
  )
}

// --- Assignment pool ---------------------------------------------------------

function AssignmentPoolSection({ board, onSave, fetchFn, base }: {
  board: Board
  onSave: (patch: BoardSettingsPatch) => Promise<KanbanStoreResult<Board>>
  fetchFn: FetchFn
  base: string
}) {
  const [draft, setDraft] = useState(() => assignmentPoolDraftOf(board))
  const patch = useMemo(() => assignmentPoolPatchOf(draft), [draft])
  const stored = useMemo(() => JSON.stringify(assignmentPoolPatchOf(assignmentPoolDraftOf(board))), [board])
  const dirty = JSON.stringify(patch) !== stored
  const save = useSectionSave()

  const [strategies, setStrategies] = useState<string[] | null>(null)
  const [strategiesError, setStrategiesError] = useState<string | null>(null)
  useEffect(() => {
    let canceled = false
    listAssignmentStrategies(fetchFn, base).then(result => {
      if (canceled) return
      if (result.ok) { setStrategies(result.value); setStrategiesError(null) } else setStrategiesError(result.error)
    })
    return () => { canceled = true }
  }, [fetchFn, base])

  // Groups only: kanban-store refuses a pool that names a human.
  const principals = usePrincipals()
  const groupOptions = useMemo(() => {
    if (!principals.enabled) return null
    if (principals.loading && principals.list.length === 0) return null
    return pickablePrincipals(principals.list, { query: '', kind: 'group', excludeIDs: [] })
      .map(group => ({ value: group.id, label: `${group.display_name} (${group.id})` }))
  }, [principals])
  // The stored values stay on the form even when their owner no longer offers
  // them: the record is true whatever the list says.
  const shownGroups = groupOptions && draft.principalID && !groupOptions.some(option => option.value === draft.principalID)
    ? [...groupOptions, { value: draft.principalID, label: `${draft.principalID} (not offered here)` }]
    : groupOptions ?? []
  const shownStrategies = strategies && draft.strategy && !strategies.includes(draft.strategy)
    ? [...strategies, draft.strategy]
    : strategies ?? []

  return (
    <SettingsSection id="assignment-pool" title="Assignment pool" scope="board" storedBy="kanban-store · board"
      precedence="Per card, a tag rule that names a principal outranks the pool; the pool outranks the board's default principal, which takes the card only when nobody in the pool is available."
      save={{ dirty, state: save, onSave: () => save.run(() => onSave(patch)), label: 'Save pool', extra: (
        <>
        {board.assignment_pool && (
          <button
            type="button"
            className="bp-cancel"
            disabled={save.saving}
            title="Remove the pool. New cards then take the board's default principal alone."
            onClick={() => save.run(() => onSave(CLEAR_ASSIGNMENT_POOL_PATCH))}
          >Clear pool</button>
        )}
        </>
      ) }}>
      <p className="bks-help">
        A card that arrives with no assignee goes to one member of this group who is working at that moment, by the hours
        and time off each member declares on the Principals page. A member with no declared week is never available.
        When nobody is, the card takes the default principal above, or arrives unassigned and its history says why.
      </p>
      <div className="bks-field" data-field="bks-assignment-pool-group">
        <label className="bks-field-label" htmlFor="bks-assignment-pool-group">Group</label>
        {principals.enabled ? (
          <select
            id="bks-assignment-pool-group"
            className="bks-id-select"
            value={draft.principalID}
            disabled={groupOptions === null && !draft.principalID}
            onChange={e => setDraft({ ...draft, principalID: e.target.value })}
          >
            <option value="">{groupOptions === null ? 'loading…' : '— no pool —'}</option>
            {shownGroups.map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
        ) : (
          <>
            <input id="bks-assignment-pool-group" className="bks-id-input" value={draft.principalID} placeholder="principal_000030"
              onChange={e => setDraft({ ...draft, principalID: e.target.value })} />
            <p className="bks-help">This host has no route to principal-store, so the group id is typed here.</p>
          </>
        )}
        {principals.error && <div className="bridge-error bks-error">{principals.error}</div>}
      </div>
      <div className="bks-field" data-field="bks-assignment-pool-strategy">
        <label className="bks-field-label" htmlFor="bks-assignment-pool-strategy">Strategy</label>
        <select
          id="bks-assignment-pool-strategy"
          className="bks-id-select"
          value={draft.strategy}
          disabled={strategies === null && !draft.strategy}
          onChange={e => setDraft({ ...draft, strategy: e.target.value })}
        >
          <option value="">{strategies === null && !strategiesError ? 'loading…' : '— choose —'}</option>
          {shownStrategies.map(strategy => <option key={strategy} value={strategy}>{assignmentStrategyLabel(strategy)}</option>)}
        </select>
        <p className="bks-help">How to choose among the members available when a card arrives. kanban-store serves the list.</p>
        {strategiesError && <div className="bridge-error bks-error">{strategiesError}</div>}
      </div>
    </SettingsSection>
  )
}

// --- Classifier --------------------------------------------------------------

function ClassifierSection({ board, onSave, fetchFn, mailBasePath }: {
  board: Board
  onSave: (patch: BoardSettingsPatch) => Promise<KanbanStoreResult<Board>>
  fetchFn: FetchFn
  mailBasePath: string
}) {
  const [draft, setDraft] = useState(() => classifierDraftOf(board))
  // The free-text account list is parsed on save, not per keystroke: parsing
  // as you type eats the comma you just typed.
  const [accountsText, setAccountsText] = useState(() => draft.mailAccountIDs.join(', '))
  const [accounts, setAccounts] = useState<MailAccount[] | null>(null)
  const [accountsError, setAccountsError] = useState<string | null>(null)
  const save = useSectionSave()

  useEffect(() => {
    if (!mailBasePath) return
    let canceled = false
    listMailAccounts(fetchFn, mailBasePath).then(result => {
      if (canceled) return
      if (result.ok) { setAccounts(result.value); setAccountsError(null) } else setAccountsError(result.error)
    })
    return () => { canceled = true }
  }, [fetchFn, mailBasePath])

  // What Save would send: with a mail route the ticked ids, without one the
  // text as parsed.
  const effectiveDraft = useMemo(
    () => (mailBasePath ? draft : { ...draft, mailAccountIDs: parseMailAccountIDs(accountsText) }),
    [draft, mailBasePath, accountsText],
  )
  const patch = useMemo(() => classifierPatchOf(effectiveDraft), [effectiveDraft])
  const stored = useMemo(() => JSON.stringify(classifierPatchOf(classifierDraftOf(board))), [board])
  const dirty = JSON.stringify(patch) !== stored

  const toggleAccount = (id: string, on: boolean) => {
    setDraft({
      ...draft,
      mailAccountIDs: on
        ? (draft.mailAccountIDs.includes(id) ? draft.mailAccountIDs : [...draft.mailAccountIDs, id])
        : draft.mailAccountIDs.filter(candidate => candidate !== id),
    })
  }
  // An id the board names that mailstack's list lacks stays on the form,
  // ticked and flagged: silently dropping it on the next save would be a
  // change nobody made.
  const unlistedIDs = accounts ? draft.mailAccountIDs.filter(id => !accounts.some(account => account.id === id)) : []

  return (
    <SettingsSection id="classifier" title="Classifier" scope="board" storedBy="kanban-store · board"
      precedence="The scheduler job that runs email-classifier owns when it runs; this owns what it runs with."
      save={{ dirty, state: save, onSave: () => save.run(() => onSave(patch)), label: 'Save classifier' }}>
      <p className="bks-help">
        How mail becomes cards on this board: what email-classifier runs with. The scheduler still owns when it runs.
        Switching it off clears the classifier from the board.
      </p>
      <label className="bks-check">
        <input type="checkbox" checked={draft.enabled} onChange={e => setDraft({ ...draft, enabled: e.target.checked })} />
        A classifier files mail onto this board
      </label>
      {draft.enabled && (
        <>
          <label className="bks-field">
            <span className="bks-field-label">Vocabulary</span>
            <input value={draft.vocabulary} placeholder="work" onChange={e => setDraft({ ...draft, vocabulary: e.target.value })} />
            <span className="bks-help">
              One of email-classifier's vocabularies. The classifier owns the list — <code>email-classifier -list-vocabularies</code> prints
              it — and refuses a board naming one it lacks. Nothing serves it over HTTP, so it is typed here.
            </span>
          </label>
          <div className="bks-field">
            <span className="bks-field-label">Mail accounts</span>
            {mailBasePath ? (
              <>
                {accounts === null && !accountsError && <span className="bks-help">Loading mailstack's accounts…</span>}
                {accounts && accounts.length === 0 && unlistedIDs.length === 0 && <span className="bks-help">mailstack lists no accounts.</span>}
                <div className="bks-accounts">
                  {(accounts ?? []).map(account => (
                    <label key={account.id} className="bks-check">
                      <input
                        type="checkbox"
                        checked={draft.mailAccountIDs.includes(account.id)}
                        onChange={e => toggleAccount(account.id, e.target.checked)}
                      />
                      {account.label} <span className="bp-id">{account.id}</span>
                      <span className="bks-account-detail">{account.provider}{account.enabled ? '' : ', disabled'}</span>
                    </label>
                  ))}
                  {unlistedIDs.map(id => (
                    <label key={id} className="bks-check">
                      <input type="checkbox" checked onChange={() => toggleAccount(id, false)} />
                      <span className="bp-id">{id}</span>
                      <span className="bks-account-detail">not in mailstack's list</span>
                    </label>
                  ))}
                </div>
                {accountsError && <div className="bridge-error bks-error">{accountsError}</div>}
              </>
            ) : (
              <>
                <input value={accountsText} placeholder="gmail-personal, demo-work" onChange={e => setAccountsText(e.target.value)} />
                <span className="bks-help">
                  mailstack account ids, comma separated. This host has no route to mailstack, so they are typed rather than picked.
                </span>
              </>
            )}
            <span className="bks-help">Explicit, never "every account": the classifier reads only the accounts named here.</span>
          </div>
          <label className="bks-field">
            <span className="bks-field-label">Organization</span>
            <input value={draft.organizationID} placeholder="principal_000023" onChange={e => setDraft({ ...draft, organizationID: e.target.value })} />
            <span className="bks-help">
              The principal-store group the classifier's model calls are made for, as llm-bridge-server operations: that group's
              budget and grants apply. kanban-store checks on save that it is an active group.
            </span>
          </label>
          <label className="bks-check">
            <input type="checkbox" checked={draft.holdNewCards} onChange={e => setDraft({ ...draft, holdNewCards: e.target.checked })} />
            Hold new cards — every filed card is created parked, so no autoworker picks it up before the pipeline that owns it releases it
          </label>
        </>
      )}
    </SettingsSection>
  )
}
