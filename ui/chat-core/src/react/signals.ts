import { useCallback, useEffect, useMemo, useState } from 'react';
import { useStore } from 'zustand';
import { useChatContext } from './context.js';
import { sessionTypeOfSignal, splitSignalsByAttention, type SignalAttentionGroup } from '../net/attention.js';
import { groupSignalsByRequest, type Signal, type SignalAnswerDraft, type SignalRequest } from '../net/signals.js';
import { answerSignalRequest, subscribeToSignalChanges } from '../store/signalResolve.js';
import { readRetryingUnanswered, UnansweredReadError, UNANSWERED_READ_RECHECK_MS } from '../net/unansweredRead.js';

// Data layer for session signals — the open questions and notifications a
// session is waiting on.
//
// Same shape of problem as `refDetail.ts`, and for the same reason: a RefChip is
// a passive linkification of an id, so one transcript can mount many chips on
// the SAME session, and every open panel would otherwise put its own /signals
// request on the wire. Loads therefore go through a short-lived promise cache
// keyed by session id and the duplicates share one flight.
//
// It is NOT `refDetail.ts`'s cache, and not because of where the code lives: a
// chip detail is read-only and can simply age out, while a signal is ANSWERED
// from the surface that displays it. This cache has to be invalidated the
// instant a resolve lands anywhere in the tab, which is what
// `subscribeToSignalChanges` delivers and what a pure TTL cannot.

/** How long an open-signal read stays fresh. Matches `refDetail.ts`'s
 *  `REF_TTL_MS`: a signal panel shows live state, and a stale question is worse
 *  than a second request. Resolves invalidate this on their own, so the TTL only
 *  has to cover signals minted or closed by someone else. */
const OPEN_SIGNALS_TTL_MS = 30_000;

interface CacheSlot {
  at: number;
  /** null is the real answer for "this bridge-server has no /signals route". */
  promise: Promise<Signal[] | null>;
}

/** Keyed by session id; '' is the cross-session inbox read. */
const openSignalsCache = new Map<string, CacheSlot>();

/** Drop every cached signal read. Tests call this between cases; the app calls
 *  it through the resolve path, which has to invalidate rather than wait out the
 *  TTL. */
export function clearOpenSignalsCache(): void {
  openSignalsCache.clear();
}

/** What a signal surface knows at any moment. */
export interface OpenSignalsState {
  /** Open signals for this session, on every surface, newest first as the
   *  server ordered them. Empty while loading, empty on a server with no
   *  signals route. Not filtered by who should answer them: see
   *  `useOpenSignalsByAttention` for the operator / background split. */
  signals: Signal[];
  /** The same signals grouped into the unit that actually gets answered. */
  requests: SignalRequest[];
  /** False once the server has answered 404 — this bridge-server has no signals
   *  route, so every signal surface stays hidden rather than erroring. */
  available: boolean;
  loading: boolean;
  error: string | null;
  /** Re-read from the server, dropping the cached flight for this session. */
  reload: () => void;
  /**
   * Submit one whole request group. Offered for a host that renders its own
   * answer UI; the shipped `SignalRequestCard` calls the same store verb
   * directly, so both go through one implementation of the resolve semantics.
   *
   * `answersBySignalId` is keyed by signal id (what a form holds). The
   * title-keyed payload the server pairs answers by is derived inside the verb.
   */
  resolve: (
    request: SignalRequest,
    answersBySignalId: Readonly<Record<string, SignalAnswerDraft>>,
  ) => Promise<void>;
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * Open signals for one session, or across every session when `sessionId` is
 * omitted, on every surface.
 *
 * Freshness comes from four places and none of them is a timer: the `signal`
 * frame on the session-events stream, which is how a question closed by anything
 * OUTSIDE this tab reaches it (the session answered it itself, another client
 * did, a turn ended and superseded it); the in-process announcement every local
 * resolve makes, which keeps two surfaces in the same tab from disagreeing; an
 * explicit `reload`; and, last, the 30s cache TTL. Two more cover a read that
 * failed or went stale while nobody could see it: the browser coming back
 * online or the page back into view asks again, and a read this host's server
 * never answered is retried within seconds (`readRetryingUnanswered`) and then
 * every 30 seconds until it is answered.
 */
export function useOpenSignals(sessionId?: string): OpenSignalsState {
  const { api } = useChatContext();
  const key = sessionId ?? '';
  const [signals, setSignals] = useState<Signal[]>([]);
  const [available, setAvailable] = useState(true);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [reloadToken, setReloadToken] = useState(0);

  const reload = useCallback(() => {
    // Dropping the slot first is what makes this a re-READ. Bumping the token
    // alone would re-run the effect and be handed the same cached promise.
    openSignalsCache.delete(key);
    setReloadToken((t) => t + 1);
  }, [key]);

  // A resolve on any surface — this one, another chip, a host's own inbox —
  // means the open set may have changed here too.
  useEffect(() => subscribeToSignalChanges(reload), [reload]);

  // The browser is back online, or the page is shown again after the phone put
  // it away: ask again if the last answer is old or was a failure (a failed
  // read is never left in the cache). Every surface on the key runs this, and
  // the first reload's flight is the one the rest share.
  useEffect(() => {
    if (typeof window === 'undefined' || typeof document === 'undefined') return undefined;
    const onReturn = () => {
      if (document.visibilityState === 'hidden') return;
      const slot = openSignalsCache.get(key);
      if (!slot || Date.now() - slot.at >= OPEN_SIGNALS_TTL_MS) reload();
    };
    window.addEventListener('online', onReturn);
    document.addEventListener('visibilitychange', onReturn);
    return () => {
      window.removeEventListener('online', onReturn);
      document.removeEventListener('visibilitychange', onReturn);
    };
  }, [key, reload]);

  useEffect(() => {
    let live = true;
    let recheck: ReturnType<typeof setTimeout> | null = null;
    setLoading(true);
    const now = Date.now();
    const hit = openSignalsCache.get(key);
    let flight: Promise<Signal[] | null>;
    if (hit && now - hit.at < OPEN_SIGNALS_TTL_MS) {
      flight = hit.promise;
    } else {
      // A read that gets no answer is retried within seconds inside this one
      // flight, so every surface sharing it waits quietly through a dropped
      // request rather than each showing an error.
      flight = readRetryingUnanswered(() => api.getOpenSignals(sessionId ? { sessionId } : undefined));
      openSignalsCache.set(key, { at: now, promise: flight });
      // A rejection must not be cached as a permanent failure — the next panel
      // to open should be free to retry. Only evict this exact promise, or a
      // retry already in flight would be thrown away with it.
      flight.catch(() => {
        if (openSignalsCache.get(key)?.promise === flight) openSignalsCache.delete(key);
      });
    }
    flight.then(
      (result) => {
        if (!live) return;
        setError(null);
        // null is the 404: the feature is not deployed here. Not an error, and
        // not an empty list that would read as "deployed and quiet".
        setAvailable(result !== null);
        setSignals(result ?? []);
        setLoading(false);
      },
      (e: unknown) => {
        if (!live) return;
        setError(messageOf(e));
        setLoading(false);
        // Nothing else re-reads this on a timer, so a read that got no answer
        // would stay failed until some other event. Its error says "still
        // trying"; this keeps that true. A refusal waits for an event or a
        // reload, as before: asking again will not change the answer.
        if (e instanceof UnansweredReadError) recheck = setTimeout(reload, UNANSWERED_READ_RECHECK_MS);
      },
    );
    return () => {
      live = false;
      if (recheck !== null) clearTimeout(recheck);
    };
  }, [api, key, sessionId, reloadToken, reload]);

  const requests = useMemo(() => groupSignalsByRequest(signals), [signals]);

  const resolve = useCallback(
    (request: SignalRequest, answersBySignalId: Readonly<Record<string, SignalAnswerDraft>>) =>
      answerSignalRequest(api, request, answersBySignalId),
    [api],
  );

  return { signals, requests, available, loading, error, reload, resolve };
}

/** The cross-session open signals, split by whose they are to answer. */
export interface OpenSignalsByAttentionState {
  /** Raised by interactive and herald sessions: what needs the operator. The
   *  only group that may raise a "needs you" marker or count. */
  operator: SignalAttentionGroup;
  /** Raised by autonomous, system and external sessions, and by any session
   *  whose type is unknown. Listed apart, never counted as needing the
   *  operator. */
  background: SignalAttentionGroup;
  /** The raising session's type as the split saw it — the signal's own
   *  `session_type`, else the loaded session row's — or '' when neither knows.
   *  For labeling a background card; '' is to be shown as unknown. */
  sessionTypeOf: (signal: Signal) => string;
  available: boolean;
  loading: boolean;
  error: string | null;
  reload: () => void;
}

/**
 * Every open signal across every session, split by `net/attention.ts`'s rule.
 *
 * The same one cross-session read as `useOpenSignals()` — same cache entry,
 * same resolve announcements — so splitting costs no request. A signal that
 * carries no `session_type` is joined to the session row the store already
 * holds; one the store does not hold either stays typeless, and typeless is
 * background.
 */
export function useOpenSignalsByAttention(): OpenSignalsByAttentionState {
  const { store } = useChatContext();
  const { signals, available, loading, error, reload } = useOpenSignals();
  // The row types this split reads, as one string, rather than the whole session
  // map. The map is replaced on every change to any session — the list stream
  // rewrites a row each time a session's status moves — and depending on it
  // handed every surface new request objects several times a second, so a
  // signals page holding hundreds of cards re-rendered all of them while the
  // operator sat still. Measured 2026-10-20: 6.1s of main-thread time in 10s
  // idle on 345 open signals. A string compares by value, so this re-renders
  // only when a type the split depends on actually changes.
  const rowTypesKey = useStore(store, (s) => sessionRowTypesKey(signals, s.sessions));
  const rowTypes = useMemo(() => parseSessionRowTypesKey(rowTypesKey), [rowTypesKey]);
  const sessionTypeOfRow = useCallback((sessionId: string) => rowTypes.get(sessionId), [rowTypes]);
  const split = useMemo(() => splitSignalsByAttention(signals, sessionTypeOfRow), [signals, sessionTypeOfRow]);
  const sessionTypeOf = useCallback((signal: Signal) => sessionTypeOfSignal(signal, sessionTypeOfRow), [sessionTypeOfRow]);
  const operatorRequests = useMemo(() => groupSignalsByRequest(split.operatorSignals), [split]);
  const backgroundRequests = useMemo(() => groupSignalsByRequest(split.backgroundSignals), [split]);
  return {
    operator: { signals: split.operatorSignals, requests: operatorRequests },
    background: { signals: split.backgroundSignals, requests: backgroundRequests },
    sessionTypeOf,
    available,
    loading,
    error,
    reload,
  };
}

/** The loaded row type of every signal's session that carries no
 *  `session_type` of its own — the only signals the split looks a row up for —
 *  as JSON `[id, type]` pairs. A session the store does not hold is left out,
 *  which is the same answer the lookup gives: no row, no type. Exported for
 *  its test. */
export function sessionRowTypesKey(
  signals: readonly Signal[],
  sessions: ReadonlyMap<string, { type: string }>,
): string {
  const pairs: [string, string][] = [];
  for (const signal of signals) {
    if (signal.sessionType !== '') continue;
    const row = sessions.get(signal.sessionId);
    if (row) pairs.push([signal.sessionId, row.type]);
  }
  return JSON.stringify(pairs);
}

export function parseSessionRowTypesKey(key: string): Map<string, string> {
  return new Map(JSON.parse(key) as [string, string][]);
}
