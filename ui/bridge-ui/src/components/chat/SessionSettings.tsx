import { useEffect, useState } from 'react'
import {
  useActiveSession,
  usePendingSession,
  useSessionActions,
  useHarness,
  useHarnessCapabilities,
  useModels,
  useManagedSession,
  useSessionInfo,
  type useSessionControls,
} from '@kayushkin/chat-core'
import type { NewSessionTarget } from './useNewSessionTarget'
import styles from './Chat.module.css'

/** The session controls, as the parent resolved them. See the note on `useSessionSettings`. */
type SessionControls = ReturnType<typeof useSessionControls>

/** The per-session settings, split by how often they are reached for.
 *
 *  This file used to be `ControlsBar.tsx` and drew a second bar between the thread and
 *  the composer. The bar is gone: its controls moved onto the top bar, which is where
 *  the rest of the session's state already lived, and the row it occupied went back to
 *  the transcript. The rename is not cosmetic — nothing here is a bar any more, and a
 *  file called `ControlsBar` that renders two disconnected clusters would be lying about
 *  what it does.
 *
 *  The split is by REACH, not by capability:
 *
 *  - `SessionSettingsInline` is what sits on the top bar: the model picker and Compact.
 *    Those are the two a person touches mid-conversation, so they cost a click of zero.
 *  - `SessionSettingsPanel` is what sits inside the header's details dropdown: effort
 *    and Fork. Both are set once and then left alone, so they cost a click of one.
 *
 *  Everything is still gated on the harness's own capability set (`GET /harnesses`),
 *  never a per-harness list written here — a harness that does not advertise `model`
 *  simply gets no model picker.
 */

/** Format a token count the way a person reads one: `142k`, `1M`, `1.5M`.
 *
 *  Not `toLocaleString` — `142,336 / 1,000,000` is nineteen characters and this has to
 *  fit on a strip beside a session name. The full, exact pair stays on the strip's
 *  `title`, so the rounding here costs nothing a hover cannot recover.
 *
 *  Rounds rather than truncates: a window at 999,600 tokens reading `999k` and then
 *  jumping to `1M` is the honest sequence, where truncation would show `999k` for the
 *  last four hundred tokens before the limit and make a nearly-full window look like it
 *  had room. */
export function formatTokens(n: number): string {
  if (n >= 1_000_000) {
    const millions = n / 1_000_000
    // One decimal below 10M, none above — `12.3M` is noise at that size, `1.5M` is not.
    const text = millions >= 10 ? millions.toFixed(0) : millions.toFixed(1)
    return `${text.replace(/\.0$/, '')}M`
  }
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`
  return String(n)
}

/** Everything both clusters need, resolved once.
 *
 *  ⚠️ `controls` arrives as a PROP and is not re-resolved here. `useSessionControls`'s
 *  `compacting` is hook-local `useState`, set by whichever instance called `compact()`.
 *  The Turns pane shows a "Compacting context…" strip off the same flag, so a second
 *  call of the hook in either place would give one of them a flag that can never go
 *  true. `Chat` owns the single instance and hands it to every consumer.
 *
 *  The gate is SPLIT, not one flag. Model and effort are PRE-START settings: they are
 *  the two things a chat is worth choosing before it exists, and a user who has to send
 *  a message first has already spent a turn on the wrong model. So they resolve for a
 *  pending pane as well as a live session. Compact and Fork do not — neither means
 *  anything before there is a session to compact or fork.
 *
 *  The two halves also write to different places. On a live session a pick is a
 *  `POST /sessions/{id}/config`. On a pending pane there is no session id to post to, so
 *  the pick is recorded on the pending pane itself (`patchPending`) and chat-core applies
 *  it in the single config call it already makes right after the lazy create. */
export function useSessionSettings(newTarget: NewSessionTarget, controls: SessionControls) {
  const { id, summary } = useActiveSession()
  const pending = usePendingSession()
  const { patchPending } = useSessionActions()

  // A pending pane names the harness its first message will create the session on, which
  // is what lets the capability gate and the model list resolve before the session exists.
  const isPending = !id && !!pending
  const harness = summary?.harness ?? pending?.harness ?? null
  const capabilities = useHarnessCapabilities(harness)
  // ⚠️ `pty` is NOT in `capabilities`. It is a top-level boolean on the harness registry
  // row — `GET /harnesses` reports claude_code with `pty: true` and capabilities
  // `['compact','fork','model','effort','tools','system_prompt']` — so `capabilities.has('pty')`
  // is false for every harness that supports it, and a control gated that way would
  // never appear at all. See chat-core's `useHarness`.
  const harnessRow = useHarness(harness)
  const models = useModels(harness)
  const { setConfig } = controls
  const { session: managed } = useManagedSession(id)
  const { info } = useSessionInfo(id)

  // Model & effort are LIVE changes on an existing session (POST /config via setConfig).
  // Seed the selects from the session's current harnessConfig so they reflect what is in
  // effect, then hold the choice locally so the control stays responsive before the config
  // refetch lands. setConfig is LOUD (it rethrows on any non-2xx); the rejection is
  // swallowed here only so a refused change does not become an unhandled rejection — it
  // still surfaces through `controls.error`, and the select never advances to a value the
  // server rejected.
  const configuredModel = managed?.harnessConfig?.model ?? ''
  const configuredEffort = managed?.harnessConfig?.effort ?? ''
  const [model, setModel] = useState('')
  const [effort, setEffort] = useState('')
  useEffect(() => { setModel(configuredModel) }, [configuredModel])
  useEffect(() => { setEffort(configuredEffort) }, [configuredEffort])

  const hasLiveSession = !!id && !!summary

  // On a pending pane the store IS the value: `patchPending` is synchronous, so there is
  // nothing to reconcile against and no local copy that could disagree with it.
  const modelValue = isPending ? pending?.model ?? '' : model
  const effortValue = isPending ? pending?.effort ?? '' : effort
  // The level the harness says it runs at (SessionInfo.effort). With nothing chosen it
  // names the default the harness picked — Claude Code's differs by model. A pending
  // pane has no harness yet to ask, so it has none.
  const reportedEffort = isPending ? '' : info?.effort ?? ''

  // A deliberate pick is also the user telling us what they want next time, so it is
  // written back as the harness's saved default — MERGED over the record already stored,
  // never in place of it (see `rememberDefaults`; a bare write deletes that harness's
  // spend ceiling and disabled-tool list).
  //
  // The live path remembers only AFTER the server accepts. A refused model is not a
  // preference, and saving it would make every subsequent new chat start on a model this
  // instance has already said it cannot run. A pending pane has nothing to refuse it yet,
  // so its pick is recorded immediately.
  const remember = (override: { model?: string; effort?: string }) => {
    if (harness) newTarget.rememberDefaults(harness, override)
  }

  const chooseModel = (next: string) => {
    if (isPending) {
      patchPending({ model: next })
      remember({ model: next })
      return
    }
    setModel(next)
    void setConfig({ model: next }).then(() => remember({ model: next })).catch(() => {})
  }
  const chooseEffort = (next: string) => {
    if (isPending) {
      patchPending({ effort: next })
      remember({ effort: next })
      return
    }
    setEffort(next)
    void setConfig({ effort: next }).then(() => remember({ effort: next })).catch(() => {})
  }

  return {
    capabilities,
    models,
    modelValue,
    effortValue,
    reportedEffort,
    chooseModel,
    chooseEffort,
    isPending,
    hasLiveSession,
    /** What the harness reports it is actually RUNNING, which is not the same question as
     *  what override is set. See the note in `ModelPicker`. */
    runningModel: info?.model ?? '',
    /** Whether this harness can be switched between events and pty at all. */
    supportsPty: harnessRow?.pty === true,
    /** Which I/O mode the session is in now: `events`, `pty`, or empty before the
     *  summary lands. */
    sessionMode: summary?.mode ?? '',
    /** True when either half of the gate is open, so a caller can skip rendering a
     *  container that would otherwise be empty. */
    anySettings: hasLiveSession || isPending,
  }
}

type Settings = ReturnType<typeof useSessionSettings>

/** The model picker.
 *
 *  The select's VALUE is the override on `harness_config`, and an empty value honestly
 *  means "no override; the harness picks". That leaves a picker whose whole job is model
 *  selection unable to say what the session is actually running. The harness reports that
 *  separately as `info.model`, so it is named on the EMPTY OPTION's label rather than
 *  seeded as the value: seeding it would make an un-overridden session look identical to
 *  one deliberately pinned to that model, and re-picking the shown value would convert a
 *  default into an override without the user asking for one.
 *
 *  ## Short names
 *
 *  Each option shows model-store's `short_name` — `opus-4.6` rather than
 *  `Claude Opus 4.6` — because this now sits on a top bar beside eight other controls and
 *  a 180px select was the widest thing on it. The full `label` moves to the option's own
 *  `title`, so the shortening costs a hover rather than the information.
 *
 *  When a model has no `short_name` yet the option falls back to the model ID, and that is
 *  deliberately NOT a fallback to `label`: the ID is the model's canonical identity rather
 *  than a guess at a nickname, and showing it makes a missing nickname visible so it gets
 *  filled in. Falling back to the full name would hide the gap forever. */
/** A trailing bracketed variant on a running-model string, e.g. the `[1m]` in
 *  `claude-opus-5[1m]`. */
const MODEL_VARIANT_SUFFIX = /\[[^\]]+\]$/

/** Resolve what the harness says it is RUNNING against the model registry, as a short
 *  form for the closed select and a full form for its tooltip.
 *
 *  A plain `find` on the id is not enough, and the gap is visible rather than theoretical:
 *  Claude Code reports `claude-opus-5[1m]` for the million-token context variant, and the
 *  registry has no row under that id — only `claude-opus-5`. The exact lookup missed, and
 *  the placeholder fell all the way through to the raw string, which is the long name in a
 *  132px select that the short names existed to get rid of.
 *
 *  So: match exactly, and failing that match again with the variant stripped — then put
 *  the variant BACK on the short name (`opus-5[1m]`). Dropping it is not an option. It is
 *  the harness distinguishing a 1M-context model from a 200k one, which is the single most
 *  useful thing about that string, and a picker that quietly reported the wrong context
 *  window would be worse than one that reported an ugly id.
 *
 *  When neither lookup matches, the raw string survives untouched. That is the registry
 *  honestly having nothing to say about this model, not a license to invent a nickname. */
function matchRunningModel(
  models: { value: string; label: string; shortName: string }[],
  runningModel: string,
): { short: string; full: string } {
  if (!runningModel) return { short: '', full: '' }

  const exact = models.find((m) => m.value === runningModel)
  if (exact) return { short: exact.shortName || exact.value, full: exact.label }

  const variant = runningModel.match(MODEL_VARIANT_SUFFIX)?.[0] ?? ''
  if (variant) {
    const base = runningModel.slice(0, -variant.length)
    const baseMatch = models.find((m) => m.value === base)
    if (baseMatch) {
      return {
        short: `${baseMatch.shortName || baseMatch.value}${variant}`,
        full: `${baseMatch.label} ${variant}`,
      }
    }
  }

  return { short: runningModel, full: runningModel }
}

function ModelPicker({ settings }: { settings: Settings }) {
  const { models, modelValue, chooseModel, runningModel } = settings

  const running = matchRunningModel(models, runningModel)
  // The placeholder falls back to the model ID, NEVER to `label`. `label` carries the
  // per-million cost suffix (`Claude Opus 5 ($15/$75)`), which is a fine thing to read in
  // a dropdown row and a terrible thing to squeeze into a 132px closed select — a
  // `<select>` cannot ellipsis, so it clipped mid-character to `auto (Claude Opus 5 ($15/$7`.
  // The ID is short, canonical, and matches what the options themselves fall back to.
  const runningShort = running.short
  const placeholder = runningShort ? `auto (${runningShort})` : 'Model'
  const title = runningShort
    ? `Model — running ${running.full}${modelValue ? '' : ' (harness default, no override set)'}`
    : 'Model'

  return (
    <select
      className={`bc-ctrl-select ${styles.modelPicker}`}
      value={modelValue}
      onChange={(e) => chooseModel(e.target.value)}
      aria-label="Model"
      title={title}
    >
      <option value="" title={title}>{placeholder}</option>
      {models.map((m) => (
        <option key={m.value} value={m.value} title={m.label}>
          {m.shortName || m.value}
        </option>
      ))}
    </select>
  )
}

/** The always-visible cluster on the top bar: the model picker, Compact, and — only when
 *  something has just been refused — the error chip.
 *
 *  Compact is icon-only now, and the percentage that used to be baked into its label has
 *  moved to the context strip along the header's bottom edge. It was on the button because
 *  there was nowhere else to put it; there is now, and a button that reports a measurement
 *  AND performs an action was doing two jobs.
 *
 *  The error chip stays inline rather than moving into the dropdown with the rest of the
 *  overflow. A refused setting is not a detail to go looking for — without it the select
 *  silently snaps back to its old value and the user is told nothing. */
export function SessionSettingsInline({
  settings,
  controls,
}: {
  settings: Settings
  controls: SessionControls
}) {
  const { capabilities, models, hasLiveSession, anySettings } = settings
  const { compact, compacting, error } = controls

  if (!anySettings) return null

  return (
    <>
      {capabilities.has('model') && models.length > 0 && <ModelPicker settings={settings} />}

      {/* Compact stays behind the LIVE half of the gate. There is nothing to compact
          before a session exists. */}
      {hasLiveSession && capabilities.has('compact') && (
        <button
          className={styles.iconButton}
          onClick={() => void compact().catch(() => {})}
          disabled={compacting}
          aria-label={compacting ? 'Compacting context' : 'Compact context'}
          title={compacting ? 'Compacting context…' : 'Compact context'}
        >
          {compacting ? '⏳' : '🗜'}
        </button>
      )}

      {/* A refused control is STATED, and stated visibly. Without it the select silently
          snaps back to its old value and the user is told nothing.

          The message stays as visible text rather than collapsing to a bare `⚠` with the
          sentence on a tooltip. Hover is not a channel on a touch screen and is not a
          channel anyone consults unprompted, so a hover-only refusal is a silent one with
          extra steps. It is width-capped and ellipsised instead: the row is `nowrap`, so an
          unbounded sentence here would shove the rest of the cluster off the edge, but a
          bounded one reads as "something was refused, and roughly what" — and the full text
          is still on `title` and `aria-label` for the rest. */}
      {error && (
        <span
          className={`bc-status-chip bc-status-chip-error ${styles.settingsError}`}
          role="status"
          title={error}
          aria-label={`Control error: ${error}`}
        >
          <span className="bc-status-chip-label">⚠ {error}</span>
        </span>
      )}
    </>
  )
}

const EFFORT_OPTIONS = [
  { value: 'low', label: 'Low' },
  { value: 'medium', label: 'Medium' },
  { value: 'high', label: 'High' },
  { value: 'xhigh', label: 'XHigh' },
  { value: 'max', label: 'Max' },
]

/** A reported level in the same words as the options; a level the list does not know
 *  is shown as the harness sent it. */
function effortLabel(effort: string): string {
  return EFFORT_OPTIONS.find((option) => option.value === effort)?.label ?? effort
}

/** The overflow cluster, rendered inside the header's details dropdown: reasoning effort
 *  and Fork.
 *
 *  Both are set-once settings. Effort is picked when a chat starts and rarely touched
 *  again, and Fork is a thing done deliberately rather than in passing — neither earns a
 *  permanent slot on a row that has to stay one line wide.
 *
 *  Effort keeps the pre-start half of the gate (it is a pending-pane setting like model),
 *  Fork keeps the live half. */
export function SessionSettingsPanel({
  settings,
  controls,
}: {
  settings: Settings
  controls: SessionControls
}) {
  const {
    capabilities,
    effortValue,
    reportedEffort,
    chooseEffort,
    hasLiveSession,
    anySettings,
    supportsPty,
    sessionMode,
  } = settings
  const { fork, forking, switchMode } = controls
  const [switchingMode, setSwitchingMode] = useState(false)

  if (!anySettings) return null
  const showEffort = capabilities.has('effort')
  const showFork = hasLiveSession && capabilities.has('fork')
  const showMode = hasLiveSession && supportsPty
  if (!showEffort && !showFork && !showMode) return null

  const isPty = sessionMode === 'pty'
  const nextMode: 'events' | 'pty' = isPty ? 'events' : 'pty'

  return (
    <div className={styles.detailsSection}>
      <span className={styles.detailsSectionLabel}>Session settings</span>
      {showEffort && (
        <select
          className="bc-ctrl-select"
          value={effortValue}
          onChange={(e) => chooseEffort(e.target.value)}
          aria-label="Reasoning effort"
          title="Reasoning effort"
        >
          {/* The empty option names the level in force only while nothing is chosen;
              once a level is chosen it shows as itself, and a "Default (High)" beside a
              chosen High would read as a second setting. */}
          <option value="">
            {effortValue === '' && reportedEffort ? `Default (${effortLabel(reportedEffort)})` : 'Default'}
          </option>
          {EFFORT_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>{option.label}</option>
          ))}
        </select>
      )}
      {showFork && (
        <button
          className="bc-ctrl-btn"
          onClick={() => void fork().catch(() => {})}
          disabled={forking}
          title="Fork session"
        >
          {forking ? 'Forking…' : 'Fork'}
        </button>
      )}
      {/* The events ↔ pty switch, and the only way to reach a terminal from chat.
          Behind the `⋯` panel rather than on the header row: 23 of 9,629 sessions on this
          host are pty, so it is a control almost nobody wants on screen, and the row it
          would sit on already carries nine.

          It is NOT gated on the turn being idle, and bridge-ui's is. The server kills and
          respawns the harness with --resume, so switching mid-generation loses the partial
          response — but the client cannot tell a running turn from a stale `model_generating`
          it has not settled yet, and disabling the button on that reading is what leaves a
          user unable to act on a session that is already stuck. The server enforces the
          real rule; a refusal comes back on `controls.error` rather than being predicted
          here. */}
      {showMode && (
        <button
          className="bc-ctrl-btn"
          onClick={() => {
            setSwitchingMode(true)
            void switchMode(nextMode)
              .catch(() => {})
              .finally(() => setSwitchingMode(false))
          }}
          disabled={switchingMode}
          aria-label={`Switch to ${nextMode} mode`}
          title={
            isPty
              ? 'Switch back to events mode — the terminal closes'
              : 'Switch to pty mode — restarts the harness with --resume and opens a terminal'
          }
        >
          {switchingMode ? 'Switching…' : `Mode: ${isPty ? 'pty' : 'events'}`}
        </button>
      )}
    </div>
  )
}

/** The context readout: a small ring in the header row that fills as the context window
 *  does, with the percentage printed inside it. The exact token pair is in its title. It sits
 *  next to the cost chip, so the two numbers about what a session has used read as one
 *  group, and it takes no row of its own; it replaced a strip along the header's bottom
 *  edge that did.
 *
 *  ⚠️ Renders NOTHING without a real measurement. "0%" is a different answer from "no
 *  reading yet", not a smaller one — a session that has reported no usage has no context
 *  measurement at all, and painting an empty ring with `0%` beside it would claim it did.
 *
 *  The arc is clamped to a full circle while the readout is not: a session over its limit
 *  should say `120%`, which is the fact, but an arc past 360° just draws over itself.
 *  `data-fill` carries the clamped figure so the clamp can be asserted without doing
 *  geometry on the SVG. */
export function ContextRing({
  tokens,
  limit,
  pct,
}: {
  tokens: number
  limit: number
  pct: number
}) {
  if (!(tokens > 0 && limit > 0)) return null

  const tone = pct >= 90 ? 'Crit' : pct >= 70 ? 'Warn' : ''
  const pctLabel = pct.toFixed(0)
  const fill = Math.min(100, pct)
  // A circle of radius 10 in a 24×24 box; the arc is a dash of `fill`% of its length.
  const circumference = 2 * Math.PI * 10

  return (
    <span
      className={`${styles.contextRing}${tone ? ` ${styles[`contextRing${tone}`]}` : ''}`}
      title={`Context window: ${tokens.toLocaleString()} / ${limit.toLocaleString()} tokens (${pctLabel}%)`}
    >
      <svg viewBox="0 0 24 24" className={styles.contextRingSvg} aria-hidden>
        <circle cx="12" cy="12" r="10" className={styles.contextRingTrack} />
        <circle
          cx="12"
          cy="12"
          r="10"
          className={styles.contextRingArc}
          data-fill={fill}
          strokeDasharray={`${(fill / 100) * circumference} ${circumference}`}
          transform="rotate(-90 12 12)"
        />
        <text x="12" y="12" className={styles.contextRingNumber}>{pctLabel}</text>
      </svg>
      <span
        className={styles.contextRingReadout}
        role="status"
        aria-label={`Context window ${pctLabel}% full, ${tokens.toLocaleString()} of ${limit.toLocaleString()} tokens`}
      >
        {pctLabel}%
      </span>
    </span>
  )
}
