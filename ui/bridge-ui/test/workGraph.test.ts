import { describe, expect, it } from 'vitest'
import {
  branchesByHeadSha,
  branchesForList,
  githubBranchUrl,
  githubCommitUrl,
  layoutCommitGraph,
  otherRepos,
  reposByRecentActivity,
  sessionColor,
  sessionHue,
  sessionsInGraph,
  workGraphChatHref,
  workGraphRepoGraphPath,
  worktreeName,
  type WorkGraph,
  type WorkGraphBranch,
  type WorkGraphCommit,
  type WorkGraphRepoActivity,
} from '../src/workGraph'

function commit(sha: string, parents: string[], madeBy?: { session: string; subcommand: string; at: number }): WorkGraphCommit {
  return {
    sha,
    parents,
    committed_at: 0,
    author_name: 'a',
    subject: sha,
    on_origin_branch: false,
    made_by: madeBy
      ? { sha, session_id: madeBy.session, git_subcommand: madeBy.subcommand, recorded_at_unix_nano: madeBy.at }
      : undefined,
  }
}

function branch(name: string, head: string, sessions: WorkGraphBranch['sessions'] = [], isRemote = false): WorkGraphBranch {
  return {
    ref: isRemote ? `refs/remotes/${name}` : `refs/heads/${name}`,
    name,
    is_remote: isRemote,
    head_sha: head,
    committed_at: 0,
    merged_into_default: false,
    worktree_path: '',
    sessions,
  }
}

describe('layoutCommitGraph', () => {
  it('draws a straight history in one lane', () => {
    const layout = layoutCommitGraph([commit('c', ['b']), commit('b', ['a']), commit('a', [])])
    expect(layout.laneCount).toBe(1)
    expect(layout.rows.map(row => row.column)).toEqual([0, 0, 0])
    expect(layout.rows[0].segments).toEqual([{ part: 'bottom', fromColumn: 0, toColumn: 0 }])
    expect(layout.rows[1].segments).toEqual([
      { part: 'top', fromColumn: 0, toColumn: 0 },
      { part: 'bottom', fromColumn: 0, toColumn: 0 },
    ])
    expect(layout.rows[2].segments).toEqual([{ part: 'top', fromColumn: 0, toColumn: 0 }])
  })

  it('opens a lane for a merge parent and joins it back at the fork point', () => {
    // m merges f into b; f and b both come from a.
    const layout = layoutCommitGraph([
      commit('m', ['b', 'f']),
      commit('f', ['a']),
      commit('b', ['a']),
      commit('a', []),
    ])
    expect(layout.laneCount).toBe(2)
    expect(layout.rows.map(row => [row.commit.sha, row.column])).toEqual([['m', 0], ['f', 1], ['b', 0], ['a', 0]])
    expect(layout.rows[0].segments).toContainEqual({ part: 'bottom', fromColumn: 0, toColumn: 1 })
    // f's lane and b's lane both wait for a and meet at a's row, the fork point.
    expect(layout.rows[1].segments).toContainEqual({ part: 'bottom', fromColumn: 1, toColumn: 1 })
    expect(layout.rows[3].segments).toEqual([
      { part: 'top', fromColumn: 0, toColumn: 0 },
      { part: 'top', fromColumn: 1, toColumn: 0 },
    ])
  })

  it('gives each unmerged branch head its own lane and runs the others through', () => {
    const layout = layoutCommitGraph([
      commit('x', ['a']),
      commit('y', ['a']),
      commit('a', []),
    ])
    expect(layout.rows.map(row => row.column)).toEqual([0, 1, 0])
    expect(layout.rows[1].segments).toContainEqual({ part: 'through', fromColumn: 0, toColumn: 0 })
    expect(layout.rows[2].segments).toEqual([
      { part: 'top', fromColumn: 0, toColumn: 0 },
      { part: 'top', fromColumn: 1, toColumn: 0 },
    ])
  })

  it('says how far right each row draws, so its text can start there', () => {
    const layout = layoutCommitGraph([commit('x', ['a']), commit('y', ['a']), commit('a', [])])
    expect(layout.rows.map(row => row.widestColumn)).toEqual([0, 1, 1])
  })

  it('keeps a lane to the bottom edge when its parent is older than the list', () => {
    const layout = layoutCommitGraph([commit('b', ['a'])])
    expect(layout.rows[0].segments).toEqual([{ part: 'bottom', fromColumn: 0, toColumn: 0 }])
  })
})

describe('session colors', () => {
  it('gives an id the same color every time and different ids different hues', () => {
    expect(sessionColor('br_1790493336445928690')).toBe(sessionColor('br_1790493336445928690'))
    expect(sessionHue('br_1790493336445928690')).not.toBe(sessionHue('br_1790493336445928691'))
    const hue = sessionHue('br_1790578299365904445')
    expect(hue).toBeGreaterThanOrEqual(0)
    expect(hue).toBeLessThan(360)
  })
})

describe('sessionsInGraph', () => {
  it('joins what a session made with the branches it moved, latest first, and skips blank ids', () => {
    const graph: WorkGraph = {
      repo: { id: 15, name: 'dash', path: '/r/dash' },
      default_branch: 'main',
      truncated: false,
      commits: [
        commit('c', ['b'], { session: 'br_2', subcommand: 'merge', at: 30 }),
        commit('b', ['a'], { session: 'br_1', subcommand: 'commit', at: 20 }),
        commit('a', [], { session: 'br_1', subcommand: 'commit', at: 10 }),
      ],
      branches: [
        branch('main', 'c', [
          { session_id: 'br_2', ref_updates: 1, checkouts: 0, git_subcommands: ['merge'], first_at_unix_nano: 30, last_at_unix_nano: 30 },
          { session_id: '', ref_updates: 4, checkouts: 0, git_subcommands: ['pull'], first_at_unix_nano: 1, last_at_unix_nano: 99 },
        ]),
        branch('topic', 'b', [
          { session_id: 'br_1', ref_updates: 2, checkouts: 1, git_subcommands: ['commit', 'checkout'], first_at_unix_nano: 5, last_at_unix_nano: 20 },
        ]),
      ],
    }
    const sessions = sessionsInGraph(graph)
    expect(sessions.map(s => s.sessionId)).toEqual(['br_2', 'br_1'])
    const first = sessions.find(s => s.sessionId === 'br_1')!
    expect(first.commitShas).toEqual(['b', 'a'])
    expect(first.commitSubcommands).toEqual([{ subcommand: 'commit', count: 2 }])
    expect(first.gitSubcommands).toEqual(['commit', 'checkout'])
    expect(first.refUpdates).toBe(2)
    expect(first.checkouts).toBe(1)
    expect(first.branches.map(b => b.branch.name)).toEqual(['topic'])
  })
})

describe('repo picker', () => {
  const activity = (id: number, name: string, lastAt: number): WorkGraphRepoActivity => ({
    repo: { id, name, path: `/r/${name}` },
    default_branch: 'main',
    deleted_branches: [],
    branches: [branch('main', 'x', [
      { session_id: 'br_1', ref_updates: 1, checkouts: 0, git_subcommands: ['commit'], first_at_unix_nano: 0, last_at_unix_nano: lastAt },
    ])],
  })

  it('puts the most recently active repo first', () => {
    const choices = reposByRecentActivity([activity(1, 'a', 10), activity(2, 'b', 30), activity(3, 'c', 20)])
    expect(choices.map(c => c.repo.name)).toEqual(['b', 'c', 'a'])
  })

  it('offers every other repo by name, matched on id not name', () => {
    const active = reposByRecentActivity([activity(2, 'dash', 1)])
    const rest = otherRepos([{ id: 3, name: 'zeta' }, { id: 2, name: 'dash' }, { id: 4, name: 'dash' }], active)
    expect(rest.map(r => r.id)).toEqual([4, 3])
  })
})

describe('small rules', () => {
  it('builds the graph path with the repo-store id', () => {
    expect(workGraphRepoGraphPath('/api/work-graph', 15, 300)).toBe('/api/work-graph/repos/15/graph?max_commits=300')
  })
  it('lists the default branch first, then newest heads', () => {
    const old = { ...branch('old', 'a'), committed_at: 1 }
    const fresh = { ...branch('fresh', 'b'), committed_at: 9 }
    const main = { ...branch('main', 'c'), committed_at: 5 }
    const remoteMain = { ...branch('origin/main', 'c', [], true), committed_at: 5 }
    expect(branchesForList([old, remoteMain, fresh, main], 'main').map(b => b.name)).toEqual(['main', 'fresh', 'origin/main', 'old'])
  })
  it('lists local branches before remote ones at a head', () => {
    const byHead = branchesByHeadSha([branch('origin/main', 'c', [], true), branch('main', 'c')])
    expect(byHead.get('c')!.map(b => b.name)).toEqual(['main', 'origin/main'])
  })
  it('names a worktree by its last segment', () => {
    expect(worktreeName('/home/k/repos/dash-wt-work-graph/')).toBe('dash-wt-work-graph')
  })
})

describe('links out', () => {
  const onGitHub = { github_url: 'https://github.com/kayushkin/dash' }
  const notOnGitHub = { github_url: undefined }

  it('links a commit to GitHub only when origin has it', () => {
    const pushed = { ...commit('abc1234def', []), on_origin_branch: true }
    expect(githubCommitUrl(onGitHub, pushed)).toBe('https://github.com/kayushkin/dash/commit/abc1234def')
    expect(githubCommitUrl(onGitHub, commit('abc1234def', []))).toBeNull()
    expect(githubCommitUrl(notOnGitHub, pushed)).toBeNull()
  })

  it("links only origin's remote-tracking branches, never a local one or origin/HEAD", () => {
    expect(githubBranchUrl(onGitHub, branch('origin/feature/x y', 'a', [], true)))
      .toBe('https://github.com/kayushkin/dash/tree/feature/x%20y')
    expect(githubBranchUrl(onGitHub, branch('main', 'a'))).toBeNull()
    expect(githubBranchUrl(onGitHub, branch('upstream/main', 'a', [], true))).toBeNull()
    expect(githubBranchUrl(onGitHub, branch('origin/HEAD', 'a', [], true))).toBeNull()
    expect(githubBranchUrl(notOnGitHub, branch('origin/main', 'a', [], true))).toBeNull()
  })

  it('opens the chat on the work graph, a repo, and a commit', () => {
    expect(workGraphChatHref('/chat', 15)).toBe('/chat?view=work-graph&repo=15')
    expect(workGraphChatHref('/chat', 15, 'abc1234')).toBe('/chat?view=work-graph&repo=15&commit=abc1234')
  })
})
