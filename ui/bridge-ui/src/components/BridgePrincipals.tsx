import { useSearchParams } from 'react-router-dom'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { useBridgeConfig } from '../context'
import {
  PRINCIPALS_SEARCH_LIMIT, addGroupMember, createPrincipal, getPrincipal,
  listPrincipalKinds, patchPrincipal, removeGroupMember, searchPrincipals, setPrincipalDisabled,
} from '../principalStoreClient'
import { createGrant, listEffectiveGrants, listGrantRelations, revokeGrant } from '../grantStoreClient'
import { PrincipalGrantsSection, type PrincipalGrantsAccess } from './PrincipalGrants'
import { PrincipalAvailabilitySection } from './PrincipalAvailabilitySection'
import type { PatchPrincipalRequest, PrincipalStoreResult, PrincipalsSearch } from '../principalStoreClient'
import { pickablePrincipals, principalInitials, principalIsDisabled } from '../usePrincipals'
import type { PrincipalKindFilter } from '../usePrincipals'
import type { FetchFn } from '../types'
import type { Principal, Principal as PrincipalDetail } from '@kayushkin/principal-store-types'
import type { PrincipalKind } from '../principalStoreClient'

/**
 * Top-level Principals page: the editor for principal-store's directory of the
 * humans and groups a card can be assigned to (and, later, a permission
 * granted to).
 *
 * Left, the roster: the store's own prefix search, a kind filter and a "show
 * disabled" toggle, plus the form that creates a principal. Right, the one
 * selected: its id in monospace so it can be pasted into a chat, its editable
 * name and email, its disabled state, and its memberships — a human's groups
 * or a group's members — each removable, with a picker to add one. Below them,
 * its grants from grant-store: per relation, the agents, harness instances,
 * environments, skills and tools it holds (a person's includes their groups'),
 * shown only when the host proxies grant-store.
 *
 * Every mutation goes to the store and the affected views are re-read from it;
 * nothing here is updated optimistically, because the store is the source of
 * truth and its refusals are the interesting part. A refusal is shown in place,
 * in the server's own words: principal-store's 400s name the vocabulary, the
 * PATCH key it will not take, or that a group cannot be a member of a group.
 *
 * Renders nothing when the host passed no `principalStoreBasePath`, which is
 * also when `BridgeLayout` shows no Principals tab.
 */
export function BridgePrincipals() {
  const { fetch: fetchFn, principalStoreBasePath, grantStoreBasePath } = useBridgeConfig()
  if (!principalStoreBasePath) return null
  return <PrincipalsPage fetchFn={fetchFn} base={principalStoreBasePath} grantStoreBase={grantStoreBasePath} />
}

const KIND_FILTERS: { value: PrincipalKindFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'human', label: 'People' },
  { value: 'group', label: 'Groups' },
]

/** A human-readable label for a kind, for badges and the create form. The
 *  vocabulary itself comes from `GET /kinds`; only the wording is here, and an
 *  unknown kind is shown as the kind's own name rather than dropped. */
function kindLabel(kind: string): string {
  if (kind === 'human') return 'Person'
  if (kind === 'group') return 'Group'
  return kind
}

function PrincipalsPage({ fetchFn, base, grantStoreBase }: { fetchFn: FetchFn; base: string; grantStoreBase: string }) {
  const [query, setQuery] = useState('')
  const [kind, setKind] = useState<PrincipalKindFilter>('all')
  const [showDisabled, setShowDisabled] = useState(false)

  const [list, setList] = useState<Principal[]>([])
  const [listLoading, setListLoading] = useState(true)
  const [listError, setListError] = useState<string | null>(null)

  const [kinds, setKinds] = useState<PrincipalKind[]>([])
  const [kindsError, setKindsError] = useState<string | null>(null)

  // `?id=<principal id>`: the principal a reference chip opened.
  const [searchParams] = useSearchParams()
  const linkedID = searchParams.get('id')
  const [selectedID, setSelectedID] = useState<string | null>(linkedID)
  useEffect(() => { if (linkedID) setSelectedID(linkedID) }, [linkedID])
  const [detail, setDetail] = useState<PrincipalDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)

  // Reads race: a slow answer to "pri" must not land after the answer to
  // "priya". Each read takes a ticket and only the latest one may set state.
  const listTicket = useRef(0)
  const detailTicket = useRef(0)

  const search = useMemo<PrincipalsSearch>(
    () => ({ query, kind, includeDisabled: showDisabled, limit: PRINCIPALS_SEARCH_LIMIT }),
    [query, kind, showDisabled],
  )

  const loadList = useCallback(async () => {
    const ticket = ++listTicket.current
    setListLoading(true)
    const result = await searchPrincipals(fetchFn, base, search)
    if (ticket !== listTicket.current) return
    if (result.ok) {
      setList(result.value)
      setListError(null)
    } else {
      // The last good list stays on screen beside the error; an empty list
      // would say "nobody matched" when the truth is "the store did not answer".
      setListError(result.error)
    }
    setListLoading(false)
  }, [fetchFn, base, search])

  const loadDetail = useCallback(async (id: string) => {
    const ticket = ++detailTicket.current
    setDetailLoading(true)
    const result = await getPrincipal(fetchFn, base, id)
    if (ticket !== detailTicket.current) return
    if (result.ok) {
      setDetail(result.value)
      setDetailError(null)
    } else {
      setDetailError(result.error)
    }
    setDetailLoading(false)
  }, [fetchFn, base])

  useEffect(() => { void loadList() }, [loadList])

  useEffect(() => {
    listPrincipalKinds(fetchFn, base).then(result => {
      if (result.ok) { setKinds(result.value); setKindsError(null) } else setKindsError(result.error)
    })
  }, [fetchFn, base])

  useEffect(() => {
    if (!selectedID) { setDetail(null); setDetailError(null); return }
    void loadDetail(selectedID)
  }, [selectedID, loadDetail])

  // After a write, re-read the roster and the open principal. Both, always:
  // a rename shows in the list, a disable moves the row out of the default
  // listing, a membership shows on the group AND on the human.
  const reloadAfterMutation = useCallback(async () => {
    await Promise.all([loadList(), selectedID ? loadDetail(selectedID) : Promise.resolve()])
  }, [loadList, loadDetail, selectedID])

  const onCreated = useCallback((created: Principal) => {
    // Clear the filters so the new row is in the listing it lands in, then
    // open it. A group created under the People filter would otherwise be
    // selected and invisible at once.
    setQuery('')
    setKind('all')
    setSelectedID(created.id)
  }, [])

  // Undefined, and the section absent, when the host proxies no grant-store:
  // a section that could only say "not configured" is worse than none.
  const grantsAccess = useMemo<PrincipalGrantsAccess | undefined>(() => grantStoreBase ? {
    listRelations: () => listGrantRelations(fetchFn, grantStoreBase),
    listEffective: principalID => listEffectiveGrants(fetchFn, grantStoreBase, principalID),
    grant: (principalID, relation, resourceType, resourceID) =>
      createGrant(fetchFn, grantStoreBase, { principal_id: principalID, relation, resource_type: resourceType, resource_id: resourceID, note: '' }),
    revoke: grantID => revokeGrant(fetchFn, grantStoreBase, grantID),
  } : undefined, [fetchFn, grantStoreBase])

  const searchCandidates = useCallback(
    (candidateQuery: string, candidateKind: PrincipalKind) =>
      searchPrincipals(fetchFn, base, { query: candidateQuery, kind: candidateKind, includeDisabled: false, limit: PRINCIPALS_SEARCH_LIMIT }),
    [fetchFn, base],
  )

  return (
    <div className="bp-container">
      <div className="bp-header">
        <h2 className="bp-title">Principals</h2>
        <p className="bp-subtitle">
          The people and groups work can be assigned to. Ids are the join key everywhere else; names are for display.
        </p>
      </div>
      <div className="bp-columns">
        <section className="bp-list-pane" aria-label="Principals">
          <div className="bp-toolbar">
            <input
              type="search"
              className="bp-search"
              placeholder="Search by name or email…"
              value={query}
              onChange={e => setQuery(e.target.value)}
              aria-label="Search principals"
            />
            <div className="bp-kinds" role="group" aria-label="Principal kind">
              {KIND_FILTERS.map(k => (
                <button
                  key={k.value}
                  type="button"
                  className="bp-kind"
                  aria-pressed={kind === k.value}
                  onClick={() => setKind(k.value)}
                >{k.label}</button>
              ))}
            </div>
            <label className="bp-check">
              <input type="checkbox" checked={showDisabled} onChange={e => setShowDisabled(e.target.checked)} />
              <span>Show disabled</span>
            </label>
          </div>

          <PrincipalCreateForm
            kinds={kinds}
            kindsError={kindsError}
            create={body => createPrincipal(fetchFn, base, body)}
            onCreated={async created => { onCreated(created); await loadList() }}
          />

          <PrincipalListView
            principals={list}
            selectedID={selectedID}
            onSelect={setSelectedID}
            loading={listLoading}
            error={listError}
          />
        </section>

        <section className="bp-detail-pane" aria-label="Selected principal">
          {!selectedID ? (
            <div className="bp-empty">Select a principal to see its details.</div>
          ) : detailError && !detail ? (
            <div className="bridge-error bp-error">{detailError}</div>
          ) : !detail ? (
            <div className="bp-empty">Loading…</div>
          ) : (
            <PrincipalDetailView
              detail={detail}
              fetchFn={fetchFn}
              base={base}
              loading={detailLoading}
              readError={detailError}
              save={patch => patchPrincipal(fetchFn, base, detail.id, patch)}
              setDisabled={disabled => setPrincipalDisabled(fetchFn, base, detail.id, disabled)}
              addMembership={(groupID, memberID) => addGroupMember(fetchFn, base, groupID, memberID)}
              removeMembership={(groupID, memberID) => removeGroupMember(fetchFn, base, groupID, memberID)}
              searchCandidates={searchCandidates}
              onChanged={reloadAfterMutation}
              onOpen={setSelectedID}
              grants={grantsAccess}
            />
          )}
        </section>
      </div>
    </div>
  )
}

// --- the roster ------------------------------------------------------------

export interface PrincipalListViewProps {
  principals: Principal[]
  selectedID: string | null
  onSelect: (id: string) => void
  loading: boolean
  /** The last read's failure. Shown beside whatever list is on screen; the
   *  list is not blanked, because "nobody" and "unknown" are different. */
  error: string | null
}

/** The roster rows. Whether disabled principals are in `principals` is the
 *  server's decision (`include_disabled`); this only renders what it was
 *  given and flags the disabled ones. */
export function PrincipalListView({ principals, selectedID, onSelect, loading, error }: PrincipalListViewProps) {
  return (
    <div className="bp-list-wrap">
      {error && <div className="bridge-error bp-error">Could not list principals: {error}</div>}
      {loading && principals.length === 0 && !error && <div className="bp-empty">Loading…</div>}
      {!loading && principals.length === 0 && !error && <div className="bp-empty">No principals match.</div>}
      {principals.length > 0 && (
        <ul className="bp-list" aria-busy={loading}>
          {principals.map(p => (
            <li key={p.id}>
              <button
                type="button"
                className={`bp-row${p.id === selectedID ? ' bp-row-selected' : ''}${principalIsDisabled(p) ? ' bp-row-disabled' : ''}`}
                data-principal-id={p.id}
                aria-pressed={p.id === selectedID}
                onClick={() => onSelect(p.id)}
              >
                <PrincipalAvatar principal={p} />
                <span className="bp-row-text">
                  <span className="bp-row-name">{p.display_name}</span>
                  {p.email && <span className="bp-row-email">{p.email}</span>}
                </span>
                {principalIsDisabled(p) && <span className="bp-badge bp-badge-disabled">disabled</span>}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** The initials avatar for a person, the group glyph for a group — the same
 *  vocabulary the kanban tiles use, so a face here is the face on the card. */
function PrincipalAvatar({ principal, large = false }: { principal: Principal; large?: boolean }) {
  const className = `bp-avatar bp-avatar-${principal.kind}${large ? ' bp-avatar-large' : ''}`
  if (principal.kind === 'group') {
    return <span className={className} aria-hidden="true">👥</span>
  }
  return <span className={className} aria-hidden="true">{principalInitials(principal.display_name)}</span>
}

// --- creating one ----------------------------------------------------------

interface PrincipalCreateFormProps {
  /** From `GET /kinds`. Empty until it answers; the form cannot submit before. */
  kinds: PrincipalKind[]
  kindsError: string | null
  create: (body: { kind: PrincipalKind; display_name: string; email?: string }) => Promise<PrincipalStoreResult<Principal>>
  onCreated: (created: Principal) => Promise<void>
}

function PrincipalCreateForm({ kinds, kindsError, create, onCreated }: PrincipalCreateFormProps) {
  const [open, setOpen] = useState(false)
  const [kind, setKind] = useState<PrincipalKind | ''>('')
  const [displayName, setDisplayName] = useState('')
  const [email, setEmail] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // The kind defaults to the first the store names once it has answered, and
  // to nothing before: a form that assumed "human" would be wrong on the day
  // the vocabulary changes, which is what `/kinds` exists to prevent.
  const effectiveKind: PrincipalKind | '' = kind || kinds[0] || ''

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!effectiveKind || !displayName.trim()) return
    setBusy(true)
    setError(null)
    const body: { kind: PrincipalKind; display_name: string; email?: string } = {
      kind: effectiveKind,
      display_name: displayName.trim(),
    }
    if (effectiveKind === 'human' && email.trim()) body.email = email.trim()
    const result = await create(body)
    if (result.ok) {
      setDisplayName('')
      setEmail('')
      setOpen(false)
      await onCreated(result.value)
    } else {
      setError(result.error)
    }
    setBusy(false)
  }

  if (!open) {
    return (
      <div className="bp-create-toggle">
        <button type="button" className="bi-add-btn" onClick={() => setOpen(true)}>+ New principal</button>
      </div>
    )
  }

  return (
    <form className="bp-create" onSubmit={submit}>
      <div className="bp-create-row">
        <select
          className="bp-create-kind"
          value={effectiveKind}
          onChange={e => setKind(e.target.value as PrincipalKind)}
          aria-label="Kind"
          disabled={kinds.length === 0}
        >
          {kinds.length === 0 && <option value="">{kindsError ? 'kinds unavailable' : 'loading kinds…'}</option>}
          {kinds.map(k => <option key={k} value={k}>{kindLabel(k)}</option>)}
        </select>
        <input
          className="bp-create-name"
          placeholder="Display name"
          value={displayName}
          onChange={e => setDisplayName(e.target.value)}
          aria-label="Display name"
          required
        />
      </div>
      {effectiveKind === 'human' && (
        <input
          className="bp-create-email"
          type="email"
          placeholder="Email (optional)"
          value={email}
          onChange={e => setEmail(e.target.value)}
          aria-label="Email"
        />
      )}
      {kindsError && <div className="bridge-error bp-error">Could not load kinds: {kindsError}</div>}
      {error && <div className="bridge-error bp-error">{error}</div>}
      <div className="bp-create-actions">
        <button type="submit" className="bi-save-btn" disabled={busy || !effectiveKind || !displayName.trim()}>
          {busy ? 'Creating…' : 'Create'}
        </button>
        <button type="button" className="bp-cancel" onClick={() => { setOpen(false); setError(null) }}>Cancel</button>
      </div>
    </form>
  )
}

// --- the selected principal ------------------------------------------------

export interface PrincipalDetailViewProps {
  detail: PrincipalDetail
  /** The host's authenticated fetch and principal-store's base path. The
   *  Availability section reads and writes several routes of its own — the day
   *  codes, the reasons, the absences, the reduced answer — and threading a
   *  callback per route through here would say less than the two it needs. */
  fetchFn: FetchFn
  base: string
  /** A re-read is in flight. The old detail stays on screen meanwhile. */
  loading: boolean
  /** The last re-read's failure, if the detail on screen may be stale. */
  readError: string | null
  save: (patch: PatchPrincipalRequest) => Promise<PrincipalStoreResult<Principal>>
  setDisabled: (disabled: boolean) => Promise<PrincipalStoreResult<Principal>>
  addMembership: (groupID: string, memberID: string) => Promise<PrincipalStoreResult<unknown>>
  removeMembership: (groupID: string, memberID: string) => Promise<PrincipalStoreResult<unknown>>
  /** The store's prefix search, narrowed to one kind, for the pickers. */
  searchCandidates: (query: string, kind: PrincipalKind) => Promise<PrincipalStoreResult<Principal[]>>
  /** Called after any successful write; the host re-reads what it shows. */
  onChanged: () => Promise<void>
  /** Open another principal — a member or a group named in this one's lists. */
  onOpen: (id: string) => void
  /** grant-store's routes. Absent, the grants section is not shown. */
  grants?: PrincipalGrantsAccess
}

export function PrincipalDetailView({
  detail, fetchFn, base, loading, readError, save, setDisabled, addMembership, removeMembership, searchCandidates,
  onChanged, onOpen, grants,
}: PrincipalDetailViewProps) {
  const disabled = principalIsDisabled(detail)
  const [statusBusy, setStatusBusy] = useState(false)
  const [statusError, setStatusError] = useState<string | null>(null)

  const toggleDisabled = async () => {
    setStatusBusy(true)
    setStatusError(null)
    const result = await setDisabled(!disabled)
    if (result.ok) await onChanged()
    else setStatusError(result.error)
    setStatusBusy(false)
  }

  const isGroup = detail.kind === 'group'

  return (
    <div className={`bp-detail${disabled ? ' bp-detail-disabled' : ''}`} data-principal-id={detail.id} aria-busy={loading}>
      <header className="bp-detail-head">
        <PrincipalAvatar principal={detail} large />
        <div className="bp-detail-title">
          <h3 className="bp-detail-name">{detail.display_name}</h3>
          <div className="bp-detail-meta">
            <span className={`bp-badge bp-badge-${detail.kind}`}>{kindLabel(detail.kind)}</span>
            <code className="bp-id" title="The id other stores join on">{detail.id}</code>
            {disabled && <span className="bp-badge bp-badge-disabled">disabled</span>}
          </div>
        </div>
      </header>

      {readError && <div className="bridge-error bp-error">Could not re-read this principal: {readError}</div>}

      <section className="bp-section">
        <h4 className="bp-section-title">Details</h4>
        {/* Keyed on the row's version so a save or a re-read resets the fields
            to what the store now holds, rather than leaving stale edits. */}
        <PrincipalEditForm key={`${detail.id}:${detail.updated_at}`} detail={detail} save={save} onSaved={onChanged} />
      </section>

      <section className="bp-section">
        <h4 className="bp-section-title">Status</h4>
        <div className="bp-status-row">
          <span className={`bp-status ${disabled ? 'bp-status-disabled' : 'bp-status-active'}`}>
            {disabled ? `Disabled since ${formatEpochSeconds(detail.disabled_at)}` : 'Active'}
          </span>
          <button
            type="button"
            className={disabled ? 'bi-save-btn' : 'bp-danger-btn'}
            onClick={() => { void toggleDisabled() }}
            disabled={statusBusy}
          >
            {statusBusy ? '…' : disabled ? 'Enable' : 'Disable'}
          </button>
        </div>
        <p className="bp-hint">
          {isGroup
            ? 'A disabled group keeps its members and its assignments, but is not offered for new ones. There is no delete.'
            : 'A disabled person keeps their assignments, shown struck through, but is not offered for new ones. There is no delete.'}
        </p>
        {statusError && <div className="bridge-error bp-error">{statusError}</div>}
      </section>

      {!isGroup && (
        <PrincipalAvailabilitySection
          key={`availability:${detail.id}`}
          detail={detail}
          fetchFn={fetchFn}
          base={base}
          save={save}
          onSaved={onChanged}
        />
      )}

      {isGroup ? (
        <MembershipsSection
          key={`members:${detail.id}`}
          title="Members"
          emptyText="No members yet."
          entries={detail.members ?? []}
          pickerKind="human"
          pickerPlaceholder="Add a person…"
          selfID={detail.id}
          add={memberID => addMembership(detail.id, memberID)}
          remove={memberID => removeMembership(detail.id, memberID)}
          searchCandidates={searchCandidates}
          onChanged={onChanged}
          onOpen={onOpen}
        />
      ) : (
        <MembershipsSection
          key={`groups:${detail.id}`}
          title="Groups"
          emptyText="In no groups."
          entries={detail.groups ?? []}
          pickerKind="group"
          pickerPlaceholder="Add to a group…"
          selfID={detail.id}
          add={groupID => addMembership(groupID, detail.id)}
          remove={groupID => removeMembership(groupID, detail.id)}
          searchCandidates={searchCandidates}
          onChanged={onChanged}
          onOpen={onOpen}
        />
      )}

      {grants && (
        <PrincipalGrantsSection key={`grants:${detail.id}`} detail={detail} access={grants} onOpen={onOpen} />
      )}
    </div>
  )
}

function formatEpochSeconds(seconds: number): string {
  return new Date(seconds * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

function PrincipalEditForm({
  detail, save, onSaved,
}: {
  detail: PrincipalDetail
  save: (patch: PatchPrincipalRequest) => Promise<PrincipalStoreResult<Principal>>
  onSaved: () => Promise<void>
}) {
  const [displayName, setDisplayName] = useState(detail.display_name)
  const [email, setEmail] = useState(detail.email)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Only what changed goes in the PATCH. principal-store takes exactly two
  // keys and refuses any other, and sending an unchanged name alongside a
  // changed email would be a write it does not need.
  const patch: PatchPrincipalRequest = {}
  if (displayName.trim() !== detail.display_name) patch.display_name = displayName.trim()
  if (detail.kind === 'human' && email.trim() !== detail.email) patch.email = email.trim()
  const dirty = Object.keys(patch).length > 0

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!dirty || !displayName.trim()) return
    setBusy(true)
    setError(null)
    const result = await save(patch)
    if (result.ok) await onSaved()
    else setError(result.error)
    setBusy(false)
  }

  return (
    <form className="bp-edit" onSubmit={submit}>
      <label className="bp-field">
        <span className="bp-field-label">Display name</span>
        <input className="bp-input" value={displayName} onChange={e => setDisplayName(e.target.value)} required />
      </label>
      {detail.kind === 'human' && (
        <label className="bp-field">
          <span className="bp-field-label">Email</span>
          <input className="bp-input" type="email" value={email} onChange={e => setEmail(e.target.value)} />
        </label>
      )}
      {error && <div className="bridge-error bp-error">{error}</div>}
      <div className="bp-edit-actions">
        <button type="submit" className="bi-save-btn" disabled={!dirty || busy || !displayName.trim()}>
          {busy ? 'Saving…' : 'Save'}
        </button>
        {dirty && !busy && (
          <button type="button" className="bp-cancel" onClick={() => { setDisplayName(detail.display_name); setEmail(detail.email) }}>
            Revert
          </button>
        )}
      </div>
    </form>
  )
}

// --- memberships -----------------------------------------------------------

export interface MembershipsSectionProps {
  title: string
  emptyText: string
  /** A group's members, or a human's groups — whichever this principal has. */
  entries: Principal[]
  /** What the picker offers: humans for a group's member list, groups for a
   *  human's group list. */
  pickerKind: PrincipalKind
  pickerPlaceholder: string
  /** The open principal, never offered to itself. */
  selfID: string
  add: (otherID: string) => Promise<PrincipalStoreResult<unknown>>
  remove: (otherID: string) => Promise<PrincipalStoreResult<unknown>>
  searchCandidates: (query: string, kind: PrincipalKind) => Promise<PrincipalStoreResult<Principal[]>>
  onChanged: () => Promise<void>
  onOpen: (id: string) => void
  /** A refusal to show on first render. The section owns the live one; this
   *  seeds it, so a static render can show what a 400 looks like here. */
  initialError?: string | null
}

export function MembershipsSection({
  title, emptyText, entries, pickerKind, pickerPlaceholder, selfID, add, remove, searchCandidates, onChanged, onOpen,
  initialError = null,
}: MembershipsSectionProps) {
  const [error, setError] = useState<string | null>(initialError)
  const [busyID, setBusyID] = useState<string | null>(null)

  const run = async (otherID: string, outcome: Promise<PrincipalStoreResult<unknown>>): Promise<boolean> => {
    setBusyID(otherID)
    setError(null)
    const result = await outcome
    if (result.ok) await onChanged()
    else setError(result.error)
    setBusyID(null)
    return result.ok
  }

  const excludeIDs = useMemo(() => [selfID, ...entries.map(e => e.id)], [selfID, entries])

  return (
    <section className="bp-section bp-memberships">
      <h4 className="bp-section-title">{title} <span className="bp-count">{entries.length}</span></h4>
      <ul className="bp-membership-list">
        {entries.map(entry => (
          <li key={entry.id} className={`bp-membership${principalIsDisabled(entry) ? ' bp-membership-disabled' : ''}`} data-principal-id={entry.id}>
            <button type="button" className="bp-membership-open" onClick={() => onOpen(entry.id)} title={`Open ${entry.display_name}`}>
              <PrincipalAvatar principal={entry} />
              <span className="bp-membership-name">{entry.display_name}</span>
              {entry.email && <span className="bp-membership-email">{entry.email}</span>}
              {principalIsDisabled(entry) && <span className="bp-badge bp-badge-disabled">disabled</span>}
            </button>
            <button
              type="button"
              className="bp-membership-remove"
              title={`Remove ${entry.display_name}`}
              aria-label={`Remove ${entry.display_name}`}
              disabled={busyID !== null}
              onClick={() => { void run(entry.id, remove(entry.id)) }}
            >×</button>
          </li>
        ))}
        {entries.length === 0 && <li className="bp-empty-inline">{emptyText}</li>}
      </ul>
      {error && <div className="bridge-error bp-error bp-membership-error">{error}</div>}
      <PrincipalPicker
        kind={pickerKind}
        placeholder={pickerPlaceholder}
        excludeIDs={excludeIDs}
        search={searchCandidates}
        busy={busyID !== null}
        onPick={id => run(id, add(id))}
      />
    </section>
  )
}

/** How many candidates the picker lists before asking for a narrower search. */
const PICKER_MATCHES_SHOWN = 30

/**
 * A picker over the store's prefix search, narrowed to one kind. Whoever is
 * already in the list, the open principal itself, and anyone disabled are never
 * offered — `pickablePrincipals` is the same rule the kanban assignee picker
 * applies, so the two never disagree about who can be chosen.
 *
 * A search that failed renders the failure, not an empty list: an empty list
 * would claim there is nobody to add.
 */
function PrincipalPicker({
  kind, placeholder, excludeIDs, search, busy, onPick,
}: {
  kind: PrincipalKind
  placeholder: string
  excludeIDs: string[]
  search: (query: string, kind: PrincipalKind) => Promise<PrincipalStoreResult<Principal[]>>
  busy: boolean
  onPick: (id: string) => Promise<boolean>
}) {
  const [query, setQuery] = useState('')
  const [candidates, setCandidates] = useState<Principal[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const ticket = useRef(0)

  useEffect(() => {
    const mine = ++ticket.current
    search(query, kind).then(result => {
      if (mine !== ticket.current) return
      if (result.ok) { setCandidates(result.value); setError(null) } else setError(result.error)
    })
  }, [query, kind, search])

  // The server already applied the query (to name AND email, as a prefix), so
  // the client pass filters by exclusion only — re-applying the text here would
  // drop a match the store found by email.
  const matches = useMemo(
    () => candidates ? pickablePrincipals(candidates, { query: '', kind, excludeIDs }) : [],
    [candidates, kind, excludeIDs],
  )
  const shown = matches.slice(0, PICKER_MATCHES_SHOWN)

  return (
    <div className="bp-picker" data-picker-kind={kind}>
      <input
        type="search"
        className="bp-picker-query"
        placeholder={placeholder}
        value={query}
        onChange={e => setQuery(e.target.value)}
        aria-label={placeholder}
      />
      {error && <div className="bridge-error bp-error">Could not search principals: {error}</div>}
      {!error && candidates === null && <div className="bp-empty-inline">Loading…</div>}
      {!error && candidates !== null && (
        <ul className="bp-picker-matches">
          {shown.map(p => (
            <li key={p.id}>
              <button
                type="button"
                className="bp-picker-match"
                data-principal-id={p.id}
                disabled={busy}
                title={`Add ${p.display_name}`}
                onClick={() => { void onPick(p.id) }}
              >
                <PrincipalAvatar principal={p} />
                <span className="bp-membership-name">{p.display_name}</span>
                {p.email && <span className="bp-membership-email">{p.email}</span>}
              </button>
            </li>
          ))}
          {shown.length === 0 && (
            <li className="bp-empty-inline">
              {candidates.length === 0
                ? (query.trim() ? `No ${kind === 'group' ? 'groups' : 'people'} match “${query.trim()}”.` : `principal-store has no ${kind === 'group' ? 'groups' : 'people'}.`)
                : `Every matching ${kind === 'group' ? 'group' : 'person'} is already here.`}
            </li>
          )}
          {matches.length > shown.length && (
            <li className="bp-empty-inline">{matches.length - shown.length} more — narrow the search.</li>
          )}
        </ul>
      )}
    </div>
  )
}
