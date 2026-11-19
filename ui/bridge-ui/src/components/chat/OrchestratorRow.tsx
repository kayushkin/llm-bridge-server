import { useCallback, useEffect, useMemo, useState } from 'react'
import { useOpenSignalsByAttention, useSessionNames, useSessionStatus } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import {
  conductorState,
  conductorStatusText,
  newestSignalTime,
  signalsRaisedSince,
  type ConductorFacts,
} from '../../conductorState'
import { signalSection } from '../../signalAttentionSections'
import { withoutPeerMessageSignals } from '../../peerMessages'
import { useWaitingPeerMessages } from '../PeerMessageCards'
import { chatKey } from './storageKeys'
import { NeedsYouDropdown } from './SignalsInbox'
import { ConnectedServersPeerMessages, useConnectedServerWaitingCounts } from './ConnectedServersPeerMessages'
import { useOpenConnectedServerSession } from './refChipServer'

// OrchestratorConfig mirrors the configView the orchestrator service returns from
// GET/PUT /config and POST /config/reset-week.
export interface OrchestratorConfig {
  enabled: boolean
  week_limit_usd: number
  week_spent_usd: number
  week_remaining_usd: number
  week_start: string
  week_resets_at: string
  /** The llm-bridge session id of the orchestrator's home session. */
  home_session_id: string
}

// How often to re-read the settings: autonomous mode, and whether orchestratord answers.
const CONFIG_POLL_MS = 30_000
// The newest signal you have opened from the row. Older ones are waiting, not new.
// The key the conductor dock used, so what you had seen there stays seen.
const SEEN_SIGNALS_KEY = chatKey('conductor-signals-seen-up-to')

function readSeenUpTo(): string | null {
  try {
    return window.localStorage.getItem(SEEN_SIGNALS_KEY)
  } catch {
    return null
  }
}

export interface OrchestratorRowProps {
  /** The home session's id, from `GET /home-session`; null until orchestratord names it. */
  homeSessionId: string | null
  /** Why the home session could not be found or opened, verbatim; shown on the row. */
  homeSessionError: string | null
  /** Opens the orchestrator's home session as a chat, the way a session row does. */
  onOpenHomeSession: () => void
  /** Opens every open signal on the thread pane's full width. */
  onOpenSignalsPage: () => void
  /** Opens the session a Needs you card belongs to, the way a session row does. */
  onSelectSession: (sessionId: string) => void
  /** Whether the home session is the session on screen. */
  selected: boolean
}

// OrchestratorRow is the orchestrator's one place in the sidebar, pinned under "+ New":
// its name (the home session's own), what it is doing, and how many signals need you.
// A click on the row opens the home session; the count and ▸ drop Needs you down
// under it, with a way to the signals page;
// the gear opens its settings, fetched from the orchestrator service (proxied at
// `orchestratorBasePath`). The row carries the conductor's `data-state`, so a host
// theme draws its figure in `.bc-conductor-mascot`. Unreachable, it still renders and
// says so rather than breaking the sidebar. A host that proxies no orchestrator does
// not render it.
export function OrchestratorRow({ homeSessionId, homeSessionError, onOpenHomeSession, onOpenSignalsPage, onSelectSession, selected }: OrchestratorRowProps) {
  const { fetch: apiFetch, orchestratorBasePath, routes } = useBridgeConfig()
  const [cfg, setCfg] = useState<OrchestratorConfig | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState(false)
  // The Needs you dropdown under the row. Shut on every load: open, it pushes the list
  // down, and the count beside ▸ already says whether there is anything in it.
  const [signalsOpen, setSignalsOpen] = useState(false)
  const [limitDraft, setLimitDraft] = useState('')
  const [busy, setBusy] = useState(false)

  // Needs you: the operator's questions, the messages from connected servers that
  // wait on a person here, and those waiting on a connected server this one
  // administers — counted the way the signals page counts them. A message's own
  // signal is taken out of the questions so it is counted once.
  const { operator, error: signalsError } = useOpenSignalsByAttention()
  const operatorQuestions = useMemo(() => withoutPeerMessageSignals(operator), [operator])
  const needsYouSection = useMemo(() => signalSection(operatorQuestions), [operatorQuestions])
  const peerMessages = useWaitingPeerMessages()
  const connectedServerWaiting = useConnectedServerWaitingCounts()
  const openConnectedServerSession = useOpenConnectedServerSession()
  const selectConnectedServerSession = useCallback(
    (connectionId: string, sessionId: string) => openConnectedServerSession?.({ connectionId, sessionId }),
    [openConnectedServerSession],
  )
  const needsYou = needsYouSection.signalCount + peerMessages.waiting.length + connectedServerWaiting.total
  const raisedAt = useMemo(
    () => [...operatorQuestions.signals.map((signal) => signal.createdAt), ...peerMessages.waiting.map((message) => message.created_at)],
    [operatorQuestions, peerMessages.waiting],
  )
  const [seenUpTo, setSeenUpTo] = useState<string | null>(readSeenUpTo)

  const homeSessionStatus = useSessionStatus(homeSessionId)
  const sessionName = useSessionNames(homeSessionId ? [homeSessionId] : [])
  // The lookup answers the bare id until the name has loaded; the row says what the
  // orchestrator is until then, rather than show an id.
  const loadedName = homeSessionId ? sessionName(homeSessionId) : null
  const label = loadedName && loadedName !== homeSessionId ? loadedName : 'Orchestrator'

  const load = useCallback(async () => {
    try {
      const r = await apiFetch(`${orchestratorBasePath}/config`)
      if (!r.ok) throw new Error(`config ${r.status}`)
      const c: OrchestratorConfig = await r.json()
      setCfg(c)
      setError(null)
    } catch (e) {
      setError(String((e as Error)?.message ?? e))
    }
  }, [apiFetch, orchestratorBasePath])

  useEffect(() => {
    void load()
    // A hidden tab asks nothing: nobody is looking at the row.
    const timer = setInterval(() => !document.hidden && void load(), CONFIG_POLL_MS)
    return () => clearInterval(timer)
  }, [load])

  // The draft follows the saved limit only while the panel is shut, so a poll never
  // overwrites a number being typed.
  useEffect(() => {
    if (cfg && !expanded) setLimitDraft(String(cfg.week_limit_usd))
  }, [cfg, expanded])

  const patch = useCallback(async (body: Record<string, unknown>) => {
    setBusy(true)
    try {
      const r = await apiFetch(`${orchestratorBasePath}/config`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!r.ok) throw new Error(`save ${r.status}`)
      const c: OrchestratorConfig = await r.json()
      setCfg(c)
      setLimitDraft(String(c.week_limit_usd))
      setError(null)
    } catch (e) {
      setError(String((e as Error)?.message ?? e))
    } finally {
      setBusy(false)
    }
  }, [apiFetch, orchestratorBasePath])

  const resetWeek = useCallback(async () => {
    setBusy(true)
    try {
      const r = await apiFetch(`${orchestratorBasePath}/config/reset-week`, { method: 'POST' })
      if (!r.ok) throw new Error(`reset ${r.status}`)
      const c: OrchestratorConfig = await r.json()
      setCfg(c)
      setError(null)
    } catch (e) {
      setError(String((e as Error)?.message ?? e))
    } finally {
      setBusy(false)
    }
  }, [apiFetch, orchestratorBasePath])

  // Opening the dropdown is seeing what is in it, so it is what clears the alert.
  const toggleSignals = () => {
    if (!signalsOpen) {
      const newest = newestSignalTime(raisedAt)
      if (newest) {
        setSeenUpTo(newest)
        try {
          window.localStorage.setItem(SEEN_SIGNALS_KEY, newest)
        } catch {
          // Storage refused (private mode, quota): the alert comes back on the next load.
        }
      }
    }
    setSignalsOpen((open) => !open)
  }

  const enabled = cfg?.enabled ?? false
  const unreachable = !cfg && !!error
  const unreachableBecause = (unreachable ? error : null) ?? homeSessionError
  const facts: ConductorFacts = {
    reachable: unreachableBecause ? false : cfg ? true : null,
    enabled: cfg ? cfg.enabled : null,
    homeSessionState: homeSessionStatus?.state ?? null,
    needsYou,
    newNeedsYou: signalsRaisedSince(raisedAt, seenUpTo),
  }
  const state = conductorState(facts)
  const statusText = conductorStatusText(state, facts)

  const saveLimit = () => {
    const v = parseFloat(limitDraft)
    if (!Number.isNaN(v) && v >= 0 && v !== cfg?.week_limit_usd) void patch({ week_limit_usd: v })
  }

  const pct = cfg && cfg.week_limit_usd > 0
    ? Math.min(100, (100 * cfg.week_spent_usd) / cfg.week_limit_usd)
    : 0

  return (
    <div className={`bc-orchestrator bc-conductor ${enabled ? 'bc-orchestrator-enabled' : ''}`} data-state={state}>
      <div className={`bc-session-item bc-orchestrator-row ${selected ? 'bc-session-item-selected' : ''}`}>
        <button
          className="bc-session-item-main bc-conductor-crab"
          onClick={onOpenHomeSession}
          aria-current={selected || undefined}
          title={unreachableBecause ? `Orchestrator unreachable: ${unreachableBecause}` : `Open the orchestrator’s chat — ${statusText}`}
        >
          <span className="bc-conductor-mascot" aria-hidden />
          <span className="bc-session-harness bc-orchestrator-badge" aria-hidden>🎬</span>
          <span className={`bc-orchestrator-dot ${unreachable ? 'off' : enabled ? 'on' : 'idle'}`} />
          <span className="bc-session-label bc-orchestrator-label">{label}</span>
          {/* An alert's words are the count beside them, so the row leaves them out. */}
          {state !== 'alert' && <span className="bc-orchestrator-status">{statusText}</span>}
        </button>
        <button
          type="button"
          className={`bc-orchestrator-signals ${needsYou > 0 ? 'bc-orchestrator-signals-waiting' : ''}`}
          onClick={toggleSignals}
          aria-expanded={signalsOpen}
          title={needsYou > 0 ? `${needsYou} need${needsYou === 1 ? 's' : ''} you` : 'Nothing needs you'}
          aria-label={needsYou > 0 ? `Signals: ${needsYou} need you` : 'Signals'}
        >
          {needsYou > 0 && <span className="bc-orchestrator-needs-count">{needsYou}</span>}
          <span aria-hidden>{signalsOpen ? '▾' : '▸'}</span>
        </button>
        <button
          type="button"
          className="bc-orchestrator-caret"
          onClick={() => setExpanded(x => !x)}
          aria-expanded={expanded}
          title="Orchestrator settings"
        >{expanded ? '▾' : '⚙'}</button>
      </div>

      {/* Counts what waits on each connected server for the number above; draws nothing. */}
      <ConnectedServersPeerMessages
        drawn={false}
        countInHeading={false}
        onWaitingCount={connectedServerWaiting.report}
        onSelectSession={selectConnectedServerSession}
      />

      {signalsOpen && (
        <NeedsYouDropdown
          onSelectSession={onSelectSession}
          onSelectConnectedServerSession={selectConnectedServerSession}
          onOpenSignalsPage={() => {
            setSignalsOpen(false)
            onOpenSignalsPage()
          }}
        />
      )}

      {homeSessionError && (
        <div className="bc-orchestrator-error" role="alert">
          Cannot open the orchestrator’s session: {homeSessionError}
        </div>
      )}
      {/* A failed read is why the count might be short; saying so only on the
          signals page would hide the reason from the row that shows the number. */}
      {signalsError && (
        <div className="bc-orchestrator-error" role="alert">Could not read the signals: {signalsError}</div>
      )}
      {peerMessages.error && (
        <div className="bc-orchestrator-error" role="alert">
          Could not read the messages from connected servers: {peerMessages.error}
        </div>
      )}

      {expanded && (
        <div className="bc-orchestrator-panel">
          {/* The orchestrator's own page is not in the navigation; this is its way in. */}
          {routes.orchestrator && (
            <a className="bc-orchestrator-page-link" href={routes.orchestrator}>
              Open the orchestrator page ↗
            </a>
          )}
          {unreachable ? (
            <div className="bc-orchestrator-error">Orchestrator offline ({error})</div>
          ) : cfg ? (
            <>
              <button
                className={`bc-orchestrator-enable ${enabled ? 'on' : 'off'}`}
                disabled={busy}
                onClick={() => void patch({ enabled: !enabled })}
                title="Autonomous mode: lets the Orchestrator run scheduled sweeps and ping you. Chatting works either way."
              >
                {enabled ? '● Autonomous mode on — click to disable' : '○ Enable autonomous mode'}
              </button>

              <label className="bc-orchestrator-limit" title="0 means no limit">
                <span>Weekly limit&nbsp;$</span>
                <input
                  type="number"
                  min="0"
                  step="1"
                  value={limitDraft}
                  disabled={busy}
                  onChange={e => setLimitDraft(e.target.value)}
                  onBlur={saveLimit}
                  onKeyDown={e => { if (e.key === 'Enter') (e.target as HTMLInputElement).blur() }}
                />
              </label>

              <div className="bc-orchestrator-meter">
                <div className="bc-orchestrator-meter-bar">
                  <div className="bc-orchestrator-meter-fill" style={{ width: `${pct}%` }} />
                </div>
                <div className="bc-orchestrator-meter-label">
                  ${cfg.week_spent_usd.toFixed(2)} spent · {cfg.week_limit_usd > 0
                    ? `$${cfg.week_remaining_usd.toFixed(2)} left this week`
                    : 'no weekly limit'}
                </div>
              </div>

              <div className="bc-orchestrator-actions">
                <span className="bc-orchestrator-reset-note">
                  resets {new Date(cfg.week_resets_at).toLocaleDateString()}
                </span>
                <button className="bc-orchestrator-reset" disabled={busy} onClick={() => void resetWeek()}>Reset week</button>
              </div>
              {error && <div className="bc-orchestrator-error">{error}</div>}
            </>
          ) : (
            <div className="bc-orchestrator-loading">Loading…</div>
          )}
        </div>
      )}
    </div>
  )
}
