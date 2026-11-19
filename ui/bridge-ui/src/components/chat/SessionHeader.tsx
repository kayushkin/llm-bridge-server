import { useBridgeConfig } from '../../context'
import { SessionPreviewLinks } from './SessionPreviewLinks'
import { Link } from 'react-router-dom'
import { useEffect, useMemo, useRef, useState } from 'react'
import {
  useActiveSession,
  useSessionList,
  useSessionActions,
  useSessionCost,
  useSpendCeiling,
  useContextUsage,
  useSessionInfo,
  useManagedSession,
  usePendingSession,
  useSessionStatus,
  useHarnessCapabilities,
  type useSessionControls,
} from '@kayushkin/chat-core'
import type {
  HarnessConfigCustom,
  ManagedSessionDetail,
  SessionPermissionState,
  SessionSummary,
} from '@kayushkin/chat-core'
import { useBridgeHarnesses } from '../../useBridgeHarnesses'
import { useBridgeInstances } from '../../useBridgeInstances'
import { useBridgeMachines } from '../../useBridgeMachines'
import { usePrincipals } from '../../usePrincipals'
import { useInstanceReachable } from '../../useInstanceReachable'
import { formatCost } from '../../utils'
import { CostBreakdown } from './CostBreakdown'
import { EditableName } from './EditableName'
import { StatusDot } from './StatusDot'
import { statusActivityWord } from './sessionStatusWords'
import { SystemPromptModal } from './SystemPromptModal'
import { ToolsPanel } from './ToolsPanel'
import type { Machine } from '../../types'
import { isArchivedFolder, toBridgeSessionInfo } from './bridgeAdapters'
import { useSelectSession } from './useSelectSession'
import {
  ContextRing,
  SessionSettingsInline,
  SessionSettingsPanel,
  type useSessionSettings,
} from './SessionSettings'
import styles from './Chat.module.css'
import LensChip from './LensChip'
import { SessionProjectControl } from './SessionProjectControl'
import { PANE_KEYS, type PaneDrawable, type PaneKey, type PanesHidden } from './panePersistence'
import type { CSSProperties } from 'react'

type SessionControls = ReturnType<typeof useSessionControls>
type Settings = ReturnType<typeof useSessionSettings>

interface SessionHeaderProps {
  panesHidden: PanesHidden
  /** Which conditionally-drawable panes this session can draw right now. Separate from
   *  `panesHidden` because it is a fact about the session rather than the user's choice —
   *  see `panePersistence.ts`'s `PaneDrawable`. */
  paneDrawable: PaneDrawable
  /** Flips one pane's visibility, leaving the others as they are: a view tab clicked with
   *  Shift (or Ctrl/⌘), which is how two panes end up side by side. */
  togglePane: (key: PaneKey) => void
  /** Shows one pane and hides the rest: a plain click on a view tab. */
  showOnlyPane: (key: PaneKey) => void
  /** How the Turns pane draws its text — the lens chip beside the view tabs. Raw is a
   *  mode of that pane, not a pane (see `panePersistence.ts`'s note on `PaneKey`). */
  raw: boolean
  setRaw: (v: boolean) => void
  markdown: boolean
  setMarkdown: (v: boolean) => void
  /** Per-session settings, resolved ONCE by `Chat` and shared. See `useSessionSettings`. */
  settings: Settings
  /** The single `useSessionControls` instance. See the warning on `useSessionSettings`. */
  controls: SessionControls
}

/** The top bar: everything the active session is, doing, and set to, on one row.
 *
 *  ## What changed, and why it is one bar now
 *
 *  There used to be two. This header carried identity (status, name, harness, machine,
 *  cost, permission mode, Tools, Prompt, Done, nav arrows, pane toggles) and a separate
 *  `bc-controls-bar` sat between the thread and the composer carrying model, effort,
 *  Compact and Fork. Two rows of chrome for one session, and the split was historical
 *  rather than meaningful — it came from bridge-ui's own layout, where the bar predates
 *  the header.
 *
 *  Merging them bought back a row of vertical space for the transcript, which is the only
 *  thing on this page anybody is actually reading.
 *
 *  ## What stays on the row, and what went behind the caret
 *
 *  The row carries what is worth knowing WITHOUT asking: the status dot, the session
 *  name, cost, the model picker, Compact, the view toggles, and Done. Everything else
 *  moved into the details dropdown — the harness and machine by full name, the nav
 *  arrows, the permission controls, Tools, Prompt, Fork and effort — because each is
 *  either read once or set once, and a row that shows everything shows nothing.
 *
 *  Two things deliberately survived the cull in miniature. The machine keeps its emoji
 *  and its reachability DOT, without its name: the name is what made it wide, but the dot
 *  is the only thing on screen that says the host a session is pinned to has gone away,
 *  and burying that behind a click would mean a dead session looks healthy. The harness
 *  keeps its logo for the same reason in reverse — it is already only an image, it costs
 *  16px, and it is how you tell two otherwise identical sessions apart at a glance.
 *
 *  ## The row no longer wraps
 *
 *  `.bc-header-row` is `flex-wrap: wrap` in bridge-ui's stylesheet, and every chip on it
 *  is `flex-shrink: 0` while `.bc-session-name` has no `min-width: 0`. Nothing could give,
 *  so a long name pushed the right-hand cluster onto a second line and the "one bar" was
 *  two again at any realistic width. `styles.headerRow` sets `nowrap` and `styles.headerName`
 *  makes the name the single shrinkable element, so narrowing truncates the name — the one
 *  thing on the row that survives being cut short — instead of reflowing the chrome.
 *
 *  ## The context readout is a ring beside the cost
 *
 *  `ContextRing` sits in the row, grouped with the cost chip, rather than taking a strip
 *  of its own along the bottom edge. See the note on that component. */
export default function SessionHeader({
  panesHidden,
  paneDrawable,
  togglePane,
  showOnlyPane,
  raw,
  setRaw,
  markdown,
  setMarkdown,
  settings,
  controls,
}: SessionHeaderProps) {
  const { id, summary } = useActiveSession()
  // Not chat-core's bare `select`: opening a session from here also records it as its
  // instance's last session, the same as the sidebar. See `useSelectSession`.
  const select = useSelectSession()
  // A new chat that has not been sent yet has no session id, so it reaches this header
  // through the same `id === null` as "nothing selected". Without the pending record the
  // two draw identically and a freshly opened chat looks like an empty pane.
  const pending = usePendingSession()
  const { groups } = useSessionList()
  const { rename, archive, unarchive } = useSessionActions()
  const { instanceMap } = useBridgeInstances()
  const { machineMap } = useBridgeMachines()
  const { harnessMap, basePath } = useBridgeHarnesses()

  // Cost + context are PURE selectors (no network); info is lazily fetched once per
  // session and cached by the store.
  const { totalUsd, byModel, byQuerySource } = useSessionCost(id)
  const { tokens, limit, pct } = useContextUsage(id)
  const { info, loading: infoLoading } = useSessionInfo(id)
  // `setPermissionState` is optimistic and reverts on failure; harnessConfig carries all
  // three axes the controls read and write — the mode, the sandbox `disableNetwork` gate,
  // and the Custom mode's raw `permissionModeCustom` knobs.
  const { session: managed, setPermissionState } = useManagedSession(id)
  const { setCeiling } = useSpendCeiling(id)

  // The pair the cost chip measures spend against. Both fields ride on the detail read
  // above, NOT on the session summary — `spend_usd` / `max_budget_usd` are deliberately
  // absent from the chat summary wire, because a ceiling is one session's fact and the
  // sidebar query loads a hundred rows.
  //
  // ZERO MEANS NO CEILING (the convention `--max-budget-usd` and `ManagedSession` both
  // use), so an absent and a zero `maxBudgetUsd` collapse to the same answer here: no
  // ceiling, and the chip renders exactly as it did before this existed.
  const maxBudgetUsd = managed?.maxBudgetUsd ?? 0
  const spendCeiling = maxBudgetUsd > 0
    ? { spendUSD: managed?.spendUsd ?? 0, maxBudgetUSD: maxBudgetUsd }
    : undefined

  // What the session is doing right now, as the server decided it. The row carries
  // it for every session, so this is right in the same commit as a switch.
  const status = useSessionStatus(id)

  // Reachability for the machine dot. Resolved before the pending-pane early return
  // below, because a hook cannot be called conditionally — and because a pending chat
  // already names the instance its first message will land on, so it can say the machine
  // is gone before anything is typed.
  const chipInstanceId = summary?.instanceId ?? pending?.instanceId
  const machineReachable = useInstanceReachable(chipInstanceId)

  const [toolsOpen, setToolsOpen] = useState(false)
  const [promptOpen, setPromptOpen] = useState(false)

  // One authoring per capability, read by BOTH the button that opens a panel and the
  // panel's own render site.
  //
  // `toolsOpen` / `promptOpen` are component state, and this header stays mounted when
  // the active session changes — while `capabilities` is recomputed from the NEW
  // session's harness. So the toggle outlives the capability that justified it. Gating
  // only the button would leave the panel mounted after a switch to a harness that does
  // not carry the capability: the button disappears and the panel keeps rendering. Both
  // are reporting capabilities — the panels draw `SessionInfo`, which codex, hermes and
  // the rest never emit — so without the gate the Tools drawer tells such a session
  // "the harness emits this after its first init", which it never will.
  const capabilities = useHarnessCapabilities(summary?.harness ?? null)
  const toolsCapabilityHeld = capabilities.has('tools')
  const systemPromptCapabilityHeld = capabilities.has('system_prompt')

  // The details dropdown behind the caret. Closes on an outside mousedown or Escape.
  //
  // The open state holds WHICH session it was opened for, not a bare boolean, so
  // switching sessions closes it by derivation. A boolean would need an effect to reset
  // it, and that effect would leave the previous session's panel painted over the new one
  // for a frame.
  //
  // ⚠️ A pending pane has no `id`, so its key is the literal below rather than null —
  // null is the closed state, and using it would make the dropdown impossible to open
  // before the first send. That matters more than it used to: the dropdown is now where
  // the pre-start effort picker lives.
  const PENDING_DETAILS_KEY = '__pending__'
  const detailsKey = id ?? PENDING_DETAILS_KEY
  const [detailsOpenFor, setDetailsOpenFor] = useState<string | null>(null)
  const detailsOpen = detailsOpenFor !== null && detailsOpenFor === detailsKey
  const detailsRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!detailsOpen) return
    // Closes through the setter itself rather than a wrapper: a wrapper closes over the
    // key and is rebuilt every render, which would re-subscribe these two listeners on
    // every render of an open panel.
    const onDocClick = (e: MouseEvent) => {
      if (!detailsRef.current) return
      if (!detailsRef.current.contains(e.target as Node)) setDetailsOpenFor(null)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setDetailsOpenFor(null)
    }
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocClick)
      document.removeEventListener('keydown', onKey)
    }
  }, [detailsOpen])

  // Flatten the grouped list into display order for prev/next navigation. Cheap (session
  // count), never touches transcripts.
  const orderedIds = useMemo(() => {
    const out: string[] = []
    for (const g of groups) for (const s of g.sessions) out.push(s.sessionId)
    return out
  }, [groups])

  const idx = id ? orderedIds.indexOf(id) : -1
  const hasPrev = idx > 0
  const hasNext = idx >= 0 && idx < orderedIds.length - 1

  const viewControls = (
    <ViewTabs
      panesHidden={panesHidden}
      paneDrawable={paneDrawable}
      togglePane={togglePane}
      showOnlyPane={showOnlyPane}
    />
  )
  const lensChip = (
    <LensChip
      raw={raw}
      setRaw={setRaw}
      rawDisabled={panesHidden.turns}
      markdown={markdown}
      setMarkdown={setMarkdown}
    />
  )

  const detailsToggle = (
    <button
      type="button"
      className={styles.iconButton}
      onClick={() => setDetailsOpenFor(detailsOpen ? null : detailsKey)}
      aria-expanded={detailsOpen}
      aria-label="Session details and settings"
      title="Session details and settings"
    >
      ⋯
    </button>
  )

  if (!id || !summary) {
    // A pending chat names the instance/harness its first message will create the session
    // on. That target is the store's, not a guess, so the header can say it before the
    // session exists — which is the whole difference between "new chat on Claude Code"
    // and a blank pane.
    const pendingInstance = pending?.instanceId ? instanceMap.get(pending.instanceId) : undefined
    const pendingMachine =
      pendingInstance?.machine ??
      (pendingInstance ? machineMap.get(pendingInstance.machine_id) : undefined)
    const pendingHarness = pending?.harness ?? pendingInstance?.harness_type
    const pendingHarnessInfo = pendingHarness ? harnessMap.get(pendingHarness) : undefined

    return (
      <div className="bc-header" data-harness={pendingHarness || undefined}>
        <div className={`bc-header-row ${styles.headerRow}`}>
          <StatusDot state="placeholder" title={pending ? 'New chat — not started' : 'No session'} />
          {pendingHarness && (
            <HarnessMark harness={pendingHarness} info={pendingHarnessInfo} basePath={basePath} />
          )}
          {pendingMachine && <MachineMark machine={pendingMachine} reachable={machineReachable} />}
          {pending ? (
            <span className={`bc-session-name ${styles.headerName}`}>
              New chat{pendingInstance ? ` — ${pendingInstance.name}` : ''}
            </span>
          ) : (
            <span className={`bc-session-name bc-session-name-empty ${styles.headerName}`}>—</span>
          )}
          <div className={`bc-header-right ${styles.headerCluster}`}>
            {/* Model is a PRE-START setting — the whole point of resolving it before a
                session exists is that picking it after the first send is already too
                late. */}
            <SessionSettingsInline settings={settings} controls={controls} />
            <div className={styles.headerDivider} />
            {viewControls}
          {lensChip}
            <div className={styles.headerDivider} />
            <div className="bc-details-wrap" ref={detailsRef}>
              {detailsToggle}
              {detailsOpen && (
                <div className={styles.detailsPanel} role="dialog" aria-label="Session details">
                  <SessionSettingsPanel settings={settings} controls={controls} />
                  {(pendingMachine || pending?.principalId) && (
                    <div className={styles.detailsSection}>
                      <span className={styles.detailsSectionLabel}>Target</span>
                      {pendingMachine && <MachineChip machine={pendingMachine} reachable={machineReachable} />}
                      {pending?.principalId && <StartedAsChip principalId={pending.principalId} pending />}
                    </div>
                  )}
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    )
  }

  const instance = summary.instanceId ? instanceMap.get(summary.instanceId) : undefined
  const machine = instance?.machine ?? (instance ? machineMap.get(instance.machine_id) : undefined)
  const state = summary.state
  // The status dot is the only thing on this header that says what the session is doing,
  // and on its own it cannot tell a turn that has been on one tool for four minutes from
  // one that has just started. What it is doing rides in the dot's own label.
  const activityLabel = statusActivityWord(status)
  const stateTitle = activityLabel && activityLabel !== state ? `${state} · ${activityLabel}` : state
  const isArchived = isArchivedFolder(summary.folderName)

  // Server-registered harness metadata drives the header's logo image and its warm
  // harness-keyed tint (the `--bc-harness` var + `data-harness` attr the bc-header
  // gradient reads). Falls through to the theme accent when the harness isn't registered
  // or has no tint — never a hardcoded color.
  const harnessInfo = harnessMap.get(summary.harness)
  const headerStyle: CSSProperties | undefined = harnessInfo?.tint
    ? ({ ['--bc-harness']: harnessInfo.tint } as CSSProperties)
    : undefined

  return (
    <div className="bc-header" style={headerStyle} data-harness={summary.harness || undefined}>
      <div className={`bc-header-row ${styles.headerRow}`}>
        <StatusDot state={state} title={stateTitle} />

        {summary.harness && (
          <HarnessMark harness={summary.harness} info={harnessInfo} basePath={basePath} />
        )}
        {machine && <MachineMark machine={machine} reachable={machineReachable} />}

        <EditableName
          value={summary.displayName || summary.sessionId}
          onSave={(next) => rename(id, next)}
          className={`bc-session-name ${styles.headerName}`}
        />

        {/* The gate is spend OR a ceiling, not spend alone. A capped session that has not
            been billed for anything yet is exactly the session whose ceiling is worth
            reading, and a `totalUsd > 0` gate hides the chip on every one of them. */}
        {/* What the session has used, as one group: the context ring, then spend. */}
        <span className={styles.usageGroup}>
          <ContextRing tokens={tokens} limit={limit} pct={pct} />
          {/* Once the detail has loaded, the chip shows on every session, even at $0.00
              with no ceiling: its drill-down is where a ceiling is set, and a session
              is worth capping before it has spent anything. */}
          {(totalUsd > 0 || spendCeiling || managed) && (
            <CostBreakdown
              aggregate={{ totalUsd, byModel, bySource: byQuerySource }}
              ceiling={spendCeiling}
              ceilingEditor={managed ? {
                spendUSD: managed.spendUsd ?? 0,
                maxBudgetUSD: maxBudgetUsd,
                setCeiling,
              } : undefined}
            />
          )}
        </span>

        <SessionPreviewLinks sessionId={summary.sessionId} />

        <SessionProjectControl sessionId={summary.sessionId} />

        <div className={`bc-header-right ${styles.headerCluster}`}>
          <SessionSettingsInline settings={settings} controls={controls} />
          {/* No divider before the tabs: their strip has its own border. */}
          {viewControls}
          {lensChip}
          <div className={styles.headerDivider} />
          <button
            className={`${styles.iconButton} ${styles.iconButtonDone}`}
            onClick={() => (isArchived ? unarchive(id) : archive(id))}
            aria-pressed={isArchived}
            aria-label={isArchived ? 'Reopen (unarchive)' : 'Mark done (archive)'}
            title={isArchived ? 'Reopen (unarchive)' : 'Mark done (archive)'}
          >
            {isArchived ? '↺' : '✓'}
          </button>
          <div className="bc-details-wrap" ref={detailsRef}>
            {detailsToggle}
            {detailsOpen && (
              <div className={styles.detailsPanel} role="dialog" aria-label="Session details">
                {/* Navigation left the row because the sidebar is already a better way to
                    reach another session on a desktop — but it is kept rather than
                    deleted, because it is the only keyboard-reachable prev/next there is. */}
                <div className={styles.detailsSection}>
                  <span className={styles.detailsSectionLabel}>Navigate</span>
                  <div className="bc-nav-arrows">
                    <button
                      className="bc-nav-arrow"
                      disabled={!hasPrev}
                      title="Previous session"
                      aria-label="Previous session"
                      onClick={() => hasPrev && select(orderedIds[idx - 1])}
                    >
                      ‹
                    </button>
                    <button
                      className="bc-nav-arrow"
                      disabled={!hasNext}
                      title="Next session"
                      aria-label="Next session"
                      onClick={() => hasNext && select(orderedIds[idx + 1])}
                    >
                      ›
                    </button>
                  </div>
                  {machine && <MachineChip machine={machine} reachable={machineReachable} />}
                  {summary.principalId && <StartedAsChip principalId={summary.principalId} />}
                </div>

                {managed?.harnessConfig?.permissionMode !== undefined && (
                  <div className={styles.detailsSection}>
                    <span className={styles.detailsSectionLabel}>Permissions</span>
                    <PermissionControls
                      harness={summary.harness}
                      mode={managed.harnessConfig.permissionMode}
                      disableNetwork={managed.harnessConfig.disableNetwork === true}
                      custom={managed.harnessConfig.permissionModeCustom}
                      supportedModes={harnessInfo?.supported_permission_modes}
                      supportsDisableNetwork={harnessInfo?.supports_disable_network === true}
                      onChange={(next) =>
                        // Optimistic in the hook; it reverts + rethrows on failure. Swallow
                        // the rejection here so a failed PUT doesn't become an unhandled
                        // rejection — the revert already makes the failure visible. The
                        // promise is RETURNED, not dropped: it is what holds the controls
                        // disabled for the duration of the write.
                        setPermissionState(next).catch(() => {})
                      }
                    />
                  </div>
                )}

                <SessionSettingsPanel settings={settings} controls={controls} />

                {(toolsCapabilityHeld || systemPromptCapabilityHeld) && (
                  <div className={styles.detailsSection}>
                    <span className={styles.detailsSectionLabel}>Inspect</span>
                    {toolsCapabilityHeld && (
                      <button
                        className={`bc-ctrl-btn ${toolsOpen ? 'bc-ctrl-btn-active' : ''}`}
                        onClick={() => setToolsOpen((v) => !v)}
                        aria-pressed={toolsOpen}
                        title="Tools, slash commands, sub-agents, skills & MCP servers for this session"
                      >
                        🧰 Tools
                      </button>
                    )}
                    {systemPromptCapabilityHeld && (
                      <button
                        className="bc-ctrl-btn"
                        onClick={() => setPromptOpen(true)}
                        disabled={!info}
                        title={
                          info
                            ? "View this session's system prompt, model & working directory"
                            : 'No session info reported yet'
                        }
                      >
                        📄 Prompt
                      </button>
                    )}
                  </div>
                )}

                <SessionDetailsPanel summary={summary} detail={managed} />
              </div>
            )}
          </div>
        </div>
      </div>

      {toolsCapabilityHeld && toolsOpen && (
        <div className={styles.toolsDrawer}>
          {infoLoading ? (
            <div className={styles.toolsLoading}>Loading session tools…</div>
          ) : info ? (
            <ToolsPanel info={toBridgeSessionInfo(info)} />
          ) : (
            <div className={styles.toolsLoading}>
              No tool info reported yet. The harness emits this after its first init.
            </div>
          )}
        </div>
      )}

      {systemPromptCapabilityHeld && promptOpen && info && (
        <SystemPromptModal info={toBridgeSessionInfo(info)} onClose={() => setPromptOpen(false)} />
      )}
    </div>
  )
}

/** The harness logo on the top bar — image, else emoji, else the short name.
 *
 *  bridge-ui's `bc-harness-chip` already renders image-only when the registry has an
 *  image, which is true for the harnesses in daily use. This keeps that class (and so the
 *  harness tint) but never renders the text label beside it: the label is what made the
 *  chip wide, and the logo alone already answers "which harness is this". The full label
 *  stays on the title. */
function HarnessMark({
  harness,
  info,
  basePath,
}: {
  harness: string
  info: { label?: string; image?: string; emoji?: string } | undefined
  basePath: string
}) {
  const label = info?.label || harness
  return (
    <span className="bc-harness-chip" title={label} aria-label={label}>
      {info?.image ? (
        <img className="bc-harness-chip-img" src={`${basePath}${info.image}`} alt="" />
      ) : info?.emoji ? (
        <span className="bc-harness-chip-emoji" aria-hidden>{info.emoji}</span>
      ) : (
        <span className="bc-harness-chip-label">{label}</span>
      )}
    </span>
  )
}

/** The machine on the top bar, reduced to its emoji and its reachability dot.
 *
 *  The NAME is what moved into the dropdown — it is the widest thing the old chip carried
 *  and the least often needed. The dot did not move, and that is the whole point of this
 *  component existing rather than the chip simply being deleted: it is the only thing on
 *  screen that says the host a session is pinned to has gone away, and a session on a dead
 *  machine that looks exactly like a healthy one is the failure this dot was added to
 *  prevent.
 *
 *  `reachable === null` means unknown — no answer yet, or the status read failed — and is
 *  drawn as its own state rather than folded into "unreachable", because "I could not ask"
 *  and "I asked and it is down" are different facts. */
function MachineMark({ machine, reachable }: { machine: Machine; reachable: boolean | null }) {
  return (
    <span className="bc-machine-chip" title={machineTitle(machine, reachable)}>
      <span className="bc-machine-chip-emoji" aria-hidden>{machine.emoji || '🖥'}</span>
      <span className={`bc-machine-chip-dot ${reachableClass(reachable)}`} aria-hidden />
    </span>
  )
}

/** The full machine chip — emoji, NAME, dot — as shown inside the details dropdown. */
/**
 * Who the session was (or, on a pending pane, will be) started as. The
 * session carries only the principal-store id; the name is resolved live from
 * the shared directory so a rename shows without a copy going stale. A host
 * that proxies no principal-store, or an id the directory does not know,
 * shows the id itself — the chip is a true record either way.
 */
function StartedAsChip({ principalId, pending = false }: { principalId: string; pending?: boolean }) {
  const principals = usePrincipals()
  const principal = principals.byId.get(principalId)
  const name = principal?.display_name ?? principalId
  const detail = principal
    ? `${principal.kind === 'group' ? 'group' : 'person'}${principal.disabled_at ? ', disabled' : ''}`
    : principals.enabled && !principals.loading ? 'not in the directory' : ''
  return (
    <span
      className="bc-started-as"
      data-principal-id={principalId}
      title={`${pending ? 'Will be started as' : 'Started as'} ${name} (${principalId})${detail ? ` — ${detail}` : ''}. Its grants decide what the session is offered.`}
    >
      <span className="bc-started-as-label">{pending ? 'starts as' : 'as'}</span> {name}
    </span>
  )
}

function MachineChip({ machine, reachable }: { machine: Machine; reachable: boolean | null }) {
  return (
    <span className="bc-machine-chip" title={machineTitle(machine, reachable)}>
      <span className="bc-machine-chip-emoji" aria-hidden>{machine.emoji || '🖥'}</span>
      <span className="bc-machine-chip-label">{machine.name}</span>
      <span className={`bc-machine-chip-dot ${reachableClass(reachable)}`} aria-hidden />
    </span>
  )
}

function reachableClass(reachable: boolean | null): string {
  if (reachable === null) return 'bc-machine-chip-dot-unknown'
  return reachable ? 'bc-machine-chip-dot-ok' : 'bc-machine-chip-dot-fail'
}

function machineTitle(machine: Machine, reachable: boolean | null): string {
  return [
    `${machine.name}${machine.hostname ? ` (${machine.hostname})` : ''}`,
    `transport: ${machine.transport}`,
    reachable === null ? 'reachability unknown' : reachable ? 'reachable' : 'unreachable',
  ].join('\n')
}

/** Every field the gateway holds about this session, in bridge-ui's SessionDetailsPanel
 *  DOM (`bc-details-list` / `bc-details-row` / `bc-details-mono`) so it inherits the
 *  shared stylesheet.
 *
 *  It reads TWO records, and that split is the point of the whole feature. The sidebar row
 *  (`SessionSummary`) carries the twelve fields the list query returns; the other nine come
 *  from `GET /sessions/{id}`, which chat-core already fetched and cached before this panel
 *  existed. Nothing here widens the list query — a detail is one session at a time, a list
 *  is all of them.
 *
 *  `detail` is null until that lazy fetch lands, so its nine rows read "…" for a moment and
 *  then fill in. That is deliberately distinct from "—": one says the answer has not
 *  arrived, the other says the server holds no value. */
function SessionDetailsPanel({
  summary,
  detail,
}: {
  summary: SessionSummary
  detail: ManagedSessionDetail | null
}) {
  const text = (v: string | undefined) => (v && v.length ? v : '—')
  // A field the detail read has not returned yet is unknown, not empty.
  const pending = (v: string | undefined) => (detail ? text(v) : '…')
  const date = (v: string | undefined) => {
    if (!v) return '—'
    const d = new Date(v)
    return Number.isNaN(d.getTime()) ? v : d.toLocaleString()
  }

  const rows: Array<[label: string, value: string, mono?: boolean]> = [
    ['type', text(summary.type)],
    ['purpose', text(summary.purpose)],
    ['origin', pending(detail?.origin)],
    ['folder', text(summary.folderName)],
    ['state', text(summary.state)],
    ['mode', summary.mode || 'events'],
    ['harness', text(summary.harness)],
    ['agent', text(summary.agentId)],
    ['instance', text(summary.instanceId), true],
    ['working dir', pending(detail?.workingDir), true],
    ['session id', text(summary.sessionId), true],
    ['harness session', pending(detail?.harnessSessionId), true],
    // Named for the id it actually holds. The server's own field is `parent_id` and its
    // doc comment says it is neither a session id nor a general parent — it is the fork
    // parent's HARNESS id, fed to `--fork`. Labeling it "parent" beside a row called
    // "session id" invites reading one as the other.
    ['fork parent (harness id)', pending(detail?.forkParentHarnessSessionId), true],
    ['forked from', pending(detail?.forkedFromSessionId), true],
    ['spawned by', pending(detail?.managerSessionId), true],
    ['pid', detail ? (detail.pid ? String(detail.pid) : '—') : '…'],
    // Both money rows read "—" when the field is absent, never "$0.00". A server that
    // predates the spend gate reports no spend at all, and a ceiling of ZERO MEANS NO
    // CEILING — printing a dollar figure for either would invent a measurement.
    ['spend', detail ? (detail.spendUsd === undefined ? '—' : formatCost(detail.spendUsd)) : '…'],
    [
      'ceiling',
      detail ? ((detail.maxBudgetUsd ?? 0) > 0 ? formatCost(detail.maxBudgetUsd!) : '—') : '…',
    ],
    ['created', date(summary.createdAt)],
    ['updated', date(summary.updatedAt)],
  ]

  const { routes } = useBridgeConfig()
  return (
    <dl className="bc-details-list">
      {routes.effectiveConfig && (
        <div className="bc-details-row" key="effective-config">
          <dt>settings</dt>
          <dd><Link to={`${routes.effectiveConfig}?session=${encodeURIComponent(summary.sessionId)}`} title="What this session was given, setting by setting, and which layer decided each">effective config →</Link></dd>
        </div>
      )}
      {rows.map(([label, value, mono]) => (
        <div className="bc-details-row" key={label}>
          <dt>{label}</dt>
          <dd className={mono ? 'bc-details-mono' : undefined} title={value}>{value}</dd>
        </div>
      ))}
    </dl>
  )
}

/** Canonical permission-mode ordering, mirroring bridge-ui's SessionPermissionMode
 *  (MODE_ORDER) — restrictive → permissive, with the Custom escape hatch last. It orders
 *  the dropdown; it does not decide what goes in it. Which modes a session may actually
 *  take comes from the harness (`supported_permission_modes`), and a mode the harness adds
 *  but this list has not heard of is appended rather than dropped, so a server-side
 *  addition renders without a UI change. */
const PERM_MODE_ORDER: string[] = [
  'block_all',
  'plan',
  'read',
  'ask_all',
  'ask',
  'auto',
  'bypass',
  'custom',
]

const PERM_MODE_LABELS: Record<string, string> = {
  block_all: 'Block All',
  plan: 'Plan',
  read: 'Read',
  ask_all: 'Ask All',
  ask: 'Rules',
  auto: 'Auto Rules',
  bypass: 'Allow All',
  custom: 'Custom…',
}

/** One sentence per mode, from bridge-ui's MODE_TITLES. A permission mode is the one
 *  control here that decides what an agent is allowed to do to the machine, and its label
 *  alone does not say — "Auto Rules" and "Rules" are indistinguishable from four words of
 *  UI. */
const PERM_MODE_TITLES: Record<string, string> = {
  block_all: 'Deny every tool call. Agent sees the deny and can keep reasoning or ask you.',
  plan: 'Only planning tools (Read / Glob / Grep / TodoWrite). No writes, no shell.',
  read: 'Read-only inspection (Read / Glob / Grep / LS / NotebookRead). No writes, no shell.',
  ask_all: 'Skip rules and prompt on every single tool call.',
  ask: 'Default. Use permission rules; prompt only when a rule says ask or no rule matches.',
  auto: 'Auto-allow safe tools (reads + edits + planning); rules for shell / fetch / agent spawns.',
  bypass: 'Allow every tool call. AskUserQuestion still pauses for your answer.',
  custom: 'Raw harness-specific approval / sandbox knobs (advanced).',
}

/** Draw a separator before Custom so the power-user escape hatch reads as apart from the
 *  everyday modes. `<select>` has no real separator element, so it is a disabled option —
 *  every browser honors the disabled state. */
const PERM_MODE_DIVIDER_BEFORE = new Set<string>(['custom'])

/** When a harness advertises no modes at all, offer only the two every harness in the
 *  registry implements. Offering all eight instead would be inventing a capability claim
 *  the harness never made. Same fallback as bridge-ui's SessionPermissionMode. */
const PERM_MODE_UNADVERTISED_FALLBACK: string[] = ['ask', 'bypass']

/** The modes this harness will actually honor, in canonical order.
 *
 *  Offering a mode the harness does not implement is not harmless — the select shows the
 *  user a setting that silently does nothing on the next spawn, and for a permission
 *  control the failure direction is "I chose Block All and it did not block". Only `codex`
 *  advertises `custom`, and only `claude_code` and `codex` advertise `auto`. */
function supportedPermissionModes(supported: string[] | undefined, current: string): string[] {
  const advertised = supported && supported.length > 0 ? supported : PERM_MODE_UNADVERTISED_FALLBACK
  const known = PERM_MODE_ORDER.filter((m) => advertised.includes(m))
  const extras = advertised.filter((m) => !PERM_MODE_ORDER.includes(m))
  const ordered = [...known, ...extras]
  // The session's own mode always stays selectable, even when the harness no longer
  // advertises it — a `<select>` whose value is absent from its options renders blank,
  // which would report the wrong permission mode rather than an unexpected one.
  return ordered.includes(current) ? ordered : [current, ...ordered]
}

/** Which harnesses' Custom mode exposes which raw knobs, keyed by harness name and
 *  mirroring bridge-ui's CUSTOM_PANEL_SCHEMAS. `custom` means "the harness has its own
 *  approval/sandbox vocabulary", and that vocabulary is per-harness — there is no generic
 *  set of values to fall back on, so a harness absent from this map gets no panel even when
 *  it advertises `custom`. Offering it empty selects, or codex's values under another
 *  harness's name, would be inventing a capability claim.
 *
 *  Only `codex` advertises `custom` in the live registry today. */
const PERM_CUSTOM_PANEL_SCHEMAS: Record<
  string,
  { approvalOptions: { value: string; label: string }[]; sandboxOptions: { value: string; label: string }[] }
> = {
  codex: {
    approvalOptions: [
      { value: 'untrusted', label: 'untrusted (escalate on mutation)' },
      { value: 'on-request', label: 'on-request (model decides)' },
      { value: 'never', label: 'never (no codex prompts)' },
    ],
    sandboxOptions: [
      { value: 'read-only', label: 'read-only' },
      { value: 'workspace-write', label: 'workspace-write' },
      { value: 'danger-full-access', label: 'danger-full-access' },
    ],
  },
}

/** Permission controls: the mode select, the sandbox "No network" checkbox, and — in
 *  Custom mode — the harness's raw approval/sandbox knobs. Three affordances, but ONE
 *  persisted state: every change sends all three axes in a single PUT, so a pair of edits
 *  in the same React batch cannot land as two writes where the first is lost.
 *
 *  There is no local mirror of the values. `onChange` patches the store optimistically and
 *  reverts the whole detail on a failed PUT, so the props ARE the optimistic state; a
 *  `useState` copy would be a second source of truth that a revert could not reach. `busy`
 *  only serializes the writes — a second change while one is in flight would snapshot the
 *  first's optimistic value as the thing to revert to. */
function PermissionControls({
  harness,
  mode,
  disableNetwork,
  custom,
  supportedModes,
  supportsDisableNetwork,
  onChange,
}: {
  harness: string
  mode: string
  disableNetwork: boolean
  custom: HarnessConfigCustom | undefined
  supportedModes: string[] | undefined
  supportsDisableNetwork: boolean
  onChange: (state: SessionPermissionState) => Promise<void>
}) {
  const options = useMemo(
    () => supportedPermissionModes(supportedModes, mode),
    [supportedModes, mode],
  )
  const [busy, setBusy] = useState(false)
  const approval = custom?.approval ?? ''
  const sandbox = custom?.sandbox ?? ''
  const customSchema = PERM_CUSTOM_PANEL_SCHEMAS[harness]

  // Send every axis, always — including the ones this change did not touch — so the server
  // never has to merge two half-states. The network gate goes only when the harness
  // supports it: for the other eighteen harnesses the key is not ours to write, and an
  // explicit false would DELETE a value some other path may have set.
  const write = async (next: Partial<SessionPermissionState>) => {
    if (busy) return
    const state: SessionPermissionState = {
      mode: next.mode ?? mode,
      ...(supportsDisableNetwork ? { disableNetwork: next.disableNetwork ?? disableNetwork } : {}),
    }
    // The custom knobs ride along only in Custom mode. Leaving them out on the way to
    // another mode means the server keeps them, so switching to Auto and back does not
    // silently reset a user's approval policy to the default.
    if (state.mode === 'custom') {
      state.permissionModeCustom = {
        approval: next.permissionModeCustom?.approval ?? approval,
        sandbox: next.permissionModeCustom?.sandbox ?? sandbox,
      }
    }
    setBusy(true)
    try {
      await onChange(state)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className={styles.permissionControls}>
      <select
        className="bc-ctrl-select"
        value={mode}
        disabled={busy}
        onChange={(e) => void write({ mode: e.target.value })}
        aria-label="Permission mode"
        title={[
          `Permission mode: ${mode}${disableNetwork ? ' · network disabled' : ''}`,
          PERM_MODE_TITLES[mode],
        ]
          .filter(Boolean)
          .join('\n')}
      >
        {options.flatMap((m, i) => {
          const option = (
            <option key={m} value={m} title={PERM_MODE_TITLES[m] ?? ''}>
              🔒 {PERM_MODE_LABELS[m] ?? m}
              {disableNetwork ? ' ⛔' : ''}
            </option>
          )
          return PERM_MODE_DIVIDER_BEFORE.has(m) && i > 0
            ? [
                <option key={`__sep_${m}`} disabled value="">
                  ────────
                </option>,
                option,
              ]
            : [option]
        })}
      </select>
      {supportsDisableNetwork && (
        <label
          className={styles.permissionNetwork}
          title="Block outbound network access at the sandbox layer. Takes effect on the next session spawn."
        >
          <input
            type="checkbox"
            checked={disableNetwork}
            disabled={busy}
            onChange={(e) => void write({ disableNetwork: e.target.checked })}
            aria-label="Disable network access"
          />
          <span>No network</span>
        </label>
      )}
      {mode === 'custom' && customSchema && (
        <div
          className={styles.permissionCustom}
          title="Raw harness-specific approval and sandbox knobs."
        >
          <select
            className="bc-ctrl-select"
            value={approval}
            disabled={busy}
            onChange={(e) => void write({ permissionModeCustom: { approval: e.target.value } })}
            aria-label="Custom approval policy"
          >
            <option value="">(default)</option>
            {customSchema.approvalOptions.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <select
            className="bc-ctrl-select"
            value={sandbox}
            disabled={busy}
            onChange={(e) => void write({ permissionModeCustom: { sandbox: e.target.value } })}
            aria-label="Custom sandbox mode"
          >
            <option value="">(default)</option>
            {customSchema.sandboxOptions.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </div>
      )}
    </div>
  )
}

/** What each pane's tab says and what it is for. A table rather than branches inside the
 *  map, because the set is closed (`PANE_KEYS`) and TypeScript then refuses a pane added
 *  without an entry for it. `short` is what the tab shrinks to when the header is narrow;
 *  `key` is the number key that picks it (see `usePaneKeys` in BridgeChat). */
const PANE_TABS: Record<PaneKey, { label: string; short: string; title: string }> = {
  turns: { label: 'Turns', short: 'T', title: 'The conversation — one row per message' },
  timeline: { label: 'Timeline', short: 'TL', title: 'Timeline — event-granular, turn → task grouped' },
  kanban: { label: 'Cards', short: 'C', title: 'The kanban cards linked to this session' },
  git: { label: 'Git', short: 'G', title: 'The working tree — branch, status, staged and unstaged diffs' },
  attach: { label: 'Terminal', short: '>_', title: 'The session terminal — pty mode only' },
}

/** The view tabs: one labeled tab per pane this session can draw. A click shows that pane
 *  alone; Shift-, Ctrl- or ⌘-click adds it beside the others (or takes it away), which is
 *  how two panes end up side by side. The number keys 1–5 do the same (BridgeChat).
 *
 *  They replaced a row of emoji toggles (📋 ⏱ 🗂 🌿 ⌨) that nobody could read without
 *  hovering. How the Turns pane draws its text (markdown, plain, raw) is the lens chip
 *  beside them (`LensChip`).
 *
 *  ⚠️ The group keeps the accessible name "Pane visibility" and each tab keeps its pane's
 *  label as its name and `aria-pressed` as its state: the e2e specs and screen readers
 *  both find them that way. */
function ViewTabs({
  panesHidden,
  togglePane,
  showOnlyPane,
  paneDrawable,
}: Pick<SessionHeaderProps, 'panesHidden' | 'togglePane' | 'showOnlyPane' | 'paneDrawable'>) {
  const drawable = PANE_KEYS.filter(
    // A tab for a pane this session cannot draw is a control that does nothing. Absent
    // rather than disabled, because "your session is not in pty mode" is not something a
    // greyed-out tab can say (bridge-ui hides it on the same test, `attachAvailable`).
    (key) => key !== 'attach' || paneDrawable.attach === true,
  )
  return (
    <div className={`bc-pane-toggles ${styles.viewTabs}`} role="group" aria-label="Pane visibility">
      {drawable.map((key, index) => {
        const visible = !panesHidden[key]
        const tab = PANE_TABS[key]
        return (
          <button
            key={key}
            type="button"
            className={styles.viewTab}
            onClick={(event) =>
              event.shiftKey || event.ctrlKey || event.metaKey ? togglePane(key) : showOnlyPane(key)
            }
            aria-pressed={visible}
            aria-label={tab.label}
            title={`${tab.title}\nClick: show only this · Shift-click: ${visible ? 'hide' : 'add beside'} · key ${index + 1}`}
          >
            <span className={styles.viewTabLabel} aria-hidden>{tab.label}</span>
            <span className={styles.viewTabShort} aria-hidden>{tab.short}</span>
          </button>
        )
      })}
    </div>
  )
}
