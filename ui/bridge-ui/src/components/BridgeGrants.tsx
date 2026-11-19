import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { useBridgeConfig } from '../context'
import {
  createGrant, listGrantRelations, listGrants, revokeGrant, type GrantStoreResult, type GrantsFilter,
} from '../grantStoreClient'
import { filterResourceOptions, relationWording, resourceTypeWording } from '../grantResources'
import { pickablePrincipals, principalIsDisabled, usePrincipals } from '../usePrincipals'
import {
  useAgentCatalog, useInstanceCatalog, useMachineCatalog, useSkillCatalog, useToolCatalog, type ResourceCatalog,
} from '../useResourceCatalogs'
import type { FetchFn } from '../types'
import type { Grant, RelationDefinition as GrantRelation } from '@kayushkin/grant-store-types'
import type { GrantResourceType } from '../grantStoreClient'

/**
 * Top-level Grants page: grant-store's ledger of who may use what, whole —
 * every grant across every principal, filterable by holder, relation and
 * resource, with revoked ones on request — and the form that writes one.
 *
 * The section on a principal shows one holder's grants with what they inherit;
 * this page is the reverse view, where "who holds a grant on Playwright?" is
 * one filter. Names are resolved live from their owners: principal-store for
 * the holder, and each resource's own store for the resource, so a rename
 * shows here without a copy going stale.
 *
 * Renders nothing when the host passed no `grantStoreBasePath`, which is also
 * when `BridgeLayout` shows no Grants tab.
 */
export function BridgeGrants() {
  const { fetch: fetchFn, grantStoreBasePath } = useBridgeConfig()
  if (!grantStoreBasePath) return null
  return <GrantsPage fetchFn={fetchFn} base={grantStoreBasePath} />
}

/** How many grants one read asks for. Plenty for a personal box; the store
 *  pages, so a bigger one raises this rather than dropping rows. */
export const GRANTS_PAGE_LIMIT = 500

function GrantsPage({ fetchFn, base }: { fetchFn: FetchFn; base: string }) {
  const principals = usePrincipals()
  const [relations, setRelations] = useState<GrantRelation[] | null>(null)
  const [relationsError, setRelationsError] = useState<string | null>(null)
  const [grants, setGrants] = useState<Grant[] | null>(null)
  const [grantsError, setGrantsError] = useState<string | null>(null)
  const [principalFilter, setPrincipalFilter] = useState('')
  const [relationFilter, setRelationFilter] = useState('')
  const [typeFilter, setTypeFilter] = useState<GrantResourceType | ''>('')
  const [includeRevoked, setIncludeRevoked] = useState(false)
  const [busyID, setBusyID] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const ticket = useRef(0)

  useEffect(() => {
    let canceled = false
    listGrantRelations(fetchFn, base).then(result => {
      if (canceled) return
      if (result.ok) { setRelations(result.value); setRelationsError(null) } else setRelationsError(result.error)
    })
    return () => { canceled = true }
  }, [fetchFn, base])

  const filter = useMemo<GrantsFilter>(() => ({
    principalID: principalFilter, relation: relationFilter, resourceType: typeFilter, includeRevoked, limit: GRANTS_PAGE_LIMIT,
  }), [principalFilter, relationFilter, typeFilter, includeRevoked])

  const load = useCallback(async () => {
    const mine = ++ticket.current
    const result = await listGrants(fetchFn, base, filter)
    if (mine !== ticket.current) return
    if (result.ok) { setGrants(result.value); setGrantsError(null) } else setGrantsError(result.error)
  }, [fetchFn, base, filter])
  useEffect(() => { void load() }, [load])

  const revoke = async (grant: Grant) => {
    setBusyID(grant.id)
    setActionError(null)
    const result = await revokeGrant(fetchFn, base, grant.id)
    if (result.ok) await load()
    else setActionError(result.error)
    setBusyID(null)
  }

  const resourceTypes = useMemo(() => {
    const seen = new Set<GrantResourceType>()
    for (const relation of relations ?? []) for (const type of relation.resource_types) seen.add(type)
    return [...seen]
  }, [relations])
  const skillIDs = useMemo(
    () => (grants ?? []).filter(grant => grant.resource_type === 'skill').map(grant => grant.resource_id),
    [grants],
  )
  const catalogs = useCatalogs('', skillIDs)
  const principalName = (id: string) => principals.byId.get(id)?.display_name ?? id

  return (
    <div className="bp-container bg-container">
      <header className="bg-head">
        <h2 className="bg-title">Grants</h2>
        <p className="bp-hint">
          Who may use what. A session started as a principal is offered only the tools, skills, agents and instances the
          enforced relations name; a grant is revoked, never deleted.
        </p>
      </header>

      {relationsError && <div className="bridge-error bp-error">Could not read the relations: {relationsError}</div>}
      {principals.error && <div className="bridge-error bp-error">Could not read principal-store: {principals.error}</div>}

      <GrantForm
        fetchFn={fetchFn}
        base={base}
        relations={relations ?? []}
        principalsList={pickablePrincipals(principals.list, { query: '', kind: 'all', excludeIDs: new Set() })}
        onCreated={load}
      />

      <div className="bg-filters">
        <label className="bg-filter">
          <span>Holder</span>
          <select value={principalFilter} onChange={e => setPrincipalFilter(e.target.value)} aria-label="Filter by holder">
            <option value="">Anyone</option>
            {principals.list.map(principal => (
              <option key={principal.id} value={principal.id}>
                {principal.display_name}{principal.kind === 'group' ? ' (group)' : ''}{principalIsDisabled(principal) ? ' (disabled)' : ''}
              </option>
            ))}
          </select>
        </label>
        <label className="bg-filter">
          <span>Relation</span>
          <select value={relationFilter} onChange={e => setRelationFilter(e.target.value)} aria-label="Filter by relation">
            <option value="">Any</option>
            {(relations ?? []).map(relation => (
              <option key={relation.name} value={relation.name}>{relationWording(relation.name)}</option>
            ))}
          </select>
        </label>
        <label className="bg-filter">
          <span>Resource</span>
          <select value={typeFilter} onChange={e => setTypeFilter(e.target.value as GrantResourceType | '')} aria-label="Filter by resource type">
            <option value="">Any</option>
            {resourceTypes.map(type => <option key={type} value={type}>{resourceTypeWording(type).plural}</option>)}
          </select>
        </label>
        <label className="bg-filter bg-filter-check">
          <input type="checkbox" checked={includeRevoked} onChange={e => setIncludeRevoked(e.target.checked)} />
          <span>Show revoked</span>
        </label>
      </div>

      {grantsError && <div className="bridge-error bp-error">Could not read the grants: {grantsError}</div>}
      {actionError && <div className="bridge-error bp-error">{actionError}</div>}

      <table className="bg-table">
        <thead>
          <tr>
            <th>Holder</th>
            <th>Relation</th>
            <th>Resource</th>
            <th>Note</th>
            <th>Granted</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {grants === null && !grantsError && <tr><td colSpan={6} className="bp-empty-inline">Loading…</td></tr>}
          {grants !== null && grants.length === 0 && <tr><td colSpan={6} className="bp-empty-inline">No grants match.</td></tr>}
          {grants?.map(grant => {
            const catalog = catalogs[grant.resource_type]
            const option = catalog?.byID.get(grant.resource_id)
            const wording = resourceTypeWording(grant.resource_type)
            const holder = principals.byId.get(grant.principal_id)
            const revoked = grant.revoked_at !== 0
            return (
              <tr key={grant.id} className={`bg-row${revoked ? ' bg-row-revoked' : ''}`} data-grant-id={grant.id} data-principal-id={grant.principal_id}>
                <td className="bg-holder" title={grant.principal_id}>
                  {principalName(grant.principal_id)}
                  {holder?.kind === 'group' && <span className="bp-badge bp-badge-group">group</span>}
                </td>
                <td className="bg-relation" title={grant.relation}>{relationWording(grant.relation)}</td>
                <td className="bg-resource" title={`${grant.resource_type} ${grant.resource_id}`}>
                  <span className={`bp-resource-name${option ? '' : ' bg-resource-unresolved'}`}>{option ? option.label : grant.resource_id}</span>
                  <span className="bp-resource-detail">{option?.detail ? `${wording.singular} · ${option.detail}` : wording.singular}</span>
                </td>
                <td className="bg-note">{grant.note}</td>
                <td className="bg-when">
                  {formatEpochSeconds(grant.granted_at)}
                  {revoked && <span className="bp-badge bp-badge-disabled">revoked {formatEpochSeconds(grant.revoked_at)}</span>}
                </td>
                <td className="bg-actions">
                  {!revoked && (
                    <button
                      type="button"
                      className="bp-cancel bg-revoke"
                      disabled={busyID !== null}
                      onClick={() => { void revoke(grant) }}
                      aria-label={`Revoke ${grant.id}`}
                    >Revoke</button>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

/** One catalog per type, read once for the table and again by the form's
 *  picker with its own query. */
function useCatalogs(query: string, skillIDs: readonly string[]): Partial<Record<GrantResourceType, ResourceCatalog>> {
  const agent = useAgentCatalog(query)
  const instance = useInstanceCatalog(query)
  const machine = useMachineCatalog(query)
  const skill = useSkillCatalog(query, skillIDs)
  const tool = useToolCatalog(query)
  return useMemo(() => ({ agent, instance, machine, skill, tool }), [agent, instance, machine, skill, tool])
}

/** How many resource options the form's picker lists before asking for a
 *  narrower search. */
const FORM_MATCHES_SHOWN = 30

function GrantForm({ fetchFn, base, relations, principalsList, onCreated }: {
  fetchFn: FetchFn
  base: string
  relations: GrantRelation[]
  principalsList: { id: string; display_name: string; kind: string }[]
  onCreated: () => Promise<void>
}) {
  const [principalID, setPrincipalID] = useState('')
  const [relationName, setRelationName] = useState('')
  const [resourceType, setResourceType] = useState<GrantResourceType | ''>('')
  const [resourceID, setResourceID] = useState('')
  const [query, setQuery] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [outcome, setOutcome] = useState<string | null>(null)

  const relation = relations.find(candidate => candidate.name === relationName) ?? null
  // A type the chosen relation does not allow is cleared rather than sent: the
  // store would refuse it, and the picker would be offering the wrong owner.
  useEffect(() => {
    if (relation && resourceType && !relation.resource_types.includes(resourceType)) { setResourceType(''); setResourceID('') }
  }, [relation, resourceType])

  const catalogs = useCatalogs(query, [])
  const catalog = resourceType ? catalogs[resourceType] ?? null : null
  const candidates = useMemo(
    () => catalog?.matches ? filterResourceOptions(catalog.matches, '', new Set()) : null,
    [catalog],
  )
  const chosen = resourceID && catalog ? catalog.byID.get(resourceID) : undefined

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!principalID || !relationName || !resourceType || !resourceID) {
      setError('Choose a holder, a relation, a resource type and a resource.')
      return
    }
    setBusy(true)
    setError(null)
    setOutcome(null)
    const result: GrantStoreResult<Grant> = await createGrant(fetchFn, base, {
      principal_id: principalID, relation: relationName, resource_type: resourceType, resource_id: resourceID, note: note.trim(),
    })
    if (result.ok) {
      setOutcome(`${result.value.id} — ${relationWording(result.value.relation)} ${result.value.resource_type} ${result.value.resource_id}`)
      setResourceID('')
      setQuery('')
      setNote('')
      await onCreated()
    } else {
      setError(result.error)
    }
    setBusy(false)
  }

  return (
    <form className="bg-form" onSubmit={submit} aria-label="Grant">
      <label className="bg-filter">
        <span>Holder</span>
        <select value={principalID} onChange={e => setPrincipalID(e.target.value)} aria-label="Holder">
          <option value="">Choose a principal…</option>
          {principalsList.map(principal => (
            <option key={principal.id} value={principal.id}>{principal.display_name}{principal.kind === 'group' ? ' (group)' : ''}</option>
          ))}
        </select>
      </label>
      <label className="bg-filter">
        <span>Relation</span>
        <select value={relationName} onChange={e => setRelationName(e.target.value)} aria-label="Relation">
          <option value="">Choose a relation…</option>
          {relations.map(candidate => (
            <option key={candidate.name} value={candidate.name}>
              {relationWording(candidate.name)}{candidate.enforced ? '' : ' (advisory)'}
            </option>
          ))}
        </select>
      </label>
      <label className="bg-filter">
        <span>Resource type</span>
        <select value={resourceType} onChange={e => { setResourceType(e.target.value as GrantResourceType | ''); setResourceID(''); setQuery('') }} aria-label="Resource type" disabled={!relation}>
          <option value="">{relation ? 'Choose a type…' : 'Choose a relation first'}</option>
          {(relation?.resource_types ?? []).map(type => <option key={type} value={type}>{resourceTypeWording(type).plural}</option>)}
        </select>
      </label>
      <div className="bg-filter bg-resource-picker">
        <span>Resource</span>
        {chosen ? (
          <span className="bg-chosen" data-resource-id={chosen.id}>
            <span className="bp-resource-name">{chosen.label}</span>
            {chosen.detail && <span className="bp-resource-detail">{chosen.detail}</span>}
            <button type="button" className="bp-membership-remove" aria-label="Clear the resource" onClick={() => { setResourceID(''); setQuery('') }}>×</button>
          </span>
        ) : (
          <div className="bp-picker bp-resource-picker">
            <input
              type="search"
              className="bp-picker-query"
              placeholder={resourceType ? `Find ${resourceTypeWording(resourceType).singular}…` : 'Choose a type first'}
              value={query}
              disabled={!resourceType}
              onChange={e => setQuery(e.target.value)}
              aria-label="Find a resource"
            />
            {catalog?.unavailable && <p className="bp-hint">{catalog.unavailable}</p>}
            {catalog?.error && <div className="bridge-error bp-error">Could not read {resourceTypeWording(resourceType || 'agent').owner}: {catalog.error}</div>}
            {resourceType && candidates && (
              <ul className="bp-picker-matches">
                {candidates.slice(0, FORM_MATCHES_SHOWN).map(option => (
                  <li key={option.id}>
                    <button type="button" className="bp-picker-match bp-resource-match" data-resource-id={option.id} onClick={() => setResourceID(option.id)} title={`Choose ${option.label}`}>
                      <span className="bp-resource-name">{option.label}</span>
                      {option.detail && <span className="bp-resource-detail">{option.detail}</span>}
                      {option.disabled && <span className="bp-badge bp-badge-disabled">disabled</span>}
                    </button>
                  </li>
                ))}
                {candidates.length === 0 && <li className="bp-empty-inline">Nothing matches.</li>}
                {candidates.length > FORM_MATCHES_SHOWN && <li className="bp-empty-inline">{candidates.length - FORM_MATCHES_SHOWN} more — narrow the search.</li>}
              </ul>
            )}
          </div>
        )}
      </div>
      <label className="bg-filter bg-note-field">
        <span>Note</span>
        <input type="text" value={note} onChange={e => setNote(e.target.value)} placeholder="Why, for whoever reads this later" aria-label="Note" />
      </label>
      <button type="submit" className="bp-submit bg-submit" disabled={busy}>{busy ? 'Granting…' : 'Grant'}</button>
      {error && <div className="bridge-error bp-error bg-form-error">{error}</div>}
      {outcome && <div className="bp-hint bg-form-outcome">Granted: {outcome}</div>}
    </form>
  )
}

function formatEpochSeconds(seconds: number): string {
  return new Date(seconds * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}
