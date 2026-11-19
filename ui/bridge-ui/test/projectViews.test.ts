import { describe, expect, it } from 'vitest'
import type { ActivityEvent, Link, LinkedNote, Project } from '@kayushkin/project-store-types'
import { localDayStart } from '../src/projects'
import {
  busiestDayCommits,
  commitShade,
  effortRows,
  parseLandingView,
  projectSessionIds,
  projectsByStage,
  questionCount,
  questionRequestsOfSessions,
  recentCommitCount,
  stepProgress,
  timelineDayStarts,
  timelineDays,
  timelineDaySummary,
} from '../src/projectViews'

function project(id: string, name: string, extra: Partial<Project> = {}): Project {
  return {
    id, name, kind: 'project', parent_id: '', goal: '', done_when: '', stage: 'building', spend_target_share: null,
    created_by: 'test', created_at: 100, updated_at: 100, archived_at: 0, ...extra,
  }
}

function link(entityType: string, entityRef: string): Link {
  return { id: 1, project_id: 'p', entity_type: entityType, entity_ref: entityRef, label: '', note: '', created_by: '', created_at: 0 }
}

function note(status: string, type = 'todo'): LinkedNote {
  return { link_id: 1, note_id: status, type, title: '', status, priority: 0, tags: [], due_at: '', created_at: '', updated_at: '', completed_at: '' }
}

function event(kind: string, at: number, extra: Partial<ActivityEvent> = {}): ActivityEvent {
  return { kind, at, ...extra } as ActivityEvent
}

describe("a project's sessions and their questions", () => {
  const rollup = {
    links: [link('session', 'br_filed'), link('repo', '15')],
    repos: [{
      repo_store_id: 15, name: 'dash', latest_deploy: null, open_pull_requests: [],
      unmerged_branches: [{ name: 'b', head_sha: '', committed_at: 0, worktree_path: '', session_ids: ['br_branch'] }],
      recent_commits: [
        { sha: 'a', subject: '', author_name: '', committed_at: 0, session_id: 'br_commit', on_origin_branch: true },
        { sha: 'b', subject: '', author_name: '', committed_at: 0, session_id: '', on_origin_branch: true },
      ],
    }],
  }
  it('takes filed sessions, branch sessions and commit makers', () => {
    expect([...projectSessionIds(rollup)].sort()).toEqual(['br_branch', 'br_commit', 'br_filed'])
  })
  it('keeps only question groups of those sessions, and counts their questions', () => {
    const requests = [
      { sessionId: 'br_filed', signals: [{ kind: 'question' }, { kind: 'question' }, { kind: 'notification' }] },
      { sessionId: 'br_filed', signals: [{ kind: 'notification' }] },
      { sessionId: 'br_other', signals: [{ kind: 'question' }] },
    ]
    const mine = questionRequestsOfSessions(requests, projectSessionIds(rollup))
    expect(mine).toEqual([requests[0]])
    expect(questionCount(mine)).toBe(2)
  })
})

describe('stepProgress', () => {
  it('counts finished todos of all live todos, leaving notes and archived ones out', () => {
    expect(stepProgress({ notes: [note('done'), note('open'), note('done'), note('archived'), note('open', 'note')] })).toEqual({ done: 2, total: 3 })
  })
})

describe('the timeline', () => {
  const now = localDayStart(Math.floor(Date.parse('2026-10-20T12:00:00') / 1000)) + 12 * 3600
  const starts = timelineDayStarts(3, now)
  it('lays days oldest first, ending today', () => {
    expect(starts).toHaveLength(3)
    expect(starts[2]).toBe(localDayStart(now))
    expect(starts[0]).toBeLessThan(starts[1])
  })
  it('counts each kind of event on its day and drops events outside the window', () => {
    const days = timelineDays([
      event('commit_landed', now), event('commit_landed', now - 60), event('deployed', now - 30),
      event('pull_request_merged', now - 86_400), event('note_completed', now - 86_400),
      event('project_changed', now - 86_400, { field: 'stage', new_value: 'shipping' }),
      event('project_changed', now - 86_400, { field: 'goal', new_value: 'x' }),
      event('commit_landed', now - 10 * 86_400),
    ], starts)
    expect(days.map((d) => d.commits)).toEqual([0, 0, 2])
    expect(days[2].deploys).toBe(1)
    expect(days[1]).toMatchObject({ mergedPullRequests: 1, finishedSteps: 1, stageMoves: ['shipping'] })
    expect(timelineDaySummary(days[1])).toBe('1 merged pull request\n1 step finished\nmoved to shipping')
    expect(busiestDayCommits([days])).toBe(2)
  })
  it('shades on one scale, keeping a single commit visible', () => {
    expect(commitShade(0, 40)).toBe(0)
    expect(commitShade(40, 40)).toBe(1)
    expect(commitShade(1, 40)).toBeGreaterThan(0.1)
  })
})

describe('projectsByStage', () => {
  it('keeps the vocabulary order and loses no project with an unknown stage', () => {
    const columns = projectsByStage(
      [project('b', 'B', { stage: 'shipping' }), project('a', 'A', { stage: 'idea' }), project('c', 'C', { stage: 'someday' })],
      ['idea', 'building', 'shipping'],
    )
    expect(columns.map((c) => [c.stage, c.projects.map((p) => p.id)])).toEqual([
      ['idea', ['a']], ['building', []], ['shipping', ['b']], ['someday', ['c']],
    ])
  })
})

describe('effortRows', () => {
  it('turns counts into shares and carries the target', () => {
    const rows = effortRows([
      { project: project('a', 'A', { spend_target_share: 0.5 }), commits: 30, spendUsd: 1 },
      { project: project('b', 'B'), commits: 10, spendUsd: 3 },
    ])
    expect(rows.map((r) => [r.project.id, r.commitShare, r.spendShare, r.targetShare])).toEqual([
      ['b', 0.25, 0.75, null],
      ['a', 0.75, 0.25, 0.5],
    ])
  })
  it('says 0 rather than dividing by nothing', () => {
    expect(effortRows([{ project: project('a', 'A'), commits: 0, spendUsd: 0 }])[0]).toMatchObject({ commitShare: 0, spendShare: 0 })
  })
})

describe('recentCommitCount and parseLandingView', () => {
  it('counts a commit two repos list once', () => {
    const commit = { sha: 'x', subject: '', author_name: '', committed_at: 0, session_id: '', on_origin_branch: true }
    expect(recentCommitCount({ repos: [{ recent_commits: [commit] }, { recent_commits: [commit] }] as never })).toBe(1)
  })
  it('falls back to cards for anything it does not know', () => {
    expect(parseLandingView('timeline')).toBe('timeline')
    expect(parseLandingView('nonsense')).toBe('cards')
    expect(parseLandingView(null)).toBe('cards')
  })
})
