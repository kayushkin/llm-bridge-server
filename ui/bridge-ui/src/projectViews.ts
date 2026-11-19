// The projects view's rules for answering sessions' questions, for progress
// toward a project's goal, and for the landing's three pictures of all the
// projects: the timeline (a row per project, a column per day), the stage
// board, and the effort chart. Like `projects.ts`, the components fetch and
// draw, and every rule they apply is here, so it has a test.

import type { ActivityEvent, Project, Rollup } from '@kayushkin/project-store-types'
import { SESSION_ENTITY_TYPE, localDayStart } from './projects'

/** chat-core's kind for a signal that asks the person something. */
const QUESTION_KIND = 'question'

/**
 * The sessions at work on a project: the ones filed under it, the ones that
 * moved its unmerged branches, and the ones that made its recent commits. A
 * question any of them asks is the project's question.
 */
export function projectSessionIds(rollup: Pick<Rollup, 'links' | 'repos'>): Set<string> {
  const ids = new Set<string>()
  for (const link of rollup.links ?? []) {
    if (link.entity_type === SESSION_ENTITY_TYPE) ids.add(link.entity_ref)
  }
  for (const repo of rollup.repos ?? []) {
    for (const branch of repo.unmerged_branches ?? []) for (const id of branch.session_ids ?? []) ids.add(id)
    for (const commit of repo.recent_commits ?? []) if (commit.session_id) ids.add(commit.session_id)
  }
  return ids
}

/** The request groups that ask a question and belong to one of `sessionIds`,
 *  in the order given. The whole group is kept, as the chat keeps it: one
 *  tool call answers as one group. */
export function questionRequestsOfSessions<R extends { sessionId: string; signals: ReadonlyArray<{ kind: string }> }>(
  requests: readonly R[],
  sessionIds: ReadonlySet<string>,
): R[] {
  return requests.filter((r) => sessionIds.has(r.sessionId) && r.signals.some((s) => s.kind === QUESTION_KIND))
}

/** How many questions a list of request groups asks. */
export function questionCount(requests: ReadonlyArray<{ signals: ReadonlyArray<{ kind: string }> }>): number {
  return requests.reduce((n, r) => n + r.signals.filter((s) => s.kind === QUESTION_KIND).length, 0)
}

/** How many of the project's linked todos are finished, of all of them. */
export interface StepProgress {
  done: number
  total: number
}

export function stepProgress(rollup: Pick<Rollup, 'notes'>): StepProgress {
  const todos = (rollup.notes ?? []).filter((n) => n.type === 'todo' && n.status !== 'archived')
  return { done: todos.filter((n) => n.status === 'done').length, total: todos.length }
}

/** One day of one project on the timeline. */
export interface TimelineDay {
  /** Unix seconds at the day's local midnight. */
  dayStart: number
  commits: number
  deploys: number
  mergedPullRequests: number
  finishedSteps: number
  /** The stages a project moved to that day, in order. */
  stageMoves: string[]
  events: ActivityEvent[]
}

/** `days` local days ending today, oldest first, as midnights in unix
 *  seconds. Each is stepped back through Date, so a daylight-saving change
 *  does not shift the days before it. */
export function timelineDayStarts(days: number, nowSeconds: number): number[] {
  const today = localDayStart(nowSeconds)
  const starts: number[] = []
  for (let back = days - 1; back >= 0; back--) {
    const date = new Date(today * 1000)
    date.setDate(date.getDate() - back)
    starts.push(Math.floor(date.getTime() / 1000))
  }
  return starts
}

/** A project's activity laid out on `dayStarts`. Events outside them are
 *  left out; each day's events are oldest first. */
export function timelineDays(events: readonly ActivityEvent[], dayStarts: readonly number[]): TimelineDay[] {
  const out: TimelineDay[] = dayStarts.map((dayStart) => ({
    dayStart, commits: 0, deploys: 0, mergedPullRequests: 0, finishedSteps: 0, stageMoves: [], events: [],
  }))
  const byStart = new Map(out.map((d) => [d.dayStart, d]))
  for (const event of [...events].sort((a, b) => a.at - b.at)) {
    const day = byStart.get(localDayStart(event.at))
    if (!day) continue
    day.events.push(event)
    if (event.kind === 'commit_landed') day.commits += 1
    else if (event.kind === 'deployed') day.deploys += 1
    else if (event.kind === 'pull_request_merged') day.mergedPullRequests += 1
    else if (event.kind === 'note_completed') day.finishedSteps += 1
    else if (event.kind === 'project_changed' && event.field === 'stage') day.stageMoves.push(event.new_value)
  }
  return out
}

/** The most commits any project made on one day, so every row of the
 *  timeline shades on the same scale. At least 1. */
export function busiestDayCommits(rows: ReadonlyArray<readonly TimelineDay[]>): number {
  let most = 1
  for (const row of rows) for (const day of row) most = Math.max(most, day.commits)
  return most
}

/** How dark a timeline cell is, 0 for no commits and 1 for the busiest day.
 *  The square root keeps one commit visible beside a day of forty. */
export function commitShade(commits: number, busiest: number): number {
  if (commits <= 0) return 0
  return Math.min(1, Math.sqrt(commits / Math.max(1, busiest)))
}

/** One line per thing that happened on a timeline day, for its hover text. */
export function timelineDaySummary(day: TimelineDay): string {
  const parts: string[] = []
  if (day.commits > 0) parts.push(`${day.commits} ${day.commits === 1 ? 'commit' : 'commits'}`)
  if (day.deploys > 0) parts.push(`${day.deploys} ${day.deploys === 1 ? 'deploy' : 'deploys'}`)
  if (day.mergedPullRequests > 0) parts.push(`${day.mergedPullRequests} merged pull ${day.mergedPullRequests === 1 ? 'request' : 'requests'}`)
  if (day.finishedSteps > 0) parts.push(`${day.finishedSteps} ${day.finishedSteps === 1 ? 'step' : 'steps'} finished`)
  for (const stage of day.stageMoves) parts.push(`moved to ${stage.replace(/_/g, ' ')}`)
  return parts.join('\n')
}

function byName(a: Project, b: Project): number {
  return a.name.localeCompare(b.name)
}

/** Projects by stage, in the vocabulary's order of stages; a stage the
 *  vocabulary does not name is put after them, so no project is lost. */
export function projectsByStage(projects: readonly Project[], stages: readonly string[]): Array<{ stage: string; projects: Project[] }> {
  const columns = new Map<string, Project[]>(stages.map((s) => [s, []]))
  for (const project of [...projects].sort(byName)) {
    const list = columns.get(project.stage) ?? []
    list.push(project)
    columns.set(project.stage, list)
  }
  return [...columns.entries()].map(([stage, list]) => ({ stage, projects: list }))
}

/** One project's row of the effort chart: its share of the commits on its
 *  repos' default branches, its share of what its filed sessions cost, and
 *  the share of spend the person set as its target. A share is 0 to 1 of the
 *  total over every project. A repo two projects own counts for both, so the
 *  commit shares can sum to more than 1. */
export interface EffortRow {
  project: Project
  commits: number
  commitShare: number
  spendUsd: number
  spendShare: number
  /** null when nobody set a target. */
  targetShare: number | null
}

export function effortRows(rows: ReadonlyArray<{ project: Project; commits: number; spendUsd: number }>): EffortRow[] {
  const totalCommits = rows.reduce((n, r) => n + r.commits, 0)
  const totalSpend = rows.reduce((n, r) => n + r.spendUsd, 0)
  return rows
    .map((r) => ({
      project: r.project,
      commits: r.commits,
      commitShare: totalCommits > 0 ? r.commits / totalCommits : 0,
      spendUsd: r.spendUsd,
      spendShare: totalSpend > 0 ? r.spendUsd / totalSpend : 0,
      targetShare: r.project.spend_target_share ?? null,
    }))
    .sort((a, b) => b.spendShare - a.spendShare || b.commitShare - a.commitShare || byName(a.project, b.project))
}

/** Commits on the project's default branches in the rollup's window, each
 *  counted once however many of its repos list it. */
export function recentCommitCount(rollup: Pick<Rollup, 'repos'>): number {
  const seen = new Set<string>()
  for (const repo of rollup.repos ?? []) for (const commit of repo.recent_commits ?? []) seen.add(commit.sha)
  return seen.size
}

/** The landing's views. */
export const LANDING_VIEWS = ['cards', 'timeline', 'stages', 'effort'] as const
export type LandingView = (typeof LANDING_VIEWS)[number]

export const LANDING_VIEW_LABELS: Record<LandingView, string> = {
  cards: 'Cards',
  timeline: 'Timeline',
  stages: 'Stages',
  effort: 'Effort',
}

/** Where the browser keeps the landing view last chosen. */
export const LANDING_VIEW_STORAGE_KEY = 'projects:landing-view'

export function parseLandingView(stored: string | null): LandingView {
  return (LANDING_VIEWS as readonly string[]).includes(stored ?? '') ? (stored as LandingView) : 'cards'
}

/** How many days the timeline shows. */
export const TIMELINE_DAYS = 30
