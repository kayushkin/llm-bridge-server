import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useBridgeConfig } from '../context'
import { useBridgeInstances } from '../useBridgeInstances'
import type { SessionAggregate } from '@kayushkin/llm-bridge-types'
import { formatTokens, formatDuration } from '../utils'
import { listSessionSummariesUpdatedSince, type SessionSummaryRow } from '../sessionSummaryPages'
import {
  addTokenUsage,
  allTokens,
  allTokensOfUsage,
  emptyTokenUsageTotals,
  modelCallsText,
  sessionAggregatesSincePath,
  sessionUsageInWindow,
  sumOfTokenCounts,
  tokenCountsText,
  tokenUsageRecordNote,
  usagePeriodStart,
  type SessionTokenUsage,
  type TokenCounts,
  type TokenUsageTotals,
  type UsagePeriod,
} from '../sessionTokenUsage'

// One session's row on this page: what it did inside the chosen window, from
// its row of GET /sessions/aggregates?since= (msg.SessionAggregate). `tokens`
// is read by sessionTokenUsageOf: the main conversation and its subagents
// apart, each in the four kinds a model call is counted in. `cost` is how
// much the session's cost estimate grew inside the window.
interface SessionUsage {
  sessionId: string
  harness: string
  instanceId: string
  tokens: SessionTokenUsage
  cost: number
  durationMs: number
  model: string
  turns: number
}

/** One token kind's total on a card: the whole number, and under it how much
 *  of it subagents used, when they used any. */
function TokenKindCard({ label, kind, totals }: { label: string; kind: keyof TokenCounts; totals: TokenUsageTotals }) {
  const subagents = totals.subagents[kind]
  return (
    <div className="bu-total-card">
      <span className="bu-total-label">{label}</span>
      <span className="bu-total-value">{formatTokens(totals.mainConversation[kind] + subagents)}</span>
      {subagents > 0 && <span className="bu-total-part">subagents {formatTokens(subagents)}</span>}
    </div>
  )
}

/** A bar split by token kind. Each segment's title names its kind and count,
 *  because cache reads are most of a long session and leave the other three
 *  too thin to tell apart by eye. */
function TokenKindBar({ counts }: { counts: TokenCounts }) {
  const all = allTokens(counts) || 1
  const segments: Array<[keyof TokenCounts, string, string]> = [
    ['input', 'bu-token-bar-in', 'input'],
    ['output', 'bu-token-bar-out', 'output'],
    ['cacheRead', 'bu-token-bar-cache-read', 'cache read'],
    ['cacheWrite', 'bu-token-bar-cache-write', 'cache write'],
  ]
  return (
    <div className="bu-token-bar">
      {segments.map(([kind, className, name]) => (
        <div
          key={kind}
          className={className}
          style={{ width: `${(counts[kind] / all) * 100}%` }}
          title={`${formatTokens(counts[kind])} ${name}`}
        />
      ))}
    </div>
  )
}

interface LimitWindow {
  used_percent: number
  window_minutes?: number
  resets_at?: number // unix seconds
}

interface ProviderLimits {
  provider: string
  plan_type?: string
  tier?: string
  windows: Record<string, LimitWindow>
  snapshot_at: number
  source: string
  stale_after?: number
}

type LimitsResponse = Record<string, ProviderLimits | null>

interface KeyRow {
  api_key_id: string
  api_key_name: string
  api_key_hint: string
  api_key_status: string
  total_usd_24h: number
  total_usd_7d: number
  total_usd_30d: number
  fetched_at: number
}

interface Topup {
  id: number
  provider: string
  amount_usd: number
  occurred_at: number
  note: string
  created_at: number
}

interface ProviderAccount {
  configured: boolean
  admin_key_hint: string
  total_usd_24h: number
  total_usd_7d: number
  total_usd_30d: number
  topups: Topup[]
  topups_total_usd: number
  spend_since_baseline: number
  remaining_usd: number | null
  balance_since: number | null
  keys: KeyRow[]
}

interface SpendKeysResponse {
  anthropic: ProviderAccount
  openai: ProviderAccount
}

type Period = UsagePeriod

// The harness heading for a session log-store counted that the bridge's
// session list does not hold. Its usage is shown under this heading, not left
// out, so the totals stay whole.
const SESSIONS_NOT_IN_THE_LIST = 'not in the session list'

const PERIOD_WORDS: Record<Period, string> = {
  day: 'the last 24 hours',
  week: 'the last 7 days',
  month: 'the last 30 days',
}

function sourceLabel(source: string): string {
  return source || 'Interactive'
}

// Distinct, color-blind-friendly palette used by both pie charts and the
// per-source/harness chips. Indexed by hash-like position so colors stay
// stable across renders for a given key.
const PIE_COLORS = [
  '#60a5fa', '#f59e0b', '#a78bfa', '#34d399', '#f472b6',
  '#fbbf24', '#22d3ee', '#fb7185', '#4ade80', '#c084fc',
  '#fca5a5', '#93c5fd',
]

function colorFor(index: number): string {
  return PIE_COLORS[index % PIE_COLORS.length]
}

interface PieSlice {
  key: string
  label: string
  value: number
  color: string
}

function PieChart({ title, slices, valueFmt }: { title: string; slices: PieSlice[]; valueFmt: (v: number) => string }) {
  const total = slices.reduce((s, x) => s + x.value, 0)
  if (total <= 0) return null
  const r = 60
  const cx = 70
  const cy = 70
  let acc = 0
  const paths = slices.map(s => {
    const start = (acc / total) * Math.PI * 2 - Math.PI / 2
    acc += s.value
    const end = (acc / total) * Math.PI * 2 - Math.PI / 2
    const x1 = cx + r * Math.cos(start)
    const y1 = cy + r * Math.sin(start)
    const x2 = cx + r * Math.cos(end)
    const y2 = cy + r * Math.sin(end)
    const large = end - start > Math.PI ? 1 : 0
    // Single-slice (whole pie) — render as a full circle so the path doesn't
    // collapse to a zero-length arc.
    if (slices.length === 1) {
      return { key: s.key, color: s.color, d: `M ${cx - r} ${cy} a ${r} ${r} 0 1 0 ${r * 2} 0 a ${r} ${r} 0 1 0 ${-r * 2} 0` }
    }
    return { key: s.key, color: s.color, d: `M ${cx} ${cy} L ${x1} ${y1} A ${r} ${r} 0 ${large} 1 ${x2} ${y2} Z` }
  })
  return (
    <div className="bu-pie">
      <div className="bu-pie-title">{title}</div>
      <div className="bu-pie-body">
        <svg width={140} height={140} viewBox="0 0 140 140">
          {paths.map(p => (
            <path key={p.key} d={p.d} fill={p.color} stroke="var(--bg-surface)" strokeWidth={1} />
          ))}
        </svg>
        <div className="bu-pie-legend">
          {slices.map(s => {
            const pct = (s.value / total) * 100
            return (
              <div key={s.key} className="bu-pie-legend-row">
                <span className="bu-pie-swatch" style={{ background: s.color }} />
                <span className="bu-pie-legend-label">{s.label}</span>
                <span className="bu-pie-legend-value">{valueFmt(s.value)}</span>
                <span className="bu-pie-legend-pct">{pct.toFixed(0)}%</span>
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

const WINDOW_LABELS: Record<string, string> = {
  five_hour: '5-Hour',
  seven_day: '7-Day',
  seven_day_oauth_apps: '7-Day OAuth Apps',
  seven_day_opus: '7-Day Opus',
  seven_day_sonnet: '7-Day Sonnet',
  seven_day_cowork: '7-Day Cowork',
  weekly: 'Weekly',
  extra_usage: 'Extra Usage',
}

const PROVIDER_LABELS: Record<string, string> = {
  anthropic: 'Claude',
  codex: 'Codex',
}

const SPEND_PROVIDER_LABELS: Record<keyof SpendKeysResponse, string> = {
  anthropic: 'Anthropic API',
  openai: 'OpenAI / Codex API',
}

function formatSpan(diffMs: number): string {
  const mins = Math.floor(diffMs / 60000)
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  const remMins = mins % 60
  if (hrs < 24) return remMins > 0 ? `${hrs}h ${remMins}m` : `${hrs}h`
  const days = Math.floor(hrs / 24)
  const remHrs = hrs % 24
  return remHrs > 0 ? `${days}d ${remHrs}h` : `${days}d`
}

function LimitBar({ label, win }: { label: string; win: LimitWindow }) {
  const pct = Math.min(win.used_percent, 100)
  const untilResetMs = win.resets_at != null ? win.resets_at * 1000 - Date.now() : null
  // The snapshot predates the window's reset, so its percentage describes a window
  // that is over. Nothing tells us the usage of the current one; say so.
  const windowEnded = untilResetMs != null && untilResetMs <= 0
  const color = windowEnded ? '#9e9e9e' : pct >= 80 ? '#ef5350' : pct >= 50 ? '#ffa726' : '#66bb6a'
  return (
    <div className="bu-limit-item">
      <div className="bu-limit-header">
        <span className="bu-limit-label">{label}</span>
        <span className="bu-limit-pct" style={{ color }}>{windowEnded ? '?' : `${pct.toFixed(0)}%`}</span>
      </div>
      <div className="bu-limit-track">
        <div className="bu-limit-fill" style={{ width: windowEnded ? '0%' : `${pct}%`, background: color }} />
      </div>
      {untilResetMs != null && (
        <span className="bu-limit-reset">
          {windowEnded
            ? `reset ${formatSpan(-untilResetMs)} ago (was ${pct.toFixed(0)}%); no snapshot since`
            : `resets in ${formatSpan(untilResetMs)}`}
        </span>
      )}
    </div>
  )
}


export function BridgeUsage() {
  const { fetch: apiFetch, basePath, usageBasePath } = useBridgeConfig()
  const [sessions, setSessions] = useState<SessionSummaryRow[]>([])
  const [aggregates, setAggregates] = useState<SessionAggregate[]>([])
  const [limits, setLimits] = useState<LimitsResponse | null>(null)
  const [spend, setSpend] = useState<SpendKeysResponse | null>(null)
  const [expandedRaw, setExpandedRaw] = useState<Record<string, string | 'loading' | 'error' | undefined>>({})
  const [expandedKeys, setExpandedKeys] = useState<Record<string, boolean>>({})
  const [addCreditOpen, setAddCreditOpen] = useState<string | null>(null)
  const [addCreditDraft, setAddCreditDraft] = useState({ amount: '', date: '', note: '' })
  const [loading, setLoading] = useState(true)
  const [loadingUsage, setLoadingUsage] = useState(false)
  const [period, setPeriod] = useState<Period>('day')
  const inst = useBridgeInstances()

  // The session list says which harness, instance and source each session is.
  // The longest period shown is a month, and a session that used anything in
  // a window was updated in it, so the sessions updated in the last month are
  // enough. This used to fetch GET /sessions — the whole session table with
  // info blobs, 64 MB — to keep that month.
  const fetchSessions = useCallback(async () => {
    try {
      const since = usagePeriodStart('month', new Date())
      setSessions(await listSessionSummariesUpdatedSince(apiFetch, basePath, since))
    } catch (err) {
      console.error('usage: loading sessions failed', err)
    }
    finally { setLoading(false) }
  }, [apiFetch, basePath])

  // What every session did inside the chosen window, in one request to
  // log-store (proxied by the bridge): tokens, turns, duration and how much
  // each session's cost grew. A session started before the window is in it
  // when it worked inside it, with only that work.
  //
  // A tab pressed while another tab's answer is on its way must not be
  // overwritten by that answer when it lands; `newestAggregatesRequest` lets
  // only the newest request set the rows.
  const newestAggregatesRequest = useRef(0)
  const [usageError, setUsageError] = useState<string | null>(null)
  const fetchAggregates = useCallback(async () => {
    const request = ++newestAggregatesRequest.current
    setLoadingUsage(true)
    try {
      const res = await apiFetch(sessionAggregatesSincePath(basePath, usagePeriodStart(period, new Date())))
      if (request !== newestAggregatesRequest.current) return
      if (!res.ok) {
        setUsageError(`Reading usage failed: the server answered ${res.status}.`)
        return
      }
      const rows: SessionAggregate[] = await res.json() ?? []
      if (request !== newestAggregatesRequest.current) return
      setAggregates(rows)
      setUsageError(null)
    } catch (err) {
      if (request !== newestAggregatesRequest.current) return
      setUsageError(`Reading usage failed: ${err instanceof Error ? err.message : String(err)}`)
    }
    finally {
      if (request === newestAggregatesRequest.current) setLoadingUsage(false)
    }
  }, [apiFetch, basePath, period])

  // Subscription limits live on usage-store. Skipped entirely when no
  // usageBasePath is configured.
  const fetchLimits = useCallback(async () => {
    if (!usageBasePath) return
    try {
      const res = await apiFetch(`${usageBasePath}/limits`)
      if (res.ok) setLimits(await res.json())
    } catch { /* ignore */ }
  }, [apiFetch, usageBasePath])

  // Per-API-key spend (computed from usage_report token counts × per-model
  // pricing). Refreshed hourly on the server; the UI just reads the latest.
  const fetchSpend = useCallback(async () => {
    if (!usageBasePath) return
    try {
      const res = await apiFetch(`${usageBasePath}/spend/keys`)
      if (res.ok) setSpend(await res.json())
    } catch { /* ignore */ }
  }, [apiFetch, usageBasePath])

  const submitTopup = useCallback(async (provider: string) => {
    if (!usageBasePath) return
    const amount = parseFloat(addCreditDraft.amount)
    if (!isFinite(amount) || amount <= 0) return
    const body: Record<string, unknown> = { provider, amount_usd: amount, note: addCreditDraft.note }
    if (addCreditDraft.date) body.occurred_at_str = addCreditDraft.date
    try {
      const res = await apiFetch(`${usageBasePath}/spend/topups`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (res.ok) {
        setAddCreditOpen(null)
        setAddCreditDraft({ amount: '', date: '', note: '' })
        fetchSpend()
      }
    } catch { /* ignore */ }
  }, [apiFetch, addCreditDraft, fetchSpend, usageBasePath])

  const deleteTopup = useCallback(async (id: number) => {
    if (!usageBasePath) return
    if (!confirm('Delete this top-up?')) return
    try {
      const res = await apiFetch(`${usageBasePath}/spend/topups/${id}`, { method: 'DELETE' })
      if (res.ok) fetchSpend()
    } catch { /* ignore */ }
  }, [apiFetch, fetchSpend, usageBasePath])

  const toggleRaw = useCallback(async (provider: string, apiKeyID: string) => {
    if (!usageBasePath) return
    const k = `${provider}:${apiKeyID}`
    setExpandedRaw(prev => {
      if (prev[k] !== undefined) {
        const next = { ...prev }
        delete next[k]
        return next
      }
      return { ...prev, [k]: 'loading' }
    })
    if (expandedRaw[k] !== undefined) return
    try {
      const res = await apiFetch(`${usageBasePath}/spend/keys/${provider}/${apiKeyID}/raw`)
      const text = res.ok ? await res.text() : `error ${res.status}: ${await res.text()}`
      setExpandedRaw(prev => ({ ...prev, [k]: text }))
    } catch (e) {
      setExpandedRaw(prev => ({ ...prev, [k]: `error: ${e}` }))
    }
  }, [apiFetch, expandedRaw, usageBasePath])

  useEffect(() => { fetchSessions() }, [fetchSessions])
  useEffect(() => { fetchAggregates() }, [fetchAggregates])
  // usage-store reads limits rarely unless told this page is being looked at.
  // While the tab is visible, say so once a minute; usage-store then reads every
  // couple of minutes. A hidden tab sends nothing, so polling slows down again.
  useEffect(() => {
    if (!usageBasePath) return
    const markWatched = async () => {
      try {
        const res = await apiFetch(`${usageBasePath}/limits/watch`, { method: 'POST' })
        if (!res.ok) console.error(`usage-store limits/watch answered ${res.status}`)
      } catch (err) {
        console.error('usage-store limits/watch failed', err)
      }
    }
    const tick = () => {
      if (document.visibilityState !== 'visible') return
      markWatched()
      fetchLimits()
    }
    let refetchAfterWake: ReturnType<typeof setTimeout> | undefined
    const onVisibilityChange = () => {
      if (document.visibilityState !== 'visible') return
      tick()
      // The mark triggers a fresh read on the server; pick it up shortly after.
      clearTimeout(refetchAfterWake)
      refetchAfterWake = setTimeout(fetchLimits, 5000)
    }
    onVisibilityChange()
    const interval = setInterval(tick, 60000)
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      clearInterval(interval)
      clearTimeout(refetchAfterWake)
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [apiFetch, fetchLimits, usageBasePath])
  useEffect(() => {
    if (!usageBasePath) return
    fetchSpend()
    const t = setInterval(fetchSpend, 5 * 60_000)
    return () => clearInterval(t)
  }, [fetchSpend, usageBasePath])

  type Totals = { tokens: TokenUsageTotals; cost: number; duration: number; turns: number }
  type SourceGroup = { source: string; sessions: SessionUsage[]; totals: Totals }
  type HarnessGroup = { harness: string; sources: Map<string, SourceGroup>; totals: Totals; sessionCount: number }

  const emptyTotals = (): Totals => ({ tokens: emptyTokenUsageTotals(), cost: 0, duration: 0, turns: 0 })
  const addTotals = (t: Totals, u: SessionUsage) => {
    addTokenUsage(t.tokens, u.tokens)
    t.cost += u.cost; t.duration += u.durationMs; t.turns += u.turns
  }

  const harnessGroups = useMemo(() => {
    const groups = new Map<string, HarnessGroup>()
    for (const u of sessionUsageInWindow(aggregates, sessions)) {
      const usage: SessionUsage = {
        sessionId: u.sessionId,
        harness: u.placement?.harness ?? SESSIONS_NOT_IN_THE_LIST,
        instanceId: u.placement?.instanceId ?? '',
        tokens: u.tokens,
        cost: u.costUsd,
        durationMs: u.durationMs,
        model: u.model,
        turns: u.turns,
      }
      let h = groups.get(usage.harness)
      if (!h) { h = { harness: usage.harness, sources: new Map(), totals: emptyTotals(), sessionCount: 0 }; groups.set(usage.harness, h) }
      const srcKey = u.placement?.purpose ?? ''
      let src = h.sources.get(srcKey)
      if (!src) { src = { source: srcKey, sessions: [], totals: emptyTotals() }; h.sources.set(srcKey, src) }
      src.sessions.push(usage)
      addTotals(src.totals, usage)
      addTotals(h.totals, usage)
      h.sessionCount++
    }
    return groups
  }, [sessions, aggregates])

  const totals = useMemo(() => {
    const tokens = emptyTokenUsageTotals()
    let cost = 0, duration = 0, count = 0
    for (const [, g] of harnessGroups) {
      addTokenUsage(tokens, g.totals.tokens); cost += g.totals.cost
      duration += g.totals.duration; count += g.sessionCount
    }
    return { tokens, cost, duration, count }
  }, [harnessGroups])

  const harnessSlices = useMemo<PieSlice[]>(() => {
    const arr = Array.from(harnessGroups.values())
      .map((h, i) => ({
        key: h.harness,
        label: h.harness,
        value: allTokensOfUsage(h.totals.tokens),
        color: colorFor(i),
      }))
      .filter(s => s.value > 0)
      .sort((a, b) => b.value - a.value)
    return arr
  }, [harnessGroups])

  const sourceSlices = useMemo<PieSlice[]>(() => {
    const sums = new Map<string, number>()
    for (const [, h] of harnessGroups) {
      for (const [, src] of h.sources) {
        const key = src.source
        sums.set(key, (sums.get(key) ?? 0) + allTokensOfUsage(src.totals.tokens))
      }
    }
    return Array.from(sums.entries())
      .map(([key, value], i) => ({
        key: key || '__interactive',
        label: sourceLabel(key),
        value,
        color: colorFor(i),
      }))
      .filter(s => s.value > 0)
      .sort((a, b) => b.value - a.value)
  }, [harnessGroups])

  const [expandedSources, setExpandedSources] = useState<Record<string, boolean>>({})
  const toggleSource = useCallback((harness: string, source: string) => {
    const k = `${harness}::${source}`
    setExpandedSources(prev => ({ ...prev, [k]: !prev[k] }))
  }, [])

  return (
    <div className="bu-container">
      <div className="bu-header">
        <h2>Bridge Usage</h2>
        <div className="bu-period-tabs">
          {(['day', 'week', 'month'] as Period[]).map(p => (
            <button key={p} className={`bu-tab ${period === p ? 'bu-tab-active' : ''}`} onClick={() => setPeriod(p)}>
              {p === 'day' ? '24h' : p === 'week' ? '7d' : '30d'}
            </button>
          ))}
        </div>
      </div>
      <p className="bu-period-note">
        Tokens and cost used in {PERIOD_WORDS[period]}, by every session that worked in it, counted in whole clock hours.
      </p>

      {spend && (
        <div className="bu-spend-section">
          {(['anthropic', 'openai'] as Array<keyof SpendKeysResponse>).map(provider => {
            const p = spend[provider]
            const keysOpen = expandedKeys[provider] ?? false
            const addOpen = addCreditOpen === provider
            return (
              <div key={provider} className="bu-spend-account">
                <div className="bu-spend-account-row">
                  <div className="bu-spend-account-id">
                    <div className="bu-spend-account-name">{SPEND_PROVIDER_LABELS[provider]}</div>
                    <div className="bu-spend-account-hint">
                      {p.configured ? (
                        <code>{p.admin_key_hint || 'pending first refresh…'}</code>
                      ) : (
                        <span className="bu-spend-unconfigured">admin key not configured</span>
                      )}
                    </div>
                  </div>
                  <div className="bu-spend-account-totals">
                    <div className="bu-spend-cell">
                      <span className="bu-spend-window">24h</span>
                      <span className="bu-spend-amount">${p.total_usd_24h.toFixed(2)}</span>
                    </div>
                    <div className="bu-spend-cell">
                      <span className="bu-spend-window">7d</span>
                      <span className="bu-spend-amount">${p.total_usd_7d.toFixed(2)}</span>
                    </div>
                    <div className="bu-spend-cell">
                      <span className="bu-spend-window">30d</span>
                      <span className="bu-spend-amount">${p.total_usd_30d.toFixed(2)}</span>
                    </div>
                    <div className="bu-spend-cell bu-spend-balance">
                      <span className="bu-spend-window">Balance</span>
                      <span className="bu-spend-amount">
                        {p.remaining_usd != null ? `$${p.remaining_usd.toFixed(2)}` : '—'}
                      </span>
                      {p.remaining_usd != null && p.balance_since && (
                        <span className="bu-spend-fetched">
                          ${p.topups_total_usd.toFixed(2)} added · ${p.spend_since_baseline.toFixed(2)} spent
                        </span>
                      )}
                    </div>
                  </div>
                  {p.configured && (
                    <button className="bu-spend-add-btn" onClick={() => setAddCreditOpen(addOpen ? null : provider)}>
                      {addOpen ? '×' : '+ Add credit'}
                    </button>
                  )}
                </div>

                {addOpen && (
                  <div className="bu-spend-addform">
                    <input
                      type="number" step="0.01" placeholder="Amount (USD)" autoFocus
                      value={addCreditDraft.amount}
                      onChange={e => setAddCreditDraft(d => ({ ...d, amount: e.target.value }))}
                    />
                    <input
                      type="date" placeholder="Date (UTC)"
                      value={addCreditDraft.date}
                      onChange={e => setAddCreditDraft(d => ({ ...d, date: e.target.value }))}
                    />
                    <input
                      type="text" placeholder="Note (optional)"
                      value={addCreditDraft.note}
                      onChange={e => setAddCreditDraft(d => ({ ...d, note: e.target.value }))}
                    />
                    <button onClick={() => submitTopup(provider)}>Save</button>
                  </div>
                )}

                {p.topups.length > 0 && (
                  <div className="bu-spend-topups">
                    {p.topups.map(t => (
                      <div key={t.id} className="bu-spend-topup">
                        <span className="bu-spend-topup-amount">+${t.amount_usd.toFixed(2)}</span>
                        <span className="bu-spend-topup-date">{new Date(t.occurred_at * 1000).toLocaleDateString()}</span>
                        {t.note && <span className="bu-spend-topup-note">{t.note}</span>}
                        <button className="bu-spend-topup-del" onClick={() => deleteTopup(t.id)} title="Delete">×</button>
                      </div>
                    ))}
                  </div>
                )}

                {p.keys.length > 0 && (
                  <div className="bu-spend-keys">
                    <button
                      className="bu-spend-keys-toggle"
                      onClick={() => setExpandedKeys(prev => ({ ...prev, [provider]: !keysOpen }))}
                    >
                      {keysOpen ? '▼' : '▶'} {p.keys.length} API key{p.keys.length === 1 ? '' : 's'}
                    </button>
                    {keysOpen && (
                      <table className="bu-spend-table">
                        <thead>
                          <tr>
                            <th>Key</th>
                            <th className="bu-spend-num">24h</th>
                            <th className="bu-spend-num">7d</th>
                            <th className="bu-spend-num">30d</th>
                            <th></th>
                          </tr>
                        </thead>
                        <tbody>
                          {p.keys.map(k => {
                            const rawKey = `${provider}:${k.api_key_id}`
                            const raw = expandedRaw[rawKey]
                            const isOpen = raw !== undefined
                            return (
                              <Fragment key={rawKey}>
                                <tr className={k.api_key_status !== 'active' ? 'bu-spend-row-inactive' : ''}>
                                  <td>
                                    <div className="bu-spend-keyname">{k.api_key_name || k.api_key_id}</div>
                                    <div className="bu-spend-keyhint">
                                      <code>{k.api_key_hint}</code>
                                      {k.api_key_status !== 'active' && <span className="bu-spend-keystatus"> · {k.api_key_status}</span>}
                                    </div>
                                  </td>
                                  <td className="bu-spend-num">${k.total_usd_24h.toFixed(2)}</td>
                                  <td className="bu-spend-num">${k.total_usd_7d.toFixed(2)}</td>
                                  <td className="bu-spend-num">${k.total_usd_30d.toFixed(2)}</td>
                                  <td>
                                    <button className="bu-spend-toggle" onClick={() => toggleRaw(provider, k.api_key_id)}>
                                      {isOpen ? 'hide raw' : 'raw'}
                                    </button>
                                  </td>
                                </tr>
                                {isOpen && (
                                  <tr className="bu-spend-raw-row">
                                    <td colSpan={5}>
                                      <pre className="bu-spend-raw">
                                        {raw === 'loading' ? 'loading...' : raw}
                                      </pre>
                                    </td>
                                  </tr>
                                )}
                              </Fragment>
                            )
                          })}
                        </tbody>
                      </table>
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}

      {limits && (
        <div className="bu-limits-section">
          {Object.entries(limits).map(([provider, p]) => {
            if (!p) return null
            const stale = p.stale_after != null && Date.now() / 1000 > p.stale_after
            const windowEntries = Object.entries(p.windows).filter(([, w]) => w.used_percent > 0 || w.resets_at != null)
            if (windowEntries.length === 0) return null
            return (
              <div key={provider} className="bu-limits-provider">
                <div className="bu-limits-header">
                  <span className="bu-limits-title">{PROVIDER_LABELS[provider] ?? provider}</span>
                  {p.plan_type && <span className="bu-limits-tier">{p.plan_type}{p.tier?.includes('20x') ? ' 20x' : p.tier?.includes('5x') ? ' 5x' : ''}</span>}
                  {stale && <span className="bu-limits-stale" title={`snapshot from ${new Date(p.snapshot_at * 1000).toLocaleString()}`}>stale</span>}
                </div>
                <div className="bu-limits-grid">
                  {windowEntries.map(([key, win]) => (
                    <LimitBar key={key} label={WINDOW_LABELS[key] ?? key} win={win} />
                  ))}
                </div>
              </div>
            )
          })}
        </div>
      )}

      {loading ? (
        <div className="bu-loading">Loading...</div>
      ) : (
        <>
          <div className="bu-totals-row">
            <div className="bu-total-card"><span className="bu-total-label">Sessions</span><span className="bu-total-value">{totals.count}</span></div>
            <TokenKindCard label="Input" kind="input" totals={totals.tokens} />
            <TokenKindCard label="Output" kind="output" totals={totals.tokens} />
            <TokenKindCard label="Cache read" kind="cacheRead" totals={totals.tokens} />
            <TokenKindCard label="Cache write" kind="cacheWrite" totals={totals.tokens} />
            {totals.cost > 0 && <div className="bu-total-card"><span className="bu-total-label">Cost</span><span className="bu-total-value">${totals.cost.toFixed(2)}</span></div>}
            <div className="bu-total-card"><span className="bu-total-label">Time</span><span className="bu-total-value">{formatDuration(totals.duration)}</span></div>
          </div>

          {(harnessSlices.length > 0 || sourceSlices.length > 0) && (
            <div className="bu-pies-row">
              <PieChart title="By harness" slices={harnessSlices} valueFmt={formatTokens} />
              <PieChart title="By source" slices={sourceSlices} valueFmt={formatTokens} />
              <p className="bu-pies-note">
                Shares of all tokens: input, output, cache read and cache write, subagents included.
                Input counts only tokens not read from the prompt cache, so on a long session nearly all of it is cache read.
              </p>
            </div>
          )}

          {loadingUsage && <div className="bu-loading-small">Loading session details...</div>}
          {usageError && <div className="bu-usage-error" role="alert">{usageError}</div>}

          {Array.from(harnessGroups.entries())
            // Biggest first. The groups are built in the order the aggregate
            // lists sessions, which is by session id and means nothing here.
            .sort(([, a], [, b]) => allTokensOfUsage(b.totals.tokens) - allTokensOfUsage(a.totals.tokens))
            .map(([harness, group]) => {
            const sources = Array.from(group.sources.values())
              .sort((a, b) => allTokensOfUsage(b.totals.tokens) - allTokensOfUsage(a.totals.tokens))
            const groupTokens = sumOfTokenCounts(group.totals.tokens.mainConversation, group.totals.tokens.subagents)
            return (
              <div key={harness} className="bu-harness-section">
                <div className="bu-harness-header">
                  <span className="bu-harness-name">{harness}</span>
                  <span className="bu-harness-summary">
                    {group.sessionCount} sessions &middot; {tokenCountsText(groupTokens)}
                    {group.totals.cost > 0 && ` \u00B7 $${group.totals.cost.toFixed(2)}`}
                  </span>
                </div>
                <TokenKindBar counts={groupTokens} />
                <div className="bu-source-list">
                  {sources.map(src => {
                    const k = `${harness}::${src.source}`
                    const open = !!expandedSources[k]
                    const label = sourceLabel(src.source)
                    const sourceSubagentTokens = allTokens(src.totals.tokens.subagents)
                    return (
                      <div key={k} className="bu-source-group">
                        <button
                          type="button"
                          className="bu-source-row"
                          onClick={() => toggleSource(harness, src.source)}
                          aria-expanded={open}
                        >
                          <span className="bu-source-toggle">{open ? '\u25BC' : '\u25B6'}</span>
                          <span className="bu-source-label">{label}</span>
                          <span className="bu-source-count">{src.sessions.length} session{src.sessions.length === 1 ? '' : 's'}</span>
                          <span className="bu-source-tokens">
                            {tokenCountsText(sumOfTokenCounts(src.totals.tokens.mainConversation, src.totals.tokens.subagents))}
                            {sourceSubagentTokens > 0 && (
                              <span className="bu-source-subagents" title={tokenCountsText(src.totals.tokens.subagents)}>
                                {' '}&middot; subagents {formatTokens(sourceSubagentTokens)} of it
                              </span>
                            )}
                          </span>
                          {src.totals.cost > 0 && <span className="bu-source-cost">${src.totals.cost.toFixed(2)}</span>}
                          {src.totals.duration > 0 && <span className="bu-source-duration">{formatDuration(src.totals.duration)}</span>}
                        </button>
                        {open && (
                          <div className="bu-session-list">
                            {src.sessions.slice().sort((a, b) => b.cost - a.cost).map(u => {
                              const instance = inst.instanceMap.get(u.instanceId)
                              return (
                                <div key={u.sessionId} className="bu-session">
                                  <div className="bu-session-row">
                                    <span className="bu-session-id">{u.sessionId.slice(0, 16)}</span>
                                    {instance && <span className="bu-instance-label">{instance.name}</span>}
                                    <span className="bu-session-model">{u.model?.replace(/^claude-/, '').replace(/\[.*$/, '')}</span>
                                    <span>{u.turns} turns</span>
                                    {u.cost > 0 && <span>${u.cost.toFixed(3)}</span>}
                                    {u.durationMs > 0 && <span>{formatDuration(u.durationMs)}</span>}
                                  </div>
                                  <div className="bu-session-tokens" title={tokenUsageRecordNote(u.tokens.countedFrom)}>
                                    <span className="bu-session-tokens-part">main</span>
                                    <span>
                                      {tokenCountsText(u.tokens.mainConversation)}
                                      {u.tokens.mainConversationCalls > 0 && ` \u00B7 ${modelCallsText(u.tokens.mainConversationCalls)}`}
                                      {u.tokens.countedFrom === 'turn-results' && (
                                        <span className="bu-session-tokens-uncounted"> &middot; finished turns only</span>
                                      )}
                                    </span>
                                    {u.tokens.subagentCalls > 0 && (
                                      <>
                                        <span className="bu-session-tokens-part">subagents</span>
                                        <span>{tokenCountsText(u.tokens.subagents)} &middot; {modelCallsText(u.tokens.subagentCalls)}</span>
                                      </>
                                    )}
                                  </div>
                                </div>
                              )
                            })}
                          </div>
                        )}
                      </div>
                    )
                  })}
                </div>
              </div>
            )
          })}

          {harnessGroups.size === 0 && !loadingUsage && (
            <div className="bu-empty">No usage data for this period</div>
          )}
        </>
      )}
    </div>
  )
}
