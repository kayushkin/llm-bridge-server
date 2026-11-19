import { useCallback, useState, type ReactNode } from 'react'

/** Where a setting applies. Each scope is edited on the page of the thing it
 *  applies to; the global Settings page holds only `global` and `harness`. A
 *  narrower scope overrides a wider one: session over board over instance over
 *  harness over global — when a setting exists at both. `browser` is a setting
 *  kept in this browser's localStorage, so each device has its own. */
export type SettingsScope = 'global' | 'harness' | 'instance' | 'board' | 'principal' | 'session' | 'browser'

export const SETTINGS_SCOPE_LABEL: Record<SettingsScope, string> = {
  global: 'Global',
  harness: 'Per harness',
  instance: 'Per instance',
  board: 'Per board',
  principal: 'Per principal',
  session: 'Per session',
  browser: 'This browser',
}

/** What one write came to: landed, or refused in the store's own words. */
export type SaveResult = { ok: true } | { ok: false; error: string }

/** A promise that may reject, read as a SaveResult. The store's message is kept
 *  whole — a refusal is the most useful thing a settings form can show. */
export async function saveResultOf(write: Promise<unknown>): Promise<SaveResult> {
  try {
    await write
    return { ok: true }
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) }
  }
}

export interface SectionSave {
  saving: boolean
  error: string | null
  saved: boolean
  run: (write: () => Promise<SaveResult>) => Promise<void>
  setError: (error: string | null) => void
}

/** The state a section's Save button carries: in flight, the last refusal in
 *  the store's words, or a note that the last save landed. */
export function useSectionSave(): SectionSave {
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)
  const run = useCallback(async (write: () => Promise<SaveResult>) => {
    setSaving(true)
    setSaved(false)
    const result = await write()
    setSaving(false)
    if (result.ok) {
      setError(null)
      setSaved(true)
    } else {
      setError(result.error)
    }
  }, [])
  return { saving, error, saved, run, setError }
}

export interface SettingsSectionProps {
  /** Stable name for tests and anchors: `data-section`. */
  id: string
  title: string
  scope: SettingsScope
  /** The service and record that hold the value, e.g. "kanban-store · board".
   *  Absent for a section that stores nothing, such as the scope index. */
  storedBy?: string
  /** What this setting outranks or is outranked by, in one sentence. */
  precedence?: ReactNode
  help?: ReactNode
  /** A section that saves with a button. Absent for one that saves on change. */
  save?: {
    dirty: boolean
    state: Pick<SectionSave, 'saving' | 'error' | 'saved'>
    onSave: () => void
    label?: string
    /** Extra actions beside Save, e.g. a Clear button. */
    extra?: ReactNode
  }
  /** A section that saves on change reports here instead. */
  status?: { busy?: boolean; error?: string | null; saved?: boolean; note?: ReactNode }
  /** Collapsed sections show only their header until toggled. */
  collapsible?: { expanded: boolean; onToggle: () => void }
  /** Drawn beside the title, e.g. a harness icon or an "unavailable" badge. */
  badges?: ReactNode
  children?: ReactNode
  /** Drawn below the save row: a preview or a read-back that follows a save. */
  after?: ReactNode
}

/** One setting, or one group of settings saved together, framed the same way
 *  everywhere: what it is called, where it applies, which service stores it,
 *  what outranks it, and the store's own words when a save is refused. Lifted
 *  from the kanban board settings page, which had it right, so the Settings
 *  page and every other settings surface look and fail the same way. */
export function SettingsSection({ id, title, scope, storedBy, precedence, help, save, status, collapsible, badges, children, after }: SettingsSectionProps) {
  const expanded = collapsible ? collapsible.expanded : true
  const header = (
    <>
      <span className="bss-heading">
        {badges}
        <h3 className="bss-title">{title}</h3>
      </span>
      <span className="bss-meta">
        <span className={`bss-scope bss-scope-${scope}`} title="Where this setting applies">{SETTINGS_SCOPE_LABEL[scope]}</span>
        {storedBy && <span className="bss-stored-by" title="Which service stores it">{storedBy}</span>}
        {collapsible && <span className="bss-expand" aria-hidden="true">{expanded ? '−' : '+'}</span>}
      </span>
    </>
  )
  return (
    <section className={`bss-section ${expanded ? '' : 'bss-collapsed'}`} data-section={id} data-scope={scope}>
      {collapsible
        ? <button type="button" className="bss-header bss-header-button" aria-expanded={expanded} onClick={collapsible.onToggle}>{header}</button>
        : <div className="bss-header">{header}</div>}
      {expanded && (
        <div className="bss-body">
          {help && <p className="bss-help">{help}</p>}
          {precedence && <p className="bss-help bss-precedence">{precedence}</p>}
          {children}
          {save && (
            <div className="bss-actions">
              <button type="button" className="bi-save-btn" disabled={!save.dirty || save.state.saving} onClick={save.onSave}>
                {save.state.saving ? 'Saving…' : (save.label ?? 'Save')}
              </button>
              {save.extra}
              <SectionStatus {...save.state} />
            </div>
          )}
          {status && (
            <div className="bss-actions">
              {status.busy && <span className="bss-help">Saving…</span>}
              {status.note}
              <SectionStatus saving={!!status.busy} error={status.error ?? null} saved={!!status.saved} />
            </div>
          )}
          {after}
        </div>
      )}
    </section>
  )
}

export function SectionStatus({ saving, error, saved }: { saving: boolean; error: string | null; saved: boolean }) {
  return (
    <>
      {error && <div className="bridge-error bss-error">{error}</div>}
      {!error && saved && !saving && <span className="bss-saved">Saved.</span>}
    </>
  )
}

/** The scopes and where each is edited, for the index at the top of the
 *  Settings page. `page` is a `BridgeRoutes` key or a plain description. */
export const SETTINGS_SCOPE_INDEX: { scope: SettingsScope; where: string; route: 'settings' | 'instances' | 'tools' | 'kanban' | 'principals' | 'chat' | 'messageWaiting' }[] = [
  { scope: 'global', where: 'This page.', route: 'settings' },
  { scope: 'harness', where: 'This page, one section per harness.', route: 'settings' },
  { scope: 'instance', where: 'Instances (configuration, credentials), Tools (which tools an instance is offered) and Hooks (commands wired in at spawn, also global and per session).', route: 'instances' },
  { scope: 'board', where: 'Kanban → ⚙ Board settings: defaults, tag rules, classifier, ladder, working week, message triggers.', route: 'kanban' },
  { scope: 'principal', where: 'Principals: availability, time off, grants.', route: 'principals' },
  { scope: 'session', where: 'The chat header: model, effort, permission mode for one session.', route: 'chat' },
  { scope: 'browser', where: 'Messages → Waiting on you → Alert settings: the sound and pop-ups for a new message, kept in this browser only.', route: 'messageWaiting' },
]
