import { createContext, useContext, useState } from 'react'
import { useChatContext, type SessionAction } from '@kayushkin/chat-core'
import {
  SESSION_ACTION_REVIEW_LABEL,
  SESSION_ACTION_TYPE_LABEL,
  sessionActionCostText,
  sessionActionIsConfirmable,
  sessionActionOutputIsMarkdown,
  sessionActionRunBy,
  sessionActionStatusText,
  sessionActionTarget,
} from '../../sessionActionView'
import { RefChip } from './RefChip'
import { ReferenceMarkdown } from './ReferenceText'

/**
 * Session actions in the chat: a button the agent offered, and a line for each step
 * of its one run.
 *
 * The agent places a button by writing its id (`session_action_000007`) in its reply;
 * `RefChip` hands that id here and the button is drawn in the sentence. An action the
 * agent never placed is drawn where it was offered instead. Either way the button's
 * text is the agent's label, "What it runs" shows the server's own statement of the
 * command, and pressing asks again in place — Confirm or Cancel. Only Confirm reaches
 * the server, which runs the action and records who pressed it.
 *
 * Every element is phrasing content (span, button, code) so the button can sit inside
 * a paragraph; the stylesheet lays it out.
 */

export interface SessionActionsInSession {
  /** The newest record of every action in the session, by `action_id`. The offer's
   *  entry keeps the record as it was offered, so a button reads its state here. */
  newest: ReadonlyMap<string, SessionAction>
  /** The actions whose id the agent wrote in its reply, where their buttons are drawn. */
  placedInProse: ReadonlySet<string>
}

/** Empty outside a session's turn list, so an id written anywhere else — a card
 *  panel, another session's text — draws no button. */
const NO_SESSION_ACTIONS: SessionActionsInSession = { newest: new Map(), placedInProse: new Set() }

export const SessionActionsContext = createContext<SessionActionsInSession>(NO_SESSION_ACTIONS)

/** One action entry: the button where it was offered unless the agent placed it in
 *  its reply. A later step of its run draws nothing where it happened: the button
 *  itself shows how the run is going, and the server's log keeps every step. */
export function SessionActionEntry({ action }: { action: SessionAction }) {
  const { newest, placedInProse } = useContext(SessionActionsContext)
  if (action.state !== 'offered') return null
  if (placedInProse.has(action.action_id)) return null
  return <SessionActionButton action={newest.get(action.action_id) ?? action} />
}

/** A session action id written in text. Drawn as the button when this session's agent
 *  offered that action; otherwise it stays the id as written, so text can point at a
 *  button but never make one. */
export function SessionActionInText({ actionId }: { actionId: string }) {
  const action = useContext(SessionActionsContext).newest.get(actionId)
  if (!action) return <>{actionId}</>
  return <SessionActionButton action={action} />
}

function SessionActionButton({ action }: { action: SessionAction }) {
  const { api } = useChatContext()
  const [confirming, setConfirming] = useState(false)
  const [sending, setSending] = useState(false)
  const [showingCommand, setShowingCommand] = useState(false)
  const [refusal, setRefusal] = useState<string | null>(null)
  // What the person typed into the button's boxes. It goes to the server with the
  // confirm and is cleared from here as soon as it has been sent.
  const [inputValues, setInputValues] = useState<Record<string, string>>({})
  const inputs = action.offer.inputs ?? []
  const allInputsFilled = inputs.every((input) => (inputValues[input.name] ?? '') !== '')
  const confirmable = sessionActionIsConfirmable(action)

  const confirm = async () => {
    if (!allInputsFilled) return
    setSending(true)
    setRefusal(null)
    try {
      await api.runSessionAction(action.session_id, action.action_id, inputs.length > 0 ? inputValues : undefined)
      setInputValues({})
      setConfirming(false)
    } catch (error) {
      setRefusal(error instanceof Error ? error.message : String(error))
    } finally {
      setSending(false)
    }
  }

  const costText = sessionActionCostText(action)
  const target = sessionActionTarget(action)
  const review = action.review
  return (
    <span className={`bc-session-action bc-session-action-${action.state}`} data-action-id={action.action_id}>
      <span className="bc-session-action-head">
        <button
          type="button"
          className="bc-session-action-button"
          disabled={!confirmable || confirming}
          onClick={() => setConfirming(true)}
          title={SESSION_ACTION_TYPE_LABEL[action.offer.type]}
        >
          {action.offer.label}
        </button>
        {target && (
          <span className="bc-session-action-session">
            {target.verb} <RefChip kind="session" refId={target.sessionId} />
          </span>
        )}
        {costText && <span className="bc-session-action-cost">{costText}</span>}
        {review && (
          <span className={`bc-session-action-review bc-session-action-review-${review.verdict}`} title={review.reasons}>
            {SESSION_ACTION_REVIEW_LABEL[review.verdict]}
          </span>
        )}
        {action.state !== 'offered' && (
          <span className="bc-session-action-status" title={`Confirmed by ${sessionActionRunBy(action)}`}>
            {sessionActionStatusText(action)}
          </span>
        )}
        <button
          type="button"
          className="bc-session-action-what"
          aria-expanded={showingCommand}
          onClick={() => setShowingCommand((showing) => !showing)}
        >
          What it runs {showingCommand ? '▴' : '▾'}
        </button>
      </span>
      {confirming && confirmable && (
        <span className="bc-session-action-confirm" role="group" aria-label={`Confirm ${action.offer.label}`}>
          {inputs.map((input) => (
            <label key={input.name} className="bc-session-action-input">
              <span className="bc-session-action-input-label">{input.label}</span>
              <input
                type={input.secret ? 'password' : 'text'}
                className="bc-session-action-input-field"
                value={inputValues[input.name] ?? ''}
                autoComplete={input.secret ? 'new-password' : 'off'}
                spellCheck={false}
                disabled={sending}
                onChange={(event) => {
                  const value = event.target.value
                  setInputValues((values) => ({ ...values, [input.name]: value }))
                }}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') void confirm()
                }}
              />
            </label>
          ))}
          <span className="bc-session-action-confirm-question">Run this?</span>
          <button type="button" className="bc-session-action-confirm-yes" disabled={sending || !allInputsFilled} onClick={() => void confirm()}>
            {sending ? 'Starting…' : 'Confirm'}
          </button>
          <button
            type="button"
            className="bc-session-action-confirm-no"
            disabled={sending}
            onClick={() => {
              setConfirming(false)
              setRefusal(null)
              setInputValues({})
            }}
          >
            Cancel
          </button>
        </span>
      )}
      {showingCommand && (
        <code className="bc-session-action-command">
          {action.command}
          {action.state !== 'offered' && `\n\nConfirmed by ${sessionActionRunBy(action)}.`}
        </code>
      )}
      {review && (showingCommand || review.verdict !== 'approve') && (
        <span className={`bc-session-action-review-reasons bc-session-action-review-${review.verdict}`}>
          {review.verdict === 'reject' ? 'This command cannot be run. ' : ''}
          {review.reasons} <span className="bc-session-action-review-model">— {review.model}</span>
        </span>
      )}
      {refusal && <span className="bc-session-action-refusal">{refusal}</span>}
      {action.state !== 'offered' && <SessionActionOutcome action={action} />}
    </span>
  )
}

/** Actions whose output is what the person pressed the button for, shown open; the
 *  rest keep their output — a deploy log, the scheduler's reply — behind a toggle. */
const OUTPUT_IS_THE_RESULT: ReadonlySet<SessionAction['offer']['type']> = new Set([
  'run_command',
  'model_call',
  'background_agent',
  'send_to_session',
  'stop_session',
])

/** The session it started, and what came back. Who pressed it is under "What it runs".
 *  A button that acted on another session already shows that session beside it. */
function SessionActionOutcome({ action }: { action: SessionAction }) {
  const [showingOutput, setShowingOutput] = useState(false)
  const outputIsTheResult = OUTPUT_IS_THE_RESULT.has(action.offer.type)
  const resultSessionIsTheTarget = action.result_session_id === action.offer.target_session_id
  return (
    <>
      {action.result_session_id && !resultSessionIsTheTarget && (
        <span className="bc-session-action-session">
          {action.state === 'running' ? 'Working in' : 'Session'} <RefChip kind="session" refId={action.result_session_id} />
        </span>
      )}
      {action.error && <span className="bc-session-action-error">{action.error}</span>}
      {action.output && outputIsTheResult && <SessionActionResult action={action} />}
      {action.output && !outputIsTheResult && (
        <>
          <button
            type="button"
            className="bc-session-action-what bc-session-action-output-toggle"
            aria-expanded={showingOutput}
            onClick={() => setShowingOutput((showing) => !showing)}
          >
            Output {showingOutput ? '▴' : '▾'}
          </button>
          {showingOutput && <span className="bc-session-action-output">{action.output}</span>}
        </>
      )}
    </>
  )
}

/** The output as the result the button was pressed for. Markdown goes through the same
 *  renderer as the orchestrator's prose, with ids as chips — except a session action
 *  id, which stays text: output is not the agent's reply, so it places no button. */
function SessionActionResult({ action }: { action: SessionAction }) {
  if (!sessionActionOutputIsMarkdown(action)) {
    return <span className="bc-session-action-output">{action.output}</span>
  }
  return (
    <span className="bc-session-action-result">
      <SessionActionsContext.Provider value={NO_SESSION_ACTIONS}>
        <ReferenceMarkdown text={action.output ?? ''} />
      </SessionActionsContext.Provider>
    </span>
  )
}
