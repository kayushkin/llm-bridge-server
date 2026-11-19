// Session signals — the canonical record of anything a session surfaces to a
// human: a question that needs an answer, or a notification that needs at most
// an acknowledgement. Canonical Go type: llm-bridge `msg/signal.go`; read API:
// llm-bridge-server `internal/server/signals.go`.
//
// This file is the wire edge for that record: the snake_case shape the server
// sends, the camelCase shape chat-core renders from, and the mapping between
// them. It sits in its own module rather than in `types.ts` + `wireEvents.ts`
// because a signal is BOTH — the same record is read over HTTP and grouped for
// display — and splitting six small types across two files to honor a
// file-per-direction rule would leave nowhere to put `groupSignalsByRequest`,
// which is the one piece of signal logic every surface needs and none of them
// should re-derive.
//
// Read path: GET {basePath}/signals?state=open[&session_id=…], every surface.
// Which of those need the operator is decided by session type, in
// `attention.ts`, not by surface.
//
// The per-session route GET /sessions/{id}/signals exists too, but everything
// here deliberately uses the cross-session route: the per-session route answers
// 404 for BOTH "this server has no signals route" and "no such session", and
// those two need different handling. /signals only 404s for the first, so the
// answer is unambiguous.

/** One pre-baked answer offered for a question, on the wire. */
export interface SignalOptionWire {
  label: string;
  /** What the resolve verb sends back when this option is picked. Producers with
   *  no separate machine value set it to `label`. */
  value: string;
  description?: string;
}

/** A resolved question's answer, on the wire. A picked option and a typed edit
 *  are not exclusive — a human may pick an option and amend it. */
export interface SignalAnswerWire {
  option?: string;
  text?: string;
}

/** `msg.Signal` as JSON. Every field the server can send is named; the Go side
 *  marks most of them `omitempty`, so absence is normal and is not an error. */
export interface SignalWire {
  id: string;
  session_id: string;
  /** Metadata, NOT a discriminator: it records what kind of session raised the
   *  signal. `surface` is what decides where the signal renders. */
  session_type?: string;
  kind: string;
  source: string;
  /** Pairs a tool-sourced signal with the parked hook request it was minted
   *  from, so resolving the hook resolves the signal. Absent for derived
   *  signals, which have no parked request behind them. */
  request_id?: string;
  surface: string;
  title: string;
  body?: string;
  options?: SignalOptionWire[];
  allow_freeform?: boolean;
  /** Whether more than one of `options` may be picked at once. */
  allow_multiple_options?: boolean;
  answer?: SignalAnswerWire | null;
  severity?: string;
  state: string;
  /** The noteboard item this signal propagates to, when the raising session is
   *  linked to one. */
  linked_todo_id?: string;
  /** Who a surfaced question is for, as judged by the server's question
   *  triage: `assignee` (the person on the card) or `customer` (whoever asked
   *  for the work). Absent for notifications, chat questions, and questions
   *  minted before triage existed. */
  audience?: string;
  /** A reply to the customer, drafted at triage and ready to send, for a
   *  question whose audience is `customer`. `to` is empty when the sender of
   *  the card's mail could not be resolved. */
  customer_reply_draft?: SignalCustomerReplyDraftWire | null;
  created_at: string;
  resolved_at?: string | null;
}

/** `msg.SignalCustomerReplyDraft` as JSON. */
export interface SignalCustomerReplyDraftWire {
  to?: string;
  subject: string;
  body: string;
}

/** camelCase of {@link SignalCustomerReplyDraftWire}. */
export interface SignalCustomerReplyDraft {
  to: string;
  subject: string;
  body: string;
}

/** camelCase of {@link SignalOptionWire}. */
export interface SignalOption {
  label: string;
  value: string;
  description: string;
}

/** camelCase of {@link SignalAnswerWire}: the answer a RESOLVED question was
 *  closed with, as the server recorded it. Read-only here — nothing in this
 *  client composes one. */
export interface SignalAnswer {
  option?: string;
  text?: string;
}

/** An answer being COMPOSED in a form, before anything is sent.
 *
 *  Deliberately not {@link SignalAnswer}. That type mirrors the server's record
 *  and carries `option` singular, which cannot hold the answer to a question
 *  that allows several — the two were one type only while every question was
 *  pick-one, and sharing them again would put the multi-select answer somewhere
 *  it does not fit.
 *
 *  Picked options and typed text can both be set; `answerTextOf` sends both. */
export interface SignalAnswerDraft {
  /** The `value` of every option picked. More than one entry only when the
   *  signal's `allowMultipleOptions` says so. */
  pickedOptionValues?: readonly string[];
  text?: string;
}

/** camelCase of {@link SignalWire} — what every signal surface renders from. */
export interface Signal {
  id: string;
  sessionId: string;
  sessionType: string;
  kind: string;
  source: string;
  /** '' for a derived signal, which parked no hook. Everything that answers a
   *  signal branches on this, so it is always present as a string rather than
   *  optional: "no parked request" is a real, common answer. */
  requestId: string;
  surface: string;
  title: string;
  body: string;
  options: SignalOption[];
  allowFreeform: boolean;
  /** Whether more than one option may be picked. Decides checkboxes against
   *  radio buttons, and it is the ONLY thing that does — a card must never
   *  guess multi-ness from the option count or the question's wording. */
  allowMultipleOptions: boolean;
  /** null while the signal is open. NOT defaulted to an empty answer: "nobody
   *  has answered" and "answered with nothing" are different facts. */
  answer: SignalAnswer | null;
  severity: string;
  state: string;
  linkedTodoId: string;
  /** '' when nobody judged who the question is for — see the wire field. Never
   *  guessed from the surface or the session type. */
  audience: string;
  /** null unless triage drafted a reply to the customer. A draft is shown,
   *  never sent, by anything in this client. */
  customerReplyDraft: SignalCustomerReplyDraft | null;
  createdAt: string;
  /** null while the signal is open; stamped when `state` leaves `open`. */
  resolvedAt: string | null;
}

/** The answer-expected axis. Orthogonal to session type: an interactive
 *  session, a herald relay and an autonomous worker each raise both kinds. */
export const SIGNAL_KIND_QUESTION = 'question';
/** Needs at most an acknowledgement, and does not block the raising session. */
export const SIGNAL_KIND_NOTIFICATION = 'notification';

/** A structured tool call minted this signal — AskUserQuestion for a question,
 *  a notify-style tool for a notification. These carry a `requestId`. */
export const SIGNAL_SOURCE_TOOL = 'tool';
/** A classifier pass over a turn-end minted this signal from free text. These
 *  carry no `requestId`, because no hook was ever parked for them. */
export const SIGNAL_SOURCE_DERIVED = 'derived';

/** Routes to the "Needs you" inbox, the raising session's own chat, and
 *  reference chips — the attended surfaces, which is what chat-core renders. */
export const SIGNAL_SURFACE_CHAT = 'chat';
/** Routes to the raising worker's kanban card, for async review. */
export const SIGNAL_SURFACE_KANBAN = 'kanban';

export const SIGNAL_STATE_OPEN = 'open';
/** A question resolved with an answer. */
export const SIGNAL_STATE_ANSWERED = 'answered';
/** A notification seen. The server refuses this state for a question. */
export const SIGNAL_STATE_ACKNOWLEDGED = 'acknowledged';
/** Closed without an answer. */
export const SIGNAL_STATE_DISMISSED = 'dismissed';

export const SIGNAL_SEVERITY_INFO = 'info';
export const SIGNAL_SEVERITY_WARN = 'warn';

/** The two closes POST /signals/{id}/resolve accepts. `answered` is NOT one of
 *  them: an answer reaches its signal through the producer's own resolve path —
 *  the parked hook for a tool question, the session's next message for a derived
 *  one — and the server writes `answered` itself from there. */
export type SignalResolveState =
  | typeof SIGNAL_STATE_ACKNOWLEDGED
  | typeof SIGNAL_STATE_DISMISSED;

/** Project one wire signal to the camelCase render model.
 *
 *  Every field is copied explicitly (never spread), so a wire rename fails the
 *  type-check here instead of leaking a snake_case key into the client. The
 *  `omitempty` scalars default to their empty value, because on the Go side an
 *  absent `body` IS the empty body; `answer` and `resolved_at` default to null
 *  instead, because there a missing value means "not yet", which is not the
 *  same as an empty one. */
export function signalFromWire(w: SignalWire): Signal {
  return {
    id: w.id,
    sessionId: w.session_id,
    sessionType: w.session_type ?? '',
    kind: w.kind,
    source: w.source,
    requestId: w.request_id ?? '',
    surface: w.surface,
    title: w.title,
    body: w.body ?? '',
    options: (w.options ?? []).map((o) => ({
      label: o.label,
      value: o.value,
      description: o.description ?? '',
    })),
    allowFreeform: w.allow_freeform ?? false,
    // Defaults FALSE, and that is the safe direction: a server that predates
    // the field, or a producer that omits it, asked a pick-one question.
    // Defaulting the other way would offer checkboxes for a question the tool
    // will only accept one answer to.
    allowMultipleOptions: w.allow_multiple_options ?? false,
    answer: w.answer ?? null,
    severity: w.severity ?? '',
    state: w.state,
    linkedTodoId: w.linked_todo_id ?? '',
    audience: w.audience ?? '',
    customerReplyDraft: w.customer_reply_draft
      ? { to: w.customer_reply_draft.to ?? '', subject: w.customer_reply_draft.subject, body: w.customer_reply_draft.body }
      : null,
    createdAt: w.created_at,
    resolvedAt: w.resolved_at ?? null,
  };
}

/** One request's worth of signals — the unit that gets answered.
 *
 *  One AskUserQuestion tool call carries an array of questions and mints one
 *  signal per question, all sharing a `request_id`, and the resolve verb is
 *  per-request. So the request, not the signal, is what a card submits. */
export interface SignalRequest {
  /** The parked hook's request id, or '' when the group came from a derived
   *  signal (which parked nothing). */
  requestId: string;
  sessionId: string;
  signals: Signal[];
}

/** Group signals by the request they were minted from.
 *
 *  Signals with no `requestId` each get their own group keyed by signal id, so
 *  a caller never has to special-case them: a derived group simply holds one
 *  signal and answers through a different verb. */
export function groupSignalsByRequest(signals: readonly Signal[]): SignalRequest[] {
  const groups: SignalRequest[] = [];
  const byKey = new Map<string, SignalRequest>();
  for (const signal of signals) {
    // The separator is NUL because it is the one character that cannot occur in
    // a session id or a request id, so `ab` + `c` cannot collide with `a` +
    // `bc`. The no-request form is prefixed as well, so a derived signal's key
    // can never equal a real `sessionId + requestId` pair.
    //
    // WRITTEN AS THE ESCAPE, NEVER AS A RAW NUL BYTE. A raw NUL makes the whole
    // FILE binary: `file(1)` reports `data`, and every search that skips binary
    // files -- ripgrep, ugrep, git-grep without `-a`, and the `grep` every agent
    // on this box runs -- then returns ZERO matches in it, with no error and no
    // warning. git cannot diff it either. `src/reduce/otelDedup.ts` was in that
    // state for three weeks; `test/sourceIsGreppable.test.ts` now scans the tree
    // for the raw byte and is what keeps this line honest.
    const key = signal.requestId
      ? `${signal.sessionId}\u0000${signal.requestId}`
      : `\u0000signal\u0000${signal.id}`;
    let group = byKey.get(key);
    if (!group) {
      group = { requestId: signal.requestId, sessionId: signal.sessionId, signals: [] };
      byKey.set(key, group);
      groups.push(group);
    }
    group.signals.push(signal);
  }
  return groups;
}
