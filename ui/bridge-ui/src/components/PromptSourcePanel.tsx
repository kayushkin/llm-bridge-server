import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type React from 'react'
import type { FetchFn } from '../types'
import {
  PROMPT_OUTPUT_STATE_LABEL, allTags, annotationFromLabelDrafts, collectionNeedsRender, describeDriftOperation, fileEditStateLabel,
  collectionsForTabs, contextCollectionCreateBody, contextResolveQuery, filterSectionGroups, groupSections, promptOutputState, sectionWriteBody,
  type PromptCollectionView, type PromptDeliveryOptions, type PromptDrift, type PromptHarnessDelivery,
  type PromptRenderResult, type PromptSection, type PromptSectionRevision, type ResolvedContext,
} from '../promptSource'

// The prompt source: one set of sections every harness receives, the files
// they render to, the edits made to those files that are waiting to be carried
// back, and how each harness is handed the result. Backed by agent-store's
// routes on the bridge's base path.

type Api = { apiFetch: FetchFn; basePath: string }

async function readError(res: Response): Promise<string> {
  const text = await res.text()
  try {
    const parsed = JSON.parse(text)
    return parsed.error || parsed.refused_reason || text
  } catch {
    return text || `HTTP ${res.status}`
  }
}

function formatTime(epochSeconds: number): string {
  return new Date(epochSeconds * 1000).toLocaleString()
}

const SCOPE_ICON: Record<PromptCollectionView['collection']['scope'], string> = { global: '🌐 ', project: '📁 ', context: '🏷 ' }

/**
 * Makes a context collection: sections a session gets from its card's tags.
 * A blank root applies in every directory; a root limits it to sessions
 * working under that path.
 */
function NewContextCollectionForm({ apiFetch, basePath, onCreated }: Api & { onCreated: (id: number) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [rootPath, setRootPath] = useState('')
  const [title, setTitle] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  if (!open) return <button className="bprompt-link" onClick={() => setOpen(true)}>+ ticket context collection</button>

  const create = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await apiFetch(`${basePath}/prompt-collections`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(contextCollectionCreateBody(rootPath, title)),
      })
      if (!res.ok) throw new Error(await readError(res))
      const created: PromptCollectionView = await res.json()
      setOpen(false)
      setRootPath('')
      setTitle('')
      await onCreated(created.collection.id)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to create the collection')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="bfiles-actions">
      <input className="bfiles-search" placeholder="Title" value={title} onChange={e => setTitle(e.target.value)} />
      <input className="bfiles-search" placeholder="Limit to a directory (blank = every directory)" value={rootPath} onChange={e => setRootPath(e.target.value)} />
      <button className="bfiles-btn-primary" disabled={busy} onClick={create}>{busy ? 'Creating…' : 'Create'}</button>
      <button className="bfiles-btn" disabled={busy} onClick={() => setOpen(false)}>Cancel</button>
      {error && <span className="bridge-error">{error}</span>}
    </div>
  )
}

export function PromptSourcePanel({ apiFetch, basePath, reloadSignal, onViews, openCollection }: Api & {
  reloadSignal: number
  /** Called with every load, so the Files list can tell which files are renders. */
  onViews?: (views: PromptCollectionView[]) => void
  /** Ask the panel to show one collection and scroll to it. `nonce` makes a repeat request count. */
  openCollection?: { id: number; nonce: number } | null
}) {
  const panelRef = useRef<HTMLDivElement>(null)
  const [views, setViews] = useState<PromptCollectionView[]>([])
  const [options, setOptions] = useState<PromptDeliveryOptions | null>(null)
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    try {
      const [collectionsRes, optionsRes] = await Promise.all([
        apiFetch(`${basePath}/prompt-collections`),
        apiFetch(`${basePath}/prompt-delivery-options`),
      ])
      if (!collectionsRes.ok) throw new Error(await readError(collectionsRes))
      if (!optionsRes.ok) throw new Error(await readError(optionsRes))
      const data: PromptCollectionView[] = await collectionsRes.json()
      setViews(data)
      onViews?.(data)
      setOptions(await optionsRes.json())
      setSelectedId(current => current ?? data.find(view => view.collection.scope === 'global')?.collection.id ?? data[0]?.collection.id ?? null)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load the prompt source')
    } finally {
      setLoading(false)
    }
  }, [apiFetch, basePath])

  useEffect(() => { load() }, [load, reloadSignal])

  useEffect(() => {
    if (!openCollection) return
    setSelectedId(openCollection.id)
    panelRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [openCollection])

  const drifts = useMemo(() => views.flatMap(view => view.open_drifts), [views])
  const selected = views.find(view => view.collection.id === selectedId) ?? null
  const tabs = useMemo(() => collectionsForTabs(views), [views])

  if (loading) return <div className="bfiles-preview-section"><p>Loading the prompt source…</p></div>
  if (error) return <div className="bfiles-preview-section"><p className="bridge-error">Prompt source: {error}</p></div>

  return (
    <>
      {drifts.length > 0 && (
        <div className="bfiles-preview-section bprompt-drift-inbox">
          <div className="bfiles-preview-header">
            <strong>Edits made in the file — waiting for you</strong>
            <span className="bfiles-preview-hint">
              Someone edited a rendered file instead of its sections. Text changes apply by themselves, and so do added sections once
              each is tagged. What waits here removes a section, renames a heading, or adds sections still without tags. Approve to
              carry it into the sections (and out to the collection's other files); dismiss to let the next render overwrite the file.
              Either way the edited text is kept.
            </span>
          </div>
          {drifts.map(drift => (
            <DriftCard
              key={drift.id}
              drift={drift}
              sections={views.find(view => view.collection.id === drift.collection_id)?.sections ?? []}
              apiFetch={apiFetch}
              basePath={basePath}
              onSettled={load}
            />
          ))}
        </div>
      )}

      <div className="bfiles-preview-section" ref={panelRef}>
        <div className="bfiles-preview-header">
          <strong>Prompt source</strong>
          <span className="bfiles-preview-hint">
            Every prompt is edited here, as groups and sections: the host prompt every session gets, and one prompt per
            project, which a session gets when it works in that project. Each renders to the same files for every harness.
            Ticket context renders to no file: a session gets a section of it when its card carries every one of the
            section's tags.
          </span>
        </div>
        {([['Host prompt', tabs.host], ['Project prompts', tabs.projects], ['Ticket context', tabs.context]] as const).map(([label, group]) => group.length > 0 && (
          <div key={label} className="bprompt-tab-row">
            <span className="bprompt-tab-row-label">{label}</span>
            <div className="bfiles-preview-tabs">
              {group.map(view => (
                <button
                  key={view.collection.id}
                  className={`bfiles-preview-tab ${view.collection.id === selectedId ? 'bfiles-preview-tab-active' : ''}`}
                  onClick={() => setSelectedId(view.collection.id)}
                  title={view.collection.root_path}
                >
                  {SCOPE_ICON[view.collection.scope]}{view.collection.title}
                  <span className="bfiles-section-count">{view.sections.length}</span>
                  {view.open_drifts.length > 0 && <span className="bprompt-state bprompt-state-edited_on_disk">edited</span>}
                </button>
              ))}
            </div>
          </div>
        ))}
        <NewContextCollectionForm
          apiFetch={apiFetch}
          basePath={basePath}
          onCreated={async id => { await load(); setSelectedId(id) }}
        />
        {selected && options && (
          <CollectionEditor key={selected.collection.id} view={selected} options={options} apiFetch={apiFetch} basePath={basePath} onChanged={load} />
        )}
      </div>

      {options && <HarnessDeliveryPanel options={options} apiFetch={apiFetch} basePath={basePath} />}
    </>
  )
}

// ------------------------------------------------------------------ drift

function DriftCard({ drift, sections, apiFetch, basePath, onSettled }: Api & {
  drift: PromptDrift
  sections: PromptSection[]
  onSettled: () => Promise<void>
}) {
  const [tagInputs, setTagInputs] = useState<Record<number, string>>({})
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [openOperation, setOpenOperation] = useState<number | null>(null)
  const sectionsById = useMemo(() => new Map(sections.map(section => [section.id, section])), [sections])
  const canApply = drift.operations.length > 0 || !drift.held_reason
  const editedPaths = [drift.path, ...(drift.twins ?? []).map(twin => twin.path)]

  const settle = async (action: 'apply' | 'dismiss') => {
    if (action === 'dismiss' && !confirm(`Dismiss this edit to ${editedPaths.join(' and ')}? The next render will overwrite ${editedPaths.length > 1 ? 'them' : 'the file'}. The edited text stays in this drift and in the file's history.`)) return
    setBusy(true)
    setErr(null)
    try {
      const init: RequestInit = { method: 'POST' }
      if (action === 'apply') {
        init.headers = { 'Content-Type': 'application/json' }
        init.body = JSON.stringify(annotationFromLabelDrafts(drift, tagInputs, note))
      }
      const res = await apiFetch(`${basePath}/prompt-drifts/${drift.id}/${action}`, init)
      if (!res.ok) throw new Error(await readError(res))
      await onSettled()
    } catch (e) {
      setErr(e instanceof Error ? e.message : `${action} failed`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="bfiles-version-preview bprompt-drift-card">
      <div className="bfiles-version-preview-header">
        {editedPaths.map(path => <code key={path}>{path}</code>)}
        <span className="bprompt-state bprompt-state-edited_on_disk">{fileEditStateLabel(drift.status)}</span>
        <span className="bfiles-preview-hint">seen {formatTime(drift.created_at)}</span>
      </div>
      {drift.held_reason && <p className="bfiles-preview-skip">{drift.held_reason}</p>}
      {drift.annotation?.note && (
        <p className="bfiles-version-note">
          {drift.annotation.note}{drift.annotation.annotated_by ? ` — ${drift.annotation.annotated_by}` : ''}
        </p>
      )}
      <ul className="bfiles-manifest-list">
        {drift.operations.map((operation, index) => {
          const stored = drift.annotation?.inserted_sections?.find(label => label.operation_index === index)
          return (
            <li key={index} className="bprompt-drift-operation">
              <span className={`bfiles-manifest-scope bprompt-operation-${operation.kind}`}>{operation.kind}</span>
              <button className="bprompt-link" onClick={() => setOpenOperation(openOperation === index ? null : index)}>
                {describeDriftOperation(operation, sectionsById)}
              </button>
              {operation.kind === 'insert' && (
                <input
                  className="bfiles-search bprompt-tag-input"
                  placeholder="tags for the new section"
                  value={tagInputs[index] ?? (stored?.tags ?? []).join(', ')}
                  onChange={e => setTagInputs({ ...tagInputs, [index]: e.target.value })}
                />
              )}
              {openOperation === index && (
                <div className="bprompt-operation-body">
                  {operation.kind === 'update' && operation.section_id && sectionsById.get(operation.section_id) && (
                    <>
                      <div className="bfiles-preview-hint">in the sections now</div>
                      <pre className="bfiles-preview-content">{sectionsById.get(operation.section_id)!.body}</pre>
                      <div className="bfiles-preview-hint">in the file</div>
                    </>
                  )}
                  {operation.kind === 'delete' && operation.section_id
                    ? <pre className="bfiles-preview-content">{sectionsById.get(operation.section_id)?.body ?? '(section no longer exists)'}</pre>
                    : <pre className="bfiles-preview-content">{operation.body}</pre>}
                </div>
              )}
            </li>
          )
        })}
      </ul>
      <div className="bfiles-actions">
        <input className="bfiles-search" placeholder="Note for the history (optional)" value={note} onChange={e => setNote(e.target.value)} />
        <button className="bfiles-btn-primary" disabled={busy || !canApply} onClick={() => settle('apply')}>Approve</button>
        <button className="bfiles-btn" disabled={busy} onClick={() => settle('dismiss')}>Dismiss</button>
        <a className="bfiles-btn" href={`${basePath}/prompt-drifts/${drift.id}/disk-content`} target="_blank" rel="noreferrer">File as edited</a>
      </div>
      {err && <p className="bridge-error">{err}</p>}
    </div>
  )
}

// ------------------------------------------------------------- collection

function CollectionEditor({ view, options, apiFetch, basePath, onChanged }: Api & {
  view: PromptCollectionView
  options: PromptDeliveryOptions
  onChanged: () => Promise<void>
}) {
  const [tag, setTag] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  const [openSectionId, setOpenSectionId] = useState<number | null>(null)
  const [adding, setAdding] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [showRendered, setShowRendered] = useState(false)
  const [newOutput, setNewOutput] = useState('')

  const isContext = view.collection.scope === 'context'
  const tags = useMemo(() => allTags(view.sections), [view.sections])
  const tree = useMemo(() => filterSectionGroups(groupSections(view.sections), tag, query), [view.sections, tag, query])
  const [addingAfterId, setAddingAfterId] = useState<number | null>(null)
  const enabledBytes = view.rendered.length

  const post = async (path: string, body?: unknown): Promise<Response> => apiFetch(`${basePath}${path}`, {
    method: 'POST',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  const render = async () => {
    setBusy(true)
    setMessage(null)
    try {
      const res = await post(`/prompt-collections/${view.collection.id}/render`)
      if (res.status === 409) {
        const result: PromptRenderResult = await res.json()
        setMessage(result.refused_reason ?? 'Render refused')
      } else if (!res.ok) {
        throw new Error(await readError(res))
      } else {
        const result: PromptRenderResult = await res.json()
        setMessage(result.written_files.length === 0 ? 'Nothing to write: every file already matches.' : `Wrote ${result.written_files.map(file => file.path).join(', ')}`)
      }
      await onChanged()
    } catch (e) {
      setMessage(e instanceof Error ? e.message : 'Render failed')
    } finally {
      setBusy(false)
    }
  }

  const changeOutput = async (path: string, body?: unknown) => {
    setBusy(true)
    setMessage(null)
    try {
      const res = await post(path, body)
      if (!res.ok) throw new Error(await readError(res))
      setNewOutput('')
      await onChanged()
    } catch (e) {
      setMessage(e instanceof Error ? e.message : 'Failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="bfiles-preview-body">
      <div className="bfiles-meta">
        <code>{view.collection.root_path || 'every directory'}</code> · {view.sections.length} sections · {enabledBytes.toLocaleString()} characters rendered
        {view.collection.description ? ` · ${view.collection.description}` : ''}
      </div>

      {isContext ? (
        <p className="bfiles-preview-hint">
          Renders to no file. A session gets a section here when its card carries every one of the section's tags,
          compared exactly; a section with no tags reaches no session. A level-2 section goes in under its group's heading.
          Check what a card would get under “How each harness receives it”, or on the Effective config page.
        </p>
      ) : (<>
      <div className="bprompt-outputs">
        <span className="bfiles-preview-hint">Renders to</span>
        {view.outputs.map(output => {
          const state = promptOutputState(output)
          return (
            <span key={output.id} className="bprompt-output" title={output.path}>
              <code>{output.relative_path}</code>
              <span className={`bprompt-state bprompt-state-${state}`}>{PROMPT_OUTPUT_STATE_LABEL[state]}</span>
              <button className="bprompt-link" disabled={busy} onClick={() => changeOutput(`/prompt-outputs/${output.id}/${output.enabled ? 'disable' : 'enable'}`)}>
                {output.enabled ? 'stop' : 'resume'}
              </button>
            </span>
          )
        })}
        <input
          className="bfiles-search bprompt-tag-input"
          list={`bprompt-output-names-${view.collection.id}`}
          placeholder="add a file, relative to the root"
          value={newOutput}
          onChange={e => setNewOutput(e.target.value)}
        />
        <datalist id={`bprompt-output-names-${view.collection.id}`}>
          {options.prompt_file_names.map(name => <option key={name} value={name} />)}
        </datalist>
        <button className="bfiles-btn" disabled={busy || !newOutput.trim()} onClick={() => changeOutput(`/prompt-collections/${view.collection.id}/outputs`, { relative_path: newOutput.trim() })}>Add</button>
      </div>

      </>)}

      <div className="bfiles-actions">
        {!isContext && (
          <button className="bfiles-btn-primary" disabled={busy} onClick={render}>
            {busy ? 'Working…' : collectionNeedsRender(view) ? 'Render to files' : 'Render (files already match)'}
          </button>
        )}
        <button className="bfiles-btn" onClick={() => setShowRendered(!showRendered)}>{showRendered ? 'Hide' : 'Show'} {isContext ? 'every section' : 'rendered prompt'}</button>
        <input className="bfiles-search" placeholder="Filter sections…" value={query} onChange={e => setQuery(e.target.value)} />
      </div>
      {message && <p className="bfiles-scan-msg">{message}</p>}
      {showRendered && <pre className="bfiles-preview-content">{view.rendered}</pre>}

      {tags.length > 0 && (
        <div className="bprompt-tags">
          {tags.map(each => (
            <button key={each} className={`bprompt-tag ${tag === each ? 'bprompt-tag-active' : ''}`} onClick={() => setTag(tag === each ? null : each)}>{each}</button>
          ))}
        </div>
      )}

      {tree.map((entry, index) => {
        const lastInGroup = entry.children[entry.children.length - 1] ?? entry.group
        return (
          <div key={entry.group?.id ?? `ungrouped-${index}`} className="bprompt-group">
            {entry.group && (
              <SectionRow
                section={entry.group}
                summary={`${entry.children.length} section${entry.children.length === 1 ? '' : 's'} · ${entry.characters.toLocaleString()} ch`}
                isGroup
                open={openSectionId === entry.group.id}
                onToggle={() => setOpenSectionId(openSectionId === entry.group!.id ? null : entry.group!.id)}
              >
                <SectionEditor section={entry.group} options={options} apiFetch={apiFetch} basePath={basePath} onChanged={onChanged} onRefused={setMessage} />
              </SectionRow>
            )}
            <ul className={`bfiles-file-list ${entry.group ? 'bprompt-group-children' : ''}`}>
              {entry.children.map(section => (
                <SectionRow
                  key={section.id}
                  section={section}
                  summary={`${(section.heading.length + section.body.length).toLocaleString()} ch`}
                  open={openSectionId === section.id}
                  onToggle={() => setOpenSectionId(openSectionId === section.id ? null : section.id)}
                >
                  <SectionEditor section={section} options={options} apiFetch={apiFetch} basePath={basePath} onChanged={onChanged} onRefused={setMessage} />
                </SectionRow>
              ))}
            </ul>
            {lastInGroup && (addingAfterId === lastInGroup.id
              ? <SectionEditor collectionId={view.collection.id} afterSectionId={lastInGroup.id} initialLevel={2} options={options} apiFetch={apiFetch} basePath={basePath} onChanged={async () => { setAddingAfterId(null); await onChanged() }} onRefused={setMessage} />
              : <button className="bprompt-link bprompt-add-in-group" onClick={() => setAddingAfterId(lastInGroup.id)}>+ section{entry.group ? ` in ${entry.group.title}` : ''}</button>)}
          </div>
        )
      })}
      {tree.length === 0 && <p className="bfiles-empty">No section matches.</p>}

      {adding
        ? <SectionEditor collectionId={view.collection.id} initialLevel={1} options={options} apiFetch={apiFetch} basePath={basePath} onChanged={async () => { setAdding(false); await onChanged() }} onRefused={setMessage} />
        : <button className="bfiles-btn" onClick={() => setAdding(true)}>+ Add a group at the end</button>}
    </div>
  )
}

function SectionRow({ section, summary, isGroup, open, onToggle, children }: {
  section: PromptSection
  summary: string
  isGroup?: boolean
  open: boolean
  onToggle: () => void
  children: React.ReactNode
}) {
  const row = (
    <>
      <div className={`bfiles-file-header ${isGroup ? 'bprompt-group-header' : ''}`} onClick={onToggle}>
        <span className="bfiles-caret">{open ? '▾' : '▸'}</span>
        <span className="bfiles-file-title"><span className="bfiles-file-basename">{section.title}</span></span>
        {section.tags.map(each => <span key={each} className="bprompt-tag bprompt-tag-static">{each}</span>)}
        {!section.enabled && <span className="bfiles-missing-tag">off</span>}
        <span className="bfiles-version-size">{summary}</span>
      </div>
      {open && children}
    </>
  )
  return isGroup
    ? <div className={`bfiles-file ${section.enabled ? '' : 'bfiles-file-disabled'}`}>{row}</div>
    : <li className={`bfiles-file ${section.enabled ? '' : 'bfiles-file-disabled'}`}>{row}</li>
}

function SectionEditor({ section, collectionId, afterSectionId, initialLevel, options, apiFetch, basePath, onChanged, onRefused }: Api & {
  section?: PromptSection
  collectionId?: number
  afterSectionId?: number
  initialLevel?: number
  options: PromptDeliveryOptions
  onChanged: () => Promise<void>
  onRefused: (reason: string | null) => void
}) {
  const initial = {
    level: section?.level ?? initialLevel ?? 2, title: section?.title ?? '', body: section?.body ?? '',
    tagInput: (section?.tags ?? []).join(', '), enabled: section?.enabled ?? true, note: '',
  }
  const [draft, setDraft] = useState(initial)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [revisions, setRevisions] = useState<PromptSectionRevision[] | null>(null)
  const [openRevision, setOpenRevision] = useState<number | null>(null)
  const [groupTextOpen, setGroupTextOpen] = useState(false)
  const dirty = JSON.stringify({ ...draft, note: '' }) !== JSON.stringify({ ...initial, note: '' })

  const send = async (method: 'POST' | 'PUT' | 'DELETE') => {
    if (method === 'DELETE' && !confirm(`Delete the section "${section?.title}"? Its history is kept.`)) return
    setBusy(true)
    setErr(null)
    onRefused(null)
    try {
      const url = method === 'POST'
        ? `${basePath}/prompt-collections/${collectionId}/sections`
        : `${basePath}/prompt-sections/${section!.id}${method === 'DELETE' && draft.note.trim() ? `?note=${encodeURIComponent(draft.note.trim())}` : ''}`
      const res = await apiFetch(url, {
        method,
        headers: method === 'DELETE' ? undefined : { 'Content-Type': 'application/json' },
        body: method === 'DELETE' ? undefined : JSON.stringify({ ...sectionWriteBody(draft), ...(method === 'POST' && afterSectionId ? { after_section_id: afterSectionId } : {}) }),
      })
      if (!res.ok) throw new Error(await readError(res))
      const result: PromptRenderResult = await res.json()
      // The section is saved either way. A refused render is said out loud:
      // the files do not carry this change yet.
      if (result.refused_reason) onRefused(`Saved. ${result.refused_reason}`)
      await onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Failed')
    } finally {
      setBusy(false)
    }
  }

  const loadRevisions = async () => {
    if (revisions) { setRevisions(null); return }
    const res = await apiFetch(`${basePath}/prompt-sections/${section!.id}/revisions`)
    if (!res.ok) { setErr(await readError(res)); return }
    setRevisions(await res.json())
  }

  return (
    <div className="bfiles-file-body">
      <div className="bfiles-actions">
        <select className="bfiles-search bprompt-tag-input" value={draft.level} onChange={e => setDraft({ ...draft, level: Number(e.target.value) })}>
          {Array.from({ length: options.deepest_section_heading_level + 1 }, (_, level) => (
            <option key={level} value={level}>{level === 0 ? 'no heading' : level === 1 ? 'group' : 'section in a group'}</option>
          ))}
        </select>
        <input className="bfiles-search" placeholder="Title" value={draft.title} onChange={e => setDraft({ ...draft, title: e.target.value })} />
      </div>
      {draft.level === 1 && !draft.body && !groupTextOpen ? (
        <p className="bfiles-preview-hint">
          A group needs no text of its own: it holds the sections under it.{' '}
          <button className="bprompt-link" onClick={() => setGroupTextOpen(true)}>Add an introduction anyway</button>
        </p>
      ) : (
        <textarea className="bfiles-editor" value={draft.body} onChange={e => setDraft({ ...draft, body: e.target.value })} spellCheck={false} />
      )}
      <div className="bfiles-actions">
        <input className="bfiles-search" placeholder="tags, comma separated" value={draft.tagInput} onChange={e => setDraft({ ...draft, tagInput: e.target.value })} />
        <input className="bfiles-search" placeholder="Why this change (kept in history)" value={draft.note} onChange={e => setDraft({ ...draft, note: e.target.value })} />
        <label className="bfiles-meta"><input type="checkbox" checked={draft.enabled} onChange={e => setDraft({ ...draft, enabled: e.target.checked })} /> in the prompt</label>
      </div>
      <div className="bfiles-actions">
        <button className="bfiles-btn-primary" disabled={busy || !dirty} onClick={() => send(section ? 'PUT' : 'POST')}>{section ? 'Save and render' : 'Add and render'}</button>
        {section && <button className="bfiles-btn" disabled={busy} onClick={loadRevisions}>{revisions ? 'Hide history' : 'History'}</button>}
        {section && <button className="bfiles-btn" disabled={busy} onClick={() => send('DELETE')}>Delete</button>}
      </div>
      {err && <p className="bridge-error">{err}</p>}
      {revisions && (
        <ul className="bfiles-version-list">
          {revisions.map(revision => (
            <li key={revision.id}>
              <div className="bfiles-version-row" onClick={() => setOpenRevision(openRevision === revision.id ? null : revision.id)}>
                <span className="bfiles-version-time">{formatTime(revision.created_at)}</span>
                <span className="bfiles-version-source">{revision.operation} · {revision.source}{revision.drift_id ? ` #${revision.drift_id}` : ''}</span>
                <span className="bfiles-version-size">{revision.body.length.toLocaleString()} ch</span>
                {revision.note && <span className="bfiles-version-note">{revision.note}</span>}
              </div>
              {openRevision === revision.id && (
                <div className="bfiles-version-preview">
                  <div className="bfiles-version-preview-header">
                    <code>{revision.heading || '(preamble)'}</code>
                    <button className="bfiles-btn" onClick={() => setDraft({ ...draft, body: revision.body, tagInput: revision.tags.join(', '), note: `restored from revision ${revision.id}` })}>Load into the editor</button>
                  </div>
                  <pre className="bfiles-version-content">{revision.body}</pre>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- delivery

function HarnessDeliveryPanel({ options, apiFetch, basePath }: Api & { options: PromptDeliveryOptions }) {
  const [deliveries, setDeliveries] = useState<PromptHarnessDelivery[]>([])
  const [error, setError] = useState<string | null>(null)
  const [open, setOpen] = useState(false)
  const [workDir, setWorkDir] = useState('')
  const [cardTags, setCardTags] = useState('')
  const [previewHarness, setPreviewHarness] = useState<string | null>(null)
  const [preview, setPreview] = useState<ResolvedContext | null>(null)

  const load = useCallback(async () => {
    const res = await apiFetch(`${basePath}/prompt-harness-deliveries`)
    if (!res.ok) { setError(await readError(res)); return }
    setDeliveries(await res.json())
    setError(null)
  }, [apiFetch, basePath])
  useEffect(() => { if (open) load() }, [open, load])

  const save = async (delivery: PromptHarnessDelivery, change: Partial<PromptHarnessDelivery>) => {
    const next = { ...delivery, ...change }
    if (next.delivery !== 'native_file') next.native_relative_path = ''
    if (next.delivery === 'native_file' && !next.native_relative_path) next.native_relative_path = options.prompt_file_names[0] ?? ''
    const res = await apiFetch(`${basePath}/prompt-harness-deliveries/${encodeURIComponent(delivery.harness)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ delivery: next.delivery, native_relative_path: next.native_relative_path ?? '', note: 'set on the Files page' }),
    })
    if (!res.ok) { setError(await readError(res)); return }
    await load()
  }

  const showPreview = async (harness: string) => {
    if (previewHarness === harness) { setPreviewHarness(null); setPreview(null); return }
    setPreviewHarness(harness)
    setPreview(null)
    const res = await apiFetch(`${basePath}/context/resolve?${contextResolveQuery(harness, workDir, cardTags)}`)
    if (!res.ok) { setError(await readError(res)); return }
    setPreview(await res.json())
    setError(null)
  }

  return (
    <div className="bfiles-preview-section">
      <div className="bfiles-preview-header" onClick={() => setOpen(!open)} style={{ cursor: 'pointer' }}>
        <strong><span className="bfiles-caret">{open ? '▾' : '▸'}</span> How each harness receives it</strong>
        <span className="bfiles-preview-hint">Same prompt for all. The bridge injects it, except where a harness reads a rendered file on its own.</span>
      </div>
      {open && (
        <div className="bfiles-preview-body">
          {error && <p className="bridge-error">{error}</p>}
          <div className="bfiles-actions">
            <input className="bfiles-search" placeholder="Working directory for the preview (blank = host prompt only)" value={workDir} onChange={e => setWorkDir(e.target.value)} />
            <input className="bfiles-search" placeholder="Card tags for the preview, as a card would carry them" value={cardTags} onChange={e => setCardTags(e.target.value)} />
          </div>
          <ul className="bfiles-manifest-list">
            {deliveries.map(delivery => (
              <li key={delivery.harness} className="bprompt-drift-operation">
                <code className="bprompt-harness">{delivery.harness}</code>
                <select className="bfiles-search bprompt-tag-input" value={delivery.delivery} onChange={e => save(delivery, { delivery: e.target.value })}>
                  {options.deliveries.map(kind => <option key={kind} value={kind}>{kind === 'inject' ? 'bridge injects' : 'reads its own file'}</option>)}
                </select>
                {delivery.delivery === 'native_file' && (
                  <select className="bfiles-search bprompt-tag-input" value={delivery.native_relative_path ?? ''} onChange={e => save(delivery, { native_relative_path: e.target.value })}>
                    {options.prompt_file_names.map(name => <option key={name} value={name}>{name}</option>)}
                  </select>
                )}
                <button className="bprompt-link" onClick={() => showPreview(delivery.harness)}>{previewHarness === delivery.harness ? 'hide' : 'what it gets'}</button>
                {previewHarness === delivery.harness && preview && (
                  <div className="bprompt-operation-body">
                    <ul className="bfiles-manifest-list">
                      {preview.manifest.map(entry => (
                        <li key={entry.collection_id}>
                          <span className="bfiles-manifest-scope">{entry.scope}</span>
                          <code>{entry.root_path}</code>
                          <span className="bfiles-manifest-bytes">{entry.bytes.toLocaleString()} B</span>
                          <span className="bfiles-preview-hint">{entry.injected ? 'injected by the bridge' : entry.scope === 'context' ? 'no section selected' : `read by the harness from ${entry.read_natively_from}`}</span>
                          {entry.scope === 'context' && (
                            <ul className="bfiles-manifest-list">
                              {(entry.matched_sections ?? []).map(section => (
                                <li key={section.section_id}>✓ {section.title} <span className="bfiles-preview-hint">{section.tags.join(', ')}</span></li>
                              ))}
                              {(entry.unmatched_sections ?? []).map(section => (
                                <li key={section.section_id}>✗ {section.title} <span className="bfiles-preview-hint">{section.reason}</span></li>
                              ))}
                            </ul>
                          )}
                        </li>
                      ))}
                    </ul>
                    {preview.content
                      ? <pre className="bfiles-preview-content">{preview.content}</pre>
                      : <p className="bfiles-preview-skip">Nothing injected: every collection that applies is in the file this harness reads.</p>}
                  </div>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}
