import type {
  Entry,
  FolderListWire,
  HarnessConfig,
  HarnessMeta,
  HookResolveInput,
  ManagedSessionDetail,
  MessagesResponse,
  ModelOption,
  PendingHook,
  RecentBundleResponse,
  SearchHit,
  SearchHitWire,
  SearchResponse,
  SessionConfig,
  SessionAction,
  SessionFile,
  SessionPermissionState,
  SessionSummaryFilterAxes,
  SummaryResponse,
  ValidatorsResponse,
  SessionPrincipal,
} from './types.js';
import type { FileNamedByTools, FileNamedByToolsContent } from '../reduce/fileMentions.js';
import { SUMMARY_FILTER_AXES } from './types.js';
import type { Signal, SignalResolveState, SignalWire } from './signals.js';
import { SIGNAL_STATE_OPEN, signalFromWire } from './signals.js';
import { sessionBriefFromWire, type LooseEndState, type SessionBrief, type SessionBriefViewWire } from './sessionBriefs.js';
import type {
  HarnessConfigWire,
  HarnessInfoWire,
  HookEventWire,
  ManagedSessionDetailWire,
  StoreModelWire,
} from './wireEvents.js';
import { sessionInfoFromWire } from './sessionInfo.js';
import { summaryFromManaged } from '../sync/sse.js';
import { HOOK_PHASE_AWAITING, pendingHookFromWire } from '../store/pendingHooks.js';

/** How many content-search hits to ask `GET /sessions/search` for.
 *
 *  This is the cap log-store has always applied (`handleSearch` defaults `limit`
 *  to 100). It used to apply because the client sent no `limit` at all, so the
 *  bound was the server's fallback rather than anyone's choice, and the client
 *  could not tell a complete result from a truncated one. Sending it explicitly
 *  changes no behavior and makes `truncated` answerable.
 *
 *  Raising it is NOT free, and the reason is not the request. Measured on this
 *  host 2026-08-02: a broad query matches thousands of sessions (`the` 11,124,
 *  `noteboard` 8,110, `deploy` 3,635), and of a 100-hit page only 8–45 sessions
 *  are in the loaded sidebar window, so 55–92% of hits already render nothing.
 *  Every extra hit is another row the list cannot paint until summaries can be
 *  fetched by id. Raise this together with that fetch, never before it. */
export const SEARCH_HIT_LIMIT = 100;

/** Map the opaque snake_case `harness_config` bag to the camelCase `HarnessConfig`.
 *  The bridge's well-known keys are copied explicitly (so a wire rename fails the
 *  type-check here); every OTHER key is carried through unchanged — `harness_config`
 *  is opaque on the Go side, and dropping an unnamed knob would make this layer lossy.
 *  Absent well-known fields stay absent; nothing is invented. */
function harnessConfigFromWire(w: HarnessConfigWire): HarnessConfig {
  const {
    permission_mode,
    disable_network,
    permission_mode_custom,
    model,
    effort,
    ...rest
  } = w;
  const cfg: HarnessConfig = { ...rest };
  if (permission_mode !== undefined) cfg.permissionMode = permission_mode;
  if (disable_network !== undefined) cfg.disableNetwork = disable_network;
  if (permission_mode_custom !== undefined) {
    cfg.permissionModeCustom = {
      approval: permission_mode_custom.approval,
      sandbox: permission_mode_custom.sandbox,
    };
  }
  if (model !== undefined) cfg.model = model;
  if (effort !== undefined) cfg.effort = effort;
  return cfg;
}

/** Map the snake_case `msg.HarnessInfo` from `GET /harnesses` to the camelCase
 *  `HarnessMeta`. The single source of truth for this mapping — every field is copied
 *  explicitly (a wire rename fails the type-check here), so no snake_case key leaks into
 *  the client. `capabilities` defaults to `[]` only to keep the type non-optional; the
 *  wire always sends it. Absent optional fields stay absent. */
function harnessMetaFromWire(w: HarnessInfoWire): HarnessMeta {
  return {
    name: w.name,
    label: w.label,
    emoji: w.emoji,
    tint: w.tint,
    available: w.available,
    capabilities: w.capabilities ?? [],
    hookEvents: w.hook_events,
    supportedProviders: w.supported_providers,
    supportedPermissionModes: w.supported_permission_modes,
    pty: w.pty,
    supportsDisableNetwork: w.supports_disable_network,
  };
}

/** Project a snake_case `GET /models` row to the controls-bar `ModelOption`. `value` is
 *  the model id the config POST sends; `label` mirrors bridge-ui's `name ($in/$out)`
 *  format, falling back to just the name (or id) when cost is not reported — never
 *  fabricating a cost; `shortName` carries the registry's dense nickname through for a
 *  picker that has no room for the full label. */
function modelOptionFromWire(w: StoreModelWire): ModelOption {
  const name = w.name || w.id;
  const label =
    typeof w.input_cost === 'number' && typeof w.output_cost === 'number'
      ? `${name} ($${w.input_cost}/$${w.output_cost})`
      : name;
  // A row with no `short_name` gets the empty string, deliberately NOT the name or the id.
  // "This model has no nickname yet" is a real and different answer from "its nickname
  // happens to equal its full name", and substituting one for the other here would erase
  // that distinction for every consumer downstream. What to draw in a picker when the
  // nickname is missing — the full label, an abbreviation, the raw id — is a presentation
  // decision that belongs to the UI at the edge, which can see how much room it has. This
  // mapper is a transport layer: it reports what the registry said and nothing more.
  return { value: w.id, label, provider: w.provider, shortName: w.short_name ?? '' };
}

/** Project a create/fork response (the canonical snake_case `msg.ManagedSession`) to a
 *  `CreatedSession`. The wire id field is `session_id` — read it from the CANONICAL key
 *  (never a `sessionId` that these endpoints don't emit) and surface it camelCase, while
 *  carrying the rest of the payload through unchanged. */
function createdSessionFromWire(w: Record<string, unknown>): CreatedSession {
  const sessionId = typeof w['session_id'] === 'string' ? (w['session_id'] as string) : '';
  return { ...w, sessionId };
}

/** The error every non-2xx response throws.
 *
 *  It carries the HTTP status and the raw response body alongside the message,
 *  because some refusals mean something specific and a caller has to be able to
 *  tell them apart. The spend-ceiling gate is the one that forced this: the
 *  server refuses a send with a 402 whose JSON body names both dollar figures
 *  (`internal/server/sessions.go` `writeRefusalIfOverBudget`), and the only way
 *  to read those figures used to be to pick the JSON back out of the English
 *  message string — which is guessing, not reading.
 *
 *  `message` keeps the exact text it always had, so anything already rendering
 *  `e.message` is unchanged.
 *
 *  `body` is the response text, empty when the body could not be read. It is
 *  passed through unchanged: no parsing, no truncation — the layer that knows
 *  what shape to expect does the parsing. */
export class ApiError extends Error {
  override readonly name = 'ApiError';
  /** HTTP status of the refused response. */
  readonly status: number;
  /** The response body verbatim, or '' when it could not be read. */
  readonly body: string;
  /** The request method, e.g. 'POST'. */
  readonly method: string;
  /** The path under `basePath`, e.g. '/sessions/br_1/send'. */
  readonly path: string;

  constructor(init: { message: string; status: number; body: string; method: string; path: string }) {
    super(init.message);
    this.status = init.status;
    this.body = init.body;
    this.method = init.method;
    this.path = init.path;
  }
}

/** The auth'd fetch + API root the client speaks through. dash passes its
 *  cookie-credentialed `apiFetch` and `basePath = '/api/bridge'`. */
export interface ApiClientConfig {
  fetch: typeof fetch;
  basePath: string;
}

/** Mutation response from POST /sessions and POST /sessions/{id}/send etc. */
export interface CreatedSession {
  sessionId: string;
  [k: string]: unknown;
}

/** The answer to POST /sessions/{id}/send (llm-bridge `SendMessageResponse`).
 *  `status` is `sent`, or `held` when the server is waiting to restart and will
 *  send the message once it can; `heldMessageId` then names it, and the session's
 *  stream carries a `message_held_for_restart` system event for it. */
export interface SendResult {
  status: 'sent' | 'held';
  messageId?: string;
  heldMessageId?: string;
}

/**
 * Typed wrapper over the additive chat-page endpoints (§5 of the architecture doc)
 * plus the existing bridge mutation endpoints. Every method is a thin, honest
 * pass-through — no formatting or fallback: it returns exactly what the backend
 * sends, and throws loudly on a non-2xx so callers can revert optimistic state.
 */
export class ApiClient {
  private readonly doFetch: typeof fetch;
  readonly basePath: string;

  constructor(config: ApiClientConfig) {
    this.doFetch = config.fetch;
    this.basePath = config.basePath.replace(/\/$/, '');
  }

  /** The auth'd fetch, for the SSE transport (which streams rather than
   *  awaiting JSON). Same credentialed function the read/mutation methods use. */
  fetchFor(): typeof fetch {
    return this.doFetch;
  }

  private async getJSON<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await this.doFetch(`${this.basePath}${path}`, init);
    if (!res.ok) {
      // GET refusals never carried their body in the message and still don't;
      // reading it here would change nothing a caller sees. It is read onto the
      // ApiError all the same, so a future caller that needs it has it.
      const body = await res.text().catch(() => '');
      throw new ApiError({
        message: `GET ${path} failed: ${res.status} ${res.statusText}`,
        status: res.status,
        body,
        method: 'GET',
        path,
      });
    }
    return (await res.json()) as T;
  }

  private async postJSON<T>(path: string, body?: unknown): Promise<T> {
    const res = await this.doFetch(`${this.basePath}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) {
      const detail = await res.text().catch(() => '');
      throw new ApiError({
        message: `POST ${path} failed: ${res.status} ${res.statusText} ${detail}`.trim(),
        status: res.status,
        body: detail,
        method: 'POST',
        path,
      });
    }
    return (await res.json().catch(() => ({}))) as T;
  }

  private async putJSON<T>(path: string, body?: unknown): Promise<T> {
    const res = await this.doFetch(`${this.basePath}${path}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) {
      const detail = await res.text().catch(() => '');
      throw new ApiError({
        message: `PUT ${path} failed: ${res.status} ${res.statusText} ${detail}`.trim(),
        status: res.status,
        body: detail,
        method: 'PUT',
        path,
      });
    }
    return (await res.json().catch(() => ({}))) as T;
  }

  /** DELETE with no body either way.
   *
   *  The routes this serves answer 204, so there is nothing to parse and nothing is
   *  returned. It is LOUD on the same terms as the other three: a refusal throws an
   *  `ApiError` carrying the status and the server's message, so an optimistic caller
   *  can put back what it removed. */
  private async deleteRequest(path: string): Promise<void> {
    const res = await this.doFetch(`${this.basePath}${path}`, { method: 'DELETE' });
    if (!res.ok) {
      const detail = await res.text().catch(() => '');
      throw new ApiError({
        message: `DELETE ${path} failed: ${res.status} ${res.statusText} ${detail}`.trim(),
        status: res.status,
        body: detail,
        method: 'DELETE',
        path,
      });
    }
  }

  // --- chat-page read endpoints ---

  /** Projected sidebar list, newest first, paginated, and optionally narrowed to
   *  the given axes server-side.
   *
   *  Filtering here rather than over the returned page is the whole point: the
   *  page is a strict newest-first prefix, and on a box whose recent history is
   *  mostly machine traffic that prefix is spent before it reaches the sessions
   *  the user wants. Sieving it client-side can only ever throw rows away.
   *
   *  Each axis is sent as REPEATED parameters (`?type=a&type=b`), never joined
   *  with commas: these values are free-form strings from the sessions table — a
   *  purpose on this box reads "browser verification + A/B perf" — and one
   *  containing a comma would be silently cut in half by a join. */
  getSummary(opts?: {
    limit?: number;
    before?: string;
    filter?: SessionSummaryFilterAxes;
    /** Look up these exact sessions, whatever their position in the recency
     *  order. NOT a seventh filter axis — a LOOKUP, for a caller that already
     *  holds ids and needs what they are called.
     *
     *  Paging cannot answer that question. The signals inbox lists open signals
     *  across every session, and the row it needs a name from may be thousands
     *  deep in a listing ordered by updated_at, so no page size the sidebar
     *  would ever ask for reaches it. Naming either lookup switches the request
     *  to the POST-body encoding — see the note in the method body. */
    sessionIds?: readonly string[];
    /** Look up the CHILDREN of these sessions — "what did these spawn?". Also a
     *  lookup rather than an axis, and unanswerable by paging for a sharper
     *  reason than `sessionIds`: a child is ordered by its OWN recency, not its
     *  parent's, so a session that spawned 106 subagents last week has them
     *  scattered thousands of rows deep.
     *
     *  Several parents at once, deliberately. A caller asks about a whole page of
     *  rows in ONE request, which is also what lets it learn WHICH rows have
     *  children — no count exists anywhere, and none was added. */
    managerSessionIds?: readonly string[];
  }): Promise<SummaryResponse> {
    // The id lookups go as a POST body, never on the URL. Their lists are
    // unbounded — one id per session the sidebar has loaded — and on this host
    // the query-string encoding reached 93 KB, which nginx answers not with a
    // refusal but by destroying the whole HTTP/2 connection (GOAWAY at
    // ~11.5 KB of URL, measured), killing every other request in flight with
    // it. The listing/paging path stays on GET, where the request is small and
    // conditional GET rides the browser cache.
    if (opts?.sessionIds?.length || opts?.managerSessionIds?.length) {
      return this.postJSON<SummaryResponse>('/sessions/summary', {
        ...(opts.limit != null ? { limit: opts.limit } : {}),
        ...(opts.before ? { before: opts.before } : {}),
        ...(opts.filter?.harness?.length ? { harnesses: opts.filter.harness } : {}),
        ...(opts.filter?.status?.length ? { statuses: opts.filter.status } : {}),
        ...(opts.filter?.type?.length ? { types: opts.filter.type } : {}),
        ...(opts.filter?.purpose?.length ? { purposes: opts.filter.purpose } : {}),
        ...(opts.filter?.mode?.length ? { modes: opts.filter.mode } : {}),
        ...(opts.filter?.machine?.length ? { machines: opts.filter.machine } : {}),
        ...(opts.filter?.principal?.length ? { principals: opts.filter.principal } : {}),
        ...(opts.sessionIds?.length ? { session_ids: opts.sessionIds } : {}),
        ...(opts.managerSessionIds?.length ? { manager_session_ids: opts.managerSessionIds } : {}),
      });
    }
    const params = new URLSearchParams();
    if (opts?.limit != null) params.set('limit', String(opts.limit));
    if (opts?.before) params.set('before', opts.before);
    if (opts?.filter) {
      for (const axis of SUMMARY_FILTER_AXES) {
        for (const value of opts.filter[axis] ?? []) params.append(axis, value);
      }
    }
    const qs = params.toString();
    return this.getJSON<SummaryResponse>(`/sessions/summary${qs ? `?${qs}` : ''}`);
  }

  /** The N most-recent sessions, each with summary + materialized turns, in one trip. */
  getRecentBundle(opts?: { n?: number; turns?: number }): Promise<RecentBundleResponse> {
    const params = new URLSearchParams();
    if (opts?.n != null) params.set('n', String(opts.n));
    if (opts?.turns != null) params.set('turns', String(opts.turns));
    const qs = params.toString();
    return this.getJSON<RecentBundleResponse>(`/sessions/recent-bundle${qs ? `?${qs}` : ''}`);
  }

  /** The folder list, in the order the server keeps it (`GET /folders`).
   *
   *  This is the single source of truth for BOTH which folders exist and what
   *  order they go in. Folders are rows in their own right: one can be created
   *  with nothing in it, and it stays real until it is deleted. Deriving the list
   *  from the loaded sessions instead — which is what chat-core did — gets both
   *  facts wrong at once, inventing an order the user never chose and making an
   *  empty folder unrepresentable.
   *
   *  `folder_order` is coerced to `[]` by the handler when the store returns a nil
   *  slice, but a nil Go slice serializes to JSON `null` in general, so the shape
   *  is checked here rather than trusted. */
  async listFolders(): Promise<string[]> {
    const wire = await this.getJSON<FolderListWire>('/folders');
    return Array.isArray(wire?.folder_order) ? wire.folder_order : [];
  }

  /** Create a folder (`POST /folders`, body `{name}`).
   *
   *  A folder is a row of its own, so it can be created empty and stays real until
   *  it is deleted. The server INSERTs at `MAX(position)+1` with
   *  `ON CONFLICT(name) DO NOTHING`, so creating one that already exists is a
   *  no-op rather than an error — but a blank name is a 400.
   *
   *  LOUD: `postJSON` throws on any non-2xx so the caller reverts its optimistic
   *  list instead of showing a folder the server never took. */
  createFolder(name: string): Promise<unknown> {
    return this.postJSON('/folders', { name });
  }

  /** Delete a folder (`DELETE /folders/{name}`).
   *
   *  ⚠️ This is TWO writes in one transaction, and the second one is easy to miss:
   *  `Store.DeleteFolder` first runs `UPDATE sessions SET folder_name='' WHERE
   *  folder_name=?` and only then drops the row. Every session in the folder is
   *  un-filed, not deleted — a caller mirroring this optimistically has to move
   *  those rows too, or the sidebar keeps drawing a header for a folder the server
   *  no longer holds.
   *
   *  LOUD: throws on any non-2xx. */
  deleteFolder(name: string): Promise<unknown> {
    return this.deleteRequest(`/folders/${encodeURIComponent(name)}`);
  }

  /** Rename a folder (`PUT /folders/{name}`, body `{new_name}`).
   *
   *  ⚠️ Also two writes, and the second has two shapes. `Store.RenameFolder` moves
   *  every session (`UPDATE sessions SET folder_name=new WHERE folder_name=old`),
   *  then — if `new_name` ALREADY EXISTS — deletes the old row and lets its
   *  sessions join the existing folder at that folder's own position (a merge).
   *  Only when the new name is free is the row itself renamed, keeping its place.
   *
   *  The server treats an empty name or `old === new` as a no-op and answers 400
   *  for a blank body, so callers should not send those at all.
   *
   *  LOUD: throws on any non-2xx. */
  renameFolder(name: string, newName: string): Promise<unknown> {
    return this.putJSON(`/folders/${encodeURIComponent(name)}`, { new_name: newName });
  }

  /** File a session into a folder, or un-file it (`PUT /sessions/{id}/folder`,
   *  body `{folder}`). An empty string clears the assignment.
   *
   *  ⚠️ A move into a folder that does not exist CREATES it: `Store.SetSessionFolder`
   *  INSERTs the name (`ON CONFLICT DO NOTHING`) before updating the session row, so
   *  this one call is both halves of "new folder, and put this in it" and callers do
   *  not need a separate create first.
   *
   *  Unlike the folder routes, this one calls `notifyChanged`, so the session-list
   *  stream re-broadcasts the row and the server confirms the move on its own. The
   *  optimistic patch is what makes the click feel instant, not what makes it stick.
   *
   *  LOUD: `putJSON` throws on any non-2xx (404 when the session id is unknown). */
  setSessionFolder(sessionId: string, folder: string): Promise<unknown> {
    return this.putJSON(`/sessions/${sessionId}/folder`, { folder });
  }

  /** The cheap staleness check for a set of session ids.
   *
   *  POST body, not a query string: the list is one id per cached session, so
   *  it grows with the cache toward the URL length at which nginx destroys the
   *  whole HTTP/2 connection (~11.5 KB, measured — the cliff that broke
   *  getSummary's id lookups first). This endpoint emits no ETag, so unlike
   *  getSummary there is no conditional-GET ride to preserve and no reason to
   *  keep a GET path beside it. */
  getValidators(ids: string[]): Promise<ValidatorsResponse> {
    if (ids.length === 0) return Promise.resolve({});
    return this.postJSON<ValidatorsResponse>('/sessions/validators', { ids });
  }

  /** The server's own default turn count for the bounded shape, mirrored here
   *  so a caller that names no limit still asks for the same window. */
  static readonly DEFAULT_MESSAGE_TURNS = 30;

  /**
   * Materialized tail (or a page older than `before`) for one session.
   *
   * A limit always goes on the wire, even when the caller names none. That
   * endpoint serves two shapes off the same path: with `limit` or `before` it
   * answers the bounded `{ model }` this method is typed for, and with neither
   * it answers the legacy unbounded array — every event in the session, which
   * reached 306MB and 52s for one real session on this box. Omitting the
   * parameter would therefore return a body that does not match
   * `MessagesResponse` at all, and `resp.model` would read as undefined with
   * no error anywhere. There is no case where this client wants that shape.
   */
  getMessages(id: string, opts?: { limit?: number; before?: string | number }): Promise<MessagesResponse> {
    return this.getJSON<MessagesResponse>(`/sessions/${id}/messages?${readingPageQuery(opts)}`);
  }

  /** One entry of the reading page with its tool input and output in full — what a
   *  shortened entry (`toolResultTruncated` / `toolInputTruncated`) points at.
   *  `eventId` is the entry's log-store event id (`entry.eventId`). */
  getEntry(sessionId: string, eventId: number): Promise<Entry> {
    return this.getJSON<Entry>(`/sessions/${sessionId}/entries/${eventId}`);
  }

  /**
   * The UNPROJECTED page: every entry the window holds, duplicates included, each
   * carrying its full `raw` source event.
   *
   * `getMessages` is projected by log-store — no `raw`, no duplicate entries — because
   * the Turns view renders about a tenth of what used to be sent. Measured 2026-08-25
   * on one real session: 9.91 MB against 0.95 MB, with `raw` alone 78.9% of it.
   *
   * ⚠️ Roughly TEN TIMES the bytes. Only two surfaces need it — the Raw pane, which
   * renders `entry.raw` directly, and the Timeline, which is the audit view over every
   * stored event. Nothing on the default path may call this.
   */
  getMessagesRaw(
    id: string,
    opts?: { limit?: number; before?: string | number },
  ): Promise<MessagesResponse> {
    return this.getJSON<MessagesResponse>(`/sessions/${id}/messages/raw?${messagePageQuery(opts)}`);
  }

  /** Full-text content search across session transcripts. Returns the matching
   *  session ids so the list/filter path can fold content hits in alongside the
   *  instant local name match. Async augmentation only — the caller must NOT block
   *  the local name filter on this. Throws loudly on a non-2xx.
   *
   *  The endpoint answers with a BARE ARRAY of `{session_id, match_count}`. This
   *  used to be read as `SearchResponse.sessionIds`, a property a JSON array does
   *  not have: the result was `undefined`, `new Set(undefined)` produced an empty
   *  set without throwing, and content search silently matched nothing for every
   *  query while the display-name filter went on working. Map the real shape here,
   *  at the wire edge, and fail loudly if it is not the array the backend promises. */
  async search(q: string, opts?: { limit?: number }): Promise<SearchResponse> {
    const limit = opts?.limit ?? SEARCH_HIT_LIMIT;
    const qs = new URLSearchParams({ q, limit: String(limit) }).toString();
    const wire = await this.getJSON<SearchHitWire[]>(`/sessions/search?${qs}`);
    if (!Array.isArray(wire)) {
      throw new Error(
        `GET /sessions/search returned ${typeof wire}, expected an array of {session_id, match_count}`,
      );
    }
    const hits: SearchHit[] = wire
      .map((h) => ({ sessionId: h.session_id, matchCount: h.match_count }))
      .sort((a, b) => b.matchCount - a.matchCount);
    return {
      sessionIds: hits.map((h) => h.sessionId),
      hits,
      limit,
      truncated: hits.length >= limit,
    };
  }

  /** Full per-session detail (`GET /sessions/{id}`) — the canonical ManagedSession,
   *  with its snake_case `info` mapped to a camelCase `SessionInfo`. This endpoint
   *  already exists and is live; the summary list deliberately omits `info`, so this
   *  is the lazy fetch that backs `useSessionInfo`. `info` is null when the harness
   *  has not reported one yet. Throws loudly on a non-2xx (getJSON).
   *
   *  The identity, lineage and spend fields ride through to the top level of the
   *  detail rather than onto `summary` — see `ManagedSessionDetail`. They are copied
   *  ONLY when the server sent them: an absent field stays absent, so a reader can
   *  tell "no ceiling reported" from "a ceiling of zero", which mean opposite
   *  things. That is why this maps key by key instead of defaulting like
   *  `summaryFromManaged` does. */
  async getSessionDetail(id: string): Promise<ManagedSessionDetail> {
    const wire = await this.getJSON<ManagedSessionDetailWire>(`/sessions/${id}`);
    const summary = summaryFromManaged(wire);
    return {
      sessionId: summary.sessionId,
      summary,
      info: wire.info ? sessionInfoFromWire(wire.info) : null,
      harnessConfig: wire.harness_config ? harnessConfigFromWire(wire.harness_config) : null,
      ...(wire.origin !== undefined ? { origin: wire.origin } : {}),
      ...(wire.pid !== undefined ? { pid: wire.pid } : {}),
      ...(wire.harness_session_id !== undefined
        ? { harnessSessionId: wire.harness_session_id }
        : {}),
      ...(wire.parent_id !== undefined
        ? { forkParentHarnessSessionId: wire.parent_id }
        : {}),
      ...(wire.forked_from_session_id !== undefined
        ? { forkedFromSessionId: wire.forked_from_session_id }
        : {}),
      ...(wire.manager_session_id !== undefined
        ? { managerSessionId: wire.manager_session_id }
        : {}),
      ...(wire.working_dir !== undefined ? { workingDir: wire.working_dir } : {}),
      ...(wire.spend_usd !== undefined ? { spendUsd: wire.spend_usd } : {}),
      ...(wire.max_budget_usd !== undefined ? { maxBudgetUsd: wire.max_budget_usd } : {}),
    };
  }

  // --- mutations (existing bridge endpoints; see bridge-ui useBridgeSession.ts) ---

  async createSession(opts?: {
    instanceId?: string;
    harness?: string;
    principalId?: string;
  }): Promise<CreatedSession> {
    const wire = await this.postJSON<Record<string, unknown>>('/sessions', {
      type: 'interactive',
      purpose: 'chat',
      origin: 'frontend',
      ...(opts?.instanceId ? { instance_id: opts.instanceId } : {}),
      ...(opts?.harness ? { harness: opts.harness } : {}),
      ...(opts?.principalId ? { principal_id: opts.principalId } : {}),
    });
    // POST /sessions returns the canonical `msg.ManagedSession` (snake_case) — its id
    // field is `session_id`, so read it from the canonical key rather than assuming a
    // `sessionId` these endpoints never emit.
    return createdSessionFromWire(wire);
  }

  async send(id: string, text: string): Promise<SendResult> {
    const wire = await this.postJSON<{ status: 'sent' | 'held'; message_id?: string; held_message_id?: string }>(
      `/sessions/${id}/send`,
      { message: text },
    );
    return { status: wire.status, messageId: wire.message_id, heldMessageId: wire.held_message_id };
  }

  /**
   * Share a file into a session. POST /sessions/{id}/files?filename=… with the bytes
   * as the body and their type as Content-Type. Answers the canonical record, whose
   * `path` is where the session's agent reads the file; the session's stream gets a
   * `session_file` event for it too. LOUD like every write: a refusal (too large,
   * a name that is not a plain name, file-store down) throws an `ApiError` carrying
   * the server's message.
   */
  async shareSessionFile(sessionId: string, file: Blob, filename: string): Promise<SessionFile> {
    const path = `/sessions/${encodeURIComponent(sessionId)}/files?filename=${encodeURIComponent(filename)}`;
    const res = await this.doFetch(`${this.basePath}${path}`, {
      method: 'POST',
      // A pasted or picked file the browser cannot type is still bytes; the server
      // requires a type, and this is the honest one.
      headers: { 'Content-Type': file.type || 'application/octet-stream' },
      body: file,
    });
    if (!res.ok) {
      const detail = await res.text().catch(() => '');
      throw new ApiError({
        message: `POST ${path} failed: ${res.status} ${res.statusText} ${detail}`.trim(),
        status: res.status,
        body: detail,
        method: 'POST',
        path,
      });
    }
    return (await res.json()) as SessionFile;
  }

  /**
   * Every action offered in a session, oldest first, each as its newest record.
   * GET /sessions/{id}/actions.
   */
  listSessionActions(sessionId: string): Promise<SessionAction[]> {
    return this.getJSON<SessionAction[]>(`/sessions/${encodeURIComponent(sessionId)}/actions`);
  }

  /**
   * Confirm a session action: llm-bridge-server runs it. POST
   * /sessions/{id}/actions/{action_id}/run answers the record as `running`; how the
   * run ends arrives as a `session_action` event. LOUD like every write: an action
   * that already ran, or whose command changed since it was offered, is a 409 that
   * throws an `ApiError` carrying the server's message. `inputs` is what the person
   * typed into the button's boxes, by input name (`SessionActionRunRequest`); a
   * missing or unknown one is a 400.
   */
  runSessionAction(sessionId: string, actionId: string, inputs?: Record<string, string>): Promise<SessionAction> {
    return this.postJSON<SessionAction>(
      `/sessions/${encodeURIComponent(sessionId)}/actions/${encodeURIComponent(actionId)}/run`,
      inputs && Object.keys(inputs).length > 0 ? { inputs } : {},
    );
  }

  /**
   * Every file on disk that the session's tool calls and their output named,
   * with absolute paths. GET /sessions/{id}/files-named-by-tools. The chat
   * matches a file an agent mentions against this list.
   */
  async listFilesNamedByTools(sessionId: string): Promise<FileNamedByTools[]> {
    const body = await this.getJSON<{ files: FileNamedByTools[] }>(
      `/sessions/${encodeURIComponent(sessionId)}/files-named-by-tools`,
    );
    return body.files;
  }

  /**
   * One file on that list as it is on disk now. GET
   * /sessions/{id}/files-named-by-tools/content?path=. A path not on the list is
   * a 404, a file over 1 MiB a 413 and one that is not text a 415, each thrown
   * as an `ApiError` carrying the server's message.
   */
  getFileNamedByToolsContent(sessionId: string, path: string): Promise<FileNamedByToolsContent> {
    return this.getJSON<FileNamedByToolsContent>(
      `/sessions/${encodeURIComponent(sessionId)}/files-named-by-tools/content?path=${encodeURIComponent(path)}`,
    );
  }

  /** Where a browser reads the bytes of an image on the files-named-by-tools list
   *  (`served_as: 'image'`): GET /sessions/{id}/files-named-by-tools/image?path=. */
  fileNamedByToolsImageUrl(sessionId: string, path: string): string {
    return `${this.basePath}/sessions/${encodeURIComponent(sessionId)}/files-named-by-tools/image?path=${encodeURIComponent(path)}`;
  }

  /** Who the sessions the caller may see were started as, most sessions first,
   *  with each principal's current name: GET /sessions/principals. A row with
   *  `principalId` '' counts the sessions started as no principal. */
  async listSessionPrincipals(): Promise<SessionPrincipal[]> {
    const body = await this.getJSON<{ principals: { principal_id: string; display_name: string; session_count: number }[] }>(
      '/sessions/principals',
    );
    return body.principals.map((row) => ({
      principalId: row.principal_id,
      displayName: row.display_name,
      sessionCount: row.session_count,
    }));
  }

  /** Where a browser reads a shared file's bytes. `inline` asks for it to be shown
   *  rather than downloaded; file-store grants that only to the types on its inline
   *  list, and serves everything else as a download whatever is asked. */
  sessionFileContentUrl(sessionId: string, fileId: string, opts?: { inline?: boolean }): string {
    const inline = opts?.inline ? '?inline=true' : '';
    return `${this.basePath}/sessions/${encodeURIComponent(sessionId)}/files/${encodeURIComponent(fileId)}/content${inline}`;
  }

  /** Where a browser reads the bytes of an image a settled tool result carried. See
   *  `ToolResultImage`: a live entry holds its bytes and needs no URL. */
  toolResultImageUrl(sessionId: string, eventId: number, index: number): string {
    return `${this.basePath}/sessions/${encodeURIComponent(sessionId)}/entries/${eventId}/images/${index}`;
  }

  /**
   * Compact a session's context. POST /sessions/{id}/compact — body `{ summary }` when a
   * caller supplies a compaction summary, else `{}`. LOUD: `postJSON` throws on any
   * non-2xx. The POST only ACKNOWLEDGES the request (a `compact_ack` system event);
   * compaction runs async and its real completion is the `compact_boundary` system entry
   * on the session's event stream — `useSessionControls` watches for that, never faking a
   * done state.
   * (bridge-ui: useBridgeSession's `compact`.)
   */
  compact(id: string, summary?: string): Promise<unknown> {
    return this.postJSON(`/sessions/${id}/compact`, summary ? { summary } : {});
  }

  /**
   * Fork a session into a new sibling. POST /sessions/{id}/fork — body
   * `{ display_name, type: 'interactive' }` (empty display name lets the server derive
   * "<parent> (fork)"). Returns the new `CreatedSession`, its id read from the canonical
   * `session_id` wire key. LOUD: throws on any non-2xx (e.g. the 409 the server returns
   * when the parent has no `harness_session_id` yet).
   * (bridge-ui: useBridgeSession's `forkSession`.)
   */
  async fork(id: string, displayName?: string): Promise<CreatedSession> {
    const wire = await this.postJSON<Record<string, unknown>>(`/sessions/${id}/fork`, {
      display_name: displayName ?? '',
      type: 'interactive',
    });
    return createdSessionFromWire(wire);
  }

  /**
   * Switch a live session between events and pty I/O mode. POST /sessions/{id}/mode with
   * body `{ mode }` (`'events' | 'pty'`). The server kills and respawns the harness via
   * --resume so history is preserved. Returns the raw response (which carries an
   * `attach_token` for a pty switch); pty attach-token management is a bridge-ui concern,
   * so chat-core passes the payload through unchanged. LOUD: throws on any non-2xx.
   * (bridge-ui: useBridgeSession's `switchMode`.)
   */
  switchMode(id: string, mode: 'events' | 'pty'): Promise<unknown> {
    return this.postJSON(`/sessions/${id}/mode`, { mode });
  }

  /**
   * Apply per-session config knobs. POST /sessions/{id}/config with the snake_case body
   * `{ model?, effort?, max_budget?, disabled_tools? }` — only the provided fields are
   * sent (an absent field is never serialized as null). This is the canonical path
   * bridge-ui uses for BOTH the controls-bar model/effort pre-start settings (applied
   * right after create) and changing them on a live session. LOUD: throws on any non-2xx.
   * (bridge-ui: useBridgeSession's `postConfig`, shared by `sendConfig` and
   * `raiseBudgetCeiling`.)
   */
  setConfig(id: string, config: SessionConfig): Promise<unknown> {
    const body: Record<string, unknown> = {};
    if (config.model !== undefined) body.model = config.model;
    if (config.effort !== undefined) body.effort = config.effort;
    if (config.maxBudget !== undefined) body.max_budget = config.maxBudget;
    if (config.disabledTools !== undefined) body.disabled_tools = config.disabledTools;
    return this.postJSON(`/sessions/${id}/config`, body);
  }

  /**
   * The registered harness types and their capabilities. GET /harnesses — the CANONICAL
   * registry the controls bar gates each control on (`capabilities`) and scopes the model
   * picker with (`supportedProviders`). Mapped snake_case → camelCase per harness. LOUD:
   * throws on any non-2xx. (bridge-ui reads the same endpoint through
   * `useBridgeHarnesses`.)
   */
  async getHarnesses(): Promise<HarnessMeta[]> {
    const wire = await this.getJSON<HarnessInfoWire[] | null>('/harnesses');
    // A nil Go slice serializes as JSON `null`; treat any non-array as empty.
    return Array.isArray(wire) ? wire.map(harnessMetaFromWire) : [];
  }

  /**
   * The available models for the controls-bar picker. GET /models — the canonical
   * model-store registry. Drops `enabled=false` rows (mirroring bridge-ui) and projects
   * each to a `ModelOption` carrying `provider` so `useModels` can filter by a harness's
   * supported providers, plus the `shortName` nickname a dense picker renders. LOUD:
   * throws on any non-2xx.
   */
  async getModels(): Promise<ModelOption[]> {
    const wire = await this.getJSON<StoreModelWire[] | null>('/models');
    // A nil Go slice serializes as JSON `null`; treat any non-array as empty.
    return Array.isArray(wire) ? wire.filter((m) => m.enabled).map(modelOptionFromWire) : [];
  }

  rename(id: string, displayName: string): Promise<unknown> {
    return this.postJSON(`/sessions/${id}/rename`, { display_name: displayName });
  }

  /**
   * Mark a session done (or undo it).
   *
   * `done: true` sets state=completed and moves the session into the canonical
   * `Archive` folder; `done: false` reverses both. The two halves are ONE atomic
   * action server-side (`handleMarkSessionDone`), which is why there is a single
   * route taking a boolean rather than an archive/unarchive pair.
   *
   * This replaces `archive()` / `unarchive()`, which POSTed to
   * `/sessions/{id}/archive` and `/sessions/{id}/unarchive`. Neither route has ever
   * existed: the gateway registers `PUT /sessions/{id}/folder` and
   * `POST /sessions/{id}/mark-done`, and "unarchive" appears in no Go source at all.
   * Probed live 2026-08-02 against one session id — archive 404, unarchive 404,
   * mark-done 204.
   */
  markSessionDone(id: string, done: boolean): Promise<unknown> {
    return this.postJSON(`/sessions/${id}/mark-done`, { done });
  }

  resume(id: string): Promise<unknown> {
    return this.postJSON(`/sessions/${id}/resume`, {});
  }

  /**
   * Interrupt/stop a running session's current turn.
   * POST /sessions/{id}/interrupt.
   *
   * This is deliberately a LOUD call: `postJSON` throws on any non-2xx, and this
   * method keeps that. The server returns 409 ("nothing was stopped") while a tool
   * still holds the turn (until the server-side gate fix ships) — that MUST surface
   * as a thrown error so the caller does NOT optimistically mark the session idle.
   * Never swallow this into a fake-idle.
   */
  interrupt(id: string): Promise<unknown> {
    return this.postJSON(`/sessions/${id}/interrupt`, {});
  }

  /**
   * Set a session's per-session permission state — the mode plus the two side-axes
   * the same endpoint persists. PUT /sessions/{id}/permission-mode.
   *
   * The bridge persists all three into `harness_config`; the prehook reads
   * `permission_mode` live, so a mode change takes effect on the session's NEXT tool
   * call without a restart, while `disable_network` and `permission_mode_custom` reach
   * the harness on its next spawn. `mode` is one of the canonical `msg.PermissionMode*`
   * values (ask / auto / bypass / plan / read / ask_all / block_all / custom) —
   * validated server-side.
   *
   * ONE PUT carries every axis, deliberately: three separate writes would let a partial
   * failure leave the UI describing a state the server is not in. An omitted axis means
   * "leave the stored value alone" (the server reads the field as absent), so callers
   * that render only some of the controls do not clear the ones they do not show.
   *
   * This is a LOUD call: `putJSON` throws on any non-2xx (e.g. 400 on an invalid mode,
   * 404 on an unknown session), so an optimistic UI update can revert.
   */
  setSessionPermissionState(id: string, state: SessionPermissionState): Promise<unknown> {
    const body: Record<string, unknown> = { mode: state.mode };
    if (state.disableNetwork !== undefined) body.disable_network = state.disableNetwork;
    if (state.permissionModeCustom !== undefined) {
      body.permission_mode_custom = {
        approval: state.permissionModeCustom.approval ?? '',
        sandbox: state.permissionModeCustom.sandbox ?? '',
      };
    }
    return this.putJSON(`/sessions/${id}/permission-mode`, body);
  }

  // --- parked hooks (the permission banner) ---

  /**
   * The hooks this session has parked on a human decision.
   * GET /sessions/{id}/hooks/pending.
   *
   * The server answers with whole `msg.Event`s, so the hook rides under `.hook`; this
   * unwraps and camelCases them, and keeps only `awaiting_resolution` entries that carry
   * a request id — the rest cannot be resolved and would be a card with no button.
   *
   * Hydration matters because the session SSE resumes from Last-Event-ID, and a hook
   * parked before the client attached is a tool call frozen with nothing on screen.
   */
  async getPendingHooks(id: string): Promise<PendingHook[]> {
    const events = await this.getJSON<Array<{ hook?: HookEventWire }>>(
      `/sessions/${id}/hooks/pending`,
    );
    if (!Array.isArray(events)) return [];
    const out: PendingHook[] = [];
    for (const ev of events) {
      const hook = pendingHookFromWire(ev?.hook);
      if (hook && hook.phase === HOOK_PHASE_AWAITING) out.push(hook);
    }
    return out;
  }

  /**
   * Deliver a decision for a parked hook.
   * POST /sessions/{id}/hooks/{request_id}/resolve.
   *
   * LOUD, like every other mutation here: `postJSON` throws on a non-2xx so the caller
   * restores the card instead of leaving the user believing a frozen tool call was
   * answered. `updatedInput` replaces the tool input wholesale (how a `user_input`
   * hook's answers reach the model) and is omitted when absent — never sent as null.
   */
  resolveHook(id: string, input: HookResolveInput): Promise<unknown> {
    const body: Record<string, unknown> = {
      behavior: input.behavior,
      resolved_by: input.resolvedBy || 'user',
    };
    if (input.updatedInput !== undefined) body.updated_input = input.updatedInput;
    if (input.message) body.message = input.message;
    return this.postJSON(
      `/sessions/${id}/hooks/${encodeURIComponent(input.requestId)}/resolve`,
      body,
    );
  }

  // --- session signals (the questions a session is waiting on) ---

  /**
   * Open signals on EVERY surface — one session's when `sessionId` is given,
   * every session's when it is not. GET /signals?state=open.
   *
   * No `surface` filter, on purpose. The server routes an autonomous session's
   * signals to the `kanban` surface, and until 2026-10-19 this read asked for
   * `surface=chat` only — so the 35 open kanban signals that name no todo were
   * shown nowhere at all. The chat now decides what needs the operator by the
   * raising session's TYPE (`net/attention.ts`), and shows the rest in a
   * separate, quieter group, which needs every surface in hand.
   *
   * ⚠️ Returns `null`, not an error and not an empty list, when the server
   * answers 404. A 404 here means this bridge-server predates the signals API,
   * and a server without the feature is not a failure to show anyone: every
   * signal surface renders nothing at all in that case. `[]` would be a lie of a
   * different kind — it says the feature is present and quiet. Any OTHER non-2xx
   * is a real failure and throws, like every other read on this client.
   *
   * The cross-session route is used even for a single session, deliberately. The
   * per-session route GET /sessions/{id}/signals answers 404 for BOTH "no
   * signals route here" and "no such session", and those two mean opposite
   * things to a caller; /signals only 404s for the first.
   */
  async getOpenSignals(opts?: {
    sessionId?: string;
    limit?: number;
  }): Promise<Signal[] | null> {
    const params = new URLSearchParams({ state: SIGNAL_STATE_OPEN });
    if (opts?.sessionId) params.set('session_id', opts.sessionId);
    if (opts?.limit != null) params.set('limit', String(opts.limit));
    const path = `/signals?${params.toString()}`;
    const res = await this.doFetch(`${this.basePath}${path}`);
    if (res.status === 404) return null;
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      throw new ApiError({
        message: `GET ${path} failed: ${res.status} ${res.statusText}`,
        status: res.status,
        body,
        method: 'GET',
        path,
      });
    }
    const wire: unknown = await res.json();
    if (!Array.isArray(wire)) {
      throw new Error(`GET ${path} returned ${typeof wire}, expected an array of signals`);
    }
    return (wire as SignalWire[]).map(signalFromWire);
  }

  /**
   * Close one signal without answering it. POST /signals/{id}/resolve.
   *
   * This is the SIGNAL-level verb, and it is not how a question gets answered: a
   * tool question resolves through its parked hook and a derived one through the
   * session's next message (see `store/signalResolve.ts`). What closes here is a
   * notification being acknowledged, or any signal being dismissed.
   *
   * The server refuses `acknowledged` for a question on purpose — a question
   * nobody answered has not been handled, and grading it "seen" would read as
   * handled on the surface that matters most, a worker's kanban card. That
   * refusal arrives as a thrown `ApiError`, which is correct: it reaches here
   * only from a click.
   *
   * LOUD for the same reason: unlike `getOpenSignals`, a 404 is NOT swallowed
   * here. A button that silently does nothing is worse than an error.
   */
  resolveSignal(signalId: string, state: SignalResolveState): Promise<unknown> {
    if (!signalId) return Promise.reject(new Error('a signal cannot be resolved without its id'));
    return this.postJSON(`/signals/${encodeURIComponent(signalId)}/resolve`, { state });
  }

  /**
   * Answer a question. POST /signals/{id}/answer.
   *
   * The ONE way to answer, whichever producer raised the question and whether
   * or not its session is still running. The server picks the delivery — hand
   * it to a still-blocked tool call, or send it as the session's next message,
   * restarting a reaped session and reopening an archived one — because a
   * request_id says a park EXISTED, not that it is still live, and only the
   * server knows which.
   *
   * `answers` is keyed by SIGNAL ID, which is what a form holds. The
   * title-keyed pairing a parked hook needs is derived server-side, next to
   * the parked tool input it has to be merged into.
   *
   * LOUD, like every other write here: a refusal — an incomplete answer, a
   * question already resolved — arrives as a thrown ApiError, because it
   * reaches here only from a click.
   */
  answerSignal(signalId: string, answers: Record<string, string>): Promise<unknown> {
    if (!signalId) return Promise.reject(new Error('a signal cannot be answered without its id'));
    return this.postJSON(`/signals/${encodeURIComponent(signalId)}/answer`, { answers });
  }

  /**
   * Chat briefs across the caller's sessions, most recently updated first.
   * GET /session-briefs.
   *
   * Returns `null` on a 404, the same as `getOpenSignals`: a bridge-server
   * that predates briefs is not a failure, and the surfaces showing briefs
   * then show nothing. Any other non-2xx throws.
   */
  async getSessionBriefs(opts?: {
    sessionIds?: readonly string[];
    looseEndState?: LooseEndState;
    updatedSince?: string;
    limit?: number;
  }): Promise<SessionBrief[] | null> {
    const params = new URLSearchParams();
    for (const id of opts?.sessionIds ?? []) params.append('session_id', id);
    if (opts?.looseEndState) params.set('loose_end_state', opts.looseEndState);
    if (opts?.updatedSince) params.set('updated_since', opts.updatedSince);
    if (opts?.limit != null) params.set('limit', String(opts.limit));
    const query = params.toString();
    const path = `/session-briefs${query ? `?${query}` : ''}`;
    const res = await this.doFetch(`${this.basePath}${path}`);
    if (res.status === 404) return null;
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      throw new ApiError({
        message: `GET ${path} failed: ${res.status} ${res.statusText}`,
        status: res.status,
        body,
        method: 'GET',
        path,
      });
    }
    const wire: unknown = await res.json();
    if (!Array.isArray(wire)) {
      throw new Error(`GET ${path} returned ${typeof wire}, expected an array of session briefs`);
    }
    return (wire as SessionBriefViewWire[])
      .map(sessionBriefFromWire)
      .filter((brief): brief is SessionBrief => brief !== null);
  }

  /** Answer a loose end: the server sends `text` to the session as a message
   *  quoting the remark, and closes it. Loud, like every other write. */
  answerLooseEnd(sessionId: string, looseEndId: string, text: string): Promise<unknown> {
    if (!sessionId || !looseEndId) return Promise.reject(new Error('a loose end is answered by its session and its id'));
    return this.postJSON(
      `/sessions/${encodeURIComponent(sessionId)}/loose-ends/${encodeURIComponent(looseEndId)}/answer`,
      { text },
    );
  }

  /** Close a loose end without an answer. */
  dismissLooseEnd(sessionId: string, looseEndId: string): Promise<unknown> {
    if (!sessionId || !looseEndId) return Promise.reject(new Error('a loose end is dismissed by its session and its id'));
    return this.postJSON(
      `/sessions/${encodeURIComponent(sessionId)}/loose-ends/${encodeURIComponent(looseEndId)}/dismiss`,
    );
  }
}

/** The bound both message pages carry. Never omitted: a request with no `limit`
 *  reaches the legacy unbounded shape, measured at 306 MB and 52s for one real
 *  session — see test/messagesAlwaysBounded.test.ts. */
function messagePageQuery(opts?: { limit?: number; before?: string | number }): string {
  const params = new URLSearchParams();
  params.set('limit', String(opts?.limit ?? ApiClient.DEFAULT_MESSAGE_TURNS));
  if (opts?.before != null) params.set('before', String(opts.before));
  return params.toString();
}

/** The reading page's query: bounded as every page is, with tool payloads shortened.
 *
 *  A page used to carry every tool's whole input and output. One grep result on
 *  br_1789409869293194266 was 620 KB of an 884 KB page, and the Turns view draws no
 *  tool output at all; with previews that page is 80 KB. A shortened entry says so
 *  (`toolResultTruncated`), and `getEntry` fetches it whole when someone opens it. */
function readingPageQuery(opts?: { limit?: number; before?: string | number }): string {
  return `${messagePageQuery(opts)}&payload=preview`;
}
