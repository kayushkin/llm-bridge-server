import { useCallback, useMemo, useState } from 'react'
import { groupSignalsByRequest, useComposer, useOpenSignals, usePendingPermissions, HOOK_SOURCE_USER_INPUT, type HookResolveInput, type PendingHook } from '@kayushkin/chat-core'
import { SignalRequestList } from './SessionSignals'

type ResolveFn = (input: HookResolveInput) => Promise<void>

/** Everything the open session is waiting on a human for, in one place.
 *
 *  Two things reach it, and they are genuinely different:
 *
 *   - **Open signals** — the canonical record of a question or a notification.
 *     Rendered by chat-core's own card and answered through
 *     `POST /signals/{id}/answer`, which is the one door however the question
 *     was raised and whether or not the session is still running.
 *   - **Parked tool calls with no signal behind them** — a permission gate.
 *     Answered allow or deny, once. There is no record to answer here and
 *     nothing to say beyond yes or no, so it keeps its own card.
 *
 *  This used to be `PermissionBanner`, and it drew AskUserQuestion itself, from
 *  the live tool input. That made the SAME question renderable twice — once
 *  here and once as a signal card — so every surface that showed signals had to
 *  be told which request ids the banner had already taken (`excludeRequestIds`,
 *  now deleted). The duplication existed for one reason: `multiSelect` lived
 *  only in the tool input, so only this component could offer a pick-many
 *  question. It is on the signal record now (`allowMultipleOptions`), and with
 *  it the second renderer had nothing left that the first could not do.
 *
 *  Answering here is also what puts questions in front of a human at all when
 *  the session is open. A parked ask always showed up, because it was on the
 *  live hook stream; a DERIVED question and a question whose park has died
 *  never did — they exist only as records, and chat rendered no records in
 *  the chat pane. They appeared solely in the sidebar `?` dropdown, which is
 *  the surface for the sessions you are NOT looking at.
 *
 *  Renders nothing when there is nothing waiting.
 *
 *  Collapses to its one header line. It opens again by itself when something
 *  arrives that was not there when it was collapsed, so collapsing hides what you
 *  have seen and never what you have not. The host keys it by session, so a
 *  collapse does not follow you into another chat.
 *
 *  Takes a session id, NOT `string | null`, and the caller guards. `useOpenSignals`
 *  reads across EVERY session when given no id — that is how the sidebar builds its
 *  marker list — so a null threaded through here would fill the chat pane with other
 *  sessions' questions the moment no session was selected. A cross-session inbox is a
 *  real surface and a different one: each card there has to say which session it
 *  belongs to, and this banner's cards deliberately do not.
 *
 *  Deliberately WITHOUT the "always allow / always deny" buttons the bridge-ui
 *  banner carries: those write a priority-200 global rule into permission-store,
 *  and the live rule set is under an open safety review. A one-shot decision is
 *  the whole of what a parked call needs; standing rules stay a deliberate trip
 *  to the permission page. */
export default function AwaitingYouBanner({ sessionId }: { sessionId: string }) {
  const { signals, error, reload } = useOpenSignals(sessionId)
  const { pending, resolve } = usePendingPermissions(sessionId)
  // The composer under this banner is where a human adds words to the options
  // they picked, so the cards' Answer sends that text too and then empties it.
  const { draft, setDraft } = useComposer(sessionId)
  const composerText = useMemo(() => ({ text: draft, clear: () => setDraft('') }), [draft, setDraft])

  const requests = useMemo(() => groupSignalsByRequest(signals), [signals])

  // A parked request is "covered" when a signal row was minted from it, because
  // the signal cards below already render that row. Everything left over is a
  // gate.
  //
  // When the signals read FAILS, nothing is covered and every parked request —
  // questions included — falls through to a permission card. That is the right
  // way round: the raw card is uglier and says less, but the session stays
  // answerable. Hiding a parked call because a SEPARATE read failed would
  // freeze it with nothing on screen to say why.
  const uncoveredParks = useMemo(() => {
    const covered = new Set(signals.map(s => s.requestId).filter(id => id !== ''))
    return pending.filter(hook => !covered.has(hook.requestId))
  }, [signals, pending])

  // What was waiting when the reader collapsed the banner, by the same keys the
  // cards are keyed by; null while it is open.
  const [waitingWhenCollapsed, setWaitingWhenCollapsed] = useState<ReadonlySet<string> | null>(null)
  const waitingKeys = useMemo(
    () => [
      ...uncoveredParks.map(hook => hook.requestId),
      ...requests.map(request => request.requestId || request.signals[0]?.id || ''),
    ],
    [uncoveredParks, requests],
  )
  const collapsed =
    waitingWhenCollapsed !== null && waitingKeys.every(key => waitingWhenCollapsed.has(key))

  if (requests.length === 0 && uncoveredParks.length === 0 && error === null) return null

  return (
    <div
      className={`bc-pending-banner${collapsed ? ' bc-pending-banner-collapsed' : ''}`}
      role="region"
      aria-label="Waiting on you"
    >
      <button
        type="button"
        className="bc-pending-banner-toggle"
        aria-expanded={!collapsed}
        onClick={() => setWaitingWhenCollapsed(collapsed ? null : new Set(waitingKeys))}
      >
        <span className="signal-disclosure-caret" aria-hidden>
          {collapsed ? '▸' : '▾'}
        </span>
        Waiting on you · {waitingKeys.length}
      </button>
      {!collapsed && (
        <>
          {/* Said out loud rather than swallowed. The parked cards below are still
              rendered — see `uncoveredParks` — so this reports a degraded surface,
              not a dead one. */}
          {error !== null && (
            <p className="bc-pending-error">Couldn’t load this session’s questions: {error}</p>
          )}
          {/* Gates first: a parked tool call is frozen mid-turn right now, whereas a
              question record may have been raised by a session that has since
              stopped. */}
          {uncoveredParks.map(hook => (
            <PermissionCard key={hook.requestId} hook={hook} resolve={resolve} />
          ))}
          {/* Collapsed to the answers. The transcript directly above already carries the
              question — a signal card that repeats it under the transcript is the same words
              twice, and the suggested answers are the only part you cannot read further up.
              The disclosure on each card puts the question back when you want it.

              It also drops every card's own freeform box: the composer right below answers
              a derived question, and a tool question's options are editable. The reasoning,
              and why the composer's Send does NOT answer a tool question, is on
              `startCollapsedToAnswers`. A card's Answer does take the composer's text
              with it, though: picks and the words typed under them go out together. */}
          <SignalRequestList
            requests={requests}
            onResolved={reload}
            startCollapsedToAnswers
            composerText={composerText}
          />
        </>
      )}
    </div>
  )
}

/** A parked tool call answered once, allow or deny.
 *
 *  Reached by every permission gate, and by the one degraded case: a
 *  `user_input` park whose signal row was never written. Recording a signal is
 *  observational on the server — it must not block the park — so a failed write
 *  leaves a question with no record. It lands here rather than nowhere, because
 *  a parked call with no card freezes the session invisibly, and the raw input
 *  is at least the truth about what was asked. */
function PermissionCard({ hook, resolve }: { hook: PendingHook; resolve: ResolveFn }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const tool = hook.toolName || ''
  // Bash is the case where the raw JSON hides the one thing that matters. Every other
  // tool's input is shown as it arrived — this layer reshapes nothing.
  const command = tool === 'Bash' ? String(readField(hook.input, 'command') ?? '') : ''
  const preview =
    tool === 'Bash' ? command || '(empty command)' : JSON.stringify(hook.input ?? {}, null, 2)
  const isUnrecordedQuestion = hook.source === HOOK_SOURCE_USER_INPUT

  const decide = useCallback(
    async (behavior: 'allow' | 'deny') => {
      setBusy(true)
      setError(null)
      try {
        await resolve({ requestId: hook.requestId, behavior })
      } catch (e) {
        // resolve() puts the card back when the server refuses; say why, because the
        // tool call is still parked and the user has to answer it again.
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setBusy(false)
      }
    },
    [hook.requestId, resolve],
  )

  return (
    <div className="bc-pending-card">
      <div className="bc-pending-card-header">
        <strong className="bc-pending-tool">{tool || hook.event || 'tool call'}</strong>
        <span className="bc-pending-event">{hook.event}</span>
      </div>
      {isUnrecordedQuestion && (
        <p className="bc-pending-note">
          This question has no signal record, so it can only be allowed or denied here — not
          answered.
        </p>
      )}
      <pre className="bc-pending-input">{preview}</pre>
      <div className="bc-pending-actions">
        <button type="button" className="bc-pending-allow" disabled={busy} onClick={() => decide('allow')}>
          Allow once
        </button>
        <button type="button" className="bc-pending-deny" disabled={busy} onClick={() => decide('deny')}>
          Deny
        </button>
      </div>
      {error && <p className="bc-pending-error">{error}</p>}
    </div>
  )
}

/** Read one string-keyed field off a raw hook input without asserting its shape. */
function readField(input: unknown, key: string): unknown {
  if (!input || typeof input !== 'object') return undefined
  return (input as Record<string, unknown>)[key]
}
