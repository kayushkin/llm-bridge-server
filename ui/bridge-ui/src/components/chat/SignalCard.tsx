import { useCallback, useState, type JSX, type ReactNode } from 'react';
import { useChatContext } from '@kayushkin/chat-core';
import {
  SIGNAL_KIND_NOTIFICATION,
  SIGNAL_SEVERITY_WARN,
  type Signal,
  type SignalAnswerDraft,
  type SignalRequest,
} from '@kayushkin/chat-core';
import {
  acknowledgeSignal,
  answerSignalRequest,
  dismissSignal,
  everyQuestionAnswered,
  questionsIn,
} from '@kayushkin/chat-core';
import { SignalBodyMarkdown, SignalInlineMarkdown } from './signalMarkdown';
import { answersWithComposerText } from '../../signalComposerText';

// Every element carries a stable, unhashed class name (`signal-*`), styled by
// this package's own `styles.css`. Lived in chat-core until 2026-09-10; that
// package is the data layer and ships no components now. The prefix is
// `signal-` rather than `ref-chip-` because these cards are not a chip's
// property; the RefChip panel is one of four surfaces they render on, and the
// mount point inside it is what carries the `ref-chip-*` name.

export interface SignalCardProps {
  signal: Signal;
  /** The answer being composed for this signal: picked options, typed text, or
   *  both. Picking leaves the text alone and typing leaves the picks alone. */
  answer?: SignalAnswerDraft;
  onChangeAnswer?: (answer: SignalAnswerDraft) => void;
  /** Send this answer immediately, without waiting for a Submit.
   *
   *  Supplied ONLY when this signal is the single question in its request, so
   *  one click is the whole answer. A request carrying several questions
   *  resolves once, with every answer together, and clicking option one of
   *  question one there would resolve it with the rest blank.
   *
   *  The card uses it for a pick-ONE question only: a pick-many question is not
   *  finished until the human says it is. */
  onAnswerAndSubmit?: (answer: SignalAnswerDraft) => void;
  /** Acknowledge a notification. Left optional because a surface that cannot
   *  refetch afterwards is better off not offering the button at all — the card
   *  renders no action rather than one that leaves a resolved row on screen. */
  onAcknowledge?: () => void;
  busy?: boolean;
  /** Drops descriptions and the body for tight surfaces (the RefChip session
   *  panel). The question, its options and its freeform box all still render:
   *  compact trims chrome, never the means of answering. */
  compact?: boolean;
  /** Open showing nothing but the answers, with a disclosure that reveals the
   *  question and its summary.
   *
   *  For the chat pane, where the transcript directly above already carries the
   *  question — repeating it under the transcript is the same words twice, and
   *  the answers are the only part you cannot read further up.
   *
   *  A question with no options keeps its title, so the card says what it is
   *  asking. A notification shows only its Acknowledge button, for the same
   *  reason the question hides — its text is the last thing the transcript said.
   *
   *  It also drops the freeform box, because the chat composer sits right under
   *  it. That is safe for both producers, and for different reasons:
   *   - A DERIVED question is answered by the session's next message: a /send
   *     closes it (`closeQuestionsAnsweredByMessage` in llm-bridge-server).
   *   - A TOOL question is NOT — a bare /send leaves its park open
   *     (`TestAnswerDerivedQuestionsLeavesToolParksToTheHookVerb`), because the
   *     harness is blocked on its hook, not on stdin. But AskUserQuestion always
   *     carries options, and every option is editable: rewriting one is how you
   *     answer a tool question in your own words here. And the card's own Answer
   *     takes whatever is in the composer with it, when the surface passes
   *     `composerText`.
   *  So a card with no options in the chat pane has nothing to click, and says
   *  so by drawing no Answer button either. */
  startCollapsedToAnswers?: boolean;
}

/** Whether a card renders its own freeform answer box: never in the chat
 *  pane, where the composer is right below — see `startCollapsedToAnswers`.
 *
 *  Read by {@link SignalRequestCard} as well as by the card, because the box
 *  decides whether a Submit button has anything to send. One rule, one place. */
function rendersFreeformBox(signal: Signal, startCollapsedToAnswers: boolean): boolean {
  if (signal.kind === SIGNAL_KIND_NOTIFICATION) return false;
  return !startCollapsedToAnswers;
}

/**
 * SignalCard renders exactly one signal record, by kind. It takes EVERYTHING
 * through props and reads no context at all, so the same card renders in the
 * raising session's chat, in a cross-session inbox, and inside another session's
 * RefChip panel.
 *
 * It composes an answer, and submits one only when the surface hands it
 * {@link SignalCardProps.onAnswerAndSubmit} — which a surface does only for a
 * request holding this question and no other. Everything else resolves through
 * {@link SignalRequestCard} below, because a tool question is one of several
 * sharing a parked request and that whole request resolves at once.
 */
export function SignalCard({
  signal,
  answer,
  onChangeAnswer,
  onAnswerAndSubmit,
  onAcknowledge,
  busy,
  compact,
  startCollapsedToAnswers,
}: SignalCardProps): JSX.Element {
  const isNotification = signal.kind === SIGNAL_KIND_NOTIFICATION;
  const options = signal.options;
  const chosen = answer?.pickedOptionValues ?? [];
  const text = answer?.text ?? '';
  // The record is the ONLY authority on this. Never inferred from the option
  // count or the question's wording: offering a pick-many form for a pick-one
  // question lets a human send an answer the tool will refuse, and offering a
  // pick-one form for a pick-many one silently drops every choice after the
  // first.
  const multiple = !isNotification && signal.allowMultipleOptions;
  // A pick-one question is answered BY the click, so it draws plain buttons and
  // no radio group: a radio's whole job is to hold a choice until a Submit, and
  // there is no Submit to hold it for. Radios stay on nothing; checkboxes stay
  // on the pick-many form, which really does wait.
  const submitsOnPick = !multiple && onAnswerAndSubmit !== undefined;

  const collapsible = startCollapsedToAnswers === true;
  const [questionShown, setQuestionShown] = useState(false);
  const questionVisible = !collapsible || questionShown;
  // See `startCollapsedToAnswers`: with no options to show, the title is what
  // tells the reader what the text box answers.
  const titleVisible = questionVisible || (!isNotification && options.length === 0);
  const kindLabel = isNotification ? 'notification' : 'question';
  const acknowledgeButton = isNotification && onAcknowledge && (
    <button type="button" className="signal-ack" disabled={busy} onClick={onAcknowledge}>
      Acknowledge
    </button>
  );

  /** The text of one option that has been rewritten, keyed by the option's own
   *  value. Local, ephemeral view state: what the human typed goes into the
   *  composed answer immediately, and this only remembers it so the row keeps
   *  showing the edit instead of snapping back to the label. */
  const [editedTextByOptionValue, setEditedTextByOptionValue] = useState<Record<string, string>>(
    {},
  );
  const [optionBeingEdited, setOptionBeingEdited] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState('');

  /** Pick one option, replacing whatever was picked before. */
  const chooseOne = (value: string) => {
    const draft: SignalAnswerDraft = { ...answer, pickedOptionValues: [value] };
    if (submitsOnPick) onAnswerAndSubmit?.(draft);
    else onChangeAnswer?.(draft);
  };

  /** Add or remove one option on a pick-many question. Typed text stays. */
  const toggleOption = (value: string) => {
    const next = chosen.includes(value) ? chosen.filter((v) => v !== value) : [...chosen, value];
    onChangeAnswer?.({ ...answer, pickedOptionValues: next });
  };

  /** Open the editor on one option. `startingText` is what the human reads on the
   *  row, which is the thing they are about to amend — see the field itself. */
  const startEditing = (optionValue: string, startingText: string) => {
    setOptionBeingEdited(optionValue);
    setEditDraft(startingText);
  };

  const cancelEditing = () => {
    setOptionBeingEdited(null);
    setEditDraft('');
  };

  const commitEdit = (optionValue: string) => {
    const rewritten = editDraft.trim();
    // An empty answer is not an answer: `answerTextOf` reads it as unanswered,
    // so committing it would leave a filled-in-looking field and a request that
    // will not submit. The editor stays open instead.
    if (rewritten === '') return;
    const previous = editedTextByOptionValue[optionValue] ?? optionValue;
    setEditedTextByOptionValue((prev) => ({ ...prev, [optionValue]: rewritten }));
    setOptionBeingEdited(null);
    setEditDraft('');
    // Rewriting an answer is choosing it — nobody retypes an option they are not
    // going to send.
    if (multiple) {
      onChangeAnswer?.({
        ...answer,
        pickedOptionValues: [...chosen.filter((v) => v !== previous), rewritten],
      });
    } else {
      chooseOne(rewritten);
    }
  };

  return (
    <div
      className={`signal-card${compact ? ' signal-card-compact' : ''}${
        collapsible && !questionShown ? ' signal-card-answers-only' : ''
      }`}
      data-signal-id={signal.id}
    >
      <div className="signal-card-header">
        {collapsible ? (
          <button
            type="button"
            className="signal-disclosure"
            aria-expanded={questionShown}
            aria-label={`${questionShown ? 'Hide' : 'Show'} the ${kindLabel}`}
            title={`${questionShown ? 'Hide' : 'Show'} the ${kindLabel}`}
            onClick={() => setQuestionShown((shown) => !shown)}
          >
            <span className="signal-disclosure-caret" aria-hidden>
              {questionShown ? '▾' : '▸'}
            </span>
            <span className={`signal-kind signal-kind-${kindLabel}`}>{kindLabel}</span>
          </button>
        ) : (
          <span className={`signal-kind signal-kind-${kindLabel}`}>{kindLabel}</span>
        )}
        {isNotification && signal.severity === SIGNAL_SEVERITY_WARN && (
          <span className="signal-severity">warn</span>
        )}
        {/* On the header line when the card is collapsible, so a collapsed
            notification is one line: its label and the one thing to do. */}
        {collapsible && acknowledgeButton}
      </div>

      {titleVisible && (
        <p className="signal-title">
          <SignalInlineMarkdown text={signal.title} />
        </p>
      )}
      {signal.body !== '' && !compact && questionVisible && (
        <SignalBodyMarkdown text={signal.body} />
      )}

      {!isNotification && options.length > 0 && (
        <div className="signal-options">
          {options.map((option) => {
            // `value` is what the resolve verb sends back; producers with no
            // separate machine value set it to the label, so the label is the
            // fallback rather than an invention.
            const optionValue = option.value || option.label;
            const rewritten = editedTextByOptionValue[optionValue];
            // What this row will actually send, rewritten or not. Selection is
            // tested against it, so an edited option stays picked.
            const answerValue = rewritten ?? optionValue;
            const shownLabel = rewritten ?? option.label;
            const picked = chosen.includes(answerValue);
            const editing = optionBeingEdited === optionValue;
            const body = (
              <span className="signal-option-body">
                <span className="signal-option-label">{shownLabel}</span>
                {option.description !== '' && !compact && questionVisible && (
                  <span className="signal-option-desc">
                    <SignalInlineMarkdown text={option.description} />
                  </span>
                )}
              </span>
            );
            return (
              <div
                key={optionValue}
                className={`signal-option${picked ? ' signal-option-selected' : ''}${
                  editing ? ' signal-option-editing' : ''
                }`}
              >
                {editing ? (
                  <>
                    <input
                      className="signal-option-edit-input"
                      // Prefilled with the LABEL — the words on the row — and not
                      // with the option's machine value, on the one thing this
                      // control is for: amending the answer you are reading.
                      // "b" is not amendable into "b, but staging first"; the
                      // sentence it stands for is. The cost is that opening the
                      // editor on an option whose value differs from its label
                      // and pressing Enter sends the label, which is visible in
                      // the field the whole time — where sending an unreadable
                      // token would not be.
                      value={editDraft}
                      autoFocus
                      disabled={busy}
                      aria-label={`Answer instead of “${option.label}”`}
                      onChange={(e) => setEditDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') {
                          e.preventDefault();
                          commitEdit(optionValue);
                        }
                        if (e.key === 'Escape') {
                          e.preventDefault();
                          cancelEditing();
                        }
                      }}
                    />
                    <button
                      type="button"
                      className="signal-option-save"
                      disabled={busy || editDraft.trim() === ''}
                      onClick={() => commitEdit(optionValue)}
                    >
                      {submitsOnPick ? 'Answer' : 'Use'}
                    </button>
                  </>
                ) : (
                  <>
                    {multiple ? (
                      <label className="signal-option-choice">
                        <input
                          // A checkbox is the whole visible difference between
                          // "pick any that apply" and "pick one", and a human
                          // reads it before touching anything. It is driven
                          // straight off the record so the form cannot promise a
                          // choice the tool will not take.
                          type="checkbox"
                          checked={picked}
                          disabled={busy || !onChangeAnswer}
                          onChange={() => toggleOption(answerValue)}
                        />
                        {body}
                      </label>
                    ) : (
                      <button
                        type="button"
                        className="signal-option-choice signal-option-pick"
                        // Pressed-ness is a claim about a control that HOLDS a
                        // state. When the click submits, nothing is held, and
                        // saying otherwise would leave a screen reader announcing
                        // a toggle that resolved the question and went away.
                        aria-pressed={submitsOnPick ? undefined : picked}
                        disabled={busy || !onChangeAnswer}
                        onClick={() => chooseOne(answerValue)}
                      >
                        {body}
                      </button>
                    )}
                    <button
                      type="button"
                      className="signal-option-edit"
                      // "Rewrite", never "Edit … before answering". The name a
                      // control answers to has to be the one thing it is: an
                      // accessible name carrying the word "answer" made every
                      // pencil on the card a match for the Submit button, by
                      // name, for a screen reader and a test alike.
                      aria-label={`Rewrite “${shownLabel}”`}
                      title="Rewrite this answer"
                      disabled={busy || !onChangeAnswer}
                      onClick={() => startEditing(optionValue, shownLabel || answerValue)}
                    >
                      ✎
                    </button>
                  </>
                )}
              </div>
            );
          })}
        </div>
      )}

      {/* The freeform box, on every surface but the chat pane — see
          `startCollapsedToAnswers`.
          NOT gated on `compact`: that hid the only way to answer a question the
          producer minted with no options, and trimming chrome must never remove
          the means of answering.
          NOT gated on `signal.allowFreeform` either. Nothing sets that field
          false — both producers hardcode it true (`signal_classifier.go:504`,
          `signals.go:115`) and the server has no path that rejects a freeform
          answer. What it does have is `signalFromWire` defaulting it to FALSE
          when the key is absent, so an older row or a producer that omits it
          loses its answer box with no way to tell. A display hint that is never
          deliberately false, and fails closed onto an unanswerable card, is not
          worth honoring. The field stays parsed — it is the wire's, and this
          layer passes it through — it simply no longer decides this. */}
      {rendersFreeformBox(signal, startCollapsedToAnswers === true) && (
        <textarea
          className="signal-freeform"
          placeholder={options.length > 0 ? '…or answer in your own words' : 'Type your answer'}
          value={text}
          disabled={busy || !onChangeAnswer}
          onChange={(e) => onChangeAnswer?.({ ...answer, text: e.target.value })}
          rows={2}
        />
      )}

      {!collapsible && acknowledgeButton && (
        <div className="signal-actions">{acknowledgeButton}</div>
      )}
    </div>
  );
}

export interface SignalRequestCardProps {
  request: SignalRequest;
  /** Called after a successful resolve so the surface can refetch. */
  onResolved?: () => void;
  /** Rendered above the questions — a cross-session inbox uses it for a link to
   *  the raising session; the in-session surfaces pass nothing. */
  header?: ReactNode;
  compact?: boolean;
  /** Passed to every card — see {@link SignalCardProps.startCollapsedToAnswers}. */
  startCollapsedToAnswers?: boolean;
  /** Offer to close a question nobody is going to answer.
   *
   *  Only meaningful for a DERIVED question: a parked tool question already has
   *  Decline, which denies the call the question came from, and that is the
   *  honest close there. A derived question parked nothing, so without this it
   *  stays open until the session's next turn happens to supersede it — which,
   *  on a card raised by a worker that has since stopped, is never.
   *
   *  Off by default, so a chat surface keeps answering as its only close. */
  allowDismissWithoutAnswer?: boolean;
  /** The chat composer's text, sent with the answers and cleared once they are
   *  accepted. For the chat pane, whose cards draw no text box because the
   *  composer is right below: see `answersWithComposerText`. */
  composerText?: ComposerTextForAnswer;
}

/** What a card needs from the chat composer: what is typed there now, and a way
 *  to empty it once that text has gone out with an answer. */
export interface ComposerTextForAnswer {
  text: string;
  clear: () => void;
}

/**
 * SignalRequestCard renders every signal minted by one parked request and
 * submits their answers TOGETHER.
 *
 * One AskUserQuestion call carries several questions and resolves once, so
 * answering a single question in isolation would resolve the whole request with
 * the rest unanswered. Submit stays disabled until every question in the request
 * has an answer — and the one shape where that leaves nothing to wait for, a
 * request holding a single pick-one question, answers on the click instead and
 * draws no Submit at all.
 *
 * ⚠️ The only thing this reads from context is the `ApiClient` singleton — never
 * the active session. Which session it is answering comes from `request`, and
 * that is precisely why it can be mounted inside another session's RefChip panel
 * and answer a question the user is not looking at.
 */
export function SignalRequestCard({
  request,
  onResolved,
  header,
  compact,
  startCollapsedToAnswers,
  allowDismissWithoutAnswer,
  composerText,
}: SignalRequestCardProps): JSX.Element {
  const { api } = useChatContext();
  const [answers, setAnswers] = useState<Record<string, SignalAnswerDraft>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const questions = questionsIn(request);
  const typedInComposer = composerText?.text ?? '';
  const allAnswered = everyQuestionAnswered(
    request,
    answersWithComposerText(questions, answers, typedInComposer),
  );

  /** The question that answers on a click, or null.
   *
   *  Three conditions, each of them load-bearing: it is the ONLY question in the
   *  request (the request resolves once, with everything), it takes ONE choice
   *  (a pick-many answer is not finished until the human says so), and it HAS
   *  options (nothing to click otherwise). */
  const sole = questions.length === 1 ? questions[0] : undefined;
  const answersOnPick =
    sole !== undefined && !sole.allowMultipleOptions && sole.options.length > 0 ? sole : null;
  // Submit needs something on the card to submit: an option or a freeform box.
  // A chat-pane card for a question with no options has neither, and is answered
  // from the composer instead.
  const answerableHere = questions.some(
    (question) =>
      question.options.length > 0 || rendersFreeformBox(question, startCollapsedToAnswers === true),
  );
  // It survives auto-submit only where a freeform box does: text typed into that
  // box has nothing else to send it.
  const submitShown =
    answerableHere &&
    (answersOnPick === null || rendersFreeformBox(answersOnPick, startCollapsedToAnswers === true));

  const setAnswer = useCallback((signalId: string, answer: SignalAnswerDraft) => {
    setAnswers((prev) => ({ ...prev, [signalId]: answer }));
  }, []);

  /** Every action here is the same three steps around a different verb: hold the
   *  card busy, surface any refusal in place, and let the surface refetch on
   *  success. A failed resolve must leave the card on screen — the question is
   *  still open and the only way out is another click. */
  const run = useCallback(
    async (verb: () => Promise<void>, afterSuccess?: () => void) => {
      if (busy) return;
      setBusy(true);
      setError(null);
      try {
        await verb();
        afterSuccess?.();
        onResolved?.();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    },
    [busy, onResolved],
  );

  /** Send a specific set of answers. Takes them as an argument rather than
   *  reading state, because the click that completes an answer has to submit the
   *  map INCLUDING that answer — a `setAnswers` before a read would still see the
   *  old one. */
  const submitAnswers = useCallback(
    (composed: Record<string, SignalAnswerDraft>) => {
      const withComposer = answersWithComposerText(questions, composed, typedInComposer);
      if (!everyQuestionAnswered(request, withComposer)) return;
      // One verb for both producers: it sends a tool request's answers back
      // through its parked hook, and a derived question's answer as the session's
      // next message. The branch lives in the store, not in this card.
      // The composer is emptied only once the answer is accepted, so a refusal
      // leaves the human's text where they typed it.
      void run(
        () => answerSignalRequest(api, request, withComposer),
        typedInComposer.trim() !== '' ? composerText?.clear : undefined,
      );
    },
    [run, api, request, questions, typedInComposer, composerText],
  );

  const submit = useCallback(() => {
    submitAnswers(answers);
  }, [submitAnswers, answers]);

  const answerAndSubmit = useCallback(
    (signalId: string, answer: SignalAnswerDraft) => {
      const composed = { ...answers, [signalId]: answer };
      setAnswers(composed);
      submitAnswers(composed);
    },
    [answers, submitAnswers],
  );

  const acknowledge = useCallback(
    (signalId: string) => {
      void run(() => acknowledgeSignal(api, signalId));
    },
    [run, api],
  );

  const dismiss = useCallback(() => {
    // A group of derived signals holds exactly one row today, but closing every
    // question in the group is what "dismiss this" means either way — leaving a
    // sibling open would be a half-closed request.
    void run(async () => {
      for (const signal of questions) await dismissSignal(api, signal.id);
    });
  }, [run, api, questions]);

  return (
    <div className="signal-request" data-session-id={request.sessionId} data-request-id={request.requestId}>
      {header}
      {request.signals.map((signal) => (
        <SignalCard
          key={signal.id}
          signal={signal}
          answer={answers[signal.id]}
          // Notifications are acknowledged, not answered — they compose nothing
          // on either producer's path, and close one at a time through the
          // signal-level verb rather than with the group.
          onChangeAnswer={
            signal.kind === SIGNAL_KIND_NOTIFICATION ? undefined : (a) => setAnswer(signal.id, a)
          }
          onAnswerAndSubmit={
            answersOnPick !== null && answersOnPick.id === signal.id
              ? (a) => answerAndSubmit(signal.id, a)
              : undefined
          }
          onAcknowledge={
            signal.kind === SIGNAL_KIND_NOTIFICATION ? () => acknowledge(signal.id) : undefined
          }
          busy={busy}
          compact={compact}
          startCollapsedToAnswers={startCollapsedToAnswers}
        />
      ))}
      {questions.length > 0 && (submitShown || allowDismissWithoutAnswer === true) && (
        <div className="signal-actions">
          {submitShown && (
            <button
              type="button"
              className="signal-submit"
              disabled={busy || !allAnswered}
              onClick={submit}
            >
              Answer
            </button>
          )}
          {/* One button for "no answer is coming", not the Decline/Dismiss pair
              this used to render.
              The pair existed because the two producers had different verbs —
              deny the parked tool call, or dismiss the row — and the card chose
              between them on `requestId`. That is evidence the card does not
              have: a requestId says a park EXISTED, not that it is still live.
              So a question whose park had died offered Decline, the only button
              that could not work, and never offered the one that could.
              The server picks now: dismissing a live park denies its tool call,
              and dismissing anything else closes the row. */}
          {allowDismissWithoutAnswer && (
            <button type="button" className="signal-dismiss" disabled={busy} onClick={dismiss}>
              Dismiss
            </button>
          )}
        </div>
      )}
      {error !== null && <p className="signal-error">{error}</p>}
    </div>
  );
}
