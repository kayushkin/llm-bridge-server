import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { GrantStoreResult } from '../grantStoreClient'
import type { Principal as PrincipalDetail } from '@kayushkin/principal-store-types'
import type { Grant, RelationDefinition as GrantRelation } from '@kayushkin/grant-store-types'
import type { GrantResourceType } from '../grantStoreClient'
import {
  filterResourceOptions, partitionGrantRows, relationWording, resourceTypeWording, type ResourceTypeWording,
} from '../grantResources'
import {
  unavailableCatalog, useAgentCatalog, useInstanceCatalog, useMachineCatalog, useSkillCatalog, useToolCatalog,
  type ResourceCatalog,
} from '../useResourceCatalogs'

/**
 * A principal's "Grants" section: what grant-store holds against a person or a
 * group, one sub-section per relation the store serves, and inside it one list
 * per resource type the relation allows, each with a picker.
 *
 * Whether a list is a lock is the relation's to say: `GET /relations` marks
 * the ones llm-bridge-server enforces at session start, and the heading shows
 * it, because a list that looks like a permission will be read as one.
 *
 * A person's grants include every group's they are in. An inherited row names
 * the group and opens it; it has no revoke button, because the grant is the
 * group's and revoking it is an edit to the group.
 */

/** What the section needs from grant-store, bound by the page to the host's
 *  fetch and base path. The page memoises it: the section re-reads when it
 *  changes. */
export interface PrincipalGrantsAccess {
  listRelations: () => Promise<GrantStoreResult<GrantRelation[]>>
  listEffective: (principalID: string) => Promise<GrantStoreResult<Grant[]>>
  grant: (principalID: string, relation: string, resourceType: GrantResourceType, resourceID: string) => Promise<GrantStoreResult<unknown>>
  revoke: (grantID: string) => Promise<GrantStoreResult<unknown>>
}

export function PrincipalGrantsSection({ detail, access, onOpen }: {
  detail: PrincipalDetail
  access: PrincipalGrantsAccess
  onOpen: (principalID: string) => void
}) {
  const [relations, setRelations] = useState<GrantRelation[] | null>(null)
  const [relationsError, setRelationsError] = useState<string | null>(null)
  const [rows, setRows] = useState<Grant[] | null>(null)
  const [rowsError, setRowsError] = useState<string | null>(null)
  const ticket = useRef(0)

  useEffect(() => {
    let canceled = false
    access.listRelations().then(result => {
      if (canceled) return
      if (result.ok) { setRelations(result.value); setRelationsError(null) } else setRelationsError(result.error)
    })
    return () => { canceled = true }
  }, [access])

  const load = useCallback(async () => {
    const mine = ++ticket.current
    const result = await access.listEffective(detail.id)
    if (mine !== ticket.current) return
    if (result.ok) { setRows(result.value); setRowsError(null) } else setRowsError(result.error)
  }, [access, detail.id])

  useEffect(() => { setRows(null); setRowsError(null) }, [detail.id])
  // A person inherits their groups' grants, so a membership change moves the
  // list. The groups on the detail are what say it changed.
  const groupsKey = (detail.groups ?? []).map(group => `${group.id}:${group.disabled_at}`).join(',')
  useEffect(() => { void load() }, [load, groupsKey])

  const groupNames = useMemo(
    () => new Map((detail.groups ?? []).map(group => [group.id, group.display_name])),
    [detail.groups],
  )

  return (
    <section className="bp-section bp-grants" data-principal-id={detail.id}>
      <h4 className="bp-section-title">Grants</h4>
      <p className="bp-hint">
        {detail.kind === 'group'
          ? 'Everyone in this group inherits these grants.'
          : 'Their own grants, plus what their groups carry. A session started as this principal is offered only what the enforced relations name.'}
      </p>
      {relationsError && <div className="bridge-error bp-error">Could not read the relations: {relationsError}</div>}
      {rowsError && <div className="bridge-error bp-error">Could not read these grants: {rowsError}</div>}
      {relations === null && !relationsError && <div className="bp-empty-inline">Loading…</div>}
      {relations?.map(relation => (
        <div key={`${detail.id}:${relation.name}`} className="bp-relation" data-relation={relation.name}>
          <h5 className="bp-relation-title">
            {relationWording(relation.name)}
            <span className={`bp-badge ${relation.enforced ? 'bp-badge-enforced' : 'bp-badge-advisory'}`}>
              {relation.enforced ? 'enforced' : 'advisory'}
            </span>
          </h5>
          <p className="bp-hint">{relation.description}</p>
          {relation.resource_types.map(type => (
            <ResourceGroupForType
              key={`${detail.id}:${relation.name}:${type}`}
              relation={relation.name}
              type={type}
              principalID={detail.id}
              rows={rows}
              groupNames={groupNames}
              grant={resourceID => access.grant(detail.id, relation.name, type, resourceID)}
              revoke={grantID => access.revoke(grantID)}
              onChanged={load}
              onOpen={onOpen}
            />
          ))}
        </div>
      ))}
    </section>
  )
}

interface ResourceGroupSourceProps {
  relation: string
  type: GrantResourceType
  principalID: string
  rows: Grant[] | null
  groupNames: ReadonlyMap<string, string>
  grant: (resourceID: string) => Promise<GrantStoreResult<unknown>>
  revoke: (grantID: string) => Promise<GrantStoreResult<unknown>>
  onChanged: () => Promise<void>
  onOpen: (principalID: string) => void
}

/** Picks the owner lookup for a type. One component per type, so each calls
 *  only its own owner's hook. */
function ResourceGroupForType(props: ResourceGroupSourceProps) {
  switch (props.type) {
    case 'agent': return <AgentResourceGroup {...props} />
    case 'instance': return <InstanceResourceGroup {...props} />
    case 'machine': return <MachineResourceGroup {...props} />
    case 'skill': return <SkillResourceGroup {...props} />
    case 'tool': return <ToolResourceGroup {...props} />
    default: return <UnknownTypeResourceGroup {...props} />
  }
}

function AgentResourceGroup(props: ResourceGroupSourceProps) {
  const [query, setQuery] = useState('')
  const catalog = useAgentCatalog(query)
  return <ResourceGroup {...props} catalog={catalog} query={query} onQueryChange={setQuery} />
}

function InstanceResourceGroup(props: ResourceGroupSourceProps) {
  const [query, setQuery] = useState('')
  const catalog = useInstanceCatalog(query)
  return <ResourceGroup {...props} catalog={catalog} query={query} onQueryChange={setQuery} />
}

function MachineResourceGroup(props: ResourceGroupSourceProps) {
  const [query, setQuery] = useState('')
  const catalog = useMachineCatalog(query)
  return <ResourceGroup {...props} catalog={catalog} query={query} onQueryChange={setQuery} />
}

function SkillResourceGroup(props: ResourceGroupSourceProps) {
  const [query, setQuery] = useState('')
  const ids = useMemo(
    () => (props.rows ?? []).filter(row => row.resource_type === 'skill').map(row => row.resource_id),
    [props.rows],
  )
  const catalog = useSkillCatalog(query, ids)
  return <ResourceGroup {...props} catalog={catalog} query={query} onQueryChange={setQuery} />
}

function ToolResourceGroup(props: ResourceGroupSourceProps) {
  const [query, setQuery] = useState('')
  const catalog = useToolCatalog(query)
  return <ResourceGroup {...props} catalog={catalog} query={query} onQueryChange={setQuery} />
}

function UnknownTypeResourceGroup(props: ResourceGroupSourceProps) {
  const catalog = useMemo(
    () => unavailableCatalog(`grant-store names a “${props.type}” type this page has no lookup for, so its ids are shown as they are.`),
    [props.type],
  )
  return <ResourceGroup {...props} catalog={catalog} query="" onQueryChange={() => {}} />
}

/** How many options the picker lists before asking for a narrower search. */
const RESOURCE_MATCHES_SHOWN = 30

export interface ResourceGroupProps extends ResourceGroupSourceProps {
  catalog: ResourceCatalog
  query: string
  onQueryChange: (query: string) => void
  /** Opens the picker on first render. The group owns it live; this lets a
   *  static render show what the picker offers. */
  initialPickerOpen?: boolean
}

function withArticle(word: string): string {
  return /^[aeiou]/i.test(word) ? `an ${word}` : `a ${word}`
}

/** One relation's list of one type for one principal, with its picker.
 *  Exported for the render checks, which drive it with a fixed catalog. */
export function ResourceGroup({
  relation, type, principalID, rows, groupNames, catalog, query, onQueryChange, grant, revoke, onChanged, onOpen,
  initialPickerOpen = false,
}: ResourceGroupProps) {
  const wording = resourceTypeWording(type)
  const [pickerOpen, setPickerOpen] = useState(initialPickerOpen)
  const [busyID, setBusyID] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const { direct, inherited } = useMemo(
    () => rows ? partitionGrantRows(rows, principalID, relation, type) : { direct: [], inherited: [] },
    [rows, principalID, relation, type],
  )
  const directIDs = useMemo(() => new Set(direct.map(row => row.resource_id)), [direct])
  const candidates = useMemo(
    () => catalog.matches ? filterResourceOptions(catalog.matches, '', directIDs) : null,
    [catalog.matches, directIDs],
  )
  const shown = candidates ? candidates.slice(0, RESOURCE_MATCHES_SHOWN) : []

  const run = async (busyKey: string, outcome: Promise<GrantStoreResult<unknown>>) => {
    setBusyID(busyKey)
    setError(null)
    const result = await outcome
    if (result.ok) await onChanged()
    else setError(result.error)
    setBusyID(null)
  }

  const total = direct.length + inherited.length

  return (
    <div className="bp-resource-group" data-relation={relation} data-resource-type={type}>
      <h5 className="bp-resource-title">
        {wording.plural} <span className="bp-count">{rows ? total : '…'}</span>
      </h5>
      {catalog.unavailable && <p className="bp-hint">{catalog.unavailable}</p>}
      {catalog.error && <div className="bridge-error bp-error">Could not read {wording.owner}: {catalog.error}</div>}
      <ul className="bp-resource-list">
        {direct.map(row => (
          <ResourceRow
            key={`direct:${row.id}`}
            row={row}
            wording={wording}
            catalog={catalog}
            busy={busyID !== null}
            onRevoke={() => { void run(row.id, revoke(row.id)) }}
          />
        ))}
        {inherited.map(row => (
          <ResourceRow
            key={`inherited:${row.id}`}
            row={row}
            wording={wording}
            catalog={catalog}
            busy={busyID !== null}
            viaName={groupNames.get(row.principal_id)}
            onOpenVia={() => onOpen(row.principal_id)}
          />
        ))}
        {rows === null && <li className="bp-empty-inline">Loading…</li>}
        {rows !== null && total === 0 && <li className="bp-empty-inline">None.</li>}
      </ul>
      {error && <div className="bridge-error bp-error bp-membership-error">{error}</div>}
      {!catalog.unavailable && (
        <div className="bp-picker bp-resource-picker">
          <input
            type="search"
            className="bp-picker-query"
            placeholder={`Grant ${withArticle(wording.singular)}…`}
            value={query}
            onChange={e => { onQueryChange(e.target.value); setPickerOpen(true) }}
            onFocus={() => setPickerOpen(true)}
            aria-label={`Grant ${withArticle(wording.singular)}`}
          />
          {pickerOpen && candidates === null && !catalog.error && <div className="bp-empty-inline">Loading…</div>}
          {pickerOpen && candidates !== null && (
            <ul className="bp-picker-matches">
              {shown.map(option => (
                <li key={option.id}>
                  <button
                    type="button"
                    className="bp-picker-match bp-resource-match"
                    data-resource-id={option.id}
                    disabled={busyID !== null}
                    title={`Grant ${option.label}`}
                    onClick={() => { void run(option.id, grant(option.id)) }}
                  >
                    <span className="bp-resource-name">{option.label}</span>
                    {option.detail && <span className="bp-resource-detail">{option.detail}</span>}
                    {option.disabled && <span className="bp-badge bp-badge-disabled">disabled</span>}
                  </button>
                </li>
              ))}
              {shown.length === 0 && (
                <li className="bp-empty-inline">
                  {query.trim()
                    ? `No ${wording.singular} matches “${query.trim()}”.`
                    : catalog.matches && catalog.matches.length > 0
                      ? `Every ${wording.singular} is already granted.`
                      : `${wording.owner} has none.`}
                </li>
              )}
              {candidates.length > shown.length && (
                <li className="bp-empty-inline">{candidates.length - shown.length} more — narrow the search.</li>
              )}
              <li>
                <button type="button" className="bp-cancel" onClick={() => { setPickerOpen(false); onQueryChange('') }}>Done</button>
              </li>
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * One row. A resource the owner cannot name shows its raw id, with the reason
 * on hover, never nothing: the row is a true record that someone granted that
 * id, and only the name is unknown.
 */
function ResourceRow({ row, wording, catalog, busy, onRevoke, viaName, onOpenVia }: {
  row: Grant
  wording: ResourceTypeWording
  catalog: ResourceCatalog
  busy: boolean
  onRevoke?: () => void
  viaName?: string
  onOpenVia?: () => void
}) {
  const option = catalog.byID.get(row.resource_id)
  const inherited = onOpenVia !== undefined
  const unresolvedReason = option
    ? null
    : catalog.unavailable
      ?? (catalog.error
        ? `${wording.owner} did not answer: ${catalog.error}`
        : catalog.settled(row.resource_id)
          ? `${wording.owner} has no ${wording.singular} with this id`
          : 'still loading')
  const name = option ? option.label : row.resource_id
  const groupName = viaName ?? row.principal_id

  return (
    <li
      className={`bp-resource${option ? '' : ' bp-resource-unresolved'}${option?.disabled ? ' bp-resource-disabled' : ''}${inherited ? ' bp-resource-inherited' : ''}`}
      data-resource-id={row.resource_id}
      data-grant-id={row.id}
      data-principal-id={row.principal_id}
    >
      <span className="bp-resource-name" title={`${row.resource_type} ${row.resource_id} — ${row.id}${row.note ? ` — ${row.note}` : ''}${unresolvedReason ? ` — ${unresolvedReason}` : ''}`}>
        {name}
      </span>
      {option?.detail && <span className="bp-resource-detail">{option.detail}</span>}
      {option?.disabled && <span className="bp-badge bp-badge-disabled">disabled</span>}
      {inherited ? (
        <button
          type="button"
          className="bp-resource-via"
          title={`${groupName}’s grant. Open the group to revoke it.`}
          onClick={onOpenVia}
        >via {groupName}</button>
      ) : (
        <button
          type="button"
          className="bp-membership-remove"
          title={`Revoke ${name}`}
          aria-label={`Revoke ${name}`}
          disabled={busy}
          onClick={onRevoke}
        >×</button>
      )}
    </li>
  )
}
