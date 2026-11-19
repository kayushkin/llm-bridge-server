import { useEffect, useState, useCallback, useRef } from 'react'
import { createPortal } from 'react-dom'
import { useBridgeConfig } from '../context'
import { useBridgeHarnesses } from '../useBridgeHarnesses'
import type { HarnessInfo } from '../types'
import type { ConformanceMatrix, ConformanceHarnessResult, ConformanceTestResult } from '@kayushkin/llm-bridge-types'

// Feature groups mirror the categories in
// ~/repos/llm-bridge-server/conformance/matrix.go's AllFeatures slice.
// Each feature names the EventType(s) (or control-plane operation) it
// exercises so the matrix doubles as documentation of which message types
// in the protocol are covered, and which harnesses send them.
const FEATURE_GROUPS: { label: string; features: string[] }[] = [
  { label: 'Lifecycle', features: ['start', 'resume', 'fork', 'compact', 'config', 'discover', 'import'] },
  { label: 'Message round-trip', features: ['message', 'streaming'] },
  { label: 'Content blocks', features: ['block', 'tool_calls', 'thinking', 'plan'] },
  { label: 'Session metadata', features: ['session_info', 'user_message', 'context_used', 'system_prompt', 'reasoning'] },
  { label: 'Hooks / errors', features: ['hook', 'errors'] },
  { label: 'Convenience (server-derived)', features: ['usage_total', 'turn_complete'] },
]

const ALL_FEATURES = FEATURE_GROUPS.flatMap(g => g.features)

const FEATURE_LABELS: Record<string, string> = {
  start: 'Start',
  resume: 'Resume',
  fork: 'Fork',
  compact: 'Compact',
  config: 'Config',
  discover: 'Discover',
  import: 'Import',
  message: 'Message',
  streaming: 'Streaming',
  block: 'Block',
  tool_calls: 'Tool Calls',
  thinking: 'Thinking',
  plan: 'Plan',
  session_info: 'Session Info',
  user_message: 'User Message',
  context_used: 'Context Used',
  system_prompt: 'System Prompt',
  reasoning: 'Reasoning',
  hook: 'Hook',
  errors: 'Errors',
  usage_total: 'Usage Total',
  turn_complete: 'Turn Complete',
}

// Each description names the canonical EventType(s) the feature exercises so
// the matrix ties every test to the message it asserts on.
const FEATURE_DESCRIPTIONS: Record<string, string> = {
  start: 'EventSessionState — start a new session. Sends the "start" JSON-RPC command and expects EventSessionState with state=running.',
  resume: 'EventSessionState — resume a previously saved session by ID. Sends "start" with resume=true and expects a running session.',
  fork: 'EventSessionState — branch a new session from an existing one. Sends "start" with a fork param and expects a running session.',
  compact: 'EventSystem — ask the harness to compact conversation history. Expects an EventSystem response.',
  config: 'EventSystem — apply runtime config (e.g. model, temperature) mid-session via "config:<json>". Expects EventSystem.',
  discover: 'Out-of-band: runs the harness binary with -discover and verifies it prints a valid JSON array of on-disk sessions. Not part of the EventType protocol.',
  import: 'Out-of-band: checks that the binary supports -import-history (exit code 2 = unsupported, 0 = no-op pass). Not part of the EventType protocol.',
  message: 'EventResult — send a user message and receive a non-empty text result.',
  streaming: 'EventStream — incremental delta events arrive before the final EventResult, not just a single blob.',
  block: 'EventBlock — whole finished content blocks (text, thinking, tool_use, …) emitted alongside or instead of EventStream deltas.',
  tool_calls: 'EventToolCall / EventToolResult — model-emitted tool calls are executed and their results fed back into the conversation. Skipped — requires a real LLM.',
  thinking: 'EventThinking — extended-thinking / reasoning blocks emitted as distinct events, separate from the final answer. Skipped — requires a real LLM.',
  plan: 'EventPlan — structured task-planning event distinct from thinking. Scenario-specific; emitted only when the underlying agent uses a plan tool, so most harnesses skip.',
  session_info: 'EventSessionInfo — emitted at start with the harness\'s initial metadata: system prompt, working dir, model, tools, slash commands, agents, MCP servers.',
  user_message: 'EventUserMessage — the harness echoes the caller\'s input back through the event stream so consumers have a single source of truth for what was sent.',
  context_used: 'EventResult.usage — result events include token usage fields (input/output/context window).',
  system_prompt: 'Control-plane: harness accepts a system_prompt param on session start and applies it to the conversation. Verified via EventSessionState.',
  reasoning: 'Control-plane: harness accepts a reasoning-effort config and passes it through to the model. Verified via EventSystem.',
  hook: 'EventHook — lifecycle events (PreToolUse, PostToolUse, UserPromptSubmit, …). Requires hook config in the underlying agent; scenario-specific, so most harnesses skip.',
  errors: 'EventError — errors surface as discrete events rather than crashes or silent failures. Verified via MOCK_HARNESS_EMIT_ERROR=true.',
  usage_total: 'EventUsageTotal — server-derived convenience event. Emitted by llm-bridge-server (not the harness) — cumulative session usage across every result event. The conformance runner spawns harnesses directly, so it always Skips here.',
  turn_complete: 'EventTurnComplete — server-derived convenience event. Emitted by llm-bridge-server (not the harness) — coalesced turn summary fired immediately after the terminating result/error. The conformance runner spawns harnesses directly, so it always Skips here.',
}

const STATUS_DESCRIPTIONS: Record<string, string> = {
  pass: 'The expected JSON-RPC event arrived within the timeout (60s by default, set by CONFORMANCE_EVENT_TIMEOUT) and matched the predicate for this feature.',
  fail: 'The test ran but the harness responded with an error, the wrong event type, or nothing within the timeout.',
  unsupported: 'The test ran and the harness answered, but it does not implement this feature. A verdict about the harness, unlike Skip.',
  skip: 'No verdict was reached: a precondition failed, or this runner cannot exercise the feature at all. Says nothing about the harness.',
  untested: 'No test run has covered this harness yet. Click "Run Tests" to populate the matrix.',
}

interface ConformanceResponse {
  running: boolean
  matrix: ConformanceMatrix | null
}

type HarnessState = 'working' | 'untested' | 'broken' | 'unavailable'

function classifyHarness(h: HarnessInfo, hr?: ConformanceHarnessResult): HarnessState {
  if (!h.available) return 'unavailable'
  if (!hr) return 'untested'
  if (hr.summary.passed > 0) return 'working'
  return 'broken'
}

export function BridgeConformance() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const { harnesses } = useBridgeHarnesses()
  const [response, setResponse] = useState<ConformanceResponse | null>(null)
  const [polling, setPolling] = useState(false)
  const [manualExpanded, setManualExpanded] = useState<Record<string, boolean>>({})

  const fetchMatrix = useCallback(async () => {
    const res = await apiFetch(`${basePath}/conformance`)
    if (res.ok) {
      const data: ConformanceResponse = await res.json()
      setResponse(data)
      return data.running
    }
    return false
  }, [apiFetch, basePath])

  useEffect(() => {
    fetchMatrix()
  }, [fetchMatrix])

  useEffect(() => {
    if (!polling) return
    const id = setInterval(async () => {
      const stillRunning = await fetchMatrix()
      if (!stillRunning) setPolling(false)
    }, 2000)
    return () => clearInterval(id)
  }, [polling, fetchMatrix])

  const runTests = async () => {
    const res = await apiFetch(`${basePath}/conformance/run`, { method: 'POST' })
    if (res.ok) {
      setPolling(true)
      setResponse(prev => ({ running: true, matrix: prev?.matrix ?? null }))
    }
  }

  const matrix = response?.matrix
  const running = response?.running ?? false

  const resultsByHarness: Record<string, ConformanceHarnessResult> = {}
  if (matrix) {
    for (const hr of matrix.harnesses) {
      resultsByHarness[hr.harness] = hr
    }
  }

  const toggle = (name: string, defaultExpanded: boolean) => {
    setManualExpanded(prev => {
      const current = prev[name] ?? defaultExpanded
      return { ...prev, [name]: !current }
    })
  }

  return (
    <div className="cf-container">
      <div className="cf-header">
        <div>
          <h2 className="cf-title">Harness Conformance</h2>
          <p className="cf-subtitle">
            Tests each harness against the llm-bridge subprocess protocol. Unavailable or failing harnesses are collapsed — click a row to expand.
          </p>
        </div>
        <button className="cf-run-btn" onClick={runTests} disabled={running}>
          {running ? 'Running...' : 'Run Tests'}
        </button>
      </div>

      {running && (
        <div className="cf-running-banner">
          <span className="cf-spinner" /> Testing harnesses... results will update automatically.
        </div>
      )}

      {matrix && matrix.harnesses.length > 0 && (
        <div className="cf-generated">
          Last run: {new Date(matrix.generated_at).toLocaleString()}
        </div>
      )}

      {harnesses.length === 0 ? (
        <div className="cf-empty">No harnesses registered.</div>
      ) : (
        <div className="cf-table-wrapper">
          <table className="cf-table cf-table-flipped">
            <thead>
              <tr className="cf-th-group-row">
                <th className="cf-th-harness-col cf-th-group-empty" />
                {FEATURE_GROUPS.map(g => (
                  <th
                    key={g.label}
                    className="cf-th-group"
                    colSpan={g.features.length}
                    title={g.label}
                  >
                    {g.label}
                  </th>
                ))}
                <th className="cf-th-summary-col cf-th-group-empty" />
              </tr>
              <tr>
                <th className="cf-th-harness-col">Harness</th>
                {FEATURE_GROUPS.map((g, gi) =>
                  g.features.map((feature, fi) => (
                    <th
                      key={feature}
                      className={
                        'cf-th-feature-col' +
                        (fi === 0 && gi > 0 ? ' cf-th-group-start' : '')
                      }
                    >
                      <span className="cf-feature-label">{FEATURE_LABELS[feature] || feature}</span>
                      <InfoTip text={FEATURE_DESCRIPTIONS[feature] || ''} />
                    </th>
                  ))
                )}
                <th className="cf-th-summary-col">Summary</th>
              </tr>
            </thead>
            <tbody>
              {harnesses.map(h => {
                const hr = resultsByHarness[harnessKeyFromName(h.name)]
                const state = classifyHarness(h, hr)
                const defaultExpanded = state === 'working' || state === 'untested'
                const expanded = manualExpanded[h.name] ?? defaultExpanded
                return (
                  <HarnessRow
                    key={h.name}
                    harness={h}
                    result={hr}
                    state={state}
                    expanded={expanded}
                    onToggle={() => toggle(h.name, defaultExpanded)}
                    basePath={basePath}
                  />
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <div className="cf-legend">
        <span className="cf-legend-item">
          <span className="cf-badge cf-pass">Pass</span> Feature supported
          <InfoTip text={STATUS_DESCRIPTIONS.pass} />
        </span>
        <span className="cf-legend-item">
          <span className="cf-badge cf-fail">Fail</span> Feature failed
          <InfoTip text={STATUS_DESCRIPTIONS.fail} />
        </span>
        <span className="cf-legend-item">
          <span className="cf-badge cf-unsup">No</span> Harness does not implement it
          <InfoTip text={STATUS_DESCRIPTIONS.unsupported} />
        </span>
        <span className="cf-legend-item">
          <span className="cf-badge cf-skip">Skip</span> No verdict reached
          <InfoTip text={STATUS_DESCRIPTIONS.skip} />
        </span>
        <span className="cf-legend-item">
          <span className="cf-untested">-</span> Not yet tested
          <InfoTip text={STATUS_DESCRIPTIONS.untested} />
        </span>
      </div>

      <details className="cf-how-it-works">
        <summary>How the tests work</summary>
        <p>
          Each harness is launched as a subprocess. The server writes JSON-RPC commands to its stdin
          and reads JSON event lines from its stdout. For every feature the runner sends a specific
          command and waits up to 10 seconds for an event matching a predicate (e.g. <code>EventSessionState</code>
          with <code>state=running</code>, a non-empty <code>EventResult</code>, or an <code>EventError</code>).
          A match is a <strong>Pass</strong>; a wrong/missing event or timeout is a <strong>Fail</strong>; a harness
          that declares the feature unsupported (or simply doesn't emit the optional event type) yields a <strong>Skip</strong>.
        </p>
        <p>
          Features are grouped by the part of the protocol they exercise: lifecycle commands
          (<code>start</code>, <code>resume</code>, <code>fork</code>, <code>compact</code>, <code>config</code>);
          the message round-trip pair (<code>message</code> ↔ <code>EventResult</code>, <code>streaming</code> ↔ <code>EventStream</code>);
          content-block events (<code>EventBlock</code>, <code>EventToolCall</code>/<code>EventToolResult</code>, <code>EventThinking</code>, <code>EventPlan</code>);
          session metadata (<code>EventSessionInfo</code>, <code>EventUserMessage</code>, token-usage fields, system-prompt and reasoning-effort config);
          and hook / error signalling (<code>EventHook</code>, <code>EventError</code>).
        </p>
        <p>
          Two events are special: <code>discover</code> and <code>import</code> run the binary with <code>-discover</code> /
          <code>-import-history</code> flags and inspect exit code + stdout rather than speaking the JSON-RPC protocol.
          <code>usage_total</code> and <code>turn_complete</code> are <em>server-derived</em> convenience events:
          llm-bridge-server synthesizes them from the raw event stream and broadcasts them to subscribers — harnesses
          themselves never emit them. The conformance runner spawns harnesses directly (no server in the loop), so
          those two always show <strong>Skip</strong> here. They appear in the matrix purely so every event type in the
          protocol has a row.
        </p>
      </details>
    </div>
  )
}

function HarnessRow({
  harness,
  result,
  state,
  expanded,
  onToggle,
  basePath,
}: {
  harness: HarnessInfo
  result?: ConformanceHarnessResult
  state: HarnessState
  expanded: boolean
  onToggle: () => void
  basePath: string
}) {
  const label = harness.label || harness.name
  const emoji = harness.emoji || ''

  const nameCell = (
    <td className={`cf-td-harness-col cf-harness-state-${state}`} onClick={onToggle}>
      <span className="cf-expand-chevron" aria-label={expanded ? 'Collapse' : 'Expand'}>
        {expanded ? '▾' : '▸'}
      </span>
      {harness.image
        ? <img className="cf-harness-img" src={`${basePath}${harness.image}`} alt={label} />
        : <span className="cf-emoji">{emoji}</span>
      }
      <span className="cf-harness-label">{label}</span>
      <StateDot state={state} />
    </td>
  )

  if (!expanded) {
    const message = collapsedMessage(state, result)
    return (
      <tr className={`cf-row-collapsed cf-row-state-${state}`}>
        {nameCell}
        <td className="cf-td-collapsed-msg" colSpan={ALL_FEATURES.length} onClick={onToggle}>
          {message}
        </td>
        <td className="cf-td-summary-col">
          <SummaryCell result={result} />
        </td>
      </tr>
    )
  }

  return (
    <tr className={`cf-row-expanded cf-row-state-${state}`}>
      {nameCell}
      {FEATURE_GROUPS.map((g, gi) =>
        g.features.map((feature, fi) => {
          const tr = result?.results?.find((r: ConformanceTestResult) => r.feature === feature)
          const groupStart = fi === 0 && gi > 0
          return (
            <td
              key={feature}
              className={'cf-td-result' + (groupStart ? ' cf-td-group-start' : '')}
              title={tr?.error || ''}
            >
              <CellBadge result={tr} tested={!!result} />
            </td>
          )
        })
      )}
      <td className="cf-td-summary-col">
        <SummaryCell result={result} />
      </td>
    </tr>
  )
}

function collapsedMessage(state: HarnessState, result?: ConformanceHarnessResult): string {
  if (state === 'unavailable') return 'Binary not installed'
  if (state === 'broken' && result) {
    const total = result.summary.passed + result.summary.failed + result.summary.skipped + result.summary.unsupported
    return `0 / ${total} passing — click to expand`
  }
  if (state === 'untested') return 'Not yet tested — click to expand'
  return 'click to expand'
}

function StateDot({ state }: { state: HarnessState }) {
  const title =
    state === 'working' ? 'Working — at least one feature passing' :
    state === 'untested' ? 'Not yet tested' :
    state === 'broken' ? 'Broken — all features failing' :
    'Binary not installed'
  return <span className={`cf-state-dot cf-state-dot-${state}`} title={title} />
}

function SummaryCell({ result }: { result?: ConformanceHarnessResult }) {
  if (!result) return <span className="cf-untested">-</span>
  return (
    <span className="cf-summary-text">
      <span className="cf-sum-pass">{result.summary.passed}</span>
      {result.summary.failed > 0 && <> / <span className="cf-sum-fail">{result.summary.failed}</span></>}
      {result.summary.unsupported > 0 && <> / <span className="cf-sum-unsup">{result.summary.unsupported}</span></>}
      {result.summary.skipped > 0 && <> / <span className="cf-sum-skip">{result.summary.skipped}</span></>}
    </span>
  )
}

function InfoTip({ text }: { text: string }) {
  const [show, setShow] = useState(false)
  const [coords, setCoords] = useState({ x: 0, y: 0 })
  const ref = useRef<HTMLSpanElement>(null)

  if (!text) return null

  const open = () => {
    const r = ref.current?.getBoundingClientRect()
    if (r) setCoords({ x: r.left + r.width / 2, y: r.bottom + 6 })
    setShow(true)
  }

  return (
    <>
      <span
        ref={ref}
        className="cf-info-icon"
        onMouseEnter={open}
        onMouseLeave={() => setShow(false)}
        onFocus={open}
        onBlur={() => setShow(false)}
        tabIndex={0}
        role="button"
        aria-label="More info"
      >?</span>
      {show && createPortal(
        <div className="cf-tooltip" style={{ left: coords.x, top: coords.y }} role="tooltip">
          {text}
        </div>,
        document.body,
      )}
    </>
  )
}

function CellBadge({ result, tested }: { result?: ConformanceTestResult; tested: boolean }) {
  if (!tested) return <span className="cf-untested">-</span>
  if (!result) return <span className="cf-untested">-</span>
  // Order matches the Go grading in conformance/matrix.go: a skipped test
  // reached no verdict, so it cannot also report the harness lacks the feature.
  if (result.skipped) return <span className="cf-badge cf-skip" title={result.error}>Skip</span>
  if (result.unsupported) return <span className="cf-badge cf-unsup" title={result.error}>No</span>
  if (result.passed) return <span className="cf-badge cf-pass" title={result.duration}>Pass</span>
  return <span className="cf-badge cf-fail" title={result.error}>Fail</span>
}

function harnessKeyFromName(name: string): string {
  return name.replace(/_/g, '')
}
