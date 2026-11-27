// The projects view's pure rules: project-store's paths, the project tree
// (sub-projects under their parent, streams apart), the sidebar's grouping of
// sessions by project, and what each project card counts. The components in
// `components/chat/ProjectsView.tsx`, `Sidebar.tsx` and `SessionHeader.tsx`
// fetch and draw; every rule they apply is here, so it has a test.
//
// Every record type is project-store's own, generated from its Go structs
// (`@kayushkin/project-store-types`). The list answers are unnamed maps on the
// Go side, so only their envelopes are written here.

import type { SessionSummary } from '@kayushkin/chat-core'
import type {
  ActivityEvent,
  Commit,
  EntityTypeInfo,
  Link,
  LinkedNote,
  Project,
  RepoRollup,
  Rollup,
  SessionFiling,
  Suggestion,
} from '@kayushkin/project-store-types'

/** `GET /projects` */
export interface ProjectListAnswer {
  projects: Project[]
}

/** `GET /links` and `GET /projects/{id}/links` */
export interface LinkListAnswer {
  links: Link[]
}

/** `GET /session-filings` */
export interface SessionFilingListAnswer {
  session_filings: SessionFiling[]
}

/** `POST /session-filings/{id}/accept` */
export interface AcceptedSessionFilingAnswer {
  session_filing: SessionFiling
  link: Link
}

/** `GET /vocabulary` */
export interface ProjectVocabulary {
  kinds: string[]
  stages: string[]
  default_stage: string
  entity_types: EntityTypeInfo[]
  activity_kinds: string[]
  suggestion_statuses: string[]
}

/** `GET /projects/{id}/activity` */
export interface ActivityAnswer {
  /** Unix seconds: the oldest moment the events reach back to. */
  since: number
  events: ActivityEvent[]
}

/** `GET` and `POST /projects/{id}/suggestions` */
export interface SuggestionListAnswer {
  suggestions: Suggestion[]
}

/** `POST /suggestions/{id}/accept` */
export interface AcceptedSuggestionAnswer {
  suggestion: Suggestion
  note: LinkedNote
}

/** The entity type a filed session carries in project-store. */
export const SESSION_ENTITY_TYPE = 'session'

/** project-store's kind for a project that never finishes. */
export const STREAM_KIND = 'stream'

export function projectsPath(base: string): string {
  return `${base}/projects`
}

export function projectRollupPath(base: string, projectId: string): string {
  return `${base}/projects/${encodeURIComponent(projectId)}/rollup`
}

export function projectLinksPath(base: string, projectId: string): string {
  return `${base}/projects/${encodeURIComponent(projectId)}/links`
}

export function projectLinkPath(base: string, projectId: string, linkId: number): string {
  return `${projectLinksPath(base, projectId)}/${linkId}`
}

/** Every filed session in one call. */
export function sessionLinksPath(base: string): string {
  return `${base}/links?entity_type=${SESSION_ENTITY_TYPE}`
}

export function vocabularyPath(base: string): string {
  return `${base}/vocabulary`
}

export function projectPath(base: string, projectId: string): string {
  return `${base}/projects/${encodeURIComponent(projectId)}`
}

export function projectActivityPath(base: string, projectId: string, days: number): string {
  return `${projectPath(base, projectId)}/activity?days=${days}`
}

export function projectNextStepsPath(base: string, projectId: string): string {
  return `${projectPath(base, projectId)}/next-steps`
}

export function projectSuggestionsPath(base: string, projectId: string): string {
  return `${projectPath(base, projectId)}/suggestions`
}

export function suggestionDecisionPath(base: string, suggestionId: string, decision: 'accept' | 'dismiss'): string {
  return `${base}/suggestions/${encodeURIComponent(suggestionId)}/${decision}`
}

/** The filings of the session filer that wait for a person: chats it was not
 *  sure enough of to file by itself. `proposed` is project-store's status. */
export function proposedSessionFilingsPath(base: string): string {
  return `${base}/session-filings?status=proposed`
}

export function sessionFilingDecisionPath(base: string, filingId: string, decision: 'accept' | 'dismiss'): string {
  return `${base}/session-filings/${encodeURIComponent(filingId)}/${decision}`
}

/** The body of an accept: who decided, and the project when the person picks
 *  one other than the filing proposes (project-store uses the proposed one
 *  when the body names none). */
export function acceptSessionFilingBody(decidedBy: string, projectId: string | null): string {
  return JSON.stringify(projectId ? { decided_by: decidedBy, project_id: projectId } : { decided_by: decidedBy })
}

/** The filer's odds for the project it proposes, as "70%"; null when it
 *  proposes none. */
export function sessionFilingOdds(filing: Pick<SessionFiling, 'project_id' | 'confidence'>): string | null {
  return filing.project_id ? `${Math.round(filing.confidence * 100)}%` : null
}

/** The waiting filings in the order to decide them: those that propose a
 *  project first, the likeliest first, then those that propose none; within
 *  each, the newest judgment first. */
export function sessionFilingsToDecide(filings: readonly SessionFiling[]): SessionFiling[] {
  return [...filings].sort(
    (a, b) =>
      Number(b.project_id !== '') - Number(a.project_id !== '') ||
      b.confidence - a.confidence ||
      b.created_at - a.created_at ||
      b.id.localeCompare(a.id),
  )
}

/** The route for one linked note or issue: a todo is an issue, under the
 *  host's issues base path; any other type is a note, under its notes base
 *  path. The item's own `type` decides, never the link. */
export function noteOrIssuePath(notesBase: string, issuesBase: string, itemType: string, itemId: string): string {
  const base = itemType === 'todo' ? issuesBase : notesBase
  return `${base}/items/${encodeURIComponent(itemId)}`
}

/** The body that files a session under a project. */
export function fileSessionBody(sessionId: string, createdBy: string): string {
  return JSON.stringify({ entity_type: SESSION_ENTITY_TYPE, entity_ref: sessionId, created_by: createdBy })
}

// ---------------------------------------------------------------------------
// The tree
// ---------------------------------------------------------------------------

/** A project in tree order, with how deep it sits. */
export interface ProjectTreeRow {
  project: Project
  depth: number
}

/** The landing page's two sections, each in tree order. */
export interface ProjectSections {
  projects: ProjectTreeRow[]
  streams: ProjectTreeRow[]
}

function byName(a: Project, b: Project): number {
  return a.name.localeCompare(b.name)
}

/**
 * The projects in tree order: each top-level project by name, and under it its
 * children by name, depth first. A project whose parent is not in the list is
 * drawn at the top level rather than lost. A cycle cannot come from the store
 * (it refuses one); in a bad answer the visited set stops the loop and the
 * projects in it are drawn at the top level.
 */
export function projectTree(projects: readonly Project[]): ProjectTreeRow[] {
  const ids = new Set(projects.map((p) => p.id))
  const childrenOf = new Map<string, Project[]>()
  const roots: Project[] = []
  for (const project of projects) {
    if (project.parent_id && ids.has(project.parent_id) && project.parent_id !== project.id) {
      const list = childrenOf.get(project.parent_id) ?? []
      list.push(project)
      childrenOf.set(project.parent_id, list)
    } else {
      roots.push(project)
    }
  }
  const out: ProjectTreeRow[] = []
  const visited = new Set<string>()
  const walk = (project: Project, depth: number): void => {
    if (visited.has(project.id)) return
    visited.add(project.id)
    out.push({ project, depth })
    for (const child of (childrenOf.get(project.id) ?? []).sort(byName)) walk(child, depth + 1)
  }
  for (const root of roots.sort(byName)) walk(root, 0)
  // Anything the walk never reached sits in a cycle; draw it rather than lose it.
  for (const project of [...projects].sort(byName)) walk(project, 0)
  return out
}

/** Split the tree by the kind of each top-level project: a stream and
 *  everything under it go to the streams section. */
export function projectSections(projects: readonly Project[]): ProjectSections {
  const sections: ProjectSections = { projects: [], streams: [] }
  let rootIsStream = false
  for (const row of projectTree(projects)) {
    if (row.depth === 0) rootIsStream = row.project.kind === STREAM_KIND
    ;(rootIsStream ? sections.streams : sections.projects).push(row)
  }
  return sections
}

/** A project's name with its ancestors', "LLM bridge platform › Access and gating". */
export function projectPathName(project: Project, projectsById: ReadonlyMap<string, Project>): string {
  const names = [project.name]
  const seen = new Set([project.id])
  let parent = projectsById.get(project.parent_id)
  while (parent && !seen.has(parent.id)) {
    names.unshift(parent.name)
    seen.add(parent.id)
    parent = projectsById.get(parent.parent_id)
  }
  return names.join(' › ')
}

// ---------------------------------------------------------------------------
// Filed sessions
// ---------------------------------------------------------------------------

/** Session id → the links that file it, oldest first. One session may be filed
 *  under several projects. */
export function sessionFilings(links: readonly Link[]): Map<string, Link[]> {
  const out = new Map<string, Link[]>()
  for (const link of links) {
    if (link.entity_type !== SESSION_ENTITY_TYPE) continue
    const list = out.get(link.entity_ref) ?? []
    list.push(link)
    out.set(link.entity_ref, list)
  }
  for (const list of out.values()) list.sort((a, b) => a.created_at - b.created_at || a.id - b.id)
  return out
}

/** Project id → the session ids filed under it. */
export function sessionsFiledByProject(links: readonly Link[]): Map<string, string[]> {
  const out = new Map<string, string[]>()
  for (const link of links) {
    if (link.entity_type !== SESSION_ENTITY_TYPE) continue
    const list = out.get(link.project_id) ?? []
    if (!list.includes(link.entity_ref)) list.push(link.entity_ref)
    out.set(link.project_id, list)
  }
  return out
}

/** One group of the sidebar when it is grouped by project. */
export interface SessionProjectGroup {
  /** The collapse key: the project id, or '' for the sessions filed nowhere. */
  key: string
  /** The project, or null for the "Not filed" group. */
  project: Project | null
  /** "Parent › Child", or "Not filed". */
  label: string
  sessions: SessionSummary[]
}

export const NOT_FILED_LABEL = 'Not filed'

/**
 * Regroup the sidebar's sessions under the projects they are filed under.
 *
 * `sessions` is the list as the sidebar shows it (filtered, in its own order),
 * and each group keeps that order. Groups follow the project tree; a project
 * with no session on screen gets no group. A session filed under two projects
 * appears in both. Sessions filed nowhere — or only under a project the list
 * of projects does not hold, such as an archived one — go to "Not filed", last.
 */
export function groupSessionsByProject(
  sessions: readonly SessionSummary[],
  links: readonly Link[],
  projects: readonly Project[],
): SessionProjectGroup[] {
  const projectsById = new Map(projects.map((p) => [p.id, p]))
  const filings = sessionFilings(links)
  const byProject = new Map<string, SessionSummary[]>()
  const notFiled: SessionSummary[] = []
  const seen = new Set<string>()
  for (const session of sessions) {
    // The flat list can hold a row twice only by mistake; one group row each.
    if (seen.has(session.sessionId)) continue
    seen.add(session.sessionId)
    const projectIds = [...new Set((filings.get(session.sessionId) ?? []).map((l) => l.project_id))]
      .filter((id) => projectsById.has(id))
    if (projectIds.length === 0) {
      notFiled.push(session)
      continue
    }
    for (const id of projectIds) {
      const list = byProject.get(id) ?? []
      list.push(session)
      byProject.set(id, list)
    }
  }
  const groups: SessionProjectGroup[] = []
  for (const { project } of projectTree(projects)) {
    const list = byProject.get(project.id)
    if (!list) continue
    groups.push({ key: project.id, project, label: projectPathName(project, projectsById), sessions: list })
  }
  if (notFiled.length > 0) {
    groups.push({ key: '', project: null, label: NOT_FILED_LABEL, sessions: notFiled })
  }
  return groups
}

// ---------------------------------------------------------------------------
// What a project card counts
// ---------------------------------------------------------------------------

/** kanban-store's shared state that means "waiting on someone": the one the
 *  card highlights. */
export const WAITING_WORK_STATE = 'waiting'

/** The rollup's open-card counts, waiting first and the rest by name, leaving
 *  out states with none. The states are whatever the rollup names. */
export function openCardCounts(rollup: Pick<Rollup, 'cards_by_work_state'>): Array<{ workState: string; count: number }> {
  return Object.entries(rollup.cards_by_work_state ?? {})
    .filter(([, count]) => count > 0)
    .map(([workState, count]) => ({ workState, count }))
    .sort((a, b) =>
      a.workState === WAITING_WORK_STATE ? -1 : b.workState === WAITING_WORK_STATE ? 1 : a.workState.localeCompare(b.workState),
    )
}

/** The rollup's cards grouped by work state, in the order `openCardCounts`
 *  uses, with the ones whose column maps to no state under ''. */
export function cardsByWorkState(rollup: Pick<Rollup, 'cards'>): Array<{ workState: string; cards: Rollup['cards'] }> {
  const groups = new Map<string, Rollup['cards']>()
  for (const card of rollup.cards ?? []) {
    const list = groups.get(card.work_state) ?? []
    list.push(card)
    groups.set(card.work_state, list)
  }
  return [...groups.entries()]
    .map(([workState, cards]) => ({ workState, cards }))
    .sort((a, b) =>
      a.workState === WAITING_WORK_STATE ? -1 : b.workState === WAITING_WORK_STATE ? 1 : a.workState.localeCompare(b.workState),
    )
}

export function unmergedBranchCount(rollup: Pick<Rollup, 'repos'>): number {
  return (rollup.repos ?? []).reduce((n, repo) => n + (repo.unmerged_branches ?? []).length, 0)
}

/** What the landing card says about the sessions filed under one project. */
export interface FiledSessionSpend {
  count: number
  /** Summed over the sessions whose spend is known. */
  spendUsd: number
  /** Sessions with no summary in hand, or whose summary carries no spend. */
  sessionsWithoutSpend: number
}

export function filedSessionSpend(
  sessionIds: readonly string[],
  summaries: ReadonlyMap<string, SessionSummary>,
): FiledSessionSpend {
  let spendUsd = 0
  let sessionsWithoutSpend = 0
  for (const id of sessionIds) {
    const spend = summaries.get(id)?.spendUsd
    if (typeof spend === 'number') spendUsd += spend
    else sessionsWithoutSpend += 1
  }
  return { count: sessionIds.length, spendUsd, sessionsWithoutSpend }
}

/**
 * The newest moment anything happened in a project, in unix seconds: the
 * project's own change, a link added, a deploy, a commit on the default
 * branch or an unmerged one, or a filed session moving. 0 when nothing carries a time.
 */
export function lastActivitySeconds(
  rollup: Pick<Rollup, 'project' | 'links' | 'repos'>,
  summaries: ReadonlyMap<string, SessionSummary>,
): number {
  let newest = rollup.project.updated_at || 0
  for (const link of rollup.links ?? []) {
    newest = Math.max(newest, link.created_at)
    if (link.entity_type === SESSION_ENTITY_TYPE) {
      const updatedAt = summaries.get(link.entity_ref)?.updatedAt
      const ms = updatedAt ? Date.parse(updatedAt) : NaN
      if (Number.isFinite(ms)) newest = Math.max(newest, Math.floor(ms / 1000))
    }
  }
  for (const repo of rollup.repos ?? []) {
    if (repo.latest_deploy) newest = Math.max(newest, repo.latest_deploy.created_at)
    for (const branch of repo.unmerged_branches ?? []) newest = Math.max(newest, branch.committed_at)
    for (const commit of repo.recent_commits ?? []) newest = Math.max(newest, commit.committed_at)
  }
  return newest
}

/** A project's links other than filed sessions, grouped by entity type in the
 *  order the store's vocabulary lists the types; a type the vocabulary does
 *  not name comes last, by name. */
export function ownedThingsByType(
  links: readonly Link[],
  entityTypes: readonly EntityTypeInfo[],
): Array<{ entityType: string; info: EntityTypeInfo | null; links: Link[] }> {
  const order = new Map(entityTypes.map((t, i) => [t.type, i]))
  const groups = new Map<string, Link[]>()
  for (const link of links) {
    if (link.entity_type === SESSION_ENTITY_TYPE) continue
    const list = groups.get(link.entity_type) ?? []
    list.push(link)
    groups.set(link.entity_type, list)
  }
  return [...groups.entries()]
    .map(([entityType, list]) => ({
      entityType,
      info: entityTypes.find((t) => t.type === entityType) ?? null,
      links: list,
    }))
    .sort((a, b) => {
      const ai = order.get(a.entityType) ?? Number.MAX_SAFE_INTEGER
      const bi = order.get(b.entityType) ?? Number.MAX_SAFE_INTEGER
      return ai - bi || a.entityType.localeCompare(b.entityType)
    })
}

/** A section heading for an entity type, from the type's own name:
 *  `scheduler_job` → "Scheduler jobs". */
export function entityTypeHeading(entityType: string): string {
  const words = entityType.replace(/_/g, ' ')
  return words.charAt(0).toUpperCase() + words.slice(1) + 's'
}

/** What moving a session to `targetProjectId` does to its filings: which
 *  project to file it under (null when it is already there) and which of its
 *  current links to delete. `targetProjectId` null takes it out of every
 *  project. The new link is written before any old one is deleted, so a
 *  failed write leaves the session where it was. */
export function sessionMovePlan(
  currentLinks: readonly Link[],
  targetProjectId: string | null,
): { fileUnder: string | null; unfile: Link[] } {
  const alreadyThere = targetProjectId !== null && currentLinks.some((l) => l.project_id === targetProjectId)
  return {
    fileUnder: targetProjectId !== null && !alreadyThere ? targetProjectId : null,
    unfile: currentLinks.filter((l) => l.project_id !== targetProjectId),
  }
}

// ---------------------------------------------------------------------------
// Next steps, open issues and activity
// ---------------------------------------------------------------------------

/** Who the projects page names as the author of what it writes, in
 *  project-store's `created_by`, `changed_by` and `decided_by`. */
export const PROJECTS_PAGE_AUTHOR = 'bridge-ui projects page'

/** The spans of activity the page offers, in days. */
export const ACTIVITY_DAY_CHOICES = [7, 14, 30, 90] as const

/** A branch whose last commit is older than this is called stranded: nobody
 *  is at work on it, and its work has not landed. ~/CLAUDE.md counts a tree
 *  untouched for 30 minutes as nobody's; a day leaves room for a session
 *  that paused. */
export const STRANDED_BRANCH_AGE_SECONDS = 24 * 60 * 60

/** How the chat writes a repo so it becomes a chip: `repo:<name>`. */
export function repoMention(repoName: string): string {
  return `repo:${repoName}`
}

/** How the chat writes a commit so it becomes a chip: `<repo>@<sha>`, seven
 *  characters of sha. */
export function commitMention(repoName: string, sha: string): string {
  return `${repoName}@${sha.slice(0, 7)}`
}

function isOpenTodo(note: LinkedNote): boolean {
  return note.type === 'todo' && note.status === 'open'
}

/** The project's next steps: its linked todos still open, in the rollup's
 *  order (most urgent first). */
export function openNextSteps(rollup: Pick<Rollup, 'notes'>): LinkedNote[] {
  return (rollup.notes ?? []).filter(isOpenTodo)
}

/** Linked todos already finished, newest first. */
export function finishedNextSteps(rollup: Pick<Rollup, 'notes'>): LinkedNote[] {
  return (rollup.notes ?? [])
    .filter((n) => n.type === 'todo' && n.status === 'done')
    .sort((a, b) => b.completed_at.localeCompare(a.completed_at))
}

/** Linked notes that are not todos: decisions and designs, and anything of
 *  another noteboard type. */
export function linkedNotesOtherThanTodos(rollup: Pick<Rollup, 'notes'>): LinkedNote[] {
  return (rollup.notes ?? []).filter((n) => n.type !== 'todo')
}

/**
 * How many of the repo's recent default-branch commits are newer than what is
 * deployed. The deployed sha's place in the list says it exactly; a deploy
 * older than the list falls back to comparing times. null when the repo was
 * never deployed, since "undeployed" means nothing for a repo with no deploy.
 */
export function undeployedCommitCount(repo: Pick<RepoRollup, 'recent_commits' | 'latest_deploy'>): number | null {
  const deploy = repo.latest_deploy
  if (!deploy) return null
  const commits = repo.recent_commits ?? []
  const index = commits.findIndex((c) => c.sha === deploy.commit_sha || c.sha.startsWith(deploy.commit_sha))
  if (index >= 0) return index
  return commits.filter((c) => c.committed_at > deploy.created_at).length
}

/** What on one project wants a person, counted. */
export interface ProjectAttention {
  /** Questions the project's sessions ask (`projectSessionIds` in
   *  `projectViews.ts`), counted by the caller from the open signals. */
  openQuestions: number
  waitingCards: number
  openPullRequests: number
  strandedBranches: number
  undeployedCommits: number
  suggestionsToDecide: number
  missingNotes: number
}

export function projectAttention(
  rollup: Pick<Rollup, 'cards_by_work_state' | 'repos' | 'suggestions' | 'missing_note_ids'>,
  nowSeconds: number,
  openQuestions: number,
): ProjectAttention {
  const attention: ProjectAttention = {
    openQuestions,
    waitingCards: rollup.cards_by_work_state?.[WAITING_WORK_STATE] ?? 0,
    openPullRequests: 0,
    strandedBranches: 0,
    undeployedCommits: 0,
    suggestionsToDecide: (rollup.suggestions ?? []).length,
    missingNotes: (rollup.missing_note_ids ?? []).length,
  }
  for (const repo of rollup.repos ?? []) {
    attention.openPullRequests += (repo.open_pull_requests ?? []).length
    attention.strandedBranches += (repo.unmerged_branches ?? []).filter(
      (b) => nowSeconds - b.committed_at > STRANDED_BRANCH_AGE_SECONDS,
    ).length
    attention.undeployedCommits += undeployedCommitCount(repo) ?? 0
  }
  return attention
}

export function attentionTotal(attention: ProjectAttention): number {
  return Object.values(attention).reduce((n, v) => n + v, 0)
}

/** The start of the local day holding `seconds`, in unix seconds. */
export function localDayStart(seconds: number): number {
  const date = new Date(seconds * 1000)
  date.setHours(0, 0, 0, 0)
  return Math.floor(date.getTime() / 1000)
}

/**
 * Commits on the default branches per local day, the oldest day first and
 * today last, over `days` days. Each commit counts once however many of the
 * project's repos list it.
 */
export function commitsPerDay(repos: ReadonlyArray<Pick<RepoRollup, 'recent_commits'>>, days: number, nowSeconds: number): number[] {
  const counts = new Array<number>(days).fill(0)
  const today = localDayStart(nowSeconds)
  const seen = new Set<string>()
  for (const repo of repos) {
    for (const commit of repo.recent_commits ?? []) {
      if (seen.has(commit.sha)) continue
      seen.add(commit.sha)
      const daysAgo = Math.round((today - localDayStart(commit.committed_at)) / 86_400)
      if (daysAgo >= 0 && daysAgo < days) counts[days - 1 - daysAgo] += 1
    }
  }
  return counts
}

/** One local day of activity. */
export interface ActivityDay {
  /** Unix seconds at the day's local midnight. */
  dayStart: number
  events: ActivityEvent[]
}

/** Events grouped by local day, keeping their order (the store's, newest
 *  first). */
export function activityByDay(events: readonly ActivityEvent[]): ActivityDay[] {
  const days: ActivityDay[] = []
  for (const event of events) {
    const dayStart = localDayStart(event.at)
    const last = days[days.length - 1]
    if (last && last.dayStart === dayStart) last.events.push(event)
    else days.push({ dayStart, events: [event] })
  }
  return days
}

/** The newest commit across the project's repos, or null. */
export function newestCommit(repos: ReadonlyArray<Pick<RepoRollup, 'recent_commits'>>): Commit | null {
  let newest: Commit | null = null
  for (const repo of repos) {
    for (const commit of repo.recent_commits ?? []) {
      if (!newest || commit.committed_at > newest.committed_at) newest = commit
    }
  }
  return newest
}
