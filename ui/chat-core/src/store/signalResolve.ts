import type { ApiClient } from '../net/ApiClient.js';
import {
  SIGNAL_KIND_NOTIFICATION,
  SIGNAL_STATE_ACKNOWLEDGED,
  SIGNAL_STATE_DISMISSED,
  type Signal,
  type SignalAnswerDraft,
  type SignalRequest,
} from '../net/signals.js';

// The verbs that close a signal. Ported from bridge-ui's `signalData.ts`, which
// is the working v1 of this feature.
//
// They live in `store/` rather than on `ApiClient` because answering a signal is
// ORCHESTRATION, not one request: the tool path reads the parked hook back
// before it writes (see `resolveSignalQuestions`), and the derived path resolves
// through an entirely different endpoint. `ApiClient` stays a thin, honest
// pass-through over each individual route; the decision about which route
// answers which signal is here, once, for every surface that renders a card.

/** Every mounted signal surface reads the same records, so a resolve on one has
 *  to reach the others: answering in a sidebar inbox while a RefChip panel is
 *  open on the same session otherwise leaves the panel offering a question that
 *  is already answered. The server's `signal` frame covers changes made outside
 *  this tab; this announcement covers the one the tab just made itself, without
 *  waiting for the round trip back.
 *
 *  This is a refetch trigger, NOT a cache — each surface still asks the server
 *  what is open, so the server stays the single source of truth for state. */
const signalChangeListeners = new Set<() => void>();

/** Be told that a signal was resolved somewhere in this tab. Returns the
 *  unsubscribe function. */
export function subscribeToSignalChanges(listener: () => void): () => void {
  signalChangeListeners.add(listener);
  return () => {
    signalChangeListeners.delete(listener);
  };
}

export function announceSignalsChanged(): void {
  for (const listener of signalChangeListeners) listener();
}

/** What goes on the wire for one signal: the picked options, then the typed
 *  text. Empty means unanswered.
 *
 *  A human can pick options AND write something — "these three, but staging
 *  first" — so both go out, the picks on the first line and the text after a
 *  blank line. Either alone is a whole answer too.
 *
 *  Several picked options join with ', '. That is not a display choice: the
 *  answer wire is one string per question, and a comma-joined list is what
 *  AskUserQuestion's own input schema accepts for a multiSelect answer. */
export function answerTextOf(answer: SignalAnswerDraft | undefined): string {
  const picked = (answer?.pickedOptionValues ?? []).filter((v) => v !== '').join(', ');
  const typed = (answer?.text ?? '').trim();
  return [picked, typed].filter((part) => part !== '').join('\n\n');
}

/** The signals in a request that need an ANSWER. Notifications ride in the same
 *  group but are acknowledged one at a time, so they never gate a submit. */
export function questionsIn(request: SignalRequest): Signal[] {
  return request.signals.filter((s) => s.kind !== SIGNAL_KIND_NOTIFICATION);
}

/** Whether the whole request is ready to submit.
 *
 *  One AskUserQuestion call carries several questions and resolves ONCE, so
 *  answering a single question in isolation would resolve the whole request with
 *  the rest unanswered. A surface must keep its submit disabled until this is
 *  true. */
export function everyQuestionAnswered(
  request: SignalRequest,
  answersBySignalId: Readonly<Record<string, SignalAnswerDraft>>,
): boolean {
  const questions = questionsIn(request);
  return (
    questions.length > 0 && questions.every((s) => answerTextOf(answersBySignalId[s.id]) !== '')
  );
}

/**
 * Submit one whole request group, whichever producer minted it.
 *
 * One POST, whatever raised the question and whether or not its session is
 * still running: the SERVER decides how the answer is delivered.
 *
 * This function used to make that decision. It read `requestId`, then either
 * re-fetched the parked tool input and posted a merged payload to the hook
 * route, or posted text to /send. Both were transport choices made on evidence
 * the client did not have — a `requestId` says a park EXISTED, not that it is
 * still live, and only bridge-server can tell. So the client could send an
 * answer into a park that had already died, and every other surface wanting to
 * answer a question had to reimplement the whole decision.
 *
 * `answersBySignalId` is keyed by signal id, which is what a form holds, and
 * that is now what goes on the wire. The title-keyed pairing the parked hook
 * needs is derived server-side, where the parked input already lives.
 */
export async function answerSignalRequest(
  api: ApiClient,
  request: SignalRequest,
  answersBySignalId: Readonly<Record<string, SignalAnswerDraft>>,
): Promise<void> {
  const questions = questionsIn(request);
  if (questions.length === 0) throw new Error('this request has no question to answer');
  if (!everyQuestionAnswered(request, answersBySignalId)) {
    // Kept as a local guard so a form can refuse before a round trip. The
    // server enforces it too, and the server's copy is the one that counts —
    // this one only saves a request.
    throw new Error('every question in this request has to be answered before it can be submitted');
  }
  const answers: Record<string, string> = {};
  for (const signal of questions) {
    answers[signal.id] = answerTextOf(answersBySignalId[signal.id]);
  }
  // Any question in the group addresses the whole group — the server resolves
  // the request the signal belongs to, not the one row. The first is simply
  // the one that is always there, having just been length-checked above.
  const [first] = questions as [Signal, ...Signal[]];
  await api.answerSignal(first.id, answers);
  announceSignalsChanged();
}

/**
 * Acknowledge a notification: close it without answering anything.
 *
 * Notifications are the one signal kind with no answer to deliver, so they have
 * no producer-specific resolve path — a tool notification and a derived one both
 * close here, through the signal-level verb (POST /signals/{id}/resolve).
 *
 * The server refuses this for a QUESTION on purpose: a question nobody answered
 * has not been handled, and grading it "seen" would read as handled on the
 * surface that matters most, a worker's kanban card. Dismiss it instead.
 */
export async function acknowledgeSignal(api: ApiClient, signalId: string): Promise<void> {
  await api.resolveSignal(signalId, SIGNAL_STATE_ACKNOWLEDGED);
  announceSignalsChanged();
}

/**
 * Close a signal without an answer. Says out loud that no answer is coming,
 * which is the honest close for a question the user will not take — and, for a
 * derived question, what walks its session back off `awaiting_user`.
 */
export async function dismissSignal(api: ApiClient, signalId: string): Promise<void> {
  await api.resolveSignal(signalId, SIGNAL_STATE_DISMISSED);
  announceSignalsChanged();
}
