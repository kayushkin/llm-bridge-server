// The 3D models page's data: the host's answer, as dash's
// server/three_dimensional_models.go writes it, and the pure rules the page
// draws it by.

export interface GlbSummary {
  triangles: number
  primitives: number
  materials: number
  meshes: number
  root_nodes?: string[]
}

export interface GitCommitSummary {
  sha: string
  subject: string
  authored_at: string
}

export interface ThreeDimensionalModelFile {
  path: string
  /** The work folder the file was found in: a repo, a worktree, an artifacts folder. */
  folder: string
  size_bytes: number
  modified_at: string
  summary?: GlbSummary
  summary_error?: string
  in_git_repository: boolean
  committed: boolean
  last_commit?: GitCommitSummary
}

export interface ThreeDimensionalModelRender {
  path: string
  /** Every time the agent opened this image. */
  viewed_at: string[]
  last_viewed_at: string
  /** The tool that opened it: codex's image_view or Claude Code's Read. */
  tool: string
  /** What the file is now. A file changed after last_viewed_at is no longer
   *  what the agent saw. */
  exists: boolean
  size_bytes?: number
  modified_at?: string
}

export interface ThreeDimensionalModelSession {
  session_id: string
  display_name: string
  harness: string
  state: string
  purpose: string
  created_at: string
  updated_at: string
  manager_session_id?: string
  root_session_id?: string
  lineage_error?: string
  folders: string[]
  models: ThreeDimensionalModelFile[]
  renders: ThreeDimensionalModelRender[]
}

export interface ThreeDimensionalModelRepository {
  folder: string
  models: ThreeDimensionalModelFile[]
}

export interface ThreeDimensionalModelsAnswer {
  window: string
  refreshed_at: string
  sessions: ThreeDimensionalModelSession[]
  repositories: ThreeDimensionalModelRepository[]
  problems: string[]
}

/** The address of one file the answer named. `modified` changes with the
 *  file, so the browser asks again when the agent rewrites it. */
export function threeDimensionalModelFileUrl(basePath: string, path: string, modifiedAt: string | undefined): string {
  const params = new URLSearchParams({ path })
  if (modifiedAt) params.set('modified', modifiedAt)
  return `${basePath}/file?${params.toString()}`
}

/** A session and the listed sessions started under it. */
export interface SessionTreeNode {
  session: ThreeDimensionalModelSession
  children: ThreeDimensionalModelSession[]
}

/**
 * Groups sessions under the session that started them. A session sits under
 * its root (`root_session_id`) when the root is listed too; otherwise it is a
 * top-level row of its own. Top-level rows keep the answer's order (most
 * recently active first), and so do children.
 */
export function sessionTree(sessions: ThreeDimensionalModelSession[]): SessionTreeNode[] {
  const listed = new Set(sessions.map(session => session.session_id))
  const nodes: SessionTreeNode[] = []
  const nodeBySessionId = new Map<string, SessionTreeNode>()
  for (const session of sessions) {
    if (parentOf(session, listed)) continue
    const node = { session, children: [] }
    nodes.push(node)
    nodeBySessionId.set(session.session_id, node)
  }
  for (const session of sessions) {
    const parent = parentOf(session, listed)
    if (parent) nodeBySessionId.get(parent)?.children.push(session)
  }
  return nodes
}

function parentOf(session: ThreeDimensionalModelSession, listed: Set<string>): string | null {
  const root = session.root_session_id
  if (!root || root === session.session_id || !listed.has(root)) return null
  return root
}

/** The last part of a path, with the folder above it when that tells two
 *  files with one name apart (`hoods/hybrid.glb`). */
export function shortFileLabel(path: string, folder?: string): string {
  const relative = folder && path.startsWith(folder + '/') ? path.slice(folder.length + 1) : path
  const parts = relative.split('/').filter(Boolean)
  return parts.slice(-2).join('/')
}

/** The last segment of a folder: `fantasy-movement-raid-slop-wt-races`. */
export function folderLabel(folder: string): string {
  const parts = folder.split('/').filter(Boolean)
  return parts[parts.length - 1] ?? folder
}

/** True when the render on disk is newer than the agent's last look at it. */
export function renderChangedSinceViewed(render: ThreeDimensionalModelRender): boolean {
  if (!render.exists || !render.modified_at) return false
  return new Date(render.modified_at).getTime() > new Date(render.last_viewed_at).getTime() + 1000
}

/** A short time of day in the browser's time zone, with the day when it is not today. */
export function shortTime(iso: string, now: Date = new Date()): string {
  const at = new Date(iso)
  if (!Number.isFinite(at.getTime())) return iso
  const time = at.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
  if (at.toDateString() === now.toDateString()) return time
  return `${at.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${time}`
}
