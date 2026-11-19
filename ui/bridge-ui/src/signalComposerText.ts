import type { Signal, SignalAnswerDraft } from '@kayushkin/chat-core'

/** The answers a chat-pane question card sends, with what the human typed in
 *  the chat composer added in.
 *
 *  In the chat pane the card draws no text box of its own: the composer sits
 *  right under it, and that is where a human types "these three, but staging
 *  first" after ticking options. Pressing the card's Answer used to send the
 *  ticks and leave that text sitting in the composer.
 *
 *  Which question the text answers:
 *   - When exactly one question has no options, the text answers it. Nothing on
 *     the card can answer that question, so the composer is its only way in.
 *     Joining the text to the last question instead left it unanswered and the
 *     Answer button disabled whenever it came before a question with options
 *     (seen 2026-11-07: "Which browser?" above a Yes/No question).
 *   - Otherwise the text joins the LAST question's answer. One request resolves
 *     once with every answer together, and the composer holds one piece of
 *     text, so it goes with the answer the human read last rather than being
 *     copied into each.
 *  Text already typed for that question stays first. Empty composer text
 *  changes nothing. */
export function answersWithComposerText(
  questions: readonly Signal[],
  answersBySignalId: Readonly<Record<string, SignalAnswerDraft>>,
  composerText: string,
): Record<string, SignalAnswerDraft> {
  const typed = composerText.trim()
  const target = questionTheComposerAnswers(questions)
  if (typed === '' || target === undefined) return { ...answersBySignalId }
  const existing = answersBySignalId[target.id]
  const existingText = (existing?.text ?? '').trim()
  return {
    ...answersBySignalId,
    [target.id]: {
      ...existing,
      text: existingText === '' ? typed : `${existingText}\n\n${typed}`,
    },
  }
}

function questionTheComposerAnswers(questions: readonly Signal[]): Signal | undefined {
  const questionsWithoutOptions = questions.filter((question) => question.options.length === 0)
  if (questionsWithoutOptions.length === 1) return questionsWithoutOptions[0]
  return questions[questions.length - 1]
}
