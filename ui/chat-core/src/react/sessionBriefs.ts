import { useCallback, useEffect, useState } from 'react';
import { useChatContext } from './context.js';
import type { LooseEnd, SessionBrief } from '../net/sessionBriefs.js';
import { announceSessionBriefsChanged, subscribeToSessionBriefChanges } from '../store/sessionBriefChanges.js';

/** What a brief surface knows at any moment. */
export interface SessionBriefsState {
  /** Briefs, most recently updated first, each with only its OPEN loose ends.
   *  Empty while loading and on a server with no briefs route. */
  briefs: SessionBrief[];
  /** False once the server answered 404: it has no briefs route. */
  available: boolean;
  loading: boolean;
  error: string | null;
  reload: () => void;
  /** Answer a loose end: sent to its session as a message quoting it. */
  answerLooseEnd: (looseEnd: LooseEnd, text: string) => Promise<void>;
  /** Close a loose end without an answer. */
  dismissLooseEnd: (looseEnd: LooseEnd) => Promise<void>;
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * The caller's chat briefs with their open loose ends. Re-reads on the
 * `session_brief` frame (a turn updated a brief, or someone answered or
 * dismissed a loose end anywhere), on every local answer or dismissal, and
 * when the page comes back into view. No timer.
 */
export function useSessionBriefs(opts?: { limit?: number }): SessionBriefsState {
  const { api } = useChatContext();
  const limit = opts?.limit;
  const [briefs, setBriefs] = useState<SessionBrief[]>([]);
  const [available, setAvailable] = useState(true);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [reloadToken, setReloadToken] = useState(0);

  const reload = useCallback(() => setReloadToken((token) => token + 1), []);

  useEffect(() => subscribeToSessionBriefChanges(reload), [reload]);

  useEffect(() => {
    if (typeof document === 'undefined') return undefined;
    const onReturn = () => {
      if (document.visibilityState !== 'hidden') reload();
    };
    document.addEventListener('visibilitychange', onReturn);
    return () => document.removeEventListener('visibilitychange', onReturn);
  }, [reload]);

  useEffect(() => {
    let live = true;
    setLoading(true);
    api
      .getSessionBriefs({ looseEndState: 'open', ...(limit != null ? { limit } : {}) })
      .then((answer) => {
        if (!live) return;
        if (answer === null) {
          setAvailable(false);
          setBriefs([]);
        } else {
          setAvailable(true);
          setBriefs(answer);
        }
        setError(null);
      })
      .catch((e: unknown) => {
        if (live) setError(messageOf(e));
      })
      .finally(() => {
        if (live) setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [api, limit, reloadToken]);

  const answerLooseEnd = useCallback(
    async (looseEnd: LooseEnd, text: string) => {
      await api.answerLooseEnd(looseEnd.sessionId, looseEnd.id, text);
      announceSessionBriefsChanged();
    },
    [api],
  );
  const dismissLooseEnd = useCallback(
    async (looseEnd: LooseEnd) => {
      await api.dismissLooseEnd(looseEnd.sessionId, looseEnd.id);
      announceSessionBriefsChanged();
    },
    [api],
  );

  return { briefs, available, loading, error, reload, answerLooseEnd, dismissLooseEnd };
}
