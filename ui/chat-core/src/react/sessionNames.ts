import { useEffect, useMemo, useState } from 'react';
import { useStore } from 'zustand';
import { useChatContext } from './context.js';

// Resolving a session id to the name a human calls it, for a surface that holds
// ids and nothing else.
//
// Every other surface in this package already has the `SessionSummary` in hand —
// a sidebar row renders one, a header renders one — so none of them ever needed
// this. The cross-session signals inbox is the first that does: it lists open
// signals, and a signal carries `session_id` and no name at all.
//
// Paging cannot answer it. The sidebar loads a newest-first prefix, and a
// session that raised a question and then went quiet sinks out of that prefix
// while its question stays open — which is precisely the case the inbox exists
// for. Measured on this host: of the 17 sessions holding an open chat signal,
// 11 were nowhere in the sidebar's first page.

/** How long a resolved name stays fresh. Long, because a display name changes
 *  when a human renames a session and at no other time, and the cost of being
 *  briefly stale is a label — not a wrong action. */
const NAME_TTL_MS = 300_000;

interface NameCacheSlot {
  at: number;
  name: string;
}

/** Module-level, so two mounted surfaces asking about the same session share one
 *  answer rather than each putting a request on the wire. */
const nameCache = new Map<string, NameCacheSlot>();

/** Ids currently in flight, so a re-render mid-request does not start a second
 *  one for the same session. */
const inFlight = new Set<string>();

/** Drop every resolved name. Tests call this between cases. */
export function clearSessionNameCache(): void {
  nameCache.clear();
  inFlight.clear();
}

/**
 * Resolve session ids to display names.
 *
 * Returns a lookup function rather than a map so a caller renders one card at a
 * time without indexing: `sessionName(request.sessionId)`.
 *
 * ⚠️ The fallback is the SESSION ID, never a placeholder like "Untitled" or
 * "Unknown session". An id is ugly and it is true — it is what the session is
 * called when nobody has named it, and it is still enough to tell two cards
 * apart and to search for. A friendly placeholder would make every unnamed
 * session look like the same session.
 *
 * Three tiers, cheapest first:
 *   1. the store — anything the sidebar has already loaded, free
 *   2. the name cache — anything a previous inbox render resolved
 *   3. one batched `GET /sessions/summary?session_id=…` for whatever is left
 *
 * A failed lookup is NOT surfaced as an error. The caller is rendering a
 * question that needs answering; falling back to the id keeps that card usable,
 * whereas an error banner over the inbox would hide the very thing it exists to
 * show.
 */
export function useSessionNames(sessionIds: readonly string[]): (sessionId: string) => string {
  const { api, store } = useChatContext();
  const [resolvedAt, setResolvedAt] = useState(0);

  // Joined into a single string so the effect below depends on the SET, not on
  // the array's identity. The caller rebuilds this list on every signals
  // refetch — every 30s — so an identity dependency would refire the effect
  // forever. The ids are recovered by splitting the key inside, which is what
  // keeps `wanted` itself OUT of the dependencies; listing both would put the
  // identity right back in and make the key decorative.
  const wantedKey = useMemo(() => [...new Set(sessionIds)].sort().join(','), [sessionIds]);

  // The loaded name of each wanted session, as one string, rather than the whole
  // session map. The map is replaced on every change to any session, and
  // depending on it handed the caller a new lookup function — and so a
  // re-render of every card it labels — several times a second while nothing it
  // shows had changed. A string compares by value.
  const loadedNamesKey = useStore(store, (s) => loadedSessionNamesKey(wantedKey, s.sessions));
  const loaded = useMemo(() => parseLoadedSessionNamesKey(loadedNamesKey), [loadedNamesKey]);

  useEffect(() => {
    const now = Date.now();
    const wanted = wantedKey === '' ? [] : wantedKey.split(',');
    const missing = wanted.filter((id) => {
      if (id === '' || loaded.has(id) || inFlight.has(id)) return false;
      const hit = nameCache.get(id);
      return !hit || now - hit.at >= NAME_TTL_MS;
    });
    if (missing.length === 0) return;

    let live = true;
    for (const id of missing) inFlight.add(id);
    void api
      .getSummary({ sessionIds: missing, limit: missing.length })
      .then((res) => {
        for (const summary of res.sessions ?? []) {
          nameCache.set(summary.sessionId, {
            at: Date.now(),
            // The row's own name, or its id — the same rule as the fallback
            // below, applied once here so both paths agree.
            name: summary.displayName || summary.sessionId,
          });
        }
        // Ids the server returned nothing for are cached AS their id, so a
        // deleted or never-existent session is not asked about again every
        // time the inbox refetches.
        for (const id of missing) {
          if (!nameCache.has(id)) nameCache.set(id, { at: Date.now(), name: id });
        }
        if (live) setResolvedAt(Date.now());
      })
      .catch((err: unknown) => {
        // Does not throw: the cards still render, headed by their ids, which is
        // a usable label and an honest one. Does not stay quiet either — a
        // whole inbox suddenly showing raw ids has a cause, and the console is
        // the only place that cause can be stated.
        console.warn('[chat-core] could not resolve session names', err);
      })
      .finally(() => {
        for (const id of missing) inFlight.delete(id);
      });
    return () => {
      live = false;
    };
    // `loaded` is here because a session arriving in the store means one fewer
    // id to look up; the guard above turns that into a no-op when nothing is
    // missing.
  }, [api, wantedKey, loaded]);

  return useMemo(() => {
    // `resolvedAt` is read for its DEPENDENCY, not its value: the name cache is
    // module-level and mutating it re-renders nothing, so this is what carries
    // a finished lookup into the next render.
    void resolvedAt;
    return (sessionId: string): string => {
      const loadedName = loaded.get(sessionId);
      if (loadedName !== undefined) return loadedName || sessionId;
      return nameCache.get(sessionId)?.name ?? sessionId;
    };
  }, [loaded, resolvedAt]);
}

/** `[id, displayName]` for each wanted id the store holds, as JSON. Exported
 *  for its test. */
export function loadedSessionNamesKey(
  wantedKey: string,
  sessions: ReadonlyMap<string, { displayName: string }>,
): string {
  const pairs: [string, string][] = [];
  if (wantedKey !== '') {
    for (const id of wantedKey.split(',')) {
      const row = sessions.get(id);
      if (row) pairs.push([id, row.displayName]);
    }
  }
  return JSON.stringify(pairs);
}

export function parseLoadedSessionNamesKey(key: string): Map<string, string> {
  return new Map(JSON.parse(key) as [string, string][]);
}
