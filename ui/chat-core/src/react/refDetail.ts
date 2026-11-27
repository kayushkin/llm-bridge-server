import { useEffect, useState } from 'react';
import { useChatContext } from './context.js';
import type { ApiClient } from '../net/ApiClient.js';
import type { ManagedSessionDetail, SessionPrincipal, TurnModel } from '../net/types.js';
import type { NoteOrIssue } from '../net/NotesAndIssuesClient.js';
import type { ResolvedRefMatch, ResolveResponse } from '../net/ReferenceStoreClient.js';
import type { FileNamedByTools, FileNamedByToolsContent } from '../reduce/fileMentions.js';

// Data layer for reference chips. A chip is a passive linkification of an id
// that happens to appear in a message, so the same id can appear dozens of
// times in one transcript and each occurrence mounts its own chip. Fetching per
// chip would put dozens of identical requests on the wire for one render, so
// every loader here goes through a short-lived promise cache keyed by id and
// the duplicates share one flight. Each cache is per client (ClientScopedCache):
// two stores on one page number their records independently.
//
// The cache is deliberately NOT the SessionCache: that one is the durable
// IndexedDB store for sessions the user actually opened, and a chip is a
// glance, not a visit. Polluting it with every id ever mentioned would evict
// real sessions from the warm list.

/** How long a resolved reference stays fresh. Short: a chip shows live-ish
 *  state (a session's `state`, a todo's `status`), and a stale badge is worse
 *  than a second request. Matches bridge-ui's chip cache. */
const REF_TTL_MS = 30_000;

interface CacheSlot<T> {
  at: number;
  promise: Promise<T>;
}

/** One cache per client that answers. Two stores on one page — this server's
 *  and a connected server's, each with its own ApiClient — number their
 *  records independently, so `principal_000001` or a session id must never be
 *  answered from the other store's cache. The map from client to cache is weak,
 *  so a store that goes away takes its cache with it. */
class ClientScopedCache<T> {
  private byClient = new WeakMap<object, Map<string, CacheSlot<T>>>();

  of(client: object): Map<string, CacheSlot<T>> {
    let cache = this.byClient.get(client);
    if (!cache) {
      cache = new Map();
      this.byClient.set(client, cache);
    }
    return cache;
  }

  clear(): void {
    this.byClient = new WeakMap();
  }
}

function takeCached<T>(
  scoped: ClientScopedCache<T>,
  client: object,
  key: string,
  load: () => Promise<T>,
): Promise<T> {
  const cache = scoped.of(client);
  const now = Date.now();
  const hit = cache.get(key);
  if (hit && now - hit.at < REF_TTL_MS) return hit.promise;
  const promise = load();
  cache.set(key, { at: now, promise });
  // A rejection must not be cached as a permanent failure — the next chip to
  // mount should be free to retry. Only evict this exact promise, or a retry
  // already in flight would be thrown away with it.
  promise.catch(() => {
    if (cache.get(key)?.promise === promise) cache.delete(key);
  });
  return promise;
}

/** A stable number per client, so a chip's React state is keyed by the client
 *  as well as the id: the same id under another store is another reference. */
const clientNumbers = new WeakMap<object, number>();
let nextClientNumber = 1;
function clientKey(client: object | null): string {
  if (!client) return 'none';
  let number = clientNumbers.get(client);
  if (number === undefined) {
    number = nextClientNumber++;
    clientNumbers.set(client, number);
  }
  return String(number);
}

const sessionDetailCache = new ClientScopedCache<ManagedSessionDetail>();
const noteOrIssueCache = new ClientScopedCache<NoteOrIssue>();
const transcriptCache = new ClientScopedCache<TurnModel>();
const resolvedRefCache = new ClientScopedCache<ResolvedRefMatch[]>();
const filesNamedByToolsCache = new ClientScopedCache<FileNamedByTools[]>();
const fileNamedByToolsContentCache = new ClientScopedCache<FileNamedByToolsContent>();
const sessionPrincipalsCache = new ClientScopedCache<SessionPrincipal[]>();

export function clearRefDetailCache(): void {
  sessionDetailCache.clear();
  noteOrIssueCache.clear();
  transcriptCache.clear();
  resolvedRefCache.clear();
  filesNamedByToolsCache.clear();
  fileNamedByToolsContentCache.clear();
  sessionPrincipalsCache.clear();
}

// --- batched reference resolution ---
//
// A transcript can mention dozens of bare uuids, each mounting its own chip in
// the same render pass. The resolver takes a batch, so instead of one POST per
// chip the ids queue for one flush window and go out together. The queue is
// keyed by client (WeakMap) so two providers on one page never cross wires.

/** One flush window. Long enough to collect every chip a render pass mounts,
 *  short enough to be invisible next to the network round-trip. */
const RESOLVE_FLUSH_MS = 25;
/** dash's per-call cap; a bigger queue goes out as several calls. */
const RESOLVE_BATCH_MAX = 128;

interface ResolveWaiter {
  resolve: (m: ResolvedRefMatch[]) => void;
  reject: (e: unknown) => void;
}

interface ResolveQueue {
  /** Waiters per id — an array, so a second request for an id already queued
   *  (a cache eviction racing a flush) waits alongside rather than silently
   *  replacing the first waiter and leaving its promise unsettled. */
  pending: Map<string, ResolveWaiter[]>;
  timer: ReturnType<typeof setTimeout> | null;
}

/** What looks ids up in a batch: reference-store's client. */
interface BatchResolver {
  resolve(ids: string[]): Promise<ResolveResponse>;
}

const resolveQueues = new WeakMap<BatchResolver, ResolveQueue>();

async function flushResolveQueue(client: BatchResolver, queue: ResolveQueue): Promise<void> {
  queue.timer = null;
  const taken = queue.pending;
  queue.pending = new Map();
  const ids = [...taken.keys()];
  for (let start = 0; start < ids.length; start += RESOLVE_BATCH_MAX) {
    const batch = ids.slice(start, start + RESOLVE_BATCH_MAX);
    try {
      const response = await client.resolve(batch);
      // A per-id error means that id's misses are NOT definitive — reject it so
      // the promise cache evicts and the next mount retries, rather than
      // caching a store outage as "this id names nothing".
      const errorsByID = new Map((response.errors ?? []).map((e) => [e.id, e.error]));
      for (const id of batch) {
        const waiters = taken.get(id) ?? [];
        const idError = errorsByID.get(id);
        const matches = response.results[id];
        for (const waiter of waiters) {
          if (idError !== undefined) {
            waiter.reject(new Error(idError));
          } else if (matches === undefined) {
            waiter.reject(new Error('resolver response is missing this id'));
          } else {
            waiter.resolve(matches);
          }
        }
      }
    } catch (e) {
      for (const id of batch) {
        for (const waiter of taken.get(id) ?? []) waiter.reject(e);
      }
    }
  }
}

function enqueueResolve(client: BatchResolver, id: string): Promise<ResolvedRefMatch[]> {
  let queue = resolveQueues.get(client);
  if (!queue) {
    queue = { pending: new Map(), timer: null };
    resolveQueues.set(client, queue);
  }
  const q = queue;
  return new Promise<ResolvedRefMatch[]>((resolve, reject) => {
    const waiters = q.pending.get(id);
    if (waiters) {
      waiters.push({ resolve, reject });
    } else {
      q.pending.set(id, [{ resolve, reject }]);
    }
    q.timer ??= setTimeout(() => void flushResolveQueue(client, q), RESOLVE_FLUSH_MS);
  });
}

/** What a chip knows about its target at any moment. `data` and `error` are
 *  mutually exclusive; both null means still loading. */
export interface RefDetailState<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Shared load-once-per-id effect. `enabled` false leaves the state idle
 *  without touching the network — that is how the transcript stays unfetched
 *  until the user asks for it. */
function useCachedRef<T>(
  key: string,
  enabled: boolean,
  load: () => Promise<T>,
): RefDetailState<T> {
  const [state, setState] = useState<RefDetailState<T>>({
    data: null,
    error: null,
    loading: enabled,
  });

  useEffect(() => {
    if (!enabled) {
      setState({ data: null, error: null, loading: false });
      return;
    }
    let live = true;
    setState({ data: null, error: null, loading: true });
    load().then(
      (data) => {
        if (live) setState({ data, error: null, loading: false });
      },
      (e: unknown) => {
        if (live) setState({ data: null, error: messageOf(e), loading: false });
      },
    );
    return () => {
      live = false;
    };
    // `load` closes over the stable key; re-running on its identity would refetch
    // every render. The key and the enable flag are the real inputs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, enabled]);

  return state;
}

/**
 * Full detail for a referenced session (`GET /sessions/{id}`), for the chip
 * label and its panel rows.
 *
 * This is the same endpoint `useSessionInfo` reads, but it is NOT that hook:
 * that one resolves the ACTIVE session through the store, and a chip points at
 * some other session the store has never listed.
 */
export function useSessionRefDetail(sessionId: string): RefDetailState<ManagedSessionDetail> {
  const { api } = useChatContext();
  return useCachedRef(`${clientKey(api)}|${sessionId}`, sessionId !== '', () =>
    loadSessionRefDetail(api, sessionId),
  );
}

/** What `useSessionRefDetail` loads: one flight per (client, session id)
 *  within the cache window. Exported so callers outside a ChatProvider for
 *  that client (a chip looking a session up on a connected server) share the
 *  same cache, and so the per-client rule can be tested without a DOM. */
export function loadSessionRefDetail(
  api: Pick<ApiClient, 'getSessionDetail'>,
  sessionId: string,
): Promise<ManagedSessionDetail> {
  return takeCached(sessionDetailCache, api, sessionId, () => api.getSessionDetail(sessionId));
}

/**
 * A referenced note or issue (`GET /items/{id}` of notes, then of issues) — note, todo, rank list
 * or workspace, whichever the id turns out to name.
 *
 * When the host configured no notes and issues, this reports that as an error rather
 * than loading forever: a chip that cannot resolve should say why. The message
 * is deliberately plain, because it is shown to a user who can act on it.
 */
export function useNoteOrIssueRefDetail(itemId: string): RefDetailState<NoteOrIssue> {
  const { notesAndIssues } = useChatContext();
  return useCachedRef(`${clientKey(notesAndIssues)}|${itemId}`, itemId !== '', () => {
    if (!notesAndIssues) {
      return Promise.reject(new Error('note and issue lookup is not configured here'));
    }
    return takeCached(noteOrIssueCache, notesAndIssues, itemId, () => notesAndIssues.getItem(itemId));
  });
}

/**
 * What a bare uuid names, per the host's reference resolver: every registered
 * store that recognizes the id, or an empty array for a definitive miss. Used
 * by the `uuid` chip kind — an id detected with no cue word, where the text
 * itself says nothing about the store.
 *
 * When the host configured no resolver this reports it as an error, and the
 * chip renders the id as plain text: with nowhere to ask, plain text is the
 * only honest rendering of an unclassified id.
 */
export function useResolvedRef(refId: string): RefDetailState<ResolvedRefMatch[]> {
  const { references } = useChatContext();
  const client: BatchResolver | null = references;
  return useCachedRef(`${clientKey(client)}|${refId}`, refId !== '', () => {
    if (!client) {
      return Promise.reject(new Error('reference resolver is not configured here'));
    }
    return takeCached(resolvedRefCache, client, refId, () => enqueueResolve(client, refId));
  });
}

/** Turns to pull when a chip's transcript is expanded. The session's most
 *  recent activity is what a reader wants from a glance at a reference, and it
 *  is what the endpoint returns without a `before` cursor. */
export const REF_TRANSCRIPT_TURNS = 30;

/**
 * The referenced session's recent transcript, fetched ONLY once `enabled` turns
 * true — i.e. when the user expands the panel. This is the "fully load the
 * history" affordance, and it is opt-in per chip for a reason: the response is
 * a materialized turn model, and mounting one per chip in a long transcript
 * would fetch megabytes nobody asked to read.
 *
 * A limit always goes on the wire (see ApiClient.getMessages — the unbounded
 * shape of that endpoint returned 306MB for one real session on this box).
 * `model.more` tells the caller older turns exist beyond this page; this hook
 * does not paginate, and a reader who wants the rest opens the session.
 */
export function useSessionRefTranscript(
  sessionId: string,
  enabled: boolean,
): RefDetailState<TurnModel> {
  const { api } = useChatContext();
  return useCachedRef(`${clientKey(api)}|${sessionId}`, enabled && sessionId !== '', () =>
    takeCached(transcriptCache, api, sessionId, async () => {
      const resp = await api.getMessages(sessionId, { limit: REF_TRANSCRIPT_TURNS });
      return resp.model;
    }),
  );
}

/**
 * The files a session's tools named, for matching the file chips in its
 * transcript. One request per session per cache window, however many chips
 * mount; a session still working names more files, which the next window picks
 * up.
 */
export function useFilesNamedByTools(sessionId: string): RefDetailState<FileNamedByTools[]> {
  const { api } = useChatContext();
  return useCachedRef(`${clientKey(api)}|${sessionId}`, sessionId !== '', () =>
    takeCached(filesNamedByToolsCache, api, sessionId, () => api.listFilesNamedByTools(sessionId)),
  );
}

/**
 * One of those files as it is on disk now, fetched only once `enabled` turns
 * true — when the user opens the chip. The text can run to a megabyte, so no
 * chip loads it for a glance.
 */
export function useFileNamedByToolsContent(
  sessionId: string,
  path: string,
  enabled: boolean,
): RefDetailState<FileNamedByToolsContent> {
  const { api } = useChatContext();
  const key = `${sessionId}\u0000${path}`;
  return useCachedRef(`${clientKey(api)}|${key}`, enabled && sessionId !== '' && path !== '', () =>
    takeCached(fileNamedByToolsContentCache, api, key, () => api.getFileNamedByToolsContent(sessionId, path)),
  );
}

/**
 * Who the sessions this caller may see were started as, with names and counts
 * (GET /sessions/principals): the sidebar's "started by" marker and filter read
 * it. One request per cache window however many rows ask.
 */
export function useSessionPrincipals(): RefDetailState<SessionPrincipal[]> {
  const { api } = useChatContext();
  return useCachedRef(`${clientKey(api)}|session-principals`, true, () =>
    takeCached(sessionPrincipalsCache, api, 'session-principals', () => api.listSessionPrincipals()),
  );
}
