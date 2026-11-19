import { useEffect, useRef, useState } from 'react'
import type { APISpendTotalEvent, LogRow } from '../../types'
import { formatCost } from '../../utils'

// CostBreakdown renders the session's running cost. When an
// EventAPISpendTotal (server-derived from per-call OTel telemetry) has
// landed, its cumulative TotalUSD is canonical and the chip opens a
// drill-down with ByModel + ByQuerySource breakdowns. When the session
// has no api_call telemetry yet (legacy claudecode runs, harnesses that
// don't emit it), the chip falls back to the per-turn EventResult.Cost
// sum the SessionHeader was computing before.
//
// "Fallback" here is presentation-layer pick-the-best-source, not data
// fabrication: each path reads a real signal the session actually
// produced. Nothing is invented to fill in missing data.
// CostAggregate is a pre-computed cost object a standalone consumer can feed
// directly instead of the raw LogRow stream. When supplied it takes
// precedence over the rows/fallback paths and drives a drill-down chip from
// the aggregate alone — dash's chat page uses this because it already holds an
// aggregated cost total per session and never materializes the row stream.
export interface CostAggregate {
  totalUsd: number
  byModel?: Record<string, number>
  bySource?: Record<string, number>
}

// SpendCeiling is the session's server-side spend cap and its spend against
// it — ManagedSession.max_budget_usd and .spend_usd, straight off the session
// row.
//
// Both numbers come from the session row on purpose, and not from the rows
// the rest of this component reads. bridge-server's halt gate compares
// exactly these two fields, so they are the only pair whose ratio predicts
// when the session actually stops. The drill-down's "API spend" is the
// best-available figure for display and can differ: it moves during a turn
// before the row is persisted, and on a harness that emits no api_call
// telemetry it falls back to per-turn cost, which the gate never sees.
// Showing that number against a ceiling would draw a bar that fills while
// nothing enforces it.
export interface SpendCeiling {
  spendUSD: number
  maxBudgetUSD: number
}

// ceilingTone grades spend against the ceiling for the chip's color.
// Matches the context-tokens thresholds SessionHeader already uses, so the
// two budget-ish readouts in the header change color on the same scale.
function ceilingTone(spendUSD: number, maxBudgetUSD: number): '' | 'warn' | 'crit' {
  if (maxBudgetUSD <= 0) return ''
  const pct = (spendUSD / maxBudgetUSD) * 100
  if (pct >= 90) return 'crit'
  if (pct >= 70) return 'warn'
  return ''
}

export interface CostBreakdownProps {
  /** Raw log rows to scan for the latest api_spend_total (the default path, used
   * by the host's live chat page). Optional so an `aggregate`-only consumer need
   * not supply it. */
  rows?: LogRow[]
  /** Per-turn EventResult.Cost sum used when no api_call telemetry exists.
   * Optional (defaults to 0) so an `aggregate`-only consumer need not supply
   * it. */
  fallbackTotalUSD?: number
  /** Tooltip carried over from SessionHeader (e.g. context-tokens info)
   * shown when the fallback figure is displayed. */
  fallbackTitle?: string
  /** Pre-aggregated cost object. When present, it wins over rows/fallback
   * and renders a drill-down chip from the aggregate directly. */
  aggregate?: CostAggregate
  /** The session's spend ceiling and its spend against it. Supply it only
   * for a session that has one (max_budget_usd > 0): with no ceiling the
   * chip renders exactly as it did before, and a server that predates the
   * ceiling cannot send one, so nothing appears. */
  ceiling?: SpendCeiling
  /** Lets the drill-down set the session's ceiling: raise it, lower it, or clear it
   * with 0. Resolves to null on success and to the server's refusal text otherwise.
   * Without it the panel only reads. */
  ceilingEditor?: CeilingEditorProps
}

export interface CeilingEditorProps {
  /** The gated spend the gate compares against the ceiling (the row's spend_usd). */
  spendUSD: number
  /** The ceiling now in force; 0 means none. */
  maxBudgetUSD: number
  setCeiling: (maxBudgetUSD: number) => Promise<string | null>
}

export function CostBreakdown({ rows, fallbackTotalUSD = 0, fallbackTitle, aggregate, ceiling, ceilingEditor }: CostBreakdownProps) {
  const apiSpend = latestApiSpend(rows ?? [])

  // With a ceiling, the chip reads "$3.00 / $10.00" and the ceiling's own
  // pair drives both the text and the color — see SpendCeiling for why it
  // is that pair and not the drill-down's figure.
  const tone = ceiling ? ceilingTone(ceiling.spendUSD, ceiling.maxBudgetUSD) : ''
  const chipClass = ceiling ? ` bc-cost-ceiling${tone ? ` bc-cost-ceiling-${tone}` : ''}` : ''
  const ceilingLabel = ceiling
    ? `${formatCost(ceiling.spendUSD)} / ${formatCost(ceiling.maxBudgetUSD)}`
    : ''
  const ceilingTitle = ceiling
    ? `${formatCost(ceiling.spendUSD)} spent of this session's ${formatCost(ceiling.maxBudgetUSD)} ceiling — bridge-server stops the session here`
    : ''

  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!open) return
    const onDocClick = (e: MouseEvent) => {
      if (!ref.current) return
      if (!ref.current.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocClick)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  // Pre-aggregated path: a consumer handed us a finished cost object, so
  // render the chip + drill-down straight from it without touching the
  // rows/fallback logic below.
  if (aggregate) {
    const byModel = mapEntriesSortedDesc(aggregate.byModel)
    const bySource = mapEntriesSortedDesc(aggregate.bySource)
    return (
      <div className="bc-cost-wrap" ref={ref}>
        <button
          type="button"
          className={`bc-cost bc-cost-clickable${chipClass}${open ? ' bc-cost-open' : ''}`}
          onClick={() => setOpen(o => !o)}
          title={ceiling ? `${ceilingTitle}\nclick for breakdown` : 'click for breakdown'}
          aria-expanded={open}
        >
          {ceiling ? ceilingLabel : formatCost(aggregate.totalUsd)}
          <span className="bc-cost-caret" aria-hidden>▾</span>
        </button>
        {open && (
          <div className="bc-cost-panel" role="dialog" aria-label="Cost breakdown">
            <CeilingPanelRows ceiling={ceiling} />
            {ceilingEditor && <CeilingEditor {...ceilingEditor} />}
            <div className="bc-cost-panel-row bc-cost-panel-total">
              <span className="bc-cost-panel-label">API spend</span>
              <span className="bc-cost-panel-value">{formatCost(aggregate.totalUsd)}</span>
            </div>
            {byModel.length > 0 && (
              <div className="bc-cost-panel-group">
                <div className="bc-cost-panel-group-label">By model</div>
                {byModel.map(([k, v]) => (
                  <div key={`m_${k}`} className="bc-cost-panel-row">
                    <span className="bc-cost-panel-label">{k}</span>
                    <span className="bc-cost-panel-value">{formatCost(v)}</span>
                  </div>
                ))}
              </div>
            )}
            {bySource.length > 0 && (
              <div className="bc-cost-panel-group">
                <div className="bc-cost-panel-group-label">By source</div>
                {bySource.map(([k, v]) => (
                  <div key={`s_${k}`} className="bc-cost-panel-row">
                    <span className="bc-cost-panel-label">{k}</span>
                    <span className="bc-cost-panel-value">{formatCost(v)}</span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    )
  }

  if (apiSpend && apiSpend.calls > 0) {
    const callsTitle = `${apiSpend.calls} API call${apiSpend.calls === 1 ? '' : 's'} · click for breakdown`
    return (
      <div className="bc-cost-wrap" ref={ref}>
        <button
          type="button"
          className={`bc-cost bc-cost-clickable${chipClass}${open ? ' bc-cost-open' : ''}`}
          onClick={() => setOpen(o => !o)}
          title={ceiling ? `${ceilingTitle}\n${callsTitle}` : callsTitle}
          aria-expanded={open}
        >
          {ceiling ? ceilingLabel : formatCost(apiSpend.total_usd)}
          <span className="bc-cost-caret" aria-hidden>▾</span>
        </button>
        {open && <CostDrilldownPanel spend={apiSpend} fallbackTotalUSD={fallbackTotalUSD} ceiling={ceiling} ceilingEditor={ceilingEditor} />}
      </div>
    )
  }

  if (ceiling) {
    // A ceiling with no api_call telemetry behind it. Worth showing — the
    // ceiling is a fact the user set, and the session's spend against it is
    // the number the server gates on whether or not any call has been
    // itemised yet.
    //
    // This branch renders the same clickable chip as the others rather than
    // a static one. It used to be a bare span, which made the drill-down
    // unreachable in exactly the state that reaches it: a session with a
    // ceiling and no itemised calls, i.e. every freshly-loaded one. A group
    // of rows nothing can open is a group of rows that does not exist.
    return (
      <div className="bc-cost-wrap" ref={ref}>
        <button
          type="button"
          className={`bc-cost bc-cost-clickable${chipClass}${open ? ' bc-cost-open' : ''}`}
          onClick={() => setOpen(o => !o)}
          title={`${ceilingTitle}\nclick for breakdown`}
          aria-expanded={open}
        >
          {ceilingLabel}
          <span className="bc-cost-caret" aria-hidden>▾</span>
        </button>
        {open && (
          <div className="bc-cost-panel" role="dialog" aria-label="Cost breakdown">
            <CeilingPanelRows ceiling={ceiling} />
            {ceilingEditor && <CeilingEditor {...ceilingEditor} />}
          </div>
        )}
      </div>
    )
  }

  if (fallbackTotalUSD > 0) {
    return (
      <span className="bc-cost" title={fallbackTitle}>
        {formatCost(fallbackTotalUSD)}
      </span>
    )
  }

  return null
}

// CeilingPanelRows adds the gated pair to a drill-down panel. Renders
// nothing without a ceiling, which is every session that has none.
//
// It is a separate row rather than a footnote on "API spend" because the
// two figures answer different questions and can hold different numbers:
// this one is what the server compares, that one is what the session has
// been billed for as best the client can tell.
function CeilingPanelRows({ ceiling }: { ceiling?: SpendCeiling }) {
  if (!ceiling) return null
  const remaining = ceiling.maxBudgetUSD - ceiling.spendUSD
  return (
    <div className="bc-cost-panel-group bc-cost-panel-ceiling">
      <div className="bc-cost-panel-group-label">Spend ceiling</div>
      <div className="bc-cost-panel-row">
        <span className="bc-cost-panel-label">gated spend</span>
        <span className="bc-cost-panel-value">{formatCost(ceiling.spendUSD)}</span>
      </div>
      <div className="bc-cost-panel-row">
        <span className="bc-cost-panel-label">ceiling</span>
        <span className="bc-cost-panel-value">{formatCost(ceiling.maxBudgetUSD)}</span>
      </div>
      <div className="bc-cost-panel-row">
        <span className="bc-cost-panel-label">{remaining > 0 ? 'remaining' : 'over by'}</span>
        <span className="bc-cost-panel-value">{formatCost(Math.abs(remaining))}</span>
      </div>
    </div>
  )
}

// CeilingEditor sets the session's spend ceiling from the drill-down, at any
// time — not only once the gate has halted the session, which is all the
// budget banner covers. The field starts on the ceiling in force, or empty
// when there is none. Empty or 0 clears it.
//
// A ceiling at or below the gated spend is allowed: it halts the session at
// once, and that is a fair thing to want. The hint says so before the click.
export function CeilingEditor({ spendUSD, maxBudgetUSD, setCeiling }: CeilingEditorProps) {
  // Cents, as every other figure here: a ceiling stored as 26.286004249999998
  // starts the field on 26.29.
  const current = maxBudgetUSD > 0 ? String(Math.round(maxBudgetUSD * 100) / 100) : ''
  const [draft, setDraft] = useState(current)
  const [seededFrom, setSeededFrom] = useState(current)
  const [saving, setSaving] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)

  // Follow the server when the ceiling moves under us (a save here, or the
  // banner), without wiping what the user is typing on an unrelated render.
  if (current !== seededFrom) {
    setSeededFrom(current)
    setDraft(current)
  }

  const trimmed = draft.trim()
  const next = trimmed === '' ? 0 : Number(trimmed)
  const valid = Number.isFinite(next) && next >= 0
  const unchanged = valid && trimmed === current
  const haltsAtOnce = valid && next > 0 && next <= spendUSD
  const canSave = valid && !unchanged && !saving

  const save = async () => {
    if (!canSave) return
    setSaving(true)
    setFailure(null)
    const err = await setCeiling(next)
    setSaving(false)
    if (err) setFailure(err)
  }

  return (
    <div className="bc-cost-panel-group bc-cost-ceiling-editor">
      <div className="bc-cost-panel-group-label">{maxBudgetUSD > 0 ? 'Change ceiling' : 'Set a ceiling'}</div>
      <div className="bc-cost-ceiling-editor-row">
        <span className="bc-cost-ceiling-editor-prefix" aria-hidden>$</span>
        <input
          className="bc-budget-banner-input"
          type="number"
          min="0"
          step="1"
          placeholder="no limit"
          value={draft}
          disabled={saving}
          onChange={e => setDraft(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') void save() }}
          aria-label="Spend ceiling in dollars; empty or 0 for no limit"
        />
        <button
          type="button"
          className="bc-budget-banner-raise"
          onClick={() => void save()}
          disabled={!canSave}
        >
          {saving ? 'Saving…' : next === 0 && maxBudgetUSD > 0 ? 'Remove' : 'Save'}
        </button>
      </div>
      {haltsAtOnce && (
        <div className="bc-budget-banner-hint">
          at or below the {formatCost(spendUSD)} already spent — the session stops at once
        </div>
      )}
      {failure && <div className="bc-budget-banner-failure">Could not set the ceiling: {failure}</div>}
    </div>
  )
}

function CostDrilldownPanel({ spend, fallbackTotalUSD, ceiling, ceilingEditor }: {
  spend: APISpendTotalEvent
  fallbackTotalUSD: number
  ceiling?: SpendCeiling
  ceilingEditor?: CeilingEditorProps
}) {
  const byModel = mapEntriesSortedDesc(spend.by_model)
  const bySource = mapEntriesSortedDesc(spend.by_query_source)

  // The delta exists when EventResult.Cost was recording a smaller number
  // than per-call OTel: that's the auxiliary-call overhead (session-title
  // generation, prompt-suggestion). Surface it explicitly rather than
  // forcing users to subtract.
  const overhead = fallbackTotalUSD > 0 ? spend.total_usd - fallbackTotalUSD : 0
  const showOverhead = fallbackTotalUSD > 0 && overhead > 0.000001

  return (
    <div className="bc-cost-panel" role="dialog" aria-label="Cost breakdown">
      <CeilingPanelRows ceiling={ceiling} />
      {ceilingEditor && <CeilingEditor {...ceilingEditor} />}
      <div className="bc-cost-panel-row bc-cost-panel-total">
        <span className="bc-cost-panel-label">API spend</span>
        <span className="bc-cost-panel-value">{formatCost(spend.total_usd)}</span>
      </div>
      <div className="bc-cost-panel-row bc-cost-panel-sub">
        <span className="bc-cost-panel-label">{spend.calls} call{spend.calls === 1 ? '' : 's'}</span>
        <span className="bc-cost-panel-value">
          {(spend.usage?.input_tokens ?? 0).toLocaleString()} in
          {' / '}
          {(spend.usage?.output_tokens ?? 0).toLocaleString()} out
        </span>
      </div>

      {showOverhead && (
        <div className="bc-cost-panel-row bc-cost-panel-overhead">
          <span className="bc-cost-panel-label">vs. turn cost</span>
          <span className="bc-cost-panel-value">+{formatCost(overhead)} aux</span>
        </div>
      )}

      {byModel.length > 0 && (
        <div className="bc-cost-panel-group">
          <div className="bc-cost-panel-group-label">By model</div>
          {byModel.map(([k, v]) => (
            <div key={`m_${k}`} className="bc-cost-panel-row">
              <span className="bc-cost-panel-label">{k}</span>
              <span className="bc-cost-panel-value">{formatCost(v)}</span>
            </div>
          ))}
        </div>
      )}

      {bySource.length > 0 && (
        <div className="bc-cost-panel-group">
          <div className="bc-cost-panel-group-label">By source</div>
          {bySource.map(([k, v]) => (
            <div key={`s_${k}`} className="bc-cost-panel-row">
              <span className="bc-cost-panel-label">{k}</span>
              <span className="bc-cost-panel-value">{formatCost(v)}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// latestApiSpend scans backwards for the most recent api_spend_total row.
// The derivation emits one after every EventAPICall, so the last one in
// the stream carries the cumulative state we want to display.
function latestApiSpend(rows: LogRow[]): APISpendTotalEvent | null {
  for (let i = rows.length - 1; i >= 0; i--) {
    const s = rows[i].apiSpendTotal
    if (s) return s
  }
  return null
}

function mapEntriesSortedDesc(m: Record<string, number> | undefined): Array<[string, number]> {
  if (!m) return []
  return Object.entries(m).sort((a, b) => b[1] - a[1])
}
