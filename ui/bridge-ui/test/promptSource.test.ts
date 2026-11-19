import { describe, expect, it } from 'vitest'
import {
  annotationFromLabelDrafts, collectionNeedsRender, collectionsForTabs, contextCollectionCreateBody, contextResolveQuery, parseCardTagInput, renderedOutputsByPath, describeDriftOperation, fileEditStateLabel, filterSectionGroups, groupSections, parseTagInput,
  promptOutputState, sectionMatchesFilter,
  type PromptCollectionOutput, type PromptDrift, type PromptSection,
} from '../src/promptSource'

const output = (over: Partial<PromptCollectionOutput>): PromptCollectionOutput => ({
  id: 1, collection_id: 1, relative_path: 'X.md', path: '/h/X.md', enabled: true,
  exists_on_disk: true, drifted: false, matches_render: true, ...over,
})

const section = (over: Partial<PromptSection>): PromptSection => ({
  id: 1, collection_id: 1, level: 2, title: 'Scheduler', heading: '## Scheduler', body: 'cron jobs', tags: ['scheduler'],
  position: 100, enabled: true, created_at: 0, updated_at: 0, ...over,
})

describe('promptOutputState', () => {
  it('ranks an edit on disk above being behind', () => {
    expect(promptOutputState(output({ drifted: true, matches_render: false }))).toBe('edited_on_disk')
  })
  it('tells behind from in sync from not written', () => {
    expect(promptOutputState(output({}))).toBe('in_sync')
    expect(promptOutputState(output({ matches_render: false }))).toBe('behind')
    expect(promptOutputState(output({ exists_on_disk: false, matches_render: false }))).toBe('not_written')
    expect(promptOutputState(output({ enabled: false, drifted: true }))).toBe('disabled')
  })
  it('asks for a render only when one would write something', () => {
    const view = { collection: {} as never, sections: [], rendered: '', open_drifts: [] }
    expect(collectionNeedsRender({ ...view, outputs: [output({})] })).toBe(false)
    expect(collectionNeedsRender({ ...view, outputs: [output({}), output({ matches_render: false })] })).toBe(true)
    expect(collectionNeedsRender({ ...view, outputs: [output({ drifted: true, matches_render: false })] })).toBe(false)
  })
})

describe('tags', () => {
  it('normalizes the way agent-store stores them', () => {
    expect(parseTagInput(' Stores, scheduler  stores,')).toEqual(['scheduler', 'stores'])
    expect(parseTagInput('')).toEqual([])
  })
  it('filters on tag and text together', () => {
    expect(sectionMatchesFilter(section({}), 'scheduler', 'CRON')).toBe(true)
    expect(sectionMatchesFilter(section({}), 'stores', '')).toBe(false)
    expect(sectionMatchesFilter(section({}), null, 'plumber')).toBe(false)
  })
})

describe('drift inbox', () => {
  const drift: PromptDrift = {
    id: 3, collection_id: 1, output_id: 1, path: '/h/X.md', disk_sha256: 'abc', status: 'held',
    operations: [
      { kind: 'update', section_id: 1, heading: '## Scheduler', body: 'new' },
      { kind: 'insert', after_section_id: 1, heading: '## Reminders', body: 'one coordinator' },
      { kind: 'delete', section_id: 9, heading: '## Gone' },
    ],
    annotation: { note: 'from the tagger', annotated_by: 'tagger', inserted_sections: [{ operation_index: 1, tags: ['reminders'] }] },
    created_at: 0,
  }
  const byId = new Map([[1, section({})]])

  it('describes each operation in words', () => {
    expect(drift.operations.map(operation => describeDriftOperation(operation, byId))).toEqual([
      'edits ## Scheduler', 'adds ## Reminders', 'removes ## Gone',
    ])
    expect(describeDriftOperation({ kind: 'update', section_id: 1, heading: '## Cron' }, byId)).toBe('edits ## Scheduler and renames it to ## Cron')
  })
  it('keeps the tagger labels unless the operator typed over them, and labels inserts only', () => {
    expect(annotationFromLabelDrafts(drift, {}, '').inserted_sections).toEqual([{ operation_index: 1, tags: ['reminders'] }])
    const typed = annotationFromLabelDrafts(drift, { 1: 'Scheduler nudges' }, 'mine')
    expect(typed.inserted_sections).toEqual([{ operation_index: 1, tags: ['nudges', 'scheduler'] }])
    expect(typed.note).toBe('mine')
  })
})

describe('section tree', () => {
  const sections = [
    section({ id: 1, level: 0, title: 'Preamble', heading: '', body: 'hello', tags: [] }),
    section({ id: 2, level: 1, title: 'Services', heading: '# Services', body: '', tags: ['services'] }),
    section({ id: 3, level: 2, title: 'Scheduler', heading: '## Scheduler', body: 'cron jobs', tags: ['scheduler'] }),
    section({ id: 4, level: 2, title: 'Noteboard', heading: '## Noteboard', body: 'todos', tags: ['notes'] }),
    section({ id: 5, level: 1, title: 'Rules', heading: '# Rules', body: 'be plain', tags: [] }),
  ]
  it('nests level-2 sections under the group above them', () => {
    const tree = groupSections(sections)
    expect(tree.map(entry => [entry.group?.id ?? null, entry.children.map(child => child.id)])).toEqual([[null, [1]], [2, [3, 4]], [5, []]])
  })
  it('counts a group with no text of its own by what it holds', () => {
    const services = groupSections(sections)[1]
    expect(services.group?.body).toBe('')
    expect(services.characters).toBe('# Services'.length + '## Scheduler'.length + 'cron jobs'.length + '## Noteboard'.length + 'todos'.length)
  })
  it('keeps a group when a child matches, and all children when the group itself matches', () => {
    const tree = groupSections(sections)
    expect(filterSectionGroups(tree, null, 'cron').map(entry => [entry.group?.id ?? null, entry.children.map(child => child.id)])).toEqual([[2, [3]]])
    expect(filterSectionGroups(tree, 'services', '').map(entry => entry.children.length)).toEqual([2])
    expect(filterSectionGroups(tree, null, '')).toBe(tree)
  })
})

describe('rendered files and collection tabs', () => {
  const view = (id: number, scope: 'global' | 'project' | 'context', root: string, outputs: PromptCollectionOutput[], sections = 1) => ({
    collection: { id, slug: `c${id}`, title: `c${id}`, scope, root_path: root, created_at: 0, updated_at: 0 },
    sections: Array.from({ length: sections }, (_, i) => section({ id: id * 100 + i })), outputs, rendered: '', open_drifts: [],
  })
  const views = [
    view(3, 'project', '/r/zeta', [output({ id: 5, path: '/r/zeta/AGENTS.md', relative_path: 'AGENTS.md', drifted: true, matches_render: false })]),
    view(1, 'global', '/home', [output({ id: 1, path: '/home/AGENTS.md', relative_path: 'AGENTS.md' }), output({ id: 2, path: '/home/CLAUDE.md', relative_path: 'CLAUDE.md' })]),
    view(2, 'project', '/r/alpha', [output({ id: 3, path: '/r/alpha/CLAUDE.md', relative_path: 'CLAUDE.md' })]),
    view(4, 'project', '/r/empty', [], 0),
    view(5, 'context', '/r/alpha', [], 2),
    view(6, 'context', '', [], 0),
  ]
  it('maps every rendered file to the collection that renders it, with its state', () => {
    const byPath = renderedOutputsByPath(views)
    expect([...byPath.keys()].sort()).toEqual(['/home/AGENTS.md', '/home/CLAUDE.md', '/r/alpha/CLAUDE.md', '/r/zeta/AGENTS.md'])
    expect(byPath.get('/r/zeta/AGENTS.md')).toMatchObject({ collectionId: 3, scope: 'project', state: 'edited_on_disk' })
    expect(byPath.get('/somewhere/else.md')).toBeUndefined()
  })
  it('puts the host prompt apart from project prompts, sorts those by path, and drops empty collections', () => {
    const tabs = collectionsForTabs(views)
    expect(tabs.host.map(v => v.collection.id)).toEqual([1])
    expect(tabs.projects.map(v => v.collection.id)).toEqual([2, 3])
  })
  it('lists context collections apart, every-directory first, keeping an empty one to fill', () => {
    expect(collectionsForTabs(views).context.map(v => v.collection.id)).toEqual([6, 5])
  })
})

describe('ticket context', () => {
  it('keeps card tags exactly as typed, since they are compared exactly', () => {
    expect(parseCardTagInput(' Billing, billing  ops,')).toEqual(['Billing', 'billing', 'ops'])
  })
  it('asks for a preview with each card tag as its own parameter', () => {
    expect(contextResolveQuery('claude_code', ' /r/alpha ', 'billing ops')).toBe('harness=claude_code&work_dir=%2Fr%2Falpha&tag=billing&tag=ops')
    expect(contextResolveQuery('codex', '', '')).toBe('harness=codex&work_dir=')
  })
  it('creates a context collection with a blank root for every directory', () => {
    expect(contextCollectionCreateBody('  ', ' Billing ')).toEqual({ scope: 'context', root_path: '', title: 'Billing' })
  })
})

describe('edits made in a rendered file', () => {
  it('names a rename by the heading recorded when the edit was found', () => {
    const byId = new Map()
    expect(describeDriftOperation({ kind: 'update', section_id: 9, heading: '## Cron', previous_heading: '## Scheduler' }, byId))
      .toBe('edits ## Scheduler and renames it to ## Cron')
    expect(describeDriftOperation({ kind: 'update', section_id: 9, heading: '## Cron', previous_heading: '## Scheduler' }, new Map([[9, { heading: '## Cron' }]]) as never))
      .toBe('edits ## Scheduler and renames it to ## Cron')
  })
  it('calls a held edit waiting and leaves the other states as the store names them', () => {
    expect(fileEditStateLabel('held')).toBe('waiting')
    expect(fileEditStateLabel('open')).toBe('open')
  })
})
