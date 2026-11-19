import type { Entry } from '../net/types.js';

// Pairing a tool call to its result across BOTH model shapes.
//
// The live-tail reducer keys a call and its result to ONE entry (`tool_<tool_id>`),
// so a paired call simply has `toolResult` set. The server-side materializer that
// answers `GET /sessions/{id}/messages` does not: it keys every entry by event id
// (`e_<n>`) and leaves the call and the result as two separate rows, with the
// tool id only inside each row's raw payload. A consumer that decides "did this
// call finish?" from `toolResult` alone therefore reports every cold-loaded call
// as still running — which is exactly what the timeline's ⚙ did, forever, on
// history pages. These helpers are the one shared answer.

/** The canonical tool id an entry carries, read from the raw event payload — the
 *  same field the live reducer keys pairing on. A merged live entry's `raw` is the
 *  LAST event folded in, so both sides of the payload are checked. Undefined when
 *  the event carried none (the unpairable case). */
export function toolIdOf(entry: Entry): string | undefined {
  if (entry.toolId) return entry.toolId;
  // FALLBACK, and a temporary one. `toolId` is carried by log-store on the page and
  // by the live reducer on the stream, so this only fires against a log-store that
  // predates the promotion — which is what lets the two deploy independently. Delete
  // it once the projected page ships, because by then `raw` is gone from the default
  // page and this branch can only ever return undefined.
  const raw = entry.raw as
    | { tool_call?: { tool_id?: unknown }; tool_result?: { tool_id?: unknown } }
    | undefined;
  const id = raw?.tool_call?.tool_id ?? raw?.tool_result?.tool_id;
  return typeof id === 'string' && id.length > 0 ? id : undefined;
}

/** Tool ids that have received a result, across the given entries: a merged
 *  live-reducer entry (toolResult set) and a server-materialized standalone
 *  tool_result row both count. */
export function resultedToolIds(entries: Iterable<Entry>): Set<string> {
  const ids = new Set<string>();
  for (const entry of entries) {
    if (entry.kind !== 'tool_result' && entry.toolResult === undefined) continue;
    const id = toolIdOf(entry);
    if (id) ids.add(id);
  }
  return ids;
}
