import type { OrchestratorConfig } from './OrchestratorRow'

/** What the spend-ceiling banner's box edits.
 *
 *  For most sessions it is the session's own ceiling on llm-bridge-server. The
 *  orchestrator's home session is different: the orchestrator writes that ceiling itself
 *  at the start of every turn, from its weekly limit, so a ceiling written to
 *  bridge-server lasts one turn. On 2026-10-20 a raise to $151 went back to the
 *  $86.96 already spent before the next send. For that session the box edits the
 *  weekly limit, which is the number that decides the ceiling.
 *
 *  `current` and `spent` are the pair the new figure is checked against: the
 *  figure must be above `spent`, or the session is still held. */
export type BudgetBannerTarget =
  | { kind: 'session-ceiling'; current: number | undefined; spent: number | undefined }
  | { kind: 'weekly-limit'; current: number; spent: number; resetsAt: string }

/** The orchestrator's home session is the one whose id the orchestrator names in
 *  `home_session_id`, matched by id and nothing else. */
export function budgetBannerTarget(
  sessionId: string | null,
  orchestratorConfig: OrchestratorConfig | null,
  session: { maxBudgetUSD: number | undefined; spendUSD: number | undefined },
): BudgetBannerTarget {
  if (sessionId !== null && orchestratorConfig !== null && orchestratorConfig.home_session_id === sessionId) {
    return {
      kind: 'weekly-limit',
      current: orchestratorConfig.week_limit_usd,
      spent: orchestratorConfig.week_spent_usd,
      resetsAt: orchestratorConfig.week_resets_at,
    }
  }
  return { kind: 'session-ceiling', current: session.maxBudgetUSD, spent: session.spendUSD }
}
