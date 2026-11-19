import { useCallback, useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { useBridgeConfig } from '../context'
import { deleteBundle, listBundles, listRepos, resolveBundles, setBundleEnabled, upsertBundle } from '../bundleStoreClient'
import { bundleDraftOf, bundleDraftToWire, emptyBundleDraft } from '../bundleDraft'
import type { BundleDraft, BundleDraftMember } from '../bundleDraft'
import { filterResourceOptions, type ResourceOption, type ToolRecord } from '../grantResources'
import { useSkillCatalog } from '../useResourceCatalogs'
import type { FetchFn } from '../types'
import type { Bundle, MemberKind as BundleMemberKind, ResolveResponse as BundleResolution } from '@kayushkin/bundle-store-types'
import type { Repo as RepoStoreRepo } from '@kayushkin/repo-store-types'
import styles from './BridgeBundles.module.css'

/**
 * Top-level Bundles page: bundle-store's curated session bundles — named,
 * inheritable sets of skills and tools a session is given by its repo's tags
 * and its task's tags — and a preview of what `POST /resolve` would hand a
 * session in a given repo.
 *
 * Left, every bundle with its match tags and members. Middle, repo-store's
 * repos with their effective tags, and the task tags to resolve with. Right,
 * the resolution for the selected repo, read again whenever the task tags
 * change. A refusal from either store is shown in place, in the server's own
 * words.
 *
 * Bundles are written here too: a composer at the top of the left column
 * creates one, and each card can be edited in that composer, enabled,
 * disabled or deleted. Every write goes through `POST /bundles` (an upsert on
 * the name) or the enable/disable/delete routes, and the list is read again
 * after, so what is shown is what the store holds.
 *
 * Renders nothing when the host passed no `bundleStoreBasePath`, which is also
 * when `BridgeLayout` shows no Bundles tab. Without `repoStoreBasePath` the
 * bundles still list and the preview column says there are no repos to pick.
 */
export function BridgeBundles() {
  const { fetch: fetchFn, bundleStoreBasePath, repoStoreBasePath } = useBridgeConfig()
  if (!bundleStoreBasePath) return null
  return <BundlesPage fetchFn={fetchFn} bundleStoreBase={bundleStoreBasePath} repoStoreBase={repoStoreBasePath} />
}

const OWNING_STORE: Record<BundleMemberKind, string> = { skill: 'skill-store', tool: 'tool-store' }
const MEMBER_CLASS: Record<BundleMemberKind, string> = { skill: styles.skill, tool: styles.tool }

function parseTaskTags(text: string): string[] {
  return text.split(',').map(t => t.trim()).filter(Boolean)
}

interface HeldResolution {
  repoID: number
  taskTagsKey: string
  value: BundleResolution
}

function BundlesPage({ fetchFn, bundleStoreBase, repoStoreBase }: {
  fetchFn: FetchFn
  bundleStoreBase: string
  repoStoreBase: string
}) {
  const [bundles, setBundles] = useState<Bundle[] | null>(null)
  const [bundlesError, setBundlesError] = useState<string | null>(null)
  const [composerDraft, setComposerDraft] = useState<BundleDraft | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [repos, setRepos] = useState<RepoStoreRepo[] | null>(null)
  const [reposError, setReposError] = useState<string | null>(null)

  const [taskTagsText, setTaskTagsText] = useState('')
  const [selectedRepo, setSelectedRepo] = useState<RepoStoreRepo | null>(null)
  const [resolution, setResolution] = useState<HeldResolution | null>(null)
  const [resolveError, setResolveError] = useState<string | null>(null)

  // Reads race: a slow resolution for one repo must not land after the answer
  // for the repo picked next. Each read takes a ticket and only the latest one
  // may set state.
  const resolveTicket = useRef(0)

  const taskTags = parseTaskTags(taskTagsText)
  const taskTagsKey = taskTags.join(',')

  // The list is read once on mount and again after every write, so a card
  // always shows the stored row rather than what the form sent.
  const reloadBundles = useCallback(async () => {
    const result = await listBundles(fetchFn, bundleStoreBase)
    if (result.ok) { setBundles(result.value ?? []); setBundlesError(null) } else setBundlesError(result.error)
  }, [fetchFn, bundleStoreBase])

  useEffect(() => { reloadBundles() }, [reloadBundles])

  const toggleEnabled = async (bundle: Bundle) => {
    setActionError(null)
    const result = await setBundleEnabled(fetchFn, bundleStoreBase, bundle.id, !bundle.enabled)
    if (!result.ok) { setActionError(result.error); return }
    await reloadBundles()
  }

  const removeBundle = async (bundle: Bundle) => {
    if (!window.confirm(`Delete bundle "${bundle.name}"? Sessions in repos it matches will stop getting its members.`)) return
    setActionError(null)
    const result = await deleteBundle(fetchFn, bundleStoreBase, bundle.id)
    if (!result.ok) { setActionError(result.error); return }
    if (composerDraft?.name === bundle.name) setComposerDraft(null)
    await reloadBundles()
  }

  useEffect(() => {
    if (!repoStoreBase) return
    let current = true
    listRepos(fetchFn, repoStoreBase).then(result => {
      if (!current) return
      if (result.ok) { setRepos(result.value ?? []); setReposError(null) } else setReposError(result.error)
    })
    return () => { current = false }
  }, [fetchFn, repoStoreBase])

  useEffect(() => {
    if (!selectedRepo) return
    const ticket = ++resolveTicket.current
    setResolveError(null)
    const tags = taskTagsKey ? taskTagsKey.split(',') : []
    resolveBundles(fetchFn, bundleStoreBase, selectedRepo.effective_tags ?? [], tags).then(result => {
      if (ticket !== resolveTicket.current) return
      if (result.ok) {
        setResolution({ repoID: selectedRepo.id, taskTagsKey, value: result.value })
      } else {
        setResolveError(result.error)
      }
    })
  }, [fetchFn, bundleStoreBase, selectedRepo, taskTagsKey])

  // Only a resolution for the repo and task tags on screen now is shown; one
  // for the previous pick would put another repo's skills under this repo's name.
  const currentResolution = resolution && selectedRepo
    && resolution.repoID === selectedRepo.id && resolution.taskTagsKey === taskTagsKey
    ? resolution.value : null

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <h2 className={styles.title}>Bundles</h2>
        <p className={styles.subtitle}>
          Curated skill and tool bundles, given to a session by its repo&apos;s tags and its task&apos;s tags.
          Pick a repo to preview what a session there would resolve to.
        </p>
      </div>

      <div className={styles.columns}>
        <section className={styles.column} aria-label="Bundles">
          <div className={styles.columnHead}>
            <h3 className={styles.heading}>Bundles{bundles ? ` (${bundles.length})` : ''}</h3>
            {!composerDraft && (
              <button type="button" className={styles.primaryButton} onClick={() => setComposerDraft(emptyBundleDraft())}>
                + New bundle
              </button>
            )}
          </div>
          {composerDraft && (
            <BundleComposer
              key={composerDraft.name || '__new__'}
              initial={composerDraft}
              bundles={bundles ?? []}
              fetchFn={fetchFn}
              bundleStoreBase={bundleStoreBase}
              onSaved={async () => { setComposerDraft(null); await reloadBundles() }}
              onCancel={() => setComposerDraft(null)}
            />
          )}
          {bundlesError && <p className={styles.error}>{bundlesError}</p>}
          {actionError && <p className={styles.error}>{actionError}</p>}
          {!bundles && !bundlesError && <div className={styles.empty}>Loading bundles…</div>}
          {bundles && bundles.length === 0 && <div className={styles.empty}>bundle-store holds no bundles.</div>}
          {bundles?.map(b => (
            <BundleCard
              key={b.id}
              bundle={b}
              onEdit={() => setComposerDraft(bundleDraftOf(b))}
              onToggleEnabled={() => toggleEnabled(b)}
              onDelete={() => removeBundle(b)}
            />
          ))}
        </section>

        <section className={styles.column} aria-label="Repos">
          <h3 className={styles.heading}>Repos{repos ? ` (${repos.length})` : ''}</h3>
          {!repoStoreBase ? (
            <div className={styles.empty}>This host proxies no repo-store, so there are no repos to preview.</div>
          ) : (
            <>
              <input
                className={styles.taskInput}
                placeholder="task tags, comma-separated (e.g. perf)"
                aria-label="Task tags"
                value={taskTagsText}
                onChange={e => setTaskTagsText(e.target.value)}
              />
              {reposError && <p className={styles.error}>{reposError}</p>}
              {!repos && !reposError && <div className={styles.empty}>Loading repos…</div>}
              {repos && repos.length === 0 && <div className={styles.empty}>repo-store holds no repos.</div>}
              {repos && repos.length > 0 && (
                <ul className={styles.repoList}>
                  {repos.map(r => {
                    const selected = selectedRepo?.id === r.id
                    return (
                      <li key={r.id}>
                        <button
                          type="button"
                          className={`${styles.repoRow} ${selected ? styles.repoRowSelected : ''}`}
                          aria-pressed={selected}
                          onClick={() => setSelectedRepo(r)}
                        >
                          <span className={styles.repoName}>{r.name}</span>
                          <span className={styles.chips}>
                            {(r.effective_tags ?? []).map(t => (
                              <span key={t} className={`${styles.chip} ${styles.repoTag}`}>{t}</span>
                            ))}
                          </span>
                        </button>
                      </li>
                    )
                  })}
                </ul>
              )}
            </>
          )}
        </section>

        <section className={styles.column} aria-label="Resolve preview">
          <h3 className={styles.heading}>Resolve preview</h3>
          {!selectedRepo && <div className={styles.empty}>Select a repo to preview its bundles.</div>}
          {selectedRepo && resolveError && <p className={styles.error}>{resolveError}</p>}
          {selectedRepo && !resolveError && !currentResolution && <div className={styles.empty}>Resolving…</div>}
          {selectedRepo && currentResolution && (
            <ResolutionCard repoName={selectedRepo.name} resolution={currentResolution} />
          )}
        </section>
      </div>
    </div>
  )
}

/** One bundle: its display name and unique name, what it extends, whether it
 *  is enabled, the tags that select it and the members it contributes. The
 *  actions are optional so a read-only host can draw the card without them. */
export function BundleCard({ bundle, onEdit, onToggleEnabled, onDelete }: {
  bundle: Bundle
  onEdit?: () => void
  onToggleEnabled?: () => void
  onDelete?: () => void
}) {
  const matchTags = bundle.match_tags ?? []
  const members = bundle.members ?? []
  return (
    <div className={styles.card}>
      <div className={styles.cardHead}>
        {bundle.display_name && <span className={styles.name}>{bundle.display_name}</span>}
        <code className={styles.bundleName}>{bundle.name}</code>
        {bundle.extends && <span className={styles.muted}>extends {bundle.extends}</span>}
        {!bundle.enabled && <span className={styles.disabledBadge}>disabled</span>}
        {(onEdit || onToggleEnabled || onDelete) && (
          <span className={styles.cardActions}>
            {onEdit && <button type="button" className={styles.smallButton} onClick={onEdit}>Edit</button>}
            {onToggleEnabled && (
              <button type="button" className={styles.smallButton} onClick={onToggleEnabled}>
                {bundle.enabled ? 'Disable' : 'Enable'}
              </button>
            )}
            {onDelete && <button type="button" className={`${styles.smallButton} ${styles.dangerButton}`} onClick={onDelete}>Delete</button>}
          </span>
        )}
      </div>
      {bundle.description && <div className={styles.description}>{bundle.description}</div>}
      {matchTags.length > 0 && (
        <div className={styles.chips}>
          {matchTags.map(t => <span key={t} className={`${styles.chip} ${styles.matchTag}`}>{t}</span>)}
        </div>
      )}
      {members.length > 0 && (
        <div className={styles.chips}>
          {members.map(m => (
            <MemberChip key={`${m.kind}:${m.id}`} kind={m.kind} id={m.id} name={m.name} condition={m.condition} />
          ))}
        </div>
      )}
      {(bundle.model || bundle.effort) && (
        <div className={styles.muted}>defaults: {[bundle.model, bundle.effort].filter(Boolean).join(' · ')}</div>
      )}
    </div>
  )
}

/** A member as a chip. The id is always shown: it is the member's identity,
 *  and the name beside it is only a label the owning store may share with
 *  another row. */
function MemberChip({ kind, id, name, condition }: {
  kind: BundleMemberKind
  id: number
  name?: string
  condition?: string
}) {
  return (
    <span className={`${styles.chip} ${MEMBER_CLASS[kind]}`} title={`${OWNING_STORE[kind]} ${kind} id ${id}`}>
      {name && <>{name} </>}
      <span className={styles.memberID}>#{id}</span>
      {condition && <em className={styles.condition}> if {condition}</em>}
    </span>
  )
}

function ResolutionCard({ repoName, resolution }: { repoName: string; resolution: BundleResolution }) {
  const bundles = resolution.bundles ?? []
  const skills = resolution.skills ?? []
  const tools = resolution.tools ?? []
  return (
    <div className={styles.card}>
      <div className={styles.cardHead}>
        <span className={styles.name}>{repoName}</span>
      </div>
      <div className={styles.previewRow}>
        <span className={styles.previewLabel}>bundles</span>
        {bundles.length
          ? bundles.map(b => <span key={b.id} className={`${styles.chip} ${styles.matchTag}`}>{b.name}</span>)
          : <span className={styles.muted}>none</span>}
      </div>
      <div className={styles.previewRow}>
        <span className={styles.previewLabel}>skills</span>
        {skills.length
          ? skills.map(s => <MemberChip key={s.id} kind="skill" id={s.id} name={s.name} />)
          : <span className={styles.muted}>none</span>}
      </div>
      <div className={styles.previewRow}>
        <span className={styles.previewLabel}>tools</span>
        {tools.length
          ? tools.map(t => <MemberChip key={t.id} kind="tool" id={t.id} name={t.name} />)
          : <span className={styles.muted}>none</span>}
      </div>
      {(resolution.model || resolution.effort) && (
        <div className={styles.previewRow}>
          <span className={styles.previewLabel}>defaults</span>
          <span className={styles.muted}>{[resolution.model, resolution.effort].filter(Boolean).join(' · ')}</span>
        </div>
      )}
    </div>
  )
}

/**
 * The form that writes a bundle. `initial` is an empty draft for a new bundle
 * or one read off a stored bundle for editing; either way the body sent is
 * `POST /bundles`, an upsert keyed on the name — so changing the name of an
 * existing bundle here writes a second bundle and leaves the first as it was,
 * and the form says so under the field.
 *
 * Members are picked from their owning stores by id: skills by searching
 * skill-store (it holds ~1,450, so they are not listed whole), tools from
 * tool-store's list. A member's name is stored beside its id for display and
 * never read back to find it.
 */
function BundleComposer({ initial, bundles, fetchFn, bundleStoreBase, onSaved, onCancel }: {
  initial: BundleDraft
  bundles: Bundle[]
  fetchFn: FetchFn
  bundleStoreBase: string
  onSaved: () => Promise<void>
  onCancel: () => void
}) {
  const [draft, setDraft] = useState<BundleDraft>(initial)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const editing = bundles.find(b => b.name === initial.name) ?? null
  const nameTaken = !editing && bundles.some(b => b.name === draft.name.trim())
  const renamed = editing !== null && draft.name.trim() !== editing.name

  const set = <K extends keyof BundleDraft>(key: K, value: BundleDraft[K]) => setDraft(d => ({ ...d, [key]: value }))
  const memberKey = (m: { kind: BundleMemberKind; id: string }) => `${m.kind}:${m.id}`
  const addMember = (member: BundleDraftMember) => setDraft(d => (
    d.members.some(m => memberKey(m) === memberKey(member)) ? d : { ...d, members: [...d.members, member] }
  ))
  const removeMember = (key: string) => setDraft(d => ({ ...d, members: d.members.filter(m => memberKey(m) !== key) }))
  const setCondition = (key: string, condition: string) => setDraft(d => ({
    ...d, members: d.members.map(m => memberKey(m) === key ? { ...m, condition } : m),
  }))

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const wire = bundleDraftToWire(draft)
    if (!wire.ok) { setSaveError(wire.error); return }
    setSaving(true)
    setSaveError(null)
    const result = await upsertBundle(fetchFn, bundleStoreBase, wire.value)
    setSaving(false)
    if (!result.ok) { setSaveError(result.error); return }
    await onSaved()
  }

  // A bundle cannot extend itself; the store refuses the cycle, and there is
  // no reason to offer it.
  const parents = bundles.filter(b => !editing || b.id !== editing.id)
  const skillIDs = new Set(draft.members.filter(m => m.kind === 'skill').map(m => m.id))
  const toolIDs = new Set(draft.members.filter(m => m.kind === 'tool').map(m => m.id))

  return (
    <form className={styles.composer} onSubmit={submit} aria-label={editing ? `Edit bundle ${editing.name}` : 'New bundle'}>
      <div className={styles.composerTitle}>{editing ? <>Editing <code>{editing.name}</code></> : 'New bundle'}</div>

      <label className={styles.field}>
        <span className={styles.fieldLabel}>name</span>
        <input className={styles.input} value={draft.name} onChange={e => set('name', e.target.value)} placeholder="go-service" required autoFocus={!editing} />
        {nameTaken && <span className={styles.fieldWarning}>a bundle named {draft.name.trim()} exists — saving overwrites it, members included</span>}
        {renamed && <span className={styles.fieldWarning}>the name is the key: this writes a new bundle and leaves {editing.name} as it is</span>}
      </label>
      <label className={styles.field}>
        <span className={styles.fieldLabel}>display name</span>
        <input className={styles.input} value={draft.displayName} onChange={e => set('displayName', e.target.value)} placeholder="Go service" />
      </label>
      <label className={styles.field}>
        <span className={styles.fieldLabel}>description</span>
        <textarea className={styles.input} rows={2} value={draft.description} onChange={e => set('description', e.target.value)} />
      </label>
      <label className={styles.field}>
        <span className={styles.fieldLabel}>extends</span>
        <select className={styles.input} value={draft.extendsID} onChange={e => set('extendsID', e.target.value)}>
          <option value="">— none (root bundle) —</option>
          {parents.map(b => <option key={b.id} value={String(b.id)}>{b.name}{b.display_name ? ` — ${b.display_name}` : ''}</option>)}
        </select>
      </label>
      <label className={styles.field}>
        <span className={styles.fieldLabel}>match tags</span>
        <input className={styles.input} value={draft.matchTagsText} onChange={e => set('matchTagsText', e.target.value)} placeholder="repo tags, comma-separated: react, frontend" />
        <span className={styles.fieldHint}>the bundle applies to a session whose repo has any of these tags; <code>base</code> applies everywhere</span>
      </label>
      <div className={styles.fieldRow}>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>model</span>
          <input className={styles.input} value={draft.model} onChange={e => set('model', e.target.value)} placeholder="optional default" />
        </label>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>effort</span>
          <input className={styles.input} value={draft.effort} onChange={e => set('effort', e.target.value)} placeholder="optional default" />
        </label>
        <label className={`${styles.field} ${styles.checkField}`}>
          <input type="checkbox" checked={draft.enabled} onChange={e => set('enabled', e.target.checked)} />
          <span className={styles.fieldLabel}>enabled</span>
        </label>
      </div>

      <div className={styles.field}>
        <span className={styles.fieldLabel}>members</span>
        {draft.members.length === 0 && <span className={styles.empty}>none yet — pick skills and tools below</span>}
        {draft.members.map(m => {
          const key = memberKey(m)
          return (
            <div key={key} className={styles.memberRow}>
              <MemberChip kind={m.kind} id={Number(m.id)} name={m.name} />
              <input
                className={`${styles.input} ${styles.conditionInput}`}
                value={m.condition}
                onChange={e => setCondition(key, e.target.value)}
                placeholder="only if task tag…"
                aria-label={`condition for ${m.kind} ${m.name || m.id}`}
              />
              <button type="button" className={styles.smallButton} onClick={() => removeMember(key)} aria-label={`remove ${m.kind} ${m.name || m.id}`}>×</button>
            </div>
          )
        })}
      </div>
      <div className={styles.fieldRow}>
        <SkillPicker exclude={skillIDs} onPick={addMember} />
        <ToolPicker exclude={toolIDs} onPick={addMember} />
      </div>

      {saveError && <p className={styles.error}>{saveError}</p>}
      <div className={styles.composerActions}>
        <button type="submit" className={styles.primaryButton} disabled={saving}>{saving ? 'Saving…' : editing ? 'Save' : 'Create'}</button>
        <button type="button" className={styles.smallButton} onClick={onCancel} disabled={saving}>Cancel</button>
      </div>
    </form>
  )
}

const NO_SKILL_IDS: readonly string[] = []

/** Search skill-store and add a hit as a skill member. Results show only once
 *  something is typed: an unqueried list is thirty arbitrary skills. */
function SkillPicker({ exclude, onPick }: { exclude: ReadonlySet<string>; onPick: (member: BundleDraftMember) => void }) {
  const [query, setQuery] = useState('')
  const catalog = useSkillCatalog(query, NO_SKILL_IDS)
  const matches = query.trim() && catalog.matches ? filterResourceOptions(catalog.matches, '', exclude) : null
  return (
    <div className={styles.picker}>
      <span className={styles.fieldLabel}>add skill</span>
      {catalog.unavailable ? <span className={styles.empty}>{catalog.unavailable}</span> : (
        <>
          <input className={styles.input} value={query} onChange={e => setQuery(e.target.value)} placeholder="search skill-store…" aria-label="Search skills" />
          {catalog.error && <span className={styles.error}>{catalog.error}</span>}
          {matches && <PickerResults kind="skill" options={matches} onPick={option => { onPick({ kind: 'skill', id: option.id, name: option.label, condition: '', effect: '' }); setQuery('') }} />}
        </>
      )}
    </div>
  )
}

/** Every tool-store tool, filtered as typed. Stores tool-store's `name` (what
 *  the seed and the resolver display), not the display name. */
function ToolPicker({ exclude, onPick }: { exclude: ReadonlySet<string>; onPick: (member: BundleDraftMember) => void }) {
  const { fetch: fetchFn, toolStoreBasePath } = useBridgeConfig()
  const [query, setQuery] = useState('')
  const [tools, setTools] = useState<ToolRecord[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!toolStoreBasePath) return
    let current = true
    fetchFn(`${toolStoreBasePath}/tools`)
      .then(async res => {
        if (!res.ok) throw new Error(`list tools: HTTP ${res.status}`)
        const body: unknown = await res.json()
        if (body === null) return []
        if (!Array.isArray(body)) throw new Error('list tools: the answer was not a list')
        return body as ToolRecord[]
      })
      .then(list => { if (current) { setTools(list); setError(null) } })
      .catch(err => { if (current) setError(err instanceof Error ? err.message : String(err)) })
    return () => { current = false }
  }, [fetchFn, toolStoreBasePath])

  if (!toolStoreBasePath) {
    return <div className={styles.picker}><span className={styles.fieldLabel}>add tool</span><span className={styles.empty}>This host has no route to tool-store, so tools cannot be picked here.</span></div>
  }
  const options: ResourceOption[] | null = tools && filterResourceOptions(
    tools.map(t => ({ id: String(t.id), label: t.name, detail: `${t.kind}${t.display_name && t.display_name !== t.name ? ` · ${t.display_name}` : ''}`, disabled: !t.enabled })),
    query, exclude,
  )
  return (
    <div className={styles.picker}>
      <span className={styles.fieldLabel}>add tool</span>
      <input className={styles.input} value={query} onChange={e => setQuery(e.target.value)} placeholder="filter tool-store…" aria-label="Filter tools" />
      {error && <span className={styles.error}>{error}</span>}
      {options && <PickerResults kind="tool" options={options} onPick={option => { onPick({ kind: 'tool', id: option.id, name: option.label, condition: '', effect: '' }); setQuery('') }} />}
    </div>
  )
}

function PickerResults({ kind, options, onPick }: { kind: BundleMemberKind; options: ResourceOption[]; onPick: (option: ResourceOption) => void }) {
  if (options.length === 0) return <span className={styles.empty}>no {kind}s match</span>
  return (
    <ul className={styles.pickerList}>
      {options.map(option => (
        <li key={option.id}>
          <button type="button" className={styles.pickerRow} onClick={() => onPick(option)} title={option.disabled ? `${OWNING_STORE[kind]} has this ${kind} disabled` : undefined}>
            <span className={`${styles.chip} ${MEMBER_CLASS[kind]}`}>{option.label} <span className={styles.memberID}>#{option.id}</span></span>
            {option.detail && <span className={styles.muted}>{option.detail}</span>}
            {option.disabled && <span className={styles.disabledBadge}>disabled</span>}
          </button>
        </li>
      ))}
    </ul>
  )
}
