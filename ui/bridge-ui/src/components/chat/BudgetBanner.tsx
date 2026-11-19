import { useCallback, useEffect, useState } from 'react'
import { useBudgetHalt, useManagedSession } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import { formatCost } from '../../utils'
import { budgetBannerTarget } from './budgetBannerTarget'
import type { OrchestratorConfig } from './OrchestratorRow'

/** The surface for llm-bridge-server's spend halt: the session reached its ceiling, the
 *  server interrupted it, and every further send, resume and mode switch is refused with
 *  a 402 until the ceiling moves.
 *
 *  Before this, chat showed nothing at all. The mid-turn interrupt was one error event
 *  among many and the 402 never reached the page — `useComposer.send` discarded it — so a
 *  halted session looked like a hung one. That is the wrong conclusion twice over: the
 *  session is fine, and the one control that fixes it is a number in a box.
 *
 *  Renders nothing when there is no halt, which is every session under its ceiling, every
 *  session without one, and every server that predates the gate.
 *
 *  Reuses bridge-ui's `bc-budget-banner-*` classes, which dash already loads globally from
 *  `@kayushkin/bridge-ui/styles.css`, so the two chats look the same.
 *
 *  Only the 402 body names both dollar figures (`budgetHaltFromRefusal`); a halt raised by
 *  the mid-turn error event carries neither, and used to render the server's raw sentence
 *  because there was nowhere else to read them. There is now: the same
 *  `GET /sessions/{id}` detail the header reads holds the persisted pair the gate itself
 *  compares. It is NOT the summary wire — `spend_usd`/`max_budget_usd` stay off
 *  `chat_wire.go` on purpose, because a halt is a response and not a row, and widening
 *  the sidebar query for every session to describe the rare one is the trade this avoids.
 *  The detail read is per-session, lazy and already cached by the store, so the fallback
 *  costs nothing the header has not already paid.
 *
 *  The halt's own figures still win where it has them: they are the numbers as of the
 *  moment the request was refused, while the row is whatever has been persisted since.
 *
 *  On the orchestrator's home session the box edits the orchestrator's weekly limit instead of
 *  the session ceiling, because the orchestrator rewrites that ceiling from the limit at the
 *  start of every turn (`budgetBannerTarget`). The banner asks the orchestrator's
 *  `GET /config` which session is its home, only while a halt is showing. */
export default function BudgetBanner({ sessionId }: { sessionId: string | null }) {
  const { halt, raiseCeiling, recheck } = useBudgetHalt(sessionId)
  const { session: managed } = useManagedSession(sessionId)
  const { fetch: apiFetch, orchestratorBasePath } = useBridgeConfig()
  const halted = halt !== null
  const [orchestratorConfig, setOrchestratorConfig] = useState<OrchestratorConfig | null>(null)
  const [orchestratorError, setOrchestratorError] = useState<string | null>(null)
  const [orchestratorAnswered, setOrchestratorAnswered] = useState(!orchestratorBasePath)

  const loadOrchestratorConfig = useCallback(async () => {
    try {
      const response = await apiFetch(`${orchestratorBasePath}/config`)
      if (!response.ok) throw new Error(`orchestrator /config: HTTP ${response.status}`)
      setOrchestratorConfig((await response.json()) as OrchestratorConfig)
      setOrchestratorError(null)
    } catch (e) {
      setOrchestratorError(e instanceof Error ? e.message : String(e))
    } finally {
      setOrchestratorAnswered(true)
    }
  }, [apiFetch, orchestratorBasePath])

  useEffect(() => {
    if (halted && orchestratorBasePath) void loadOrchestratorConfig()
  }, [halted, orchestratorBasePath, loadOrchestratorConfig])

  // A ceiling of ZERO means no ceiling, so it is not a figure to fall back to — a banner
  // reading "$3.00 of its $0.00 ceiling" describes a session that cannot exist.
  const rowCeiling = managed?.maxBudgetUsd
  const spendUSD = halt?.spendUSD ?? managed?.spendUsd
  const maxBudgetUSD = halt?.maxBudgetUSD ?? (rowCeiling !== undefined && rowCeiling > 0 ? rowCeiling : undefined)
  const target = budgetBannerTarget(sessionId, orchestratorConfig, { maxBudgetUSD, spendUSD })

  // Seed the input from the figure that was breached, so the user edits a real number
  // rather than an empty box.
  const seed = target.current !== undefined && target.current > 0 ? String(target.current) : ''
  // A halt is identified by its session AND its figures: raising the ceiling and hitting
  // the new one later is a different halt on the same session, and it has to re-seed.
  // The target is part of it too, since the orchestrator's answer can arrive after the halt.
  const haltKey = halt ? `${halt.sessionId}:${maxBudgetUSD ?? ''}:${spendUSD ?? ''}:${target.kind}:${target.current ?? ''}` : null

  const [nextCeiling, setNextCeiling] = useState(seed)
  const [submitting, setSubmitting] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)
  const [seededFor, setSeededFor] = useState(haltKey)

  // Re-seed during render rather than in an effect. An effect would paint an empty box
  // first and fill it a frame later, and it would also fire on every unrelated re-render,
  // wiping whatever the user had typed. Keying on the halt means the input is only ever
  // rewritten when the halt itself is a different one.
  if (haltKey !== null && haltKey !== seededFor) {
    setSeededFor(haltKey)
    setNextCeiling(seed)
    setFailure(null)
  }

  if (!halt) return null

  const parsed = Number(nextCeiling)
  const valid = nextCeiling.trim() !== '' && Number.isFinite(parsed) && parsed > 0
  // A figure at or below what was already spent puts the session straight back over the
  // line, so the very next send is refused again. Catching it here costs one round trip
  // less than letting the server prove it.
  const spent = target.spent
  const tooLow = valid && spent !== undefined && parsed <= spent
  // Until the orchestrator answers, the banner cannot know which number the box edits.
  const canSubmit = valid && !tooLow && !submitting && orchestratorAnswered

  const raiseWeeklyLimit = async (weekLimitUSD: number): Promise<string | null> => {
    try {
      const response = await apiFetch(`${orchestratorBasePath}/config`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ week_limit_usd: weekLimitUSD }),
      })
      const body = await response.json().catch(() => null)
      // A 502 means the limit was saved but the orchestrator could not move the ceiling;
      // its body says so and carries the saved config.
      if (body?.config) setOrchestratorConfig(body.config as OrchestratorConfig)
      else if (response.ok && body) setOrchestratorConfig(body as OrchestratorConfig)
      if (!response.ok) return body?.error ?? `orchestrator /config: HTTP ${response.status}`
    } catch (e) {
      return e instanceof Error ? e.message : String(e)
    }
    return recheck()
  }

  const submit = async () => {
    if (!canSubmit) return
    setSubmitting(true)
    setFailure(null)
    const err = target.kind === 'weekly-limit' ? await raiseWeeklyLimit(parsed) : await raiseCeiling(parsed)
    setSubmitting(false)
    if (err) setFailure(err)
  }

  return (
    <div className="bc-budget-banner" role="region" aria-label="Spend ceiling reached">
      <div className="bc-budget-banner-head">
        <span className="bc-budget-banner-icon" aria-hidden>⛔</span>
        <span className="bc-budget-banner-title">Stopped at its spend ceiling</span>
      </div>
      <p className="bc-budget-banner-body">
        {target.kind === 'weekly-limit' ? (
          <>
            The orchestrator has spent <strong>{formatCost(target.spent)}</strong> this week
            against the orchestrator's <strong>{formatCost(target.current)}</strong> weekly limit, so
            the orchestrator holds this session at what it has spent. Raise the weekly limit to let it
            continue; the week resets {new Date(target.resetsAt).toLocaleDateString()}.
          </>
        ) : spendUSD !== undefined && maxBudgetUSD !== undefined && maxBudgetUSD > 0 ? (
          <>
            This session has spent <strong>{formatCost(spendUSD)}</strong> of its{' '}
            <strong>{formatCost(maxBudgetUSD)}</strong> ceiling. It will not send, resume or
            switch mode until the ceiling is raised above what it has already spent.
          </>
        ) : (
          // No numbers came with the halt — say what the server said rather than invent a
          // figure.
          halt.message
        )}
      </p>
      <div className="bc-budget-banner-actions">
        <label className="bc-budget-banner-field">
          <span className="bc-budget-banner-field-label">{target.kind === 'weekly-limit' ? 'Weekly limit ($)' : 'New ceiling ($)'}</span>
          <input
            className="bc-budget-banner-input"
            type="number"
            min="0"
            step="1"
            value={nextCeiling}
            disabled={submitting}
            onChange={e => setNextCeiling(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void submit() }}
            aria-label={target.kind === 'weekly-limit' ? 'New weekly limit in dollars' : 'New spend ceiling in dollars'}
          />
        </label>
        <button
          type="button"
          className="bc-budget-banner-raise"
          onClick={() => void submit()}
          disabled={!canSubmit}
          title={tooLow && spent !== undefined
            ? `Must be above the ${formatCost(spent)} already spent`
            : target.kind === 'weekly-limit'
              ? 'Raise the orchestrator’s weekly limit and let the orchestrator continue'
              : 'Raise this session’s ceiling and let it continue'}
        >
          {submitting ? 'Raising…' : target.kind === 'weekly-limit' ? 'Raise weekly limit' : 'Raise ceiling'}
        </button>
        {tooLow && spent !== undefined && (
          <span className="bc-budget-banner-hint">
            must exceed the {formatCost(spent)} already spent{target.kind === 'weekly-limit' ? ' this week' : ''}
          </span>
        )}
      </div>
      {orchestratorError && (
        <div className="bc-budget-banner-failure">
          Could not ask the orchestrator whether it sets this session’s ceiling, so the box edits the
          session ceiling, which the orchestrator overwrites on its home session: {orchestratorError}
        </div>
      )}
      {failure && (
        <div className="bc-budget-banner-failure">
          Could not raise the {target.kind === 'weekly-limit' ? 'weekly limit' : 'ceiling'}: {failure}
        </div>
      )}
    </div>
  )
}
