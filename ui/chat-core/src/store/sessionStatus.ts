import type { SessionStatus, SessionSummary } from '../net/types.js';

// THE ONE RULE for a session's status: of two statuses for the same session, the
// one with the larger `as_of` is the newer, and the newer one wins.
//
// A status reaches this client four ways — a `session_status` event on the open
// session's stream, an upsert on the session-list stream, a row of a
// `/sessions/summary` page, and the IndexedDB cache — in no guaranteed order. A
// list upsert can land before or after the stream event that says the same thing; a
// summary page fetched a second ago can land after a live event that is newer than
// it. Arrival order therefore decides nothing here. `as_of` is the server's event
// row id for the status, unique per status and growing, so it decides everything.
//
// Every path that puts a SessionSummary into the store goes through
// `withNewestStatus`, which also keeps `summary.state === summary.status.state` —
// `state` stays a field because a great deal filters and sorts on it, but it is
// never a second opinion.

/** The status a bare `state` implies: what an old cached row, or a server that
 *  predates the status, amounts to. `as_of: 0` is the oldest a status can be, so
 *  anything real outranks it. */
export function statusFromBareState(state: string): SessionStatus {
  return { state, as_of: 0 };
}

/** The newer of two statuses. A tie keeps `incoming`: equal non-zero `as_of` means
 *  the same server event, so the two are the same status; and between two
 *  synthesized (`as_of: 0`) ones the later arrival is the better guess. */
export function newerSessionStatus(
  current: SessionStatus | undefined,
  incoming: SessionStatus,
): SessionStatus {
  if (!current) return incoming;
  return incoming.as_of >= current.as_of ? incoming : current;
}

/**
 * The row to store for `incoming`, given the row already `held` for that session:
 * `held`'s fields under `incoming`'s (when `keepHeldFields`, the other way round),
 * carrying whichever status is newer, with `state` set to match it.
 *
 * Two kinds of `incoming` carry no usable status of their own, and they mean
 * opposite things:
 *
 *  - A status that names a DIFFERENT state than `incoming.state` is an optimistic
 *    local mutation. `{ ...row, state: 'completed' }` is how every one is written, and
 *    it carries the row's old status along. It is a claim about now: taken as a status
 *    as of the held one's `as_of`, so it shows at once and the next real status the
 *    server sends outranks it.
 *  - NO status at all is a row from before the status existed — an old cache entry,
 *    an older server. It knows only a state, as of nothing (`as_of: 0`), so any real
 *    status already held outranks it. This matters for a status that arrived on the
 *    session's stream BEFORE its row did: the row must not erase it.
 */
export function withNewestStatus(
  held: SessionSummary | undefined,
  incoming: SessionSummary,
  keepHeldFields = false,
): SessionSummary {
  let own = incoming.status;
  if (own && own.state !== incoming.state) {
    own = { state: incoming.state, as_of: held?.status?.as_of ?? 0 };
  }
  own ??= statusFromBareState(incoming.state);
  const status = newerSessionStatus(held?.status, own);
  const merged = held ? (keepHeldFields ? { ...incoming, ...held } : { ...held, ...incoming }) : incoming;
  return { ...merged, status, state: status.state };
}

/** The session's status, never undefined. */
export function statusOf(summary: SessionSummary): SessionStatus {
  return summary.status ?? statusFromBareState(summary.state);
}
