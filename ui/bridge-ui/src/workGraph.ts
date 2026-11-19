import type {
  Branch,
  BranchSession,
  Commit,
  CommitAnswer,
  Graph,
  RefUpdate,
  Repo,
  RepoActivity,
} from '@kayushkin/work-graph-store-types'

// The work-graph view's pure rules: the paths it asks work-graph-store for,
// the lane layout that turns a topological commit list into a drawing, the
// color each session gets, and what each session did in a repo. The component
// (`components/chat/WorkGraphView.tsx`) only fetches and draws; everything here
// is covered by `test/workGraph.test.ts`.
//
// The records are work-graph-store's own, from `@kayushkin/work-graph-store-types`
// (rendered from its Go structs). The two answer envelopes below are written
// out here because the store builds them as unnamed maps, so tygo has nothing
// to render; each names only the generated records it wraps.

export type {
  Branch as WorkGraphBranch,
  BranchSession as WorkGraphBranchSession,
  Commit as WorkGraphCommit,
  CommitAnswer as WorkGraphCommitAnswer,
  Graph as WorkGraph,
  RefUpdate as WorkGraphRefUpdate,
  Repo as WorkGraphRepo,
  RepoActivity as WorkGraphRepoActivity,
}

/** `GET {workGraphStoreBasePath}/activity` */
export interface WorkGraphActivityAnswer {
  since_unix_nano: number
  repos: RepoActivity[]
}

/** `GET {workGraphStoreBasePath}/ref-updates` */
export interface WorkGraphRefUpdatesAnswer {
  ref_updates: RefUpdate[]
}

/** How far back the repo picker looks for repos sessions touched. */
export const WORK_GRAPH_ACTIVITY_HOURS = 72
/** Commits asked for on first load, and how many more each "older" press adds. */
export const WORK_GRAPH_COMMIT_PAGE = 300
/** The store refuses more than this in one graph. */
export const WORK_GRAPH_MAX_COMMITS = 5000

export function workGraphActivityPath(basePath: string, hours: number): string {
  return `${basePath}/activity?hours=${hours}`
}

export function workGraphRepoGraphPath(basePath: string, repoStoreId: number, maxCommits: number): string {
  return `${basePath}/repos/${encodeURIComponent(String(repoStoreId))}/graph?max_commits=${maxCommits}`
}

export function shortSha(sha: string): string {
  return sha.slice(0, 7)
}

// ---------------------------------------------------------------------------
// Links out

/** The chat's work graph opened on a repo, and on one commit in it when `sha`
 *  is given: `?view=work-graph&repo=<repo-store id>&commit=<sha>`. */
export function workGraphChatHref(chatRoute: string, repoStoreId: number, sha?: string): string {
  const params = new URLSearchParams({ view: 'work-graph', repo: String(repoStoreId) })
  if (sha) params.set('commit', sha)
  return `${chatRoute}?${params.toString()}`
}

/** The commit's page on GitHub, or null when the repo is not on GitHub or the
 *  commit is on no branch of origin (whose URL `github_url` is) — GitHub would
 *  answer 404 for it. */
export function githubCommitUrl(repo: Pick<Repo, 'github_url'>, commit: Pick<Commit, 'sha' | 'on_origin_branch'>): string | null {
  if (!repo.github_url || !commit.on_origin_branch) return null
  return `${repo.github_url}/commit/${commit.sha}`
}

/** A remote-tracking branch's page on GitHub, or null for a local branch (it
 *  may never have been pushed) and for a branch of any remote but `origin`,
 *  which is the remote repo-store's `github_url` is read from. */
export function githubBranchUrl(repo: Pick<Repo, 'github_url'>, branch: Pick<Branch, 'is_remote' | 'name'>): string | null {
  if (!repo.github_url || !branch.is_remote) return null
  const name = branch.name.startsWith('origin/') ? branch.name.slice('origin/'.length) : null
  if (!name || name === 'HEAD') return null
  return `${repo.github_url}/tree/${name.split('/').map(encodeURIComponent).join('/')}`
}

// ---------------------------------------------------------------------------
// Repo picker

/** One repo the picker offers. `lastActiveUnixNano` is 0 for a repo no session
 *  touched in the activity window. */
export interface WorkGraphRepoChoice {
  repo: Repo
  lastActiveUnixNano: number
}

/** The latest moment any session touched any branch of the repo. */
export function lastActivityOf(activity: RepoActivity): number {
  let latest = 0
  for (const branch of activity.branches) {
    for (const session of branch.sessions) {
      if (session.last_at_unix_nano > latest) latest = session.last_at_unix_nano
    }
  }
  return latest
}

/** Repos sessions touched recently, most recently active first; ties by name. */
export function reposByRecentActivity(activity: RepoActivity[]): WorkGraphRepoChoice[] {
  return activity
    .map(entry => ({ repo: entry.repo, lastActiveUnixNano: lastActivityOf(entry) }))
    .sort((a, b) => b.lastActiveUnixNano - a.lastActiveUnixNano || a.repo.name.localeCompare(b.repo.name))
}

/** Every other repo repo-store knows, by name, leaving out the active ones.
 *  Matched by repo-store id, which both stores share. */
export function otherRepos<R extends { id: number; name: string }>(all: R[], active: WorkGraphRepoChoice[]): R[] {
  const activeIds = new Set(active.map(choice => choice.repo.id))
  return all.filter(repo => !activeIds.has(repo.id)).sort((a, b) => a.name.localeCompare(b.name))
}

// ---------------------------------------------------------------------------
// Lane layout

/** One line segment in one row of the drawing. Columns are lane indexes.
 *  `top` runs from the row's top edge to its center, `bottom` from the center to
 *  its bottom edge, `through` the whole height in one lane. */
export interface WorkGraphSegment {
  part: 'top' | 'bottom' | 'through'
  fromColumn: number
  toColumn: number
}

export interface WorkGraphRow {
  commit: Commit
  /** The lane the commit's dot sits in. */
  column: number
  segments: WorkGraphSegment[]
  /** The rightmost lane this row draws in, so the row's text can start just
   *  past its own lines, as `git log --graph` does, rather than past the widest
   *  row's. */
  widestColumn: number
}

export interface WorkGraphLayout {
  rows: WorkGraphRow[]
  /** The widest the drawing gets, in lanes. */
  laneCount: number
}

/**
 * Lays a commit list out in lanes, the way `git log --graph` does.
 *
 * `commits` must be in git's topological order, newest first, which is how
 * work-graph-store sends them: every commit comes before its parents. Each lane
 * holds the sha it is waiting for. A commit takes the leftmost lane waiting for
 * it (or a free lane, when nothing listed points at it — a branch head); any
 * other lanes waiting for it join it there, so a fork is drawn at the commit
 * forked from. Its first parent continues its lane; each further parent (a merge) continues in the lane already waiting for
 * it, or in a new one. A lane freed stays where it is rather than shifting the
 * lanes to its right, so a line never jumps sideways except where it forks or
 * joins. A parent outside the list (older than `max_commits`) keeps its lane to
 * the bottom edge.
 */
export function layoutCommitGraph(commits: Commit[]): WorkGraphLayout {
  const lanes: (string | null)[] = []
  const rows: WorkGraphRow[] = []
  let laneCount = 0

  const freeLane = (avoid: number): number => {
    for (let index = 0; index < lanes.length; index++) {
      if (lanes[index] === null && index !== avoid) return index
    }
    lanes.push(null)
    return lanes.length - 1
  }

  for (const commit of commits) {
    const before = lanes.slice()
    const waiting: number[] = []
    before.forEach((sha, index) => { if (sha === commit.sha) waiting.push(index) })

    const column = waiting.length > 0 ? waiting[0] : freeLane(-1)
    for (const index of waiting) lanes[index] = null

    const parentColumns: number[] = []
    commit.parents.forEach((parent, parentIndex) => {
      if (parentIndex === 0) {
        // The first parent always continues the commit's own lane, even when
        // another lane already waits for it: the two lines then meet at the
        // parent's row, which is where the fork is, rather than one bending
        // into the other halfway there.
        lanes[column] = parent
        parentColumns.push(column)
        return
      }
      const existing = lanes.indexOf(parent)
      if (existing >= 0) {
        parentColumns.push(existing)
        return
      }
      const lane = freeLane(column)
      lanes[lane] = parent
      parentColumns.push(lane)
    })

    const segments: WorkGraphSegment[] = []
    before.forEach((sha, index) => {
      if (sha === null) return
      if (sha === commit.sha) {
        segments.push({ part: 'top', fromColumn: index, toColumn: column })
      } else {
        segments.push({ part: 'through', fromColumn: index, toColumn: index })
      }
    })
    for (const parentColumn of parentColumns) {
      segments.push({ part: 'bottom', fromColumn: column, toColumn: parentColumn })
    }

    while (lanes.length > 0 && lanes[lanes.length - 1] === null) lanes.pop()
    laneCount = Math.max(laneCount, before.length, lanes.length, column + 1)
    const widestColumn = segments.reduce((widest, segment) => Math.max(widest, segment.fromColumn, segment.toColumn), column)
    rows.push({ commit, column, segments, widestColumn })
  }

  return { rows, laneCount }
}

// ---------------------------------------------------------------------------
// Session colors

/** A hue in [0, 360) that is the same for a session id every time, anywhere.
 *  FNV-1a over the id's UTF-16 code units, then spread by the golden angle so
 *  ids differing in their last digit land far apart on the wheel. */
export function sessionHue(sessionId: string): number {
  let hash = 0x811c9dc5
  for (let index = 0; index < sessionId.length; index++) {
    hash ^= sessionId.charCodeAt(index)
    hash = Math.imul(hash, 0x01000193) >>> 0
  }
  return Math.round((hash * 137.508) % 360)
}

/** The fill a session's commits are drawn in. Mid lightness so it reads on the
 *  dark theme and on the zine theme's cream paper alike. */
export function sessionColor(sessionId: string): string {
  return `hsl(${sessionHue(sessionId)} 70% 52%)`
}

// ---------------------------------------------------------------------------
// What each session did in the repo

export interface WorkGraphSessionBranch {
  branch: Branch
  session: BranchSession
}

export interface WorkGraphSessionSummary {
  sessionId: string
  /** Commits in the listed graph this session made. */
  commitShas: string[]
  /** The git commands behind those commits, with how many each made. */
  commitSubcommands: Array<{ subcommand: string; count: number }>
  /** Branches the session moved or checked out, with its part in each. */
  branches: WorkGraphSessionBranch[]
  /** Every git command the session ran on this repo's branches or commits. */
  gitSubcommands: string[]
  refUpdates: number
  checkouts: number
  /** The latest moment the graph shows it acting, in unix nanoseconds. */
  lastAtUnixNano: number
}

/** Every session the graph names — as a commit's maker or on a branch — with
 *  what it did, most recently active first. An empty session id (a git command
 *  run outside any bridge session) is left out: it names no session. */
export function sessionsInGraph(graph: Graph): WorkGraphSessionSummary[] {
  const bySession = new Map<string, WorkGraphSessionSummary & { subcommandCounts: Map<string, number> }>()
  const entry = (sessionId: string) => {
    let summary = bySession.get(sessionId)
    if (!summary) {
      summary = {
        sessionId,
        commitShas: [],
        commitSubcommands: [],
        branches: [],
        gitSubcommands: [],
        refUpdates: 0,
        checkouts: 0,
        lastAtUnixNano: 0,
        subcommandCounts: new Map(),
      }
      bySession.set(sessionId, summary)
    }
    return summary
  }

  for (const commit of graph.commits) {
    const madeBy = commit.made_by
    if (madeBy == null || madeBy.session_id === '') continue
    const summary = entry(madeBy.session_id)
    summary.commitShas.push(commit.sha)
    summary.subcommandCounts.set(madeBy.git_subcommand, (summary.subcommandCounts.get(madeBy.git_subcommand) ?? 0) + 1)
    if (!summary.gitSubcommands.includes(madeBy.git_subcommand)) summary.gitSubcommands.push(madeBy.git_subcommand)
    summary.lastAtUnixNano = Math.max(summary.lastAtUnixNano, madeBy.recorded_at_unix_nano)
  }
  for (const branch of graph.branches) {
    for (const session of branch.sessions) {
      if (session.session_id === '') continue
      const summary = entry(session.session_id)
      summary.branches.push({ branch, session })
      summary.refUpdates += session.ref_updates
      summary.checkouts += session.checkouts
      for (const subcommand of session.git_subcommands) {
        if (!summary.gitSubcommands.includes(subcommand)) summary.gitSubcommands.push(subcommand)
      }
      summary.lastAtUnixNano = Math.max(summary.lastAtUnixNano, session.last_at_unix_nano)
    }
  }

  return [...bySession.values()]
    .map(({ subcommandCounts, ...summary }) => ({
      ...summary,
      commitSubcommands: [...subcommandCounts.entries()]
        .map(([subcommand, count]) => ({ subcommand, count }))
        .sort((a, b) => b.count - a.count || a.subcommand.localeCompare(b.subcommand)),
    }))
    .sort((a, b) => b.lastAtUnixNano - a.lastAtUnixNano || a.sessionId.localeCompare(b.sessionId))
}

/** Branches whose head is each commit, local ones first, then by name. */
export function branchesByHeadSha(branches: Branch[]): Map<string, Branch[]> {
  const byHead = new Map<string, Branch[]>()
  for (const branch of branches) {
    const list = byHead.get(branch.head_sha) ?? []
    list.push(branch)
    byHead.set(branch.head_sha, list)
  }
  for (const list of byHead.values()) {
    list.sort((a, b) => Number(a.is_remote) - Number(b.is_remote) || a.name.localeCompare(b.name))
  }
  return byHead
}

/** The side panel's branch order: the default branch, then the others newest
 *  head first, a local branch before its remote-tracking twin. */
export function branchesForList(branches: Branch[], defaultBranch: string): Branch[] {
  const isDefault = (branch: Branch) => !branch.is_remote && branch.name === defaultBranch
  return branches.slice().sort((a, b) =>
    Number(isDefault(b)) - Number(isDefault(a))
    || b.committed_at - a.committed_at
    || Number(a.is_remote) - Number(b.is_remote)
    || a.name.localeCompare(b.name))
}

/** Whether a branch is one the session moved or checked out. */
export function branchTouchedBy(branch: Branch, sessionId: string): boolean {
  return branch.sessions.some(session => session.session_id === sessionId)
}

/** The last path segment, for showing a worktree beside its branch; the full
 *  path goes in the title. */
export function worktreeName(path: string): string {
  const trimmed = path.replace(/\/+$/, '')
  return trimmed.slice(trimmed.lastIndexOf('/') + 1)
}
