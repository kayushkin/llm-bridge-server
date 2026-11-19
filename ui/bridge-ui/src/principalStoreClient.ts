import type { FetchFn } from './types'
import type { Availability, AvailabilityAnswer, GroupMembership, Principal, Principal as PrincipalDetail, TimeOff } from '@kayushkin/principal-store-types'

/** principal-store serves the kind vocabulary at GET /kinds; the wire spells it
 *  as a string, so this is the field's type, not a list. */
export type PrincipalKind = Principal['kind']
import type { PrincipalKindFilter } from './usePrincipals'

// The principal-store routes the Principals page writes through, as typed
// functions over the host's authenticated fetch.
//
// Every call answers a `PrincipalStoreResult`: the parsed body on success, and
// on failure the server's own `{"error":"…"}` text, verbatim. principal-store's
// errors are written to be read by the caller — a 400 names the vocabulary, the
// forbidden PATCH key, or that nested groups are not supported — so the page
// shows them as they arrive rather than paraphrasing. When the body carries no
// `error`, the raw text is the message; when there is no body at all, the
// status is.
//
// `usePrincipals` (the shared directory) is deliberately NOT used here. It is a
// ten-minute cache built for resolving ids on hundreds of card tiles, and this
// page is the thing that changes the directory — a list read from that cache
// would keep showing the row the user just renamed. The store's own prefix
// search (`?q=`) is what the list and the pickers read, live, on every keystroke.

export type PrincipalStoreResult<T> = { ok: true; value: T } | { ok: false; error: string }

export async function principalStoreErrorText(res: Response, verb: string): Promise<string> {
  const text = await res.text().catch(() => '')
  if (text) {
    try {
      const parsed = JSON.parse(text) as { error?: unknown }
      if (typeof parsed?.error === 'string' && parsed.error) return parsed.error
    } catch {
      // Not JSON; the raw text is the message.
    }
    return text
  }
  return `${verb} HTTP ${res.status}`
}

async function request<T>(fetchFn: FetchFn, verb: string, url: string, init?: RequestInit): Promise<PrincipalStoreResult<T>> {
  let res: Response
  try {
    res = await fetchFn(url, init)
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}` }
  }
  if (!res.ok) return { ok: false, error: await principalStoreErrorText(res, verb) }
  if (res.status === 204) return { ok: true, value: undefined as T }
  try {
    return { ok: true, value: (await res.json()) as T }
  } catch (err) {
    return { ok: false, error: `${verb}: response was not JSON (${err instanceof Error ? err.message : String(err)})` }
  }
}

const jsonInit = (method: string, body: unknown): RequestInit => ({
  method,
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})

export interface PrincipalsSearch {
  /** Prefix search over display name and email (`q`). Empty lists everyone. */
  query: string
  kind: PrincipalKindFilter
  /** Whether disabled principals are in the answer. principal-store hides
   *  them unless asked, so the default listing is the active roster. */
  includeDisabled: boolean
  limit: number
}

/** How many rows one listing asks for; the same cap the shared directory reads. */
export const PRINCIPALS_SEARCH_LIMIT = 500

/** The listing URL for a search. `include_disabled` is only sent when true, so
 *  the default request is the store's default answer — active principals. */
export function principalsSearchURL(principalStoreBasePath: string, search: PrincipalsSearch): string {
  const params = new URLSearchParams()
  const q = search.query.trim()
  if (q) params.set('q', q)
  if (search.kind !== 'all') params.set('kind', search.kind)
  if (search.includeDisabled) params.set('include_disabled', 'true')
  params.set('limit', String(search.limit))
  return `${principalStoreBasePath}/principals?${params.toString()}`
}

export function listPrincipalKinds(fetchFn: FetchFn, base: string): Promise<PrincipalStoreResult<PrincipalKind[]>> {
  return request<PrincipalKind[]>(fetchFn, 'list kinds', `${base}/kinds`)
}

export async function searchPrincipals(fetchFn: FetchFn, base: string, search: PrincipalsSearch): Promise<PrincipalStoreResult<Principal[]>> {
  const result = await request<Principal[] | null>(fetchFn, 'list principals', principalsSearchURL(base, search))
  // A JSON `null` body is how a Go handler encodes an empty slice it never
  // allocated; it means nobody matched, not that the listing is unknown.
  return result.ok ? { ok: true, value: result.value ?? [] } : result
}

/** One principal with its memberships. Disabled members and groups are asked
 *  for on purpose: this is the editor, and a membership that still exists in
 *  the table should be on screen (flagged) rather than hidden by a default
 *  meant for pickers. */
export function getPrincipal(fetchFn: FetchFn, base: string, id: string): Promise<PrincipalStoreResult<PrincipalDetail>> {
  return request<PrincipalDetail>(fetchFn, 'read principal', `${base}/principals/${encodeURIComponent(id)}?include_disabled=true`)
}

export interface CreatePrincipalRequest {
  kind: PrincipalKind
  display_name: string
  email?: string
}

export function createPrincipal(fetchFn: FetchFn, base: string, body: CreatePrincipalRequest): Promise<PrincipalStoreResult<Principal>> {
  return request<Principal>(fetchFn, 'create principal', `${base}/principals`, jsonInit('POST', body))
}

export interface PatchPrincipalRequest {
  display_name?: string
  email?: string
  /** The working week: an `Availability` to replace it, `{}` to clear it,
   *  absent to leave it alone. principal-store refuses this on a group, and
   *  refuses `kind` and `disabled_at` on anyone — those move through
   *  `/disable` and `/enable`. */
  availability?: Availability | Record<string, never>
}

export function patchPrincipal(fetchFn: FetchFn, base: string, id: string, patch: PatchPrincipalRequest): Promise<PrincipalStoreResult<Principal>> {
  return request<Principal>(fetchFn, 'update principal', `${base}/principals/${encodeURIComponent(id)}`, jsonInit('PATCH', patch))
}

/** `POST /principals/{id}/disable` or `/enable`. Both are idempotent on the
 *  server, so a double click is not an error. */
export function setPrincipalDisabled(fetchFn: FetchFn, base: string, id: string, disabled: boolean): Promise<PrincipalStoreResult<Principal>> {
  const verb = disabled ? 'disable' : 'enable'
  return request<Principal>(fetchFn, `${verb} principal`, `${base}/principals/${encodeURIComponent(id)}/${verb}`, { method: 'POST' })
}

export function addGroupMember(fetchFn: FetchFn, base: string, groupID: string, memberID: string): Promise<PrincipalStoreResult<GroupMembership>> {
  return request<GroupMembership>(
    fetchFn, 'add member',
    `${base}/principals/${encodeURIComponent(groupID)}/members/${encodeURIComponent(memberID)}`,
    { method: 'PUT' },
  )
}

export function removeGroupMember(fetchFn: FetchFn, base: string, groupID: string, memberID: string): Promise<PrincipalStoreResult<void>> {
  return request<void>(
    fetchFn, 'remove member',
    `${base}/principals/${encodeURIComponent(groupID)}/members/${encodeURIComponent(memberID)}`,
    { method: 'DELETE' },
  )
}

// --- Availability ------------------------------------------------------------
//
// The week itself is written through `patchPrincipal` — it is a field on the
// principal, not a sub-resource — so there is no setter here. What follows is
// everything that is not: the two served vocabularies, the reduced answer, and
// the absences.

/** `GET /weekday-codes` — the day codes a week may name, in week order. Read,
 *  never hardcoded: the store owns the vocabulary and the form offers exactly
 *  what it serves. */
export function listWeekdayCodes(fetchFn: FetchFn, base: string): Promise<PrincipalStoreResult<string[]>> {
  return request<string[]>(fetchFn, 'list weekday codes', `${base}/weekday-codes`)
}

/** `GET /availability-reasons` — every reason the store can give, in the order
 *  it checks them. Read so the page can say plainly when it meets one it has no
 *  wording for, instead of showing a reason it invented. */
export function listAvailabilityReasons(fetchFn: FetchFn, base: string): Promise<PrincipalStoreResult<string[]>> {
  return request<string[]>(fetchFn, 'list availability reasons', `${base}/availability-reasons`)
}

/**
 * `GET /principals/{id}/availability?at=` — the week and the absences reduced
 * to one answer for one instant. `at` is Unix seconds; omitted means now.
 *
 * A group is a 400 here by design, pointing at `/members?available_at=`: a
 * group has no hours of its own, only its members'.
 */
export function getAvailability(
  fetchFn: FetchFn, base: string, id: string, at?: number,
): Promise<PrincipalStoreResult<AvailabilityAnswer>> {
  const query = at === undefined ? '' : `?at=${encodeURIComponent(String(at))}`
  return request<AvailabilityAnswer>(
    fetchFn, 'read availability', `${base}/principals/${encodeURIComponent(id)}/availability${query}`,
  )
}

/** `GET /principals/{id}/time-off` — soonest first. A Go handler encodes an
 *  empty slice it never allocated as JSON `null`; that means no absences, not
 *  an unknown list. */
export async function listTimeOff(fetchFn: FetchFn, base: string, id: string): Promise<PrincipalStoreResult<TimeOff[]>> {
  const result = await request<TimeOff[] | null>(
    fetchFn, 'list time off', `${base}/principals/${encodeURIComponent(id)}/time-off`,
  )
  return result.ok ? { ok: true, value: result.value ?? [] } : result
}

export interface AddTimeOffRequest {
  /** Unix seconds, inclusive. */
  starts_at: number
  /** Unix seconds, exclusive, after `starts_at`. */
  ends_at: number
  note?: string
}

export function addTimeOff(
  fetchFn: FetchFn, base: string, id: string, body: AddTimeOffRequest,
): Promise<PrincipalStoreResult<TimeOff>> {
  return request<TimeOff>(
    fetchFn, 'add time off', `${base}/principals/${encodeURIComponent(id)}/time-off`, jsonInit('POST', body),
  )
}

/** `DELETE /principals/{id}/time-off/{timeOffID}` — 204. The principal is in
 *  the path on purpose: the store answers 404 for a row that is not theirs, so
 *  one person's absence can never be removed through another's page. */
export function removeTimeOff(
  fetchFn: FetchFn, base: string, id: string, timeOffID: string,
): Promise<PrincipalStoreResult<void>> {
  return request<void>(
    fetchFn, 'remove time off',
    `${base}/principals/${encodeURIComponent(id)}/time-off/${encodeURIComponent(timeOffID)}`,
    { method: 'DELETE' },
  )
}
