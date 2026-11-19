import { describe, expect, it } from 'vitest'
import type { SessionSummary } from '@kayushkin/chat-core'
import type { Link, Project, Rollup, SessionFiling } from '@kayushkin/project-store-types'
import {
  acceptSessionFilingBody,
  proposedSessionFilingsPath,
  sessionFilingDecisionPath,
  sessionFilingOdds,
  sessionFilingsToDecide,
  NOT_FILED_LABEL,
  cardsByWorkState,
  entityTypeHeading,
  filedSessionSpend,
  groupSessionsByProject,
  lastActivitySeconds,
  openCardCounts,
  ownedThingsByType,
  projectPathName,
  projectSections,
  projectTree,
  sessionLinksPath,
  sessionMovePlan,
  sessionsFiledByProject,
  unmergedBranchCount,
  activityByDay,
  attentionTotal,
  commitMention,
  commitsPerDay,
  finishedNextSteps,
  linkedNotesOtherThanTodos,
  localDayStart,
  openNextSteps,
  projectAttention,
  repoMention,
  undeployedCommitCount,
  STRANDED_BRANCH_AGE_SECONDS,
} from '../src/projects'
import type { ActivityEvent, Commit, LinkedNote } from '@kayushkin/project-store-types'

function project(id: string, name: string, extra: Partial<Project> = {}): Project {
  return {
    id, name, kind: 'project', parent_id: '', goal: '', done_when: '', stage: 'building',
    created_by: 'test', created_at: 100, updated_at: 100, archived_at: 0, ...extra,
  }
}

let nextLinkId = 1
function link(projectId: string, entityType: string, entityRef: string, createdAt = 200): Link {
  return {
    id: nextLinkId++, project_id: projectId, entity_type: entityType, entity_ref: entityRef,
    label: entityRef, note: '', created_by: 'test', created_at: createdAt,
  }
}

function session(sessionId: string, extra: Partial<SessionSummary> = {}): SessionSummary {
  return {
    sessionId, state: 'completed', harness: 'claude_code', instanceId: 'i', type: 'interactive',
    purpose: 'chat', mode: '', folderName: '', displayName: sessionId, agentId: '', principalId: '',
    updatedAt: '2026-09-28T10:00:00Z', createdAt: '2026-09-28T09:00:00Z', harnessSessionId: '',
    managerSessionId: '', ...extra,
  }
}

const PLATFORM = project('project_000003', 'LLM bridge platform', { stage: 'keeping_up' })
const ACCESS = project('project_000004', 'Access and gating', { parent_id: 'project_000003' })
const SESSIONS = project('project_000005', 'Sessions and status', { parent_id: 'project_000003' })
const DISCORD = project('project_000002', 'Discord signups')
const PERSONAL = project('project_000015', 'Personal', { kind: 'stream', stage: 'keeping_up' })
const ALL = [SESSIONS, PERSONAL, DISCORD, ACCESS, PLATFORM]

describe('projectTree — nesting', () => {
  it('puts children under their parent, each level by name', () => {
    expect(projectTree(ALL).map((r) => [r.project.id, r.depth])).toEqual([
      ['project_000002', 0],
      ['project_000003', 0],
      ['project_000004', 1],
      ['project_000005', 1],
      ['project_000015', 0],
    ])
  })

  it('keeps a project whose parent is not in the list, at the top level', () => {
    const orphan = project('project_000099', 'Orphan', { parent_id: 'project_000098' })
    expect(projectTree([orphan]).map((r) => [r.project.id, r.depth])).toEqual([['project_000099', 0]])
  })

  it('does not loop on a cycle in a bad answer, and still draws the projects in it', () => {
    const a = project('project_000010', 'A', { parent_id: 'project_000011' })
    const b = project('project_000011', 'B', { parent_id: 'project_000010' })
    expect(projectTree([a, b]).map((r) => [r.project.id, r.depth])).toEqual([
      ['project_000010', 0],
      ['project_000011', 1],
    ])
  })

  it('splits streams from projects by the top-level project kind', () => {
    const sections = projectSections(ALL)
    expect(sections.projects.map((r) => r.project.id)).toEqual([
      'project_000002', 'project_000003', 'project_000004', 'project_000005',
    ])
    expect(sections.streams.map((r) => r.project.id)).toEqual(['project_000015'])
  })

  it('names a sub-project with its parent', () => {
    const byId = new Map(ALL.map((p) => [p.id, p]))
    expect(projectPathName(ACCESS, byId)).toBe('LLM bridge platform › Access and gating')
    expect(projectPathName(DISCORD, byId)).toBe('Discord signups')
  })
})

describe('groupSessionsByProject — the sidebar', () => {
  const links = [
    link('project_000004', 'session', 'br_1'),
    link('project_000002', 'session', 'br_2'),
    link('project_000003', 'session', 'br_3'),
    link('project_000002', 'session', 'br_3'),
    link('project_000099', 'session', 'br_5'), // a project not in the list (archived)
    link('project_000002', 'repo', '15'), // not a session
  ]

  it('groups in tree order, keeps list order inside a group, and puts Not filed last', () => {
    const sessions = [
      session('br_4'),
      session('br_3'),
      session('br_1'),
      session('br_2'),
      session('br_5'),
    ]
    const groups = groupSessionsByProject(sessions, links, ALL)
    expect(groups.map((g) => [g.key, g.label, g.sessions.map((s) => s.sessionId)])).toEqual([
      ['project_000002', 'Discord signups', ['br_3', 'br_2']],
      ['project_000003', 'LLM bridge platform', ['br_3']],
      ['project_000004', 'LLM bridge platform › Access and gating', ['br_1']],
      ['', NOT_FILED_LABEL, ['br_4', 'br_5']],
    ])
  })

  it('draws no group for a project with no session on screen, and no Not filed when all are filed', () => {
    const groups = groupSessionsByProject([session('br_1')], links, ALL)
    expect(groups.map((g) => g.key)).toEqual(['project_000004'])
  })

  it('reads the session links once for every project', () => {
    expect(sessionLinksPath('/api/projects')).toBe('/api/projects/links?entity_type=session')
    expect(Object.fromEntries(sessionsFiledByProject(links))).toEqual({
      project_000004: ['br_1'],
      project_000002: ['br_2', 'br_3'],
      project_000003: ['br_3'],
      project_000099: ['br_5'],
    })
  })
})

describe('sessionMovePlan — filing and moving', () => {
  const current = [link('project_000002', 'session', 'br_1'), link('project_000003', 'session', 'br_1')]

  it('files an unfiled session and deletes nothing', () => {
    expect(sessionMovePlan([], 'project_000004')).toEqual({ fileUnder: 'project_000004', unfile: [] })
  })

  it('moves: writes the new link and deletes every other', () => {
    const plan = sessionMovePlan(current, 'project_000004')
    expect(plan.fileUnder).toBe('project_000004')
    expect(plan.unfile.map((l) => l.project_id)).toEqual(['project_000002', 'project_000003'])
  })

  it('picking a project it is already under writes nothing and takes it out of the others', () => {
    const plan = sessionMovePlan(current, 'project_000003')
    expect(plan.fileUnder).toBeNull()
    expect(plan.unfile.map((l) => l.project_id)).toEqual(['project_000002'])
  })

  it('null takes it out of every project', () => {
    expect(sessionMovePlan(current, null).unfile).toHaveLength(2)
  })
})

describe('what a project card counts', () => {
  const rollup: Rollup = {
    project: PLATFORM,
    children: [ACCESS],
    links: [link('project_000003', 'session', 'br_3', 300), link('project_000003', 'repo', '52', 150)],
    cards: [
      { card_id: 'c1', title: 'one', status: 'open', work_state: 'working' },
      { card_id: 'c2', title: 'two', status: 'open', work_state: 'waiting' },
      { card_id: 'c3', title: 'three', status: 'open', work_state: '' },
    ],
    cards_by_work_state: { working: 1, waiting: 1, not_started: 0, unmapped: 1 },
    repos: [
      {
        repo_store_id: 52, name: 'llm-bridge-server',
        unmerged_branches: [
          { name: 'a', head_sha: 'x', committed_at: 400, worktree_path: '', session_ids: [] },
          { name: 'b', head_sha: 'y', committed_at: 50, worktree_path: '', session_ids: [] },
        ],
        latest_deploy: { commit_sha: 'z', deployed_by: 'me', created_at: 350 },
        recent_commits: [], open_pull_requests: [],
      },
      { repo_store_id: 27, name: 'llm-bridge', unmerged_branches: [], latest_deploy: null, recent_commits: [], open_pull_requests: [] },
    ],
    notes: [], missing_note_ids: [], suggestions: [],
  }

  it('puts waiting first and drops states with no cards', () => {
    expect(openCardCounts(rollup)).toEqual([
      { workState: 'waiting', count: 1 },
      { workState: 'unmapped', count: 1 },
      { workState: 'working', count: 1 },
    ])
    expect(cardsByWorkState(rollup).map((g) => g.workState)).toEqual(['waiting', '', 'working'])
  })

  it('counts unmerged branches across repos', () => {
    expect(unmergedBranchCount(rollup)).toBe(2)
  })

  it('sums known spend and counts the sessions it does not know', () => {
    const summaries = new Map([['br_3', session('br_3', { spendUsd: 2.5 })]])
    expect(filedSessionSpend(['br_3', 'br_9'], summaries)).toEqual({ count: 2, spendUsd: 2.5, sessionsWithoutSpend: 1 })
  })

  it('takes the newest of every dated thing as the last activity', () => {
    expect(lastActivitySeconds(rollup, new Map())).toBe(400)
    const later = new Map([['br_3', session('br_3', { updatedAt: '1970-01-01T00:10:00Z' })]])
    expect(lastActivitySeconds(rollup, later)).toBe(600)
  })

  it('groups owned things in the vocabulary order, leaving sessions out', () => {
    const types = [
      { type: 'repo', service: 'repo-store', meaning: '' },
      { type: 'scheduler_job', service: 'scheduler', meaning: '' },
    ]
    const owned = ownedThingsByType(
      [link('p', 'scheduler_job', '7'), link('p', 'session', 'br_1'), link('p', 'widget', 'w'), link('p', 'repo', '15')],
      types,
    )
    expect(owned.map((g) => g.entityType)).toEqual(['repo', 'scheduler_job', 'widget'])
    expect(owned[2]?.info).toBeNull()
    expect(entityTypeHeading('scheduler_job')).toBe('Scheduler jobs')
  })
})

function commit(sha: string, committedAt: number): Commit {
  return { sha, subject: sha, author_name: 'a', committed_at: committedAt, session_id: '', on_origin_branch: true }
}

function note(noteId: string, extra: Partial<LinkedNote> = {}): LinkedNote {
  return {
    link_id: 1, note_id: noteId, type: 'todo', title: noteId, status: 'open', priority: 0, tags: [],
    due_at: '', created_at: '', updated_at: '', completed_at: '', ...extra,
  }
}

describe('chip mentions', () => {
  it('writes repos and commits the way the chat turns them into chips', () => {
    expect(repoMention('dash')).toBe('repo:dash')
    expect(commitMention('dash', 'c41ed053fbb978450e54791c8b9d39f848937808')).toBe('dash@c41ed05')
  })
})

describe('next steps', () => {
  const notes = [
    note('open-urgent', { priority: 3 }),
    note('design', { type: 'note' }),
    note('done-old', { status: 'done', completed_at: '2026-10-01T00:00:00Z' }),
    note('done-new', { status: 'done', completed_at: '2026-10-10T00:00:00Z' }),
    note('open-later'),
  ]
  it('keeps open todos in the store order and finished ones newest first', () => {
    expect(openNextSteps({ notes }).map((n) => n.note_id)).toEqual(['open-urgent', 'open-later'])
    expect(finishedNextSteps({ notes }).map((n) => n.note_id)).toEqual(['done-new', 'done-old'])
    expect(linkedNotesOtherThanTodos({ notes }).map((n) => n.note_id)).toEqual(['design'])
  })
})

describe('undeployedCommitCount', () => {
  const commits = [commit('ccc', 300), commit('bbb', 200), commit('aaa', 100)]
  it('counts the commits above the deployed one', () => {
    expect(undeployedCommitCount({ recent_commits: commits, latest_deploy: { commit_sha: 'bbb', deployed_by: '', created_at: 250 } })).toBe(1)
    expect(undeployedCommitCount({ recent_commits: commits, latest_deploy: { commit_sha: 'ccc', deployed_by: '', created_at: 350 } })).toBe(0)
  })
  it('compares times when the deploy is older than the list', () => {
    expect(undeployedCommitCount({ recent_commits: commits, latest_deploy: { commit_sha: 'old', deployed_by: '', created_at: 150 } })).toBe(2)
  })
  it('says nothing for a repo never deployed', () => {
    expect(undeployedCommitCount({ recent_commits: commits, latest_deploy: null })).toBeNull()
  })
})

describe('projectAttention', () => {
  it('counts what wants a person', () => {
    const now = 10_000_000
    const attention = projectAttention({
      cards_by_work_state: { waiting: 2, working: 5 },
      missing_note_ids: ['gone'],
      suggestions: [],
      repos: [{
        repo_store_id: 1, name: 'dash', latest_deploy: { commit_sha: 'b', deployed_by: '', created_at: now - 10 },
        recent_commits: [commit('c', now - 5), commit('b', now - 100)],
        open_pull_requests: [],
        unmerged_branches: [
          { name: 'fresh', head_sha: '', committed_at: now - 60, worktree_path: '', session_ids: [] },
          { name: 'stranded', head_sha: '', committed_at: now - STRANDED_BRANCH_AGE_SECONDS - 1, worktree_path: '', session_ids: [] },
        ],
      }],
    }, now, 3)
    expect(attention).toEqual({ openQuestions: 3, waitingCards: 2, openPullRequests: 0, strandedBranches: 1, undeployedCommits: 1, suggestionsToDecide: 0, missingNotes: 1 })
    expect(attentionTotal(attention)).toBe(8)
  })
})

describe('commits and activity by local day', () => {
  const now = localDayStart(Math.floor(Date.parse('2026-10-20T12:00:00') / 1000)) + 12 * 3600
  it('counts each commit once on its local day, today last', () => {
    const repos = [
      { recent_commits: [commit('t1', now - 60), commit('y1', now - 86_400)] },
      { recent_commits: [commit('t1', now - 60), commit('old', now - 20 * 86_400)] },
    ]
    expect(commitsPerDay(repos, 3, now)).toEqual([0, 1, 1])
  })
  it('groups events by day keeping their order', () => {
    const event = (at: number) => ({ kind: 'deployed', at }) as ActivityEvent
    const days = activityByDay([event(now), event(now - 60), event(now - 86_400)])
    expect(days.map((d) => d.events.length)).toEqual([2, 1])
    expect(days[0].dayStart).toBe(localDayStart(now))
  })
})

describe('session filings — what the filer left for a person', () => {
  const filing = (id: string, projectId: string, confidence: number, createdAt: number): SessionFiling => ({
    id,
    session_id: `br_${id}`,
    session_label: id,
    evidence: 'brief',
    goal: '',
    repo_store_ids: [],
    project_id: projectId,
    confidence,
    why: '',
    model: 'model-test',
    status: 'proposed',
    link_id: 0,
    filed_project_id: '',
    created_by: 'project-store session filer',
    created_at: createdAt,
    decided_by: '',
    decided_at: 0,
  })

  it('asks project-store for the proposed filings and names the decision routes', () => {
    expect(proposedSessionFilingsPath('/api/projects')).toBe('/api/projects/session-filings?status=proposed')
    expect(sessionFilingDecisionPath('/api/projects', 'session_filing_000007', 'accept')).toBe(
      '/api/projects/session-filings/session_filing_000007/accept',
    )
    expect(sessionFilingDecisionPath('/api/projects', 'session_filing_000007', 'dismiss')).toBe(
      '/api/projects/session-filings/session_filing_000007/dismiss',
    )
  })

  it('names a project in an accept only when the person picked one', () => {
    expect(JSON.parse(acceptSessionFilingBody('bridge-ui projects page', null))).toEqual({ decided_by: 'bridge-ui projects page' })
    expect(JSON.parse(acceptSessionFilingBody('bridge-ui projects page', 'project_000002'))).toEqual({
      decided_by: 'bridge-ui projects page',
      project_id: 'project_000002',
    })
  })

  it('shows odds only for a filing that proposes a project', () => {
    expect(sessionFilingOdds(filing('a', 'project_000001', 0.7, 1))).toBe('70%')
    expect(sessionFilingOdds(filing('b', 'project_000001', 0.825, 1))).toBe('83%')
    expect(sessionFilingOdds(filing('c', '', 0, 1))).toBeNull()
  })

  it('puts the likeliest proposals first and the ones with no project last', () => {
    const order = sessionFilingsToDecide([
      filing('none-old', '', 0, 10),
      filing('weak', 'project_000001', 0.3, 30),
      filing('none-new', '', 0, 40),
      filing('strong', 'project_000002', 0.8, 20),
      filing('strong-newer', 'project_000002', 0.8, 25),
    ]).map((f) => f.id)
    expect(order).toEqual(['strong-newer', 'strong', 'weak', 'none-new', 'none-old'])
  })
})
