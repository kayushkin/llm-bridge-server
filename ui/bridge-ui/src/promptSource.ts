// Wire types and pure rules for the prompt source (agent-store's
// prompt_collections / prompt_sections / prompt_drifts), kept out of the
// component so they can be tested without rendering anything.
//
// Nothing here names a prompt file or a harness. File names and delivery kinds
// are served by agent-store at GET /prompt-delivery-options and harness ids by
// GET /prompt-harness-deliveries; a copy here would go stale the day a harness
// is added.

/**
 * global: the host prompt. project: one repo's prompt. context: sections that
 * render to no file and reach a session only when its card carries every one
 * of a section's tags; root_path is '' for every directory.
 */
export type PromptCollectionScope = 'global' | 'project' | 'context'

export interface PromptCollection {
  id: number
  slug: string
  title: string
  scope: PromptCollectionScope
  root_path: string
  description?: string
  created_at: number
  updated_at: number
}

export interface PromptSection {
  id: number
  collection_id: number
  /** 0 = text with no heading, 1 = a group, 2 = a section in the group above it. */
  level: number
  title: string
  /** The markdown line agent-store composed from level and title. Read-only. */
  heading: string
  body: string
  tags: string[]
  position: number
  enabled: boolean
  created_at: number
  updated_at: number
}

export interface PromptSectionRevision {
  id: number
  section_id: number
  collection_id: number
  operation: 'create' | 'update' | 'delete'
  title: string
  heading: string
  body: string
  tags: string[]
  position: number
  enabled: boolean
  source: string
  drift_id?: number
  note?: string
  created_at: number
}

export interface PromptCollectionOutput {
  id: number
  collection_id: number
  relative_path: string
  path: string
  enabled: boolean
  accounted_sha256?: string
  accounted_at?: number
  exists_on_disk: boolean
  disk_sha256?: string
  drifted: boolean
  matches_render: boolean
  tracked_file_id?: number
}

export interface PromptDriftOperation {
  kind: 'update' | 'insert' | 'delete'
  section_id?: number
  after_section_id?: number
  before_section_id?: number
  heading?: string
  /** On an update that changes the heading: the heading it had when the edit
   *  was found. agent-store holds such an edit for a person. */
  previous_heading?: string
  body?: string
  title?: string
  tags?: string[]
}

export interface PromptDriftInsertedSectionLabel {
  operation_index: number
  tags: string[]
}

export interface PromptDriftAnnotation {
  note?: string
  inserted_sections?: PromptDriftInsertedSectionLabel[]
  annotated_by?: string
}

export interface PromptDrift {
  id: number
  collection_id: number
  output_id: number
  path: string
  accounted_sha256?: string
  disk_sha256: string
  status: string
  held_reason?: string
  operations: PromptDriftOperation[]
  annotation?: PromptDriftAnnotation
  created_at: number
  resolved_at?: number
  /** The same edit in the collection's other files; settling this drift settles them. */
  twins?: PromptDriftTwin[]
}

export interface PromptDriftTwin {
  id: number
  output_id: number
  path: string
}

export interface PromptCollectionView {
  collection: PromptCollection
  sections: PromptSection[]
  outputs: PromptCollectionOutput[]
  rendered: string
  open_drifts: PromptDrift[]
}

export interface PromptRenderResult {
  view: PromptCollectionView
  written_files: { id: number; path: string }[]
  refused_reason?: string
}

export interface PromptHarnessDelivery {
  harness: string
  delivery: string
  native_relative_path?: string
  note?: string
  updated_at: number
}

export interface PromptDeliveryOptions {
  deliveries: string[]
  prompt_file_names: string[]
  harness_config_directory_names: string[]
  drift_statuses: string[]
  tracked_file_ignore_rule_kinds: string[]
  deepest_section_heading_level: number
}

/** One section of a context collection, and why the context tags did or did not select it. */
export interface ResolvedContextSection {
  section_id: number
  title: string
  tags: string[]
  missing_tags?: string[]
  reason: string
}

export interface ResolvedContextEntry {
  collection_id: number
  slug: string
  scope: string
  root_path: string
  bytes: number
  injected: boolean
  read_natively_from?: string
  matched_sections?: ResolvedContextSection[]
  unmatched_sections?: ResolvedContextSection[]
}

export interface ResolvedContext {
  harness: string
  work_dir?: string
  delivery: string
  context_tags: string[]
  content: string
  manifest: ResolvedContextEntry[]
}

export type PromptOutputState = 'disabled' | 'not_written' | 'edited_on_disk' | 'in_sync' | 'behind'

/**
 * What one output file looks like next to the sections, in the order the
 * answers matter: an edit on disk outranks everything, because a render
 * refuses to run until it is settled.
 */
export function promptOutputState(output: PromptCollectionOutput): PromptOutputState {
  if (!output.enabled) return 'disabled'
  if (!output.exists_on_disk) return 'not_written'
  if (output.drifted) return 'edited_on_disk'
  if (output.matches_render) return 'in_sync'
  return 'behind'
}

export const PROMPT_OUTPUT_STATE_LABEL: Record<PromptOutputState, string> = {
  disabled: 'not rendered to',
  not_written: 'not written yet',
  edited_on_disk: 'edited on disk',
  in_sync: 'in sync',
  behind: 'behind the sections',
}

/** True when a render would change at least one file. */
export function collectionNeedsRender(view: PromptCollectionView): boolean {
  return view.outputs.some(output => {
    const state = promptOutputState(output)
    return state === 'behind' || state === 'not_written'
  })
}

/** Tags typed as "a, b  c" → ['a', 'b', 'c'], lowercased, de-duplicated, sorted: the form agent-store stores. */
export function parseTagInput(input: string): string[] {
  const seen = new Set<string>()
  for (const raw of input.split(/[,\s]+/)) {
    const tag = raw.trim().toLowerCase()
    if (tag) seen.add(tag)
  }
  return [...seen].sort()
}

/**
 * Card tags typed as "a, b  c" → ['a', 'b', 'c'], exactly as typed: a card's
 * tags are compared with a section's exactly, so the preview must not change
 * their case.
 */
export function parseCardTagInput(input: string): string[] {
  return [...new Set(input.split(/[,\s]+/).map(tag => tag.trim()).filter(Boolean))]
}

/** The query for GET /context/resolve: one harness, a working directory, and card tags as repeated `tag`. */
export function contextResolveQuery(harness: string, workDir: string, cardTagInput: string): string {
  const query = new URLSearchParams({ harness, work_dir: workDir.trim() })
  for (const tag of parseCardTagInput(cardTagInput)) query.append('tag', tag)
  return query.toString()
}

/** The body of POST /prompt-collections for a context collection. A blank root means every directory. */
export function contextCollectionCreateBody(rootPathInput: string, titleInput: string) {
  return { scope: 'context' as const, root_path: rootPathInput.trim(), title: titleInput.trim() }
}

export function allTags(sections: PromptSection[]): string[] {
  const seen = new Set<string>()
  for (const section of sections) for (const tag of section.tags) seen.add(tag)
  return [...seen].sort()
}

export function sectionMatchesFilter(section: PromptSection, tag: string | null, query: string): boolean {
  if (tag && !section.tags.includes(tag)) return false
  const q = query.trim().toLowerCase()
  if (!q) return true
  return section.title.toLowerCase().includes(q) || section.heading.toLowerCase().includes(q) || section.body.toLowerCase().includes(q)
}

/** One line per operation, for the drift inbox. */
export function describeDriftOperation(operation: PromptDriftOperation, sectionsById: Map<number, PromptSection>): string {
  const known = operation.section_id ? sectionsById.get(operation.section_id) : undefined
  switch (operation.kind) {
    case 'update': {
      const was = operation.previous_heading ?? known?.heading ?? `section ${operation.section_id}`
      return operation.heading && was !== operation.heading && (operation.previous_heading || known)
        ? `edits ${was} and renames it to ${operation.heading}`
        : `edits ${was || 'the preamble'}`
    }
    case 'insert':
      return `adds ${operation.heading || 'a preamble'}`
    case 'delete':
      return `removes ${known?.heading ?? operation.heading ?? `section ${operation.section_id}`}`
  }
}

/**
 * The labels to send when approving a drift: one entry per added section, from
 * what is typed in the inbox. Keyed by operation index because that is how
 * agent-store addresses them.
 */
export function annotationFromLabelDrafts(
  drift: PromptDrift,
  tagInputByOperationIndex: Record<number, string>,
  note: string,
): PromptDriftAnnotation {
  const inserted: PromptDriftInsertedSectionLabel[] = []
  drift.operations.forEach((operation, index) => {
    if (operation.kind !== 'insert') return
    const stored = drift.annotation?.inserted_sections?.find(label => label.operation_index === index)
    const typed = tagInputByOperationIndex[index]
    inserted.push({
      operation_index: index,
      tags: typed !== undefined ? parseTagInput(typed) : stored?.tags ?? [],
    })
  })
  return { note: note.trim() || drift.annotation?.note, inserted_sections: inserted, annotated_by: drift.annotation?.annotated_by }
}

/** The body a section write sends. There is no heading: agent-store composes it from level and title. */
export function sectionWriteBody(draft: { level: number; title: string; body: string; tagInput: string; enabled: boolean; note: string }) {
  return {
    level: draft.level,
    title: draft.title.trim(),
    body: draft.body,
    tags: parseTagInput(draft.tagInput),
    enabled: draft.enabled,
    note: draft.note.trim(),
  }
}

/**
 * One row of the section tree. A level-1 section is a group and owns the
 * level-2 sections that follow it, up to the next level-1 section — the same
 * nesting the rendered markdown has. `group` is null for sections that come
 * before any group (a preamble, or level-2 sections at the very top).
 */
export interface PromptSectionGroup {
  group: PromptSection | null
  children: PromptSection[]
  /** Characters of the group's own text plus all of its children's. */
  characters: number
}

const sectionCharacters = (section: PromptSection) => section.heading.length + section.body.length

export function groupSections(sections: PromptSection[]): PromptSectionGroup[] {
  const out: PromptSectionGroup[] = []
  for (const section of sections) {
    if (section.level === 1) {
      out.push({ group: section, children: [], characters: sectionCharacters(section) })
      continue
    }
    let current = out[out.length - 1]
    if (!current) {
      current = { group: null, children: [], characters: 0 }
      out.push(current)
    }
    current.children.push(section)
    current.characters += sectionCharacters(section)
  }
  return out
}

/**
 * The tree after a filter: a group stays when it matches or any child does,
 * and a group that matches by itself keeps all its children, so filtering by a
 * group's name shows the group, not an empty header.
 */
export function filterSectionGroups(groups: PromptSectionGroup[], tag: string | null, query: string): PromptSectionGroup[] {
  if (!tag && !query.trim()) return groups
  const out: PromptSectionGroup[] = []
  for (const entry of groups) {
    const groupMatches = entry.group ? sectionMatchesFilter(entry.group, tag, query) : false
    const children = groupMatches ? entry.children : entry.children.filter(child => sectionMatchesFilter(child, tag, query))
    if (groupMatches || children.length > 0) out.push({ ...entry, children })
  }
  return out
}

/** What the Files list needs to know about a tracked file that a collection renders. */
export interface RenderedOutputRef {
  collectionId: number
  collectionTitle: string
  scope: PromptCollectionScope
  relativePath: string
  state: PromptOutputState
}

/**
 * Every rendered prompt file, by absolute path. A file in this map is an output
 * of a collection: its text is the collection's sections, so the Files list
 * sends the reader to the sections instead of offering a second, free-text way
 * to edit the same words.
 */
export function renderedOutputsByPath(views: PromptCollectionView[]): Map<string, RenderedOutputRef> {
  const out = new Map<string, RenderedOutputRef>()
  for (const view of views) {
    for (const output of view.outputs) {
      out.set(output.path, {
        collectionId: view.collection.id,
        collectionTitle: view.collection.title,
        scope: view.collection.scope,
        relativePath: output.relative_path,
        state: promptOutputState(output),
      })
    }
  }
  return out
}

/**
 * The collections worth a tab: the host prompt, project prompts by root path,
 * then context collections by root path (every-directory first). An empty
 * project collection is left out; an empty context collection is not, since
 * it is made empty on this page and filled here.
 */
export function collectionsForTabs(views: PromptCollectionView[]): { host: PromptCollectionView[]; projects: PromptCollectionView[]; context: PromptCollectionView[] } {
  const byRoot = (a: PromptCollectionView, b: PromptCollectionView) => a.collection.root_path.localeCompare(b.collection.root_path)
  const shown = views.filter(view => view.sections.length > 0 || view.outputs.length > 0)
  return {
    host: shown.filter(view => view.collection.scope === 'global'),
    projects: shown.filter(view => view.collection.scope === 'project').sort(byRoot),
    context: views.filter(view => view.collection.scope === 'context').sort(byRoot),
  }
}

/** What an edit card says about its state. agent-store's `held` means the edit
 *  waits: for tags on its new sections, or for a person; `held_reason` says
 *  which, and the card shows it verbatim. */
export function fileEditStateLabel(status: string): string {
  return status === 'held' ? 'waiting' : status
}
