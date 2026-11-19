import type { FetchFn } from './types'
import type { GrantRequest as CreateGrantRequest, Grant, RelationDefinition as GrantRelation } from '@kayushkin/grant-store-types'

/** grant-store serves the resource-type vocabulary at GET /resource-types; the
 *  wire spells it as a string, so this is the field's type, not a list. */
export type GrantResourceType = Grant['resource_type']

// The grant-store routes the Grants page and a principal's grants section
// write through, as typed functions over the host's authenticated fetch.
//
// Every call answers a `GrantStoreResult`: the parsed body on success, and on
// failure the server's own `{"error":"…"}` text, verbatim. grant-store's
// errors are written to be read by the caller — a 400 names the vocabulary,
// the owner that has no such id, or the id shape it wants — so the page shows
// them as they arrive rather than paraphrasing.

export type GrantStoreResult<T> = { ok: true; value: T } | { ok: false; error: string }

export async function grantStoreErrorText(res: Response, verb: string): Promise<string> {
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

async function request<T>(fetchFn: FetchFn, verb: string, url: string, init?: RequestInit): Promise<GrantStoreResult<T>> {
  let res: Response
  try {
    res = await fetchFn(url, init)
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}` }
  }
  if (!res.ok) return { ok: false, error: await grantStoreErrorText(res, verb) }
  if (res.status === 204) return { ok: true, value: undefined as T }
  try {
    return { ok: true, value: (await res.json()) as T }
  } catch (err) {
    return { ok: false, error: `${verb}: response was not JSON (${err instanceof Error ? err.message : String(err)})` }
  }
}

/** A Go handler encodes a slice it never allocated as null; the store says it
 *  answers `[]`, but a reader that trusts that is one release away from a crash. */
async function requestList<T>(fetchFn: FetchFn, verb: string, url: string): Promise<GrantStoreResult<T[]>> {
  const result = await request<T[] | null>(fetchFn, verb, url)
  return result.ok ? { ok: true, value: result.value ?? [] } : result
}

export function listGrantRelations(fetchFn: FetchFn, base: string): Promise<GrantStoreResult<GrantRelation[]>> {
  return requestList<GrantRelation>(fetchFn, 'list relations', `${base}/relations`)
}

export function listGrantResourceTypes(fetchFn: FetchFn, base: string): Promise<GrantStoreResult<GrantResourceType[]>> {
  return requestList<GrantResourceType>(fetchFn, 'list resource types', `${base}/resource-types`)
}

export interface GrantsFilter {
  principalID?: string
  relation?: string
  resourceType?: GrantResourceType | ''
  resourceID?: string
  includeRevoked?: boolean
  limit?: number
  offset?: number
}

export function grantsListURL(base: string, filter: GrantsFilter = {}): string {
  const params = new URLSearchParams()
  if (filter.principalID) params.set('principal_id', filter.principalID)
  if (filter.relation) params.set('relation', filter.relation)
  if (filter.resourceType) params.set('resource_type', filter.resourceType)
  if (filter.resourceID) params.set('resource_id', filter.resourceID)
  if (filter.includeRevoked) params.set('include_revoked', 'true')
  if (filter.limit) params.set('limit', String(filter.limit))
  if (filter.offset) params.set('offset', String(filter.offset))
  const query = params.toString()
  return query ? `${base}/grants?${query}` : `${base}/grants`
}

export function listGrants(fetchFn: FetchFn, base: string, filter: GrantsFilter = {}): Promise<GrantStoreResult<Grant[]>> {
  return requestList<Grant>(fetchFn, 'list grants', grantsListURL(base, filter))
}

/** `GET /principals/{id}/effective` — every active grant the principal holds,
 *  directly or through an active group. Each row's `principal_id` says which. */
export function effectiveGrantsURL(base: string, principalID: string, filter: { relation?: string; resourceType?: GrantResourceType } = {}): string {
  const params = new URLSearchParams()
  if (filter.relation) params.set('relation', filter.relation)
  if (filter.resourceType) params.set('resource_type', filter.resourceType)
  const query = params.toString()
  const url = `${base}/principals/${encodeURIComponent(principalID)}/effective`
  return query ? `${url}?${query}` : url
}

export function listEffectiveGrants(
  fetchFn: FetchFn, base: string, principalID: string, filter: { relation?: string; resourceType?: GrantResourceType } = {},
): Promise<GrantStoreResult<Grant[]>> {
  return requestList<Grant>(fetchFn, 'list effective grants', effectiveGrantsURL(base, principalID, filter))
}

/** `POST /grants` — 201 the first time, 200 with the stored row after. The
 *  store asks principal-store and the resource's owner first: an id neither
 *  knows is a 400 in the store's words, an owner that did not answer a 502. */
export function createGrant(fetchFn: FetchFn, base: string, body: CreateGrantRequest): Promise<GrantStoreResult<Grant>> {
  return request<Grant>(fetchFn, 'create grant', `${base}/grants`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** `POST /grants/{id}/revoke` — idempotent, never asks an owner, never deletes. */
export function revokeGrant(fetchFn: FetchFn, base: string, grantID: string): Promise<GrantStoreResult<Grant>> {
  return request<Grant>(fetchFn, 'revoke grant', `${base}/grants/${encodeURIComponent(grantID)}/revoke`, { method: 'POST' })
}
