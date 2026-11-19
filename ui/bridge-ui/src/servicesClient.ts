import type { FetchFn } from './types'
import type {
  DatabaseRowFilter, DatabaseRowsResponse, DatabaseSchemaResponse, ServiceInventoryResponse,
} from '@kayushkin/llm-bridge-types'

// The three reads the Services page makes against llm-bridge-server, as typed
// functions over the host's authenticated fetch. Every call answers a
// `ServicesResult`: the parsed body, or the server's own error message —
// llm-bridge-server's refusals say which path or table it did not know, and
// the page shows them as they arrive.

export type ServicesResult<T> = { ok: true; value: T } | { ok: false; error: string }

async function errorText(res: Response, verb: string): Promise<string> {
  const text = await res.text().catch(() => '')
  if (!text) return `${verb}: HTTP ${res.status}`
  try {
    const parsed = JSON.parse(text) as { error?: unknown }
    const error = parsed?.error
    if (typeof error === 'string' && error) return `${verb}: ${error}`
    if (error && typeof error === 'object' && typeof (error as { message?: unknown }).message === 'string') {
      return `${verb}: ${(error as { message: string }).message}`
    }
  } catch {
    // Not JSON; the raw text is the message.
  }
  return `${verb}: ${text}`
}

async function request<T>(fetchFn: FetchFn, verb: string, url: string): Promise<ServicesResult<T>> {
  let res: Response
  try {
    res = await fetchFn(url)
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}` }
  }
  if (!res.ok) return { ok: false, error: await errorText(res, verb) }
  try {
    return { ok: true, value: (await res.json()) as T }
  } catch (err) {
    return { ok: false, error: `${verb}: response was not JSON (${err instanceof Error ? err.message : String(err)})` }
  }
}

export function listServices(fetchFn: FetchFn, basePath: string, refresh = false): Promise<ServicesResult<ServiceInventoryResponse>> {
  return request(fetchFn, 'list services', `${basePath}/services${refresh ? '?refresh=1' : ''}`)
}

export function readDatabaseSchema(fetchFn: FetchFn, basePath: string, path: string): Promise<ServicesResult<DatabaseSchemaResponse>> {
  return request(fetchFn, 'read schema', `${basePath}/services/databases/schema?${new URLSearchParams({ path })}`)
}

export interface RowsQuery {
  path: string
  table: string
  limit: number
  orderBy: string
  descending: boolean
  filters: DatabaseRowFilter[]
}

/** The query string for one rows read, in the shape the server parses:
 *  `filter` repeats as `column:op:value`, and the value keeps its own colons
 *  because the server splits only the first two. */
export function rowsQueryString(q: RowsQuery): string {
  const params = new URLSearchParams({ path: q.path, table: q.table, limit: String(q.limit), order: q.descending ? 'desc' : 'asc' })
  if (q.orderBy) params.set('order_by', q.orderBy)
  for (const f of q.filters) {
    params.append('filter', f.op === 'null' || f.op === 'not_null' ? `${f.column}:${f.op}` : `${f.column}:${f.op}:${f.value}`)
  }
  return params.toString()
}

export function readDatabaseRows(fetchFn: FetchFn, basePath: string, q: RowsQuery): Promise<ServicesResult<DatabaseRowsResponse>> {
  return request(fetchFn, 'read rows', `${basePath}/services/databases/rows?${rowsQueryString(q)}`)
}
