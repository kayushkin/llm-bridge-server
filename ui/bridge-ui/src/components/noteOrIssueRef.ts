import type { FetchFn } from '../types'

// A note's or issue's title and state, for a kanban link row that names one.
// What is left of the chat's reference-chip data after the chips moved to
// chat-core; the kanban's link list is the one reader.

// The fields a link row shows — for any item type in the one id space notes
// and issues share (note, rank and workspace in notes, todo in issues).
// `type` comes from the item itself, which is the authority.
export interface NoteOrIssueRef {
  type: string
  title: string
  status: string
  priority: number
  tags: string[]
  due_at: string
  updated_at: string
  held_at: string | null
  deleted_at: string | null
}

// getNoteOrIssue reads one id from notes, and from issues when notes has no
// such item: the two hold disjoint ids, so this finds the one that has it.
async function getNoteOrIssue(fetchFn: FetchFn, notesBasePath: string, issuesBasePath: string, itemId: string): Promise<unknown> {
  const itemPath = `/items/${encodeURIComponent(itemId)}`
  const notes = await fetchFn(`${notesBasePath}${itemPath}`)
  if (notes.ok) return notes.json()
  if (notes.status !== 404) throw new Error(`notes: ${notes.status} ${notes.statusText}`)
  const issues = await fetchFn(`${issuesBasePath}${itemPath}`)
  if (!issues.ok) throw new Error(`issues: ${issues.status} ${issues.statusText}`)
  return issues.json()
}

function str(o: Record<string, unknown>, k: string): string {
  const v = o[k]
  return typeof v === 'string' ? v : ''
}

// Eager fetches (session core + todo) run once per chip on mount. A chat with
// the same id repeated many times would otherwise refetch per chip, so results
// are cached by id for a short window and shared across chips.
const CORE_TTL_MS = 30_000
const noteOrIssueCache = new Map<string, { at: number; p: Promise<NoteOrIssueRef> }>()

function cached<T>(cache: Map<string, { at: number; p: Promise<T> }>, key: string, make: () => Promise<T>): Promise<T> {
  const now = Date.now()
  const hit = cache.get(key)
  if (hit && now - hit.at < CORE_TTL_MS) return hit.p
  const p = make()
  cache.set(key, { at: now, p })
  // A rejected fetch must not be cached as a permanent failure.
  p.catch(() => { if (cache.get(key)?.p === p) cache.delete(key) })
  return p
}

export function fetchNoteOrIssueRef(fetchFn: FetchFn, notesBasePath: string, issuesBasePath: string, itemId: string): Promise<NoteOrIssueRef> {
  return cached(noteOrIssueCache, itemId, async () => {
    const raw = await getNoteOrIssue(fetchFn, notesBasePath, issuesBasePath, itemId)
    const o = (raw ?? {}) as Record<string, unknown>
    const tags = Array.isArray(o.tags) ? (o.tags as unknown[]).filter((t): t is string => typeof t === 'string') : []
    return {
      type: str(o, 'type'),
      title: str(o, 'title'),
      status: str(o, 'status'),
      priority: typeof o.priority === 'number' ? o.priority : 0,
      tags,
      due_at: str(o, 'due_at'),
      updated_at: str(o, 'updated_at'),
      held_at: typeof o.held_at === 'string' ? o.held_at : null,
      deleted_at: typeof o.deleted_at === 'string' ? o.deleted_at : null,
    }
  })
}
