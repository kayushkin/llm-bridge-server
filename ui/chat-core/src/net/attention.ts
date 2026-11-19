// Whose attention a session's waiting needs: the operator's, or nobody's in
// particular because a background agent raised it.
//
// The ONE place that rule lives. Every surface that marks, counts or lists what
// is waiting — the sidebar's `?`, its folder rollups, the "Needs you" inbox and
// page, the conductor's alert, a session chip's `?` and its state badge — asks
// here, so no two of them can disagree about whether something needs the
// operator.
//
// Why session TYPE decides it (operator decision, 2026-10-19). Measured that
// day: 408 open signals, of which 319 came from `autonomous` sessions (253 from
// autoworkers alone) and 89 from `interactive` ones. The operator's own
// questions were a sixth of the pile. And llm-bridge-server is about to keep a
// reaped session's last state, so an autoworker that ended its turn on a
// question will stay `awaiting_user` after its process is gone — state alone
// would then mark every such worker as waiting on the operator.
//
// The types are llm-bridge's (`msg.SessionType`, `msg/server.go`):
//   - interactive: a human is in the loop (dash chat, CLI, phone). Operator's.
//   - herald: an agent-initiated question relayed to the operator in a session
//     they did not open (the `ask` CLI). It exists to ask the operator, and
//     `msg/server.go` says it is kept apart from autonomous "so frontends can
//     surface these as a 'needs you' inbox". Operator's.
//   - autonomous (autoworker, delegate, scheduled task, dispatcher…), system
//     (subagents, renamer, conformance) and external (imported from harness
//     history; the bridge did not run it and cannot say who did): background.
//
// Background is not hidden. It is listed apart, with its own count and less
// prominence, so an autoworker's recommendation stays readable without
// drowning the questions the operator has to answer.

import type { Signal, SignalRequest } from './signals.js';

/** Session types whose waiting is the operator's to answer. Everything else —
 *  including a type this client has never heard of, and no type at all — is
 *  background. */
const OPERATOR_SESSION_TYPES: ReadonlySet<string> = new Set(['interactive', 'herald']);

/** Whether a session of this type is the operator's to answer.
 *
 *  ⚠️ An empty or unknown type answers FALSE. That is a choice, not a default
 *  that slipped through: a type nobody recorded cannot be shown to be the
 *  operator's, and raising the main marker for it would put every unclassified
 *  worker back in the operator's queue. It lands in the background group, where
 *  it is still seen, and the background group names its type as unknown rather
 *  than guessing one. */
export function isOperatorSessionType(sessionType: string): boolean {
  return OPERATOR_SESSION_TYPES.has(sessionType);
}

/** The session states that mean "stopped, waiting on a human". Both are
 *  llm-bridge's own (`msg/provider.go`). Listed rather than pattern-matched: a
 *  state that merely looks like waiting — `paused`, `rate_limited` — is not
 *  something a human can answer. */
const AWAITING_HUMAN_STATES: ReadonlySet<string> = new Set(['awaiting_user', 'awaiting_permission']);

/** Whether this state, on its own, says a human is expected to answer. Says
 *  nothing about WHICH human: see {@link isSessionAwaitingOperator}. */
export function isAwaitingHumanState(state: string): boolean {
  return AWAITING_HUMAN_STATES.has(state);
}

/** Whether a session's state puts it in the operator's queue: it is waiting on
 *  a human AND it is the operator's kind of session. A background session in
 *  `awaiting_user` is not — it is listed with the background group's signals. */
export function isSessionAwaitingOperator(session: { type: string; state: string }): boolean {
  return isOperatorSessionType(session.type) && isAwaitingHumanState(session.state);
}

/**
 * The type of the session that raised a signal.
 *
 * Two sources, and both are the session's own record of the same fact, not a
 * guess standing in for it: the signal carries the raising session's type,
 * stamped by the server when it minted the signal (`session_type`), and the
 * session row carries it too. The signal's copy is used when present. When the
 * server sent none — a signal minted before the field, or a server that does
 * not send it — the session row is asked, through `sessionTypeOf`. When
 * neither knows, the answer is '' and the signal is background (see
 * {@link isOperatorSessionType}); nothing here invents a type.
 *
 * Measured 2026-10-19: every one of the 408 open signals carried
 * `session_type`, and each agreed with its session row's type.
 */
export function sessionTypeOfSignal(
  signal: Signal,
  sessionTypeOf: (sessionId: string) => string | undefined,
): string {
  if (signal.sessionType !== '') return signal.sessionType;
  return sessionTypeOf(signal.sessionId) ?? '';
}

/** Open signals split by whose they are to answer. */
export interface SignalsByAttention {
  /** Raised by interactive and herald sessions: what needs the operator. */
  operatorSignals: Signal[];
  /** Raised by every other session, or by one whose type is unknown. */
  backgroundSignals: Signal[];
}

/** Split signals into the operator's and background, keeping the server's
 *  order within each. Every signal lands in exactly one of the two. */
export function splitSignalsByAttention(
  signals: readonly Signal[],
  sessionTypeOf: (sessionId: string) => string | undefined,
): SignalsByAttention {
  const operatorSignals: Signal[] = [];
  const backgroundSignals: Signal[] = [];
  for (const signal of signals) {
    if (isOperatorSessionType(sessionTypeOfSignal(signal, sessionTypeOf))) operatorSignals.push(signal);
    else backgroundSignals.push(signal);
  }
  return { operatorSignals, backgroundSignals };
}

/** One attention group's signals, and the same grouped into the unit that
 *  gets answered. */
export interface SignalAttentionGroup {
  signals: Signal[];
  requests: SignalRequest[];
}
