import type { JSX } from 'react';
import { SignalRequestCard, type ComposerTextForAnswer } from './SignalCard';
import { useOpenSignals } from '@kayushkin/chat-core';
import { groupSignalsByRequest, type SignalRequest } from '@kayushkin/chat-core';

export interface SessionSignalsProps {
  sessionId: string;
  compact?: boolean;
  /** Heading above the cards. Omit for surfaces tight enough that the card's own
   *  "question"/"notification" label is heading enough. */
  title?: string;
}

/**
 * SessionSignals is the open chat signals raised by ONE session, answerable in
 * place. Renders nothing when the session has none, or when this bridge-server
 * has no signals route.
 *
 * It used to take `excludeRequestIds`, so a host could drop the questions its
 * own permission banner was already drawing from the live tool input. That prop
 * was the last place a client still had to know which producer raised a
 * question. It is gone because the duplication it worked around is: the record
 * now carries `allowMultipleOptions`, which was the one thing the banner could
 * render and the card could not, so a host has no reason left to draw a second
 * form for a question that is already on this one.
 *
 * Ported from bridge-ui's `SessionSignals.tsx`.
 */
export function SessionSignals({
  sessionId,
  compact,
  title,
}: SessionSignalsProps): JSX.Element | null {
  const { signals, error, reload } = useOpenSignals(sessionId);

  if (error !== null) return <p className="signal-error">Couldn’t load signals: {error}</p>;

  return (
    <SignalRequestList
      requests={groupSignalsByRequest(signals)}
      compact={compact}
      title={title}
      onResolved={reload}
    />
  );
}

export interface SignalRequestListProps {
  requests: readonly SignalRequest[];
  compact?: boolean;
  title?: string;
  onResolved?: () => void;
  /** Passed through to every card — see `SignalRequestCard`. */
  allowDismissWithoutAnswer?: boolean;
  /** Passed through to every card — see `SignalCardProps.startCollapsedToAnswers`. */
  startCollapsedToAnswers?: boolean;
  /** Passed through to every card — see `SignalRequestCardProps.composerText`. */
  composerText?: ComposerTextForAnswer;
}

/**
 * The rendered list of request groups, with no fetching of its own.
 *
 * Split out from {@link SessionSignals} so a host with its own source of signals
 * — a cross-session "Needs you" inbox, a kanban card drawer — renders the same
 * cards without going through the per-session read. It also makes the empty case
 * assertable: a bridge-server with no signals route yields zero requests, and
 * this renders nothing at all rather than an empty box or an error.
 */
export function SignalRequestList({
  requests,
  compact,
  title,
  onResolved,
  allowDismissWithoutAnswer,
  startCollapsedToAnswers,
  composerText,
}: SignalRequestListProps): JSX.Element | null {
  if (requests.length === 0) return null;
  return (
    <div className="signals" role="region" aria-label="Session signals">
      {title !== undefined && title !== '' && <div className="signals-title">{title}</div>}
      {requests.map((request) => (
        <SignalRequestCard
          // A derived group has no request id, so it is keyed by its one
          // signal's id — never by the empty string, which every derived group
          // would share.
          key={request.requestId || request.signals[0]?.id}
          request={request}
          compact={compact}
          onResolved={onResolved}
          allowDismissWithoutAnswer={allowDismissWithoutAnswer}
          startCollapsedToAnswers={startCollapsedToAnswers}
          composerText={composerText}
        />
      ))}
    </div>
  );
}
