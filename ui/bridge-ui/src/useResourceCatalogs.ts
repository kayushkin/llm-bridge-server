// The live lookups behind a principal's grants lists and the Grants page — for
// each resource type, the owner's current names and a search for the picker —
// and the card's read of which harness instances its assignees work with.
//
// One hook per type rather than one hook with a switch, because each owner is
// asked differently: instances and environments come from the bridge's shared
// polls, agents and tools are small lists read once, and skill-store holds
// about 1,450 skills (790 KB as one list, measured 2026-09-10), so skills are
// searched and resolved by id rather than downloaded whole.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useBridgeConfig } from './context'
import { useBridgeInstances } from './useBridgeInstances'
import { useBridgeMachines } from './useBridgeMachines'
import { listEffectiveGrants } from './grantStoreClient'
import {
  agentOption, compareOptionsByLabel, filterResourceOptions, instanceOption, machineOption, skillOption, toolOption,
  type AgentRecord, type ResourceOption, type SkillRecord, type ToolRecord,
} from './grantResources'

export interface ResourceCatalog {
  /** Set when this host has no route to the owner, saying so. Names cannot be
   *  shown and nothing can be picked. */
  unavailable: string | null
  /** Every option resolved so far, by id. */
  byID: ReadonlyMap<string, ResourceOption>
  /** Whether the lookup for this id has finished, found or not — what tells
   *  "still loading" apart from "the owner has no such id". */
  settled: (id: string) => boolean
  /** The options matching the current query, before the ones already on the
   *  list are taken out. Null while the owner has not answered. */
  matches: ResourceOption[] | null
  /** The owner's last failure, verbatim. */
  error: string | null
}

const NOTHING_EXCLUDED: ReadonlySet<string> = new Set()
const NO_OPTIONS: ReadonlyMap<string, ResourceOption> = new Map()

export function unavailableCatalog(reason: string): ResourceCatalog {
  return { unavailable: reason, byID: NO_OPTIONS, settled: () => true, matches: null, error: null }
}

function catalogOfList(options: ResourceOption[] | null, error: string | null, query: string): ResourceCatalog {
  return {
    unavailable: null,
    byID: options ? new Map(options.map(option => [option.id, option])) : NO_OPTIONS,
    settled: () => options !== null || error !== null,
    matches: options ? filterResourceOptions(options, query, NOTHING_EXCLUDED) : null,
    error,
  }
}

/** A whole list, read once per mount. `toOption` must be a module-level
 *  function: it is not a dependency of the read. */
function useListReadOnce<T>(url: string | null, toOption: (row: T) => ResourceOption) {
  const { fetch: fetchFn } = useBridgeConfig()
  const [options, setOptions] = useState<ResourceOption[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!url) return
    let canceled = false
    fetchFn(url)
      .then(async res => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        const body: unknown = await res.json()
        if (!Array.isArray(body)) throw new Error('the answer was not a list')
        return (body as T[]).map(toOption).sort(compareOptionsByLabel)
      })
      .then(list => { if (!canceled) { setOptions(list); setError(null) } })
      .catch(err => { if (!canceled) setError(err instanceof Error ? err.message : String(err)) })
    return () => { canceled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [url, fetchFn])
  return { options, error }
}

export function useAgentCatalog(query: string): ResourceCatalog {
  const { basePath } = useBridgeConfig()
  const { options, error } = useListReadOnce<AgentRecord>(`${basePath}/agents`, agentOption)
  return useMemo(() => catalogOfList(options, error, query), [options, error, query])
}

export function useToolCatalog(query: string): ResourceCatalog {
  const { toolStoreBasePath } = useBridgeConfig()
  const { options, error } = useListReadOnce<ToolRecord>(toolStoreBasePath ? `${toolStoreBasePath}/tools` : null, toolOption)
  return useMemo(
    () => toolStoreBasePath
      ? catalogOfList(options, error, query)
      : unavailableCatalog('This host has no route to tool-store, so tool names cannot be looked up here.'),
    [toolStoreBasePath, options, error, query],
  )
}

export function useInstanceCatalog(query: string): ResourceCatalog {
  const { instances, loading, error } = useBridgeInstances()
  const { machines } = useBridgeMachines()
  return useMemo(() => {
    const machinesByID = new Map(machines.map(machine => [machine.id, machine]))
    const answered = !loading || instances.length > 0
    const options = answered ? instances.map(instance => instanceOption(instance, machinesByID)).sort(compareOptionsByLabel) : null
    return catalogOfList(options, error, query)
  }, [instances, machines, loading, error, query])
}

export function useMachineCatalog(query: string): ResourceCatalog {
  const { machines, loading, error } = useBridgeMachines()
  return useMemo(() => {
    const answered = !loading || machines.length > 0
    return catalogOfList(answered ? machines.map(machineOption).sort(compareOptionsByLabel) : null, error, query)
  }, [machines, loading, error, query])
}

/** How many skills one search asks skill-store for. */
export const SKILL_MATCHES_LIMIT = 30

/**
 * Skills, searched rather than listed. The ids on the list are resolved one
 * `GET /skills/{id}` each, once per mount; the picker asks `?q=` on every
 * change of the query and keeps only the newest answer.
 */
export function useSkillCatalog(query: string, resolveIDs: readonly string[]): ResourceCatalog {
  const { fetch: fetchFn, skillStoreBasePath: base } = useBridgeConfig()
  const [resolved, setResolved] = useState<ReadonlyMap<string, ResourceOption>>(NO_OPTIONS)
  const [settledIDs, setSettledIDs] = useState<ReadonlySet<string>>(NOTHING_EXCLUDED)
  const [matches, setMatches] = useState<ResourceOption[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const requested = useRef(new Set<string>())
  const searchTicket = useRef(0)
  const mounted = useRef(true)
  useEffect(() => () => { mounted.current = false }, [])

  const idsKey = resolveIDs.join(',')
  useEffect(() => {
    if (!base || !idsKey) return
    for (const id of idsKey.split(',')) {
      if (requested.current.has(id)) continue
      requested.current.add(id)
      fetchFn(`${base}/skills/${encodeURIComponent(id)}`)
        .then(async res => {
          if (res.status === 404) return null
          if (!res.ok) throw new Error(`HTTP ${res.status}`)
          return skillOption(await res.json() as SkillRecord)
        })
        .then(option => {
          if (!mounted.current) return
          if (option) setResolved(prev => new Map(prev).set(id, option))
          setSettledIDs(prev => new Set(prev).add(id))
        })
        .catch(err => {
          if (!mounted.current) return
          // Not found stays settled; a failure is asked again on the next change.
          requested.current.delete(id)
          setError(err instanceof Error ? err.message : String(err))
          setSettledIDs(prev => new Set(prev).add(id))
        })
    }
  }, [base, idsKey, fetchFn])

  useEffect(() => {
    if (!base) return
    const mine = ++searchTicket.current
    const params = new URLSearchParams({ limit: String(SKILL_MATCHES_LIMIT) })
    if (query.trim()) params.set('q', query.trim())
    fetchFn(`${base}/skills?${params.toString()}`)
      .then(async res => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        const body: unknown = await res.json()
        // A Go handler encodes an empty slice it never allocated as null.
        if (body === null) return []
        if (!Array.isArray(body)) throw new Error('the answer was not a list')
        return (body as SkillRecord[]).map(skillOption).sort(compareOptionsByLabel)
      })
      .then(list => { if (mounted.current && mine === searchTicket.current) { setMatches(list); setError(null) } })
      .catch(err => { if (mounted.current && mine === searchTicket.current) setError(err instanceof Error ? err.message : String(err)) })
  }, [base, query, fetchFn])

  return useMemo(
    () => base
      ? { unavailable: null, byID: resolved, settled: (id: string) => settledIDs.has(id), matches, error }
      : unavailableCatalog('This host has no route to skill-store, so skill names cannot be looked up here.'),
    [base, resolved, settledIDs, matches, error],
  )
}

/**
 * For a card: which harness instances its assignees work with, as instance id
 * → the assignees who do — grant-store's `works_with` relation, a person's
 * effective set including their groups'. Empty, with no error, when the host
 * has no grant-store route or the card has nobody on it.
 */
export function useInstancesListedForPrincipals(principalIDs: readonly string[]): {
  listedBy: ReadonlyMap<string, string[]>
  error: string | null
} {
  const { fetch: fetchFn, grantStoreBasePath: base } = useBridgeConfig()
  const key = principalIDs.join(',')
  const [state, setState] = useState<{ listedBy: ReadonlyMap<string, string[]>; error: string | null }>(
    { listedBy: new Map(), error: null },
  )
  useEffect(() => {
    if (!base || !key) {
      setState({ listedBy: new Map(), error: null })
      return
    }
    let canceled = false
    const ids = key.split(',')
    Promise.all(ids.map(id => listEffectiveGrants(fetchFn, base, id, { relation: 'works_with', resourceType: 'instance' }).then(result => ({ id, result }))))
      .then(answers => {
        if (canceled) return
        const listedBy = new Map<string, string[]>()
        const failures: string[] = []
        for (const { id, result } of answers) {
          if (!result.ok) {
            failures.push(`${id}: ${result.error}`)
            continue
          }
          for (const row of result.value) {
            const who = listedBy.get(row.resource_id) ?? []
            if (!who.includes(id)) who.push(id)
            listedBy.set(row.resource_id, who)
          }
        }
        setState({ listedBy, error: failures.length > 0 ? failures.join('; ') : null })
      })
    return () => { canceled = true }
  }, [base, key, fetchFn])
  return state
}
