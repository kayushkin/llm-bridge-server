import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../../context'
import {
  PermissionModeAsk,
  PermissionModeAskAll,
  PermissionModeAuto,
  PermissionModeBlockAll,
  PermissionModeBypass,
  PermissionModeCustom,
  PermissionModePlan,
  PermissionModeRead,
  type HarnessInfo,
  type ManagedSession,
} from '../../types'

// SessionPermissionMode is the per-session permission-mode selector rendered
// alongside Compact / Tools / Fork in the chat controls bar. The session
// inherits the global mode at creation; this control overrides it for
// THIS session only — see snapshotPermissionModeIntoSession on the server
// for the create-time logic.
//
// Persists via PUT /sessions/{id}/permission-mode; the new value lands in
// session.harness_config.permission_mode. Read live by the PreToolUse
// prehook (takes effect on the next tool call) and forwarded to the
// harness as a start param on next spawn/resume.
//
// Options are filtered by the harness's SupportedPermissionModes.
const MODE_LABELS: Record<string, string> = {
  [PermissionModeBlockAll]: 'Block All',
  [PermissionModePlan]: 'Plan',
  [PermissionModeRead]: 'Read',
  [PermissionModeAskAll]: 'Ask All',
  [PermissionModeAsk]: 'Rules',
  [PermissionModeAuto]: 'Auto Rules',
  [PermissionModeBypass]: 'Allow All',
  [PermissionModeCustom]: 'Custom…',
}

const MODE_TITLES: Record<string, string> = {
  [PermissionModeBlockAll]: 'Deny every tool call. Agent sees the deny and can keep reasoning or ask you.',
  [PermissionModePlan]: 'Only planning tools (Read / Glob / Grep / TodoWrite). No writes, no shell.',
  [PermissionModeRead]: 'Read-only inspection (Read / Glob / Grep / LS / NotebookRead). No writes, no shell.',
  [PermissionModeAskAll]: 'Skip rules and prompt on every single tool call.',
  [PermissionModeAsk]: 'Default. Use permission rules; prompt only when a rule says ask or no rule matches.',
  [PermissionModeAuto]: 'Auto-allow safe tools (reads + edits + planning); rules for shell / fetch / agent spawns.',
  [PermissionModeBypass]: 'Allow every tool call. AskUserQuestion still pauses for your answer.',
  [PermissionModeCustom]: 'Raw harness-specific approval / sandbox knobs (advanced).',
}

// MODE_ORDER controls dropdown ordering (restrictive → permissive, then
// special at the bottom). Modes not present in the harness's supported
// list are filtered out; modes the harness adds but we haven't listed
// here append at the end so unknown additions still render.
const MODE_ORDER: string[] = [
  PermissionModeBlockAll,
  PermissionModePlan,
  PermissionModeRead,
  PermissionModeAskAll,
  PermissionModeAsk,
  PermissionModeAuto,
  PermissionModeBypass,
  PermissionModeCustom,
]

// MODE_DIVIDER_BEFORE inserts a visual separator before Custom so the
// power-user escape hatch is visually distinct from the everyday modes.
const MODE_DIVIDER_BEFORE = new Set<string>([PermissionModeCustom])

// CUSTOM_PANEL_SCHEMAS describes which harnesses' Custom mode renders
// which raw knobs. Keyed by harness name; each entry lists the available
// values per axis. Harnesses not in this map get a generic "no panel"
// fallback even if `custom` appears in their supported_permission_modes.
const CUSTOM_PANEL_SCHEMAS: Record<string, {
  approvalOptions: { value: string; label: string }[]
  sandboxOptions: { value: string; label: string }[]
}> = {
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

export interface SessionPermissionModeProps {
  session: ManagedSession
  harnesses?: HarnessInfo[]
}

export function SessionPermissionMode({
  session,
  harnesses,
}: SessionPermissionModeProps) {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const cfg = session.harness_config as Record<string, unknown> | undefined
  const initial = useMemo(() => readMode(cfg), [cfg])
  const initialDisableNetwork = useMemo(() => cfg?.disable_network === true, [cfg])
  const initialCustom = useMemo(() => readCustom(cfg), [cfg])
  const [mode, setMode] = useState<string>(initial)
  const [disableNetwork, setDisableNetwork] = useState<boolean>(initialDisableNetwork)
  const [customApproval, setCustomApproval] = useState<string>(initialCustom.approval)
  const [customSandbox, setCustomSandbox] = useState<string>(initialCustom.sandbox)
  const [busy, setBusy] = useState(false)

  useEffect(() => { setMode(initial) }, [initial])
  useEffect(() => { setDisableNetwork(initialDisableNetwork) }, [initialDisableNetwork])
  useEffect(() => {
    setCustomApproval(initialCustom.approval)
    setCustomSandbox(initialCustom.sandbox)
  }, [initialCustom.approval, initialCustom.sandbox])

  const harnessInfo = useMemo(
    () => harnesses?.find(h => h.name === session.harness),
    [harnesses, session.harness],
  )
  const supportsDisableNetwork = harnessInfo?.supports_disable_network ?? false

  const supported = useMemo<string[]>(() => {
    const advertised = harnessInfo?.supported_permission_modes
    const raw = advertised && advertised.length > 0
      ? advertised
      : [PermissionModeAsk, PermissionModeBypass]

    // Sort by canonical MODE_ORDER, append any unknown modes at the end
    // so server-side additions still render without a UI bump.
    const known = MODE_ORDER.filter(m => raw.includes(m))
    const extras = raw.filter(m => !MODE_ORDER.includes(m))
    return [...known, ...extras]
  }, [harnessInfo])

  // putState sends every persisted field in one PUT so multi-axis edits
  // that fire in the same React batch land atomically on the server.
  // permission_mode_custom is only sent when the mode is "custom" — the
  // server clears it when both fields are empty.
  const putState = useCallback(async (
    nextMode: string,
    nextDisable: boolean,
    nextApproval: string,
    nextSandbox: string,
  ) => {
    const body: Record<string, unknown> = {
      mode: nextMode,
      disable_network: nextDisable,
    }
    if (nextMode === PermissionModeCustom) {
      body.permission_mode_custom = { approval: nextApproval, sandbox: nextSandbox }
    }
    const res = await apiFetch(`${basePath}/sessions/${session.session_id}/permission-mode`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
  }, [apiFetch, basePath, session.session_id])

  const handleChange = useCallback(async (next: string) => {
    if (busy || next === mode) return
    setBusy(true)
    const prev = mode
    setMode(next)
    try {
      await putState(next, disableNetwork, customApproval, customSandbox)
    } catch {
      setMode(prev)
    } finally {
      setBusy(false)
    }
  }, [putState, mode, disableNetwork, customApproval, customSandbox, busy])

  const handleNetworkToggle = useCallback(async () => {
    if (busy) return
    setBusy(true)
    const prev = disableNetwork
    const next = !prev
    setDisableNetwork(next)
    try {
      await putState(mode, next, customApproval, customSandbox)
    } catch {
      setDisableNetwork(prev)
    } finally {
      setBusy(false)
    }
  }, [putState, mode, disableNetwork, customApproval, customSandbox, busy])

  const handleCustomApproval = useCallback(async (next: string) => {
    if (busy || next === customApproval) return
    setBusy(true)
    const prev = customApproval
    setCustomApproval(next)
    try {
      await putState(mode, disableNetwork, next, customSandbox)
    } catch {
      setCustomApproval(prev)
    } finally {
      setBusy(false)
    }
  }, [putState, mode, disableNetwork, customApproval, customSandbox, busy])

  const handleCustomSandbox = useCallback(async (next: string) => {
    if (busy || next === customSandbox) return
    setBusy(true)
    const prev = customSandbox
    setCustomSandbox(next)
    try {
      await putState(mode, disableNetwork, customApproval, next)
    } catch {
      setCustomSandbox(prev)
    } finally {
      setBusy(false)
    }
  }, [putState, mode, disableNetwork, customApproval, customSandbox, busy])

  return (
    <div className="bc-ctrl-mode" title={MODE_TITLES[mode] ?? ''}>
      <select
        className="bc-ctrl-mode-select"
        value={mode}
        disabled={busy}
        onChange={e => handleChange(e.target.value)}
        aria-label="Permission mode"
      >
        {supported.map((m, i) => {
          // Render an HR-style disabled separator row before "special"
          // modes (Custom for now). HTML <select> doesn't support real
          // separators, so use a disabled option with em-dashes — every
          // browser respects the disabled state.
          const showDivider = MODE_DIVIDER_BEFORE.has(m) && i > 0
          const opt = (
            <option key={m} value={m} title={MODE_TITLES[m] ?? ''}>
              {MODE_LABELS[m] ?? m}
            </option>
          )
          return showDivider
            ? [
                <option key={`__sep_${m}`} disabled value="">────────</option>,
                opt,
              ]
            : opt
        })}
      </select>
      {supportsDisableNetwork && (
        <label
          className="bc-ctrl-mode-network"
          title="Block outbound network access at the sandbox layer. Takes effect on the next session spawn."
        >
          <input
            type="checkbox"
            checked={disableNetwork}
            disabled={busy}
            onChange={handleNetworkToggle}
            aria-label="Disable network access"
          />
          <span>No network</span>
        </label>
      )}
      {mode === PermissionModeCustom && CUSTOM_PANEL_SCHEMAS[session.harness] && (
        <div className="bc-ctrl-mode-custom" title="Raw harness-specific approval and sandbox knobs.">
          <select
            className="bc-ctrl-mode-custom-approval"
            value={customApproval}
            disabled={busy}
            onChange={e => handleCustomApproval(e.target.value)}
            aria-label="Custom approval policy"
          >
            <option value="">(default)</option>
            {CUSTOM_PANEL_SCHEMAS[session.harness].approvalOptions.map(o => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
          <select
            className="bc-ctrl-mode-custom-sandbox"
            value={customSandbox}
            disabled={busy}
            onChange={e => handleCustomSandbox(e.target.value)}
            aria-label="Custom sandbox mode"
          >
            <option value="">(default)</option>
            {CUSTOM_PANEL_SCHEMAS[session.harness].sandboxOptions.map(o => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
        </div>
      )}
    </div>
  )
}

function readCustom(cfg: Record<string, unknown> | undefined): { approval: string; sandbox: string } {
  const raw = cfg?.permission_mode_custom
  if (!raw || typeof raw !== 'object') return { approval: '', sandbox: '' }
  const r = raw as Record<string, unknown>
  return {
    approval: typeof r.approval === 'string' ? r.approval : '',
    sandbox: typeof r.sandbox === 'string' ? r.sandbox : '',
  }
}

function readMode(cfg: Record<string, unknown> | undefined): string {
  if (!cfg) return PermissionModeAsk
  const explicit = cfg.permission_mode
  if (typeof explicit === 'string' && explicit !== '') return explicit
  if (cfg.bypass_permissions === true) return PermissionModeBypass
  return PermissionModeAsk
}
