import type { JSX } from 'react'
import type { SignalRequest } from '@kayushkin/chat-core'
import { openQuestionPreview } from '../../sessionOpenQuestions'
import { SignalRequestList } from './SessionSignals'

export interface SessionChipOpenQuestionsProps {
  sessionId: string
  /** The session's open groups that ask a question (`questionRequestsOfSession`). */
  questionRequests: readonly SignalRequest[]
  /** Whether the answer form is drawn. Held by the chip, so each chip has its own. */
  answerFormOpen: boolean
  onToggleAnswerForm: () => void
  /** Re-read the signals after an answer lands. */
  onResolved: () => void
}

/**
 * What a session chip draws after itself when its session has an open question: the
 * first question's text and an Answer button, and under them, once pressed, the same
 * answer cards every other signal surface draws (`SignalRequestList`).
 *
 * It sits INLINE, in the text after the chip, rather than in the chip's panel. The
 * panel is a portal onto `document.body` placed against the chip's rect, so several
 * chips with their panels open stack at overlapping places, each covering the
 * others. Drawn in the flow, each chip's question takes its own room, and ten chips
 * read as ten questions.
 *
 * The answer form waits behind the button because a transcript names the same
 * session many times: drawn open, every mention would be a full form.
 *
 * Renders nothing when there is no question, so a session with nothing waiting
 * stays an ordinary chip.
 */
export function SessionChipOpenQuestions({
  sessionId,
  questionRequests,
  answerFormOpen,
  onToggleAnswerForm,
  onResolved,
}: SessionChipOpenQuestionsProps): JSX.Element | null {
  const preview = openQuestionPreview(questionRequests)
  if (preview === null) return null
  const moreQuestions = preview.questionCount - 1
  return (
    <span className="ref-chip-open-questions" data-ref-kind="session" data-ref-id={sessionId}>
      <span className="ref-chip-open-question-text">
        <span className="ref-chip-open-question-label">asks you:</span> {preview.firstQuestionText}
        {moreQuestions > 0 && (
          <span className="ref-chip-open-question-more">
            {' '}
            (and {moreQuestions} more question{moreQuestions === 1 ? '' : 's'})
          </span>
        )}
      </span>{' '}
      <button
        type="button"
        className="ref-chip-answer-toggle"
        aria-expanded={answerFormOpen}
        onClick={onToggleAnswerForm}
      >
        {answerFormOpen ? 'Hide answer' : 'Answer'}
      </button>
      {answerFormOpen && (
        <span className="ref-chip-answer-form">
          <SignalRequestList requests={questionRequests} compact onResolved={onResolved} />
        </span>
      )}
    </span>
  )
}
