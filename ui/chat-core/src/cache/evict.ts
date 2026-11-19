import type { SessionCache } from './SessionCache.js';

/** Default cap on how many sessions' materialized turns the cache retains. */
export const DEFAULT_CACHE_LIMIT = 50;

/**
 * Default cap on how many session list rows the cache retains.
 *
 * The list store has exactly one live reader — the cold-boot paint in
 * `Prefetcher.hydrateFromCache`, which is ONE sidebar page wide. A row past that
 * page can never be painted, so retaining it buys nothing and costs a `getAll`
 * plus a sort on the boot path the whole cache exists to make fast. This default
 * mirrors `Prefetcher.DEFAULT_SESSIONS_PER_PAGE`; a caller that knows its own
 * page size should pass it.
 */
export const DEFAULT_LIST_CACHE_LIMIT = 100;

/**
 * LRU eviction of cached turn models, bounded by `updatedAt` (most-recently
 * touched sessions survive). Pure over the keys — testable without a live DB.
 * Returns the sessionIds to evict (the oldest beyond `limit`).
 */
export function selectEvictions(
  keys: { sessionId: string; updatedAt: string }[],
  limit = DEFAULT_CACHE_LIMIT,
): string[] {
  if (keys.length <= limit) return [];
  const sorted = keys
    .slice()
    .sort((a, b) => (a.updatedAt < b.updatedAt ? 1 : a.updatedAt > b.updatedAt ? -1 : 0));
  return sorted.slice(limit).map((k) => k.sessionId);
}

/**
 * Enforce BOTH cache bounds: the oldest turn models beyond `limit`, then the
 * oldest list rows beyond `listLimit`. Turns go first so the list pass sees the
 * surviving set.
 *
 * Returns the sessionIds whose TURNS were evicted, unchanged from before the
 * list bound existed — `enforceListBound` returns its own victims.
 */
export async function enforceCacheBound(
  cache: SessionCache,
  limit = DEFAULT_CACHE_LIMIT,
  listLimit = DEFAULT_LIST_CACHE_LIMIT,
): Promise<string[]> {
  const keys = await cache.turnKeys();
  const victims = selectEvictions(keys, limit);
  for (const id of victims) await cache.evictTurns(id);
  const evicted = new Set(victims);
  await enforceListBound(
    cache,
    listLimit,
    keys.filter((k) => !evicted.has(k.sessionId)).map((k) => k.sessionId),
  );
  return victims;
}

/**
 * Bound the list store to the newest `limit` rows.
 *
 * Without this the store is append-only: the SyncEngine writes a row for every
 * session it ever sees upserted on the live stream and nothing ever removed one,
 * so it grew with the box's whole session history while only the first page was
 * ever read back.
 *
 * A session whose TURNS are still cached keeps its row whatever its age. A
 * cached turn model with no list row is a session the sidebar cannot draw and the
 * cache still pays for, and the two bounds are set independently, so the invariant
 * has to be enforced rather than assumed. That can leave the store a little over
 * `limit`; the bound yields to it deliberately.
 */
export async function enforceListBound(
  cache: SessionCache,
  limit = DEFAULT_LIST_CACHE_LIMIT,
  /** The sessions whose turns are cached, when the caller has just read them — so one
   *  sweep reads the key set once rather than twice. Read here when absent. */
  sessionIdsWithCachedTurns?: string[],
): Promise<string[]> {
  const oldestFirst = await cache.listKeysOldestFirst();
  const over = oldestFirst.length - limit;
  if (over <= 0) return [];
  const withTurns = new Set(
    sessionIdsWithCachedTurns ?? (await cache.turnKeys()).map((k) => k.sessionId),
  );
  const victims = oldestFirst.slice(0, over).filter((id) => !withTurns.has(id));
  await cache.evictListRows(victims);
  return victims;
}
