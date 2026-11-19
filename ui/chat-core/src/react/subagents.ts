import { useEffect, useMemo, useState } from 'react';
import { useStore } from 'zustand';
import { useChatContext } from './context.js';
import { isRunningState } from '../store/sessionStates.js';
import type { SessionSummary } from '../net/types.js';

// The sessions a session spawned, for a surface that lists sessions and wants to
// show that tree.
//
// The join is `SessionSummary.managerSessionId` — the store's own parent
// pointer, written by the server's subagent promoter. Nothing is derived and no
// id is invented here: a child names its parent, and that is the whole relation.
//
// ## Why this reads the server rather than grouping what is loaded
//
// A child is ordered by its OWN updated_at, not its parent's. So a session that
// spawned 106 subagents last week has them scattered thousands of rows deep in a
// listing the sidebar reads one page of, and grouping only the loaded rows would
// show four of that 106 while looking complete. Measured on this host: 1,325
// subagent sessions across 435 parents, and 10 of the sidebar's first 60 rows
// are subagents of parents that are mostly not on that page.
//
// ## Why it ALSO reads the store
//
// A subagent spawned while you are watching arrives as an `upsert` on the
// session-list stream and lands in `state.sessions` — carrying its
// managerSessionId, because the upsert path has always projected that field.
// Reading both means the list is complete (fetch) and live (store) instead of
// having to choose.

/** How long a parent's fetched children stay fresh.
 *
 *  Short, because this is the one place where the answer changing IS the news: a
 *  parent that spawns a subagent should show it. The stream covers the live case
 *  on its own, so this is the backstop for a client that was disconnected. */
const CHILDREN_TTL_MS = 30_000;

interface ChildrenCacheSlot {
  at: number;
  /** Session ids, not summaries. The summaries live in the store, which is the
   *  single copy that the stream keeps current — caching the rows here would
   *  mean a child's state froze at the moment it was fetched, and "running" is
   *  exactly what a reader wants this list for. */
  childIds: string[];
}

/** Module-level, so two surfaces asking about the same parent share one answer
 *  rather than each putting a request on the wire. */
const childrenCache = new Map<string, ChildrenCacheSlot>();

/** Parents currently in flight, so a re-render mid-request does not start a
 *  second one for the same parent. */
const inFlight = new Set<string>();

/** Summaries for children the store has not loaded, kept beside the id list
 *  above. A fetched child is usually NOT in the store — that is the whole reason
 *  for fetching — so its row has to be held somewhere, and holding it here keeps
 *  `state.sessions` meaning "what the sidebar knows about" rather than quietly
 *  growing by every subagent anyone ever expanded. */
const fetchedChildren = new Map<string, SessionSummary>();

/** Drop every cached child list. Tests call this between cases. */
export function clearSubagentCache(): void {
  childrenCache.clear();
  inFlight.clear();
  fetchedChildren.clear();
}

/**
 * The children of each given session, newest first with running ones on top.
 *
 * Returns a lookup function rather than a Map so a caller renders one row at a
 * time without indexing: `subagentsOf(session.sessionId)`.
 *
 * An empty array means "this session spawned nothing", and it is also what a
 * parent whose children have not arrived yet reads as. The two are deliberately
 * not distinguished: the caller draws a disclosure control when the list is
 * non-empty, so an in-flight parent simply has no control yet and grows one when
 * the answer lands. A "loading" state on a control that may turn out not to
 * exist would flicker on every row of the sidebar.
 */
export function useSubagentSessions(
  parentIds: readonly string[],
): (parentId: string) => SessionSummary[] {
  const { api, store } = useChatContext();
  const loaded = useStore(store, (s) => s.sessions);
  const [fetchedAt, setFetchedAt] = useState(0);

  // Joined into a single string so the effect depends on the SET, not on the
  // array's identity — the caller rebuilds this list on every sidebar render.
  // The ids are recovered by splitting inside, which is what keeps the array
  // itself out of the dependencies.
  const wantedKey = useMemo(() => [...new Set(parentIds)].sort().join(','), [parentIds]);

  useEffect(() => {
    const now = Date.now();
    const wanted = wantedKey === '' ? [] : wantedKey.split(',');
    const missing = wanted.filter((id) => {
      if (id === '' || inFlight.has(id)) return false;
      const hit = childrenCache.get(id);
      return !hit || now - hit.at >= CHILDREN_TTL_MS;
    });
    if (missing.length === 0) return;

    let live = true;
    for (const id of missing) inFlight.add(id);
    void api
      .getSummary({ managerSessionIds: missing, limit: 500 })
      .then((res) => {
        const byParent = new Map<string, string[]>();
        for (const summary of res.sessions ?? []) {
          const parent = summary.managerSessionId ?? '';
          if (!parent) continue;
          fetchedChildren.set(summary.sessionId, summary);
          const list = byParent.get(parent);
          if (list) list.push(summary.sessionId);
          else byParent.set(parent, [summary.sessionId]);
        }
        // Every parent asked about is recorded, including the ones that came
        // back with nothing — otherwise a session that spawned nothing is
        // re-asked about on every render for the rest of the page's life.
        for (const id of missing) {
          childrenCache.set(id, { at: Date.now(), childIds: byParent.get(id) ?? [] });
        }
        if (live) setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        // Does not throw: the sidebar's job is to list sessions, and it can do
        // that with no disclosure controls at all. Does not stay quiet either —
        // a tree that never appears has a cause, and the console is the only
        // place that cause can be stated.
        console.warn('[chat-core] could not read subagent sessions', err);
      })
      .finally(() => {
        for (const id of missing) inFlight.delete(id);
      });
    return () => {
      live = false;
    };
  }, [api, wantedKey]);

  return useMemo(() => {
    // `fetchedAt` is read for its DEPENDENCY, not its value: the caches above are
    // module-level and mutating them re-renders nothing, so this is what carries
    // a finished fetch into the next render.
    void fetchedAt;

    // Children the STORE knows about, grouped once rather than per lookup. This
    // is the live half — a subagent spawned a moment ago is here before any
    // fetch could have returned it.
    const fromStore = new Map<string, SessionSummary[]>();
    for (const summary of loaded.values()) {
      const parent = summary.managerSessionId ?? '';
      if (!parent) continue;
      const list = fromStore.get(parent);
      if (list) list.push(summary);
      else fromStore.set(parent, [summary]);
    }

    return (parentId: string): SessionSummary[] => {
      const byId = new Map<string, SessionSummary>();
      for (const id of childrenCache.get(parentId)?.childIds ?? []) {
        // The STORE's copy wins where it has one: it is the row the stream keeps
        // current, so a child that has started running since the fetch reads as
        // running. The fetched copy is the fallback for a child the sidebar has
        // never loaded.
        const summary = loaded.get(id) ?? fetchedChildren.get(id);
        if (summary) byId.set(id, summary);
      }
      for (const summary of fromStore.get(parentId) ?? []) {
        byId.set(summary.sessionId, summary);
      }
      return [...byId.values()].sort(compareSubagents);
    };
  }, [loaded, fetchedAt]);
}

/**
 * Running first, then newest first.
 *
 * Running on top because that is what the list is opened to find — a parent with
 * 106 children has two you can still act on, and burying them under 104 finished
 * ones by recency would hide the answer inside the data.
 *
 * ⚠️ Terminal state is NOT a usable secondary sort here. Subagent sessions settle
 * to `idle`, not `completed` — measured on this host: 1,303 idle, 21 error, 1
 * completed out of 1,325 — so "finished" and "never started" are the same value
 * and sorting on it would order by nothing.
 */
export function compareSubagents(a: SessionSummary, b: SessionSummary): number {
  const aRunning = isRunningState(a.state);
  const bRunning = isRunningState(b.state);
  if (aRunning !== bRunning) return aRunning ? -1 : 1;
  // Descending: the newest child of a parent is the one it just spawned.
  if (a.updatedAt !== b.updatedAt) return a.updatedAt < b.updatedAt ? 1 : -1;
  // A tiebreak that is total, so the order cannot shuffle between renders on
  // rows sharing a timestamp — the server writes them to the nanosecond and ties
  // still happen.
  return a.sessionId < b.sessionId ? 1 : -1;
}
