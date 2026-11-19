import { ApiError } from './ApiClient.js';

// reference-store, reached through llm-bridge-server's gateway at
// `<bridge>/reference-store`: the one list of reference types (what each kind
// of record a chip can name looks like, which field names it, what to show
// under the name, where its page is) and the lookup of ids for the signed-in
// person, narrowed to what they may see. One reference-store per bridge
// server; a connected server's is reached through that server's connection
// proxy, so a chip there is looked up by the server that owns the record.

/** One record an id names, as reference-store answers it for the signed-in
 *  person: which type and service, the store's answer unchanged, the record's
 *  name and page, or `refused` with nothing else. */
export interface ResolvedRefMatch {
  type: string;
  service: string;
  /** The store's answer, unchanged. Absent on a refused match. */
  data?: unknown;
  /** The record's name, read by the type's label field. */
  label?: string;
  /** The record's page, the type's page template filled in; absent when the
   *  type has no page of its own. */
  page_url?: string;
  /** The record exists and the signed-in person may not see it. */
  refused?: boolean;
}

/** A store that could not say whether it holds an id. Present means the id's
 *  absence from that store is NOT known to be a miss. */
export interface ResolveRefError {
  id: string;
  type: string;
  error: string;
}

export interface ResolveResponse {
  /** Every requested id is present; an empty array is a definitive miss. */
  results: Record<string, ResolvedRefMatch[]>;
  errors?: ResolveRefError[];
}

/** How a summary field's value should be shown. A hint about the value, not a
 *  rendering: the chip formats it. reference-store's `GET /vocabulary` serves
 *  the list. */
export type ReferenceFieldFormat =
  | 'text'
  | 'number'
  | 'unix_seconds'
  | 'unix_milliseconds'
  | 'iso_time'
  | 'date'
  | 'cents'
  | 'probability'
  | 'url'
  | 'reference'
  | 'boolean';

/** One value a chip shows under a record's name. `field` is a dotted path
 *  into the record (`messages.0.room_id`). */
export interface ReferenceSummaryField {
  field: string;
  label: string;
  format: ReferenceFieldFormat;
}

/** One row of reference-store's list, as `GET /types` serves it. */
export interface ReferenceType {
  type: string;
  service: string;
  description: string;
  get?: string;
  id_patterns?: string[];
  search?: string;
  search_results?: string;
  label_field?: string;
  summary?: ReferenceSummaryField[];
  /** The record's page: `{id}` and `{data.<path>}` placeholders, already
   *  filled into a match's `page_url`. Empty: no page of its own. */
  page?: string;
  page_site?: string;
  glyph?: string;
  access: 'grant' | 'owner';
}

export interface ReferenceStoreClientConfig {
  fetch: typeof fetch;
  /** Where reference-store is reached, e.g. dash's
   *  '/api/bridge/reference-store'. */
  basePath: string;
}

export class ReferenceStoreClient {
  private readonly doFetch: typeof fetch;
  readonly basePath: string;

  constructor(config: ReferenceStoreClientConfig) {
    this.doFetch = config.fetch;
    this.basePath = config.basePath.replace(/\/$/, '');
  }

  /** What each id names, for the signed-in person: every record it names, a
   *  record they may not see marked `refused`, and `[]` for an id nothing
   *  holds. Throws on a non-2xx, so a chip says it could not ask rather than
   *  reading as "this id names nothing". */
  async resolve(ids: string[]): Promise<ResolveResponse> {
    const path = `${this.basePath}/resolve`;
    const res = await this.doFetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids }),
    });
    if (!res.ok) throw await failure(res, 'POST', path);
    return (await res.json()) as ResolveResponse;
  }

  /** The whole list of reference types. */
  async types(): Promise<ReferenceType[]> {
    const path = `${this.basePath}/types`;
    const res = await this.doFetch(path);
    if (!res.ok) throw await failure(res, 'GET', path);
    const answer = (await res.json()) as { types: ReferenceType[] };
    return answer.types;
  }
}

async function failure(res: Response, method: string, path: string): Promise<ApiError> {
  const body = await res.text().catch(() => '');
  return new ApiError({ message: `${method} ${path} failed: ${res.status} ${res.statusText}`, status: res.status, body, method, path });
}

/** A type whose ids can be spotted in prose and looked up. */
export function isResolvableReferenceType(type: ReferenceType): boolean {
  return Boolean(type.get) && (type.id_patterns?.length ?? 0) > 0;
}
