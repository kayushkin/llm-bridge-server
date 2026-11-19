import { useCallback, useEffect, useMemo, useState } from 'react'
import type { JSX } from 'react'
import { useSessionNames } from '@kayushkin/chat-core'
import type { Repo as RepoStoreRepo } from '@kayushkin/repo-store-types'
import { useBridgeConfig } from '../../context'
import { timeAgo } from '../../utils'
import {
  WORK_GRAPH_ACTIVITY_HOURS,
  WORK_GRAPH_COMMIT_PAGE,
  WORK_GRAPH_MAX_COMMITS,
  branchTouchedBy,
  branchesByHeadSha,
  branchesForList,
  githubBranchUrl,
  githubCommitUrl,
  layoutCommitGraph,
  otherRepos,
  reposByRecentActivity,
  sessionColor,
  sessionsInGraph,
  shortSha,
  workGraphActivityPath,
  workGraphRepoGraphPath,
  worktreeName,
  type WorkGraph,
  type WorkGraphActivityAnswer,
  type WorkGraphBranch,
  type WorkGraphCommit,
  type WorkGraphRepo,
  type WorkGraphRepoChoice,
  type WorkGraphSegment,
  type WorkGraphSessionSummary,
} from '../../workGraph'
import styles from './WorkGraphView.module.css'

// The work graph, opened in the chat's workspace in place of the thread, as the
// signals page and the Orchestrator are (`?view=work-graph`). It draws one repo's
// commit graph from work-graph-store, the way `git log --graph` does, with each
// commit's dot in the color of the agent session that made it, and lists the
// sessions that worked the repo and what each did. The rules — lanes, colors,
// what each session did — are `src/workGraph.ts`; this file fetches and draws.
//
// Session ids are always written in full, so the chat's reference detection and
// a copy of the text both carry the whole id.

const ROW_HEIGHT = 28
const LANE_WIDTH = 16
const GRAPH_PAD = 10
const DOT_RADIUS = 5
const HEAD_DOT_RADIUS = 6
const GRAPH_POLL_MS = 30_000

/** Line colors by lane, from the theme, so every theme draws its own. */
const LANE_COLORS = ['var(--accent)', 'var(--info)', 'var(--success)', 'var(--warning)', 'var(--error)', 'var(--text-muted)']

function laneColor(column: number): string {
  return LANE_COLORS[column % LANE_COLORS.length]
}

function laneX(column: number): number {
  return GRAPH_PAD + column * LANE_WIDTH
}

function relativeTimeFromUnixSeconds(seconds: number): string {
  return timeAgo(new Date(seconds * 1000).toISOString())
}

function relativeTimeFromUnixNano(nanoseconds: number): string {
  return timeAgo(new Date(nanoseconds / 1e6).toISOString())
}

function localTimeFromUnixNano(nanoseconds: number): string {
  return new Date(nanoseconds / 1e6).toLocaleString()
}

async function readJSON<T>(response: Response, what: string): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim()
    throw new Error(`${what}: ${response.status} ${response.statusText}${body ? ` — ${body}` : ''}`)
  }
  return (await response.json()) as T
}

export interface WorkGraphViewProps {
  /** The repo (repo-store id) and commit to open on, from `?repo=` and
   *  `?commit=`; the most recently active repo when null. */
  initialRepoId?: number | null
  initialCommitSha?: string | null
  /** Told the repo and commit shown whenever either changes, so the host can
   *  keep them in the URL. */
  onFocusChange?: (repoId: number | null, commitSha: string | null) => void
  /** Open a session in the chat, closing this view. */
  onSelectSession: (sessionId: string) => void
  onClose: () => void
}

export function WorkGraphView({ initialRepoId = null, initialCommitSha = null, onFocusChange, onSelectSession, onClose }: WorkGraphViewProps): JSX.Element {
  const { fetch: fetchFn, workGraphStoreBasePath, repoStoreBasePath } = useBridgeConfig()

  const [activeRepos, setActiveRepos] = useState<WorkGraphRepoChoice[]>([])
  const [allRepos, setAllRepos] = useState<RepoStoreRepo[]>([])
  const [pickerError, setPickerError] = useState<string | null>(null)
  const [repoId, setRepoId] = useState<number | null>(initialRepoId)
  const [maxCommits, setMaxCommits] = useState(WORK_GRAPH_COMMIT_PAGE)
  const [graph, setGraph] = useState<WorkGraph | null>(null)
  const [graphError, setGraphError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [reloadSignal, setReloadSignal] = useState(0)
  const [selectedSha, setSelectedSha] = useState<string | null>(initialCommitSha)
  // A commit opened from a link is scrolled to once, when its row first draws.
  const [scrollToSha, setScrollToSha] = useState<string | null>(initialCommitSha)
  const [highlightedSession, setHighlightedSession] = useState<string | null>(null)

  // The picker: repos sessions touched lately, most recent first, then every
  // other repo repo-store knows. The first active repo is the default; with
  // none active, nothing is picked rather than a guess.
  useEffect(() => {
    if (!workGraphStoreBasePath) return
    let canceled = false
    fetchFn(workGraphActivityPath(workGraphStoreBasePath, WORK_GRAPH_ACTIVITY_HOURS))
      .then(response => readJSON<WorkGraphActivityAnswer>(response, 'work-graph-store activity'))
      .then(answer => {
        if (canceled) return
        const ordered = reposByRecentActivity(answer.repos)
        setActiveRepos(ordered)
        setRepoId(current => current ?? ordered[0]?.repo.id ?? null)
      })
      .catch((error: Error) => { if (!canceled) setPickerError(error.message) })
    if (repoStoreBasePath) {
      fetchFn(`${repoStoreBasePath}/repos`)
        .then(response => readJSON<RepoStoreRepo[]>(response, 'repo-store repos'))
        .then(repos => { if (!canceled) setAllRepos(repos) })
        .catch((error: Error) => { if (!canceled) setPickerError(error.message) })
    }
    return () => { canceled = true }
  }, [fetchFn, workGraphStoreBasePath, repoStoreBasePath])

  useEffect(() => {
    if (!workGraphStoreBasePath || repoId === null) return
    let canceled = false
    setLoading(true)
    fetchFn(workGraphRepoGraphPath(workGraphStoreBasePath, repoId, maxCommits))
      .then(response => readJSON<WorkGraph>(response, 'work-graph-store graph'))
      .then(answer => {
        if (canceled) return
        setGraph(answer)
        setGraphError(null)
      })
      .catch((error: Error) => { if (!canceled) setGraphError(error.message) })
      .finally(() => { if (!canceled) setLoading(false) })
    return () => { canceled = true }
  }, [fetchFn, workGraphStoreBasePath, repoId, maxCommits, reloadSignal])

  // Agents move refs all the time; the graph follows while the tab is visible.
  useEffect(() => {
    if (repoId === null) return
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') setReloadSignal(signal => signal + 1)
    }, GRAPH_POLL_MS)
    return () => window.clearInterval(timer)
  }, [repoId])

  useEffect(() => {
    onFocusChange?.(repoId, selectedSha)
  }, [repoId, selectedSha, onFocusChange])

  const pickRepo = useCallback((id: number) => {
    setRepoId(id)
    setMaxCommits(WORK_GRAPH_COMMIT_PAGE)
    setGraph(null)
    setSelectedSha(null)
    setHighlightedSession(null)
  }, [])

  const shownGraph = graph && graph.repo.id === repoId ? graph : null
  const layout = useMemo(() => layoutCommitGraph(shownGraph?.commits ?? []), [shownGraph])
  const headsBySha = useMemo(() => branchesByHeadSha(shownGraph?.branches ?? []), [shownGraph])
  const sessions = useMemo(() => (shownGraph ? sessionsInGraph(shownGraph) : []), [shownGraph])
  const sessionIds = useMemo(() => sessions.map(session => session.sessionId), [sessions])
  const sessionName = useSessionNames(sessionIds)
  const commitsBySha = useMemo(() => new Map((shownGraph?.commits ?? []).map(commit => [commit.sha, commit])), [shownGraph])
  const selectedCommit = selectedSha ? commitsBySha.get(selectedSha) ?? null : null
  const otherRepoChoices = useMemo(() => otherRepos(allRepos, activeRepos), [allRepos, activeRepos])

  // A linked commit older than the page loaded: load every commit the store
  // will send, once, rather than asking the reader to page back for it.
  const linkedCommitMissing = shownGraph !== null && selectedSha !== null && !commitsBySha.has(selectedSha)
  useEffect(() => {
    if (linkedCommitMissing && shownGraph?.truncated && maxCommits < WORK_GRAPH_MAX_COMMITS) {
      setMaxCommits(WORK_GRAPH_MAX_COMMITS)
    }
  }, [linkedCommitMissing, shownGraph, maxCommits])

  useEffect(() => {
    if (!scrollToSha || !commitsBySha.has(scrollToSha)) return
    document.querySelector(`[data-commit-sha="${scrollToSha}"]`)?.scrollIntoView({ block: 'center' })
    setScrollToSha(null)
  }, [scrollToSha, commitsBySha])

  const toggleHighlight = (sessionId: string) =>
    setHighlightedSession(current => (current === sessionId ? null : sessionId))

  if (!workGraphStoreBasePath) {
    return (
      <div className="bc-workspace bc-workspace-focused">
        <WorkGraphHeader onClose={onClose} />
        <p className={styles.notice}>
          This host does not proxy work-graph-store, so there is no work graph to draw (the host sets
          <code> workGraphStoreBasePath</code>).
        </p>
      </div>
    )
  }

  const graphWidth = laneX(Math.max(layout.laneCount, 1) - 1) + GRAPH_PAD + 2

  return (
    <div className={`bc-workspace bc-workspace-focused ${styles.page}`} role="region" aria-label="Work graph">
      <WorkGraphHeader onClose={onClose} onRefresh={() => setReloadSignal(signal => signal + 1)} loading={loading}>
        <select
          className={styles.repoPicker}
          value={repoId ?? ''}
          onChange={event => pickRepo(Number(event.target.value))}
          aria-label="Repository"
        >
          {repoId === null && <option value="">Pick a repo…</option>}
          {activeRepos.length > 0 && (
            <optgroup label={`Active in the last ${WORK_GRAPH_ACTIVITY_HOURS}h`}>
              {activeRepos.map(choice => (
                <option key={choice.repo.id} value={choice.repo.id}>
                  {choice.repo.name} · {relativeTimeFromUnixNano(choice.lastActiveUnixNano)}
                </option>
              ))}
            </optgroup>
          )}
          {otherRepoChoices.length > 0 && (
            <optgroup label="Every other repo">
              {otherRepoChoices.map(repo => (
                <option key={repo.id} value={repo.id}>{repo.name}</option>
              ))}
            </optgroup>
          )}
        </select>
        {shownGraph?.repo.github_url && (
          <a className="bc-ctrl-btn" href={shownGraph.repo.github_url} target="_blank" rel="noreferrer" title={shownGraph.repo.github_url}>
            GitHub ↗
          </a>
        )}
        {shownGraph && (
          <span className={styles.muted}>
            {shownGraph.commits.length} commits{shownGraph.truncated ? ' (newest)' : ''} · {shownGraph.branches.length} branches
            {shownGraph.default_branch ? ` · default ${shownGraph.default_branch}` : ''}
          </span>
        )}
      </WorkGraphHeader>

      {pickerError && <p className={styles.error}>{pickerError}</p>}
      {graphError && <p className={styles.error}>{graphError}</p>}
      {linkedCommitMissing && !loading && !(shownGraph?.truncated && maxCommits < WORK_GRAPH_MAX_COMMITS) && (
        <p className={styles.error}>
          Commit <code>{selectedSha}</code> is not among the {shownGraph?.commits.length} commits listed: it is on no branch of {shownGraph?.repo.name}, or older than the newest {WORK_GRAPH_MAX_COMMITS}.
        </p>
      )}

      <div className={styles.body}>
        <div className={styles.graphScroll}>
          {repoId === null && !pickerError && (
            <p className={styles.notice}>No session has moved a branch in the last {WORK_GRAPH_ACTIVITY_HOURS} hours. Pick a repo above.</p>
          )}
          {repoId !== null && !shownGraph && !graphError && <p className={styles.notice}>Loading…</p>}
          {shownGraph && (
            <div className={styles.graph} style={{ height: layout.rows.length * ROW_HEIGHT }}>
              <svg
                className={styles.graphSvg}
                width={graphWidth}
                height={layout.rows.length * ROW_HEIGHT}
                aria-hidden
              >
                {layout.rows.map((row, rowIndex) => (
                  <g key={row.commit.sha}>
                    {row.segments.map((segment, segmentIndex) => (
                      <path
                        key={segmentIndex}
                        d={segmentPath(segment, rowIndex)}
                        stroke={laneColor(segment.part === 'top' ? segment.fromColumn : segment.toColumn)}
                        strokeWidth={2}
                        fill="none"
                        opacity={highlightedSession ? 0.45 : 0.85}
                      />
                    ))}
                  </g>
                ))}
                {layout.rows.map((row, rowIndex) => {
                  const madeBy = row.commit.made_by
                  const isHead = headsBySha.has(row.commit.sha)
                  const dimmed = highlightedSession !== null && madeBy?.session_id !== highlightedSession
                  return (
                    <circle
                      key={row.commit.sha}
                      cx={laneX(row.column)}
                      cy={rowIndex * ROW_HEIGHT + ROW_HEIGHT / 2}
                      r={isHead ? HEAD_DOT_RADIUS : DOT_RADIUS}
                      fill={madeBy ? sessionColor(madeBy.session_id) : 'var(--bg)'}
                      stroke={madeBy ? 'var(--bg-surface)' : 'var(--text-muted)'}
                      strokeWidth={madeBy ? 1.5 : 2}
                      opacity={dimmed ? 0.25 : 1}
                    />
                  )
                })}
              </svg>
              <ol className={styles.rows}>
                {layout.rows.map(row => (
                  <CommitRow
                    key={row.commit.sha}
                    textStart={laneX(row.widestColumn) + GRAPH_PAD + 6}
                    commit={row.commit}
                    repo={shownGraph.repo}
                    heads={headsBySha.get(row.commit.sha) ?? []}
                    defaultBranch={shownGraph.default_branch}
                    selected={row.commit.sha === selectedSha}
                    highlightedSession={highlightedSession}
                    onSelect={() => setSelectedSha(current => (current === row.commit.sha ? null : row.commit.sha))}
                  />
                ))}
              </ol>
            </div>
          )}
          {shownGraph?.truncated && maxCommits < WORK_GRAPH_MAX_COMMITS && (
            <div className={styles.more}>
              <button
                type="button"
                className="bc-ctrl-btn"
                disabled={loading}
                onClick={() => setMaxCommits(count => Math.min(count + WORK_GRAPH_COMMIT_PAGE, WORK_GRAPH_MAX_COMMITS))}
              >
                Show {WORK_GRAPH_COMMIT_PAGE} older commits
              </button>
            </div>
          )}
        </div>

        {shownGraph && (
          <aside className={styles.side}>
            {selectedCommit && (
              <CommitDetails
                commit={selectedCommit}
                repo={shownGraph.repo}
                heads={headsBySha.get(selectedCommit.sha) ?? []}
                commitsBySha={commitsBySha}
                sessionName={sessionName}
                onSelectCommit={setSelectedSha}
                onOpenSession={onSelectSession}
                onClose={() => setSelectedSha(null)}
              />
            )}
            <section className={styles.sideSection}>
              <h3 className={styles.sideTitle}>Sessions in {shownGraph.repo.name}</h3>
              {sessions.length === 0 ? (
                <p className={styles.muted}>
                  No session is recorded moving a ref here. work-graph-store knows only what its git hook saw, so older commits have no maker and are drawn hollow.
                </p>
              ) : (
                <ul className={styles.sessionList}>
                  {sessions.map(session => (
                    <SessionEntry
                      key={session.sessionId}
                      session={session}
                      name={sessionName(session.sessionId)}
                      highlighted={highlightedSession === session.sessionId}
                      onToggleHighlight={() => toggleHighlight(session.sessionId)}
                      onOpenSession={() => onSelectSession(session.sessionId)}
                    />
                  ))}
                </ul>
              )}
              <p className={styles.legendNote}>
                <svg width={12} height={12} aria-hidden><circle cx={6} cy={6} r={4} fill="var(--bg)" stroke="var(--text-muted)" strokeWidth={2} /></svg>
                {' '}made before recording began, or fetched from elsewhere
              </p>
            </section>
            <section className={styles.sideSection}>
              <h3 className={styles.sideTitle}>Branches</h3>
              <ul className={styles.branchList}>
                {branchesForList(shownGraph.branches, shownGraph.default_branch).map(branch => (
                  <li
                    key={branch.ref}
                    className={`${styles.branchListItem} ${highlightedSession && branchTouchedBy(branch, highlightedSession) ? styles.branchListItemHighlighted : ''}`}
                  >
                    <BranchChip branch={branch} isDefault={!branch.is_remote && branch.name === shownGraph.default_branch} highlightedSession={highlightedSession} />
                    <button
                      type="button"
                      className={styles.shaLink}
                      title={branch.head_sha}
                      onClick={() => setSelectedSha(branch.head_sha)}
                      disabled={!commitsBySha.has(branch.head_sha)}
                    >
                      {shortSha(branch.head_sha)}
                    </button>
                    <span className={styles.muted}>{relativeTimeFromUnixSeconds(branch.committed_at)}</span>
                    <GitHubLink href={githubBranchUrl(shownGraph.repo, branch)} title="Open this branch on GitHub" />
                  </li>
                ))}
              </ul>
            </section>
          </aside>
        )}
      </div>
    </div>
  )
}

function segmentPath(segment: WorkGraphSegment, rowIndex: number): string {
  const top = rowIndex * ROW_HEIGHT
  const middle = top + ROW_HEIGHT / 2
  const bottom = top + ROW_HEIGHT
  const fromX = laneX(segment.fromColumn)
  const toX = laneX(segment.toColumn)
  if (segment.part === 'through') return `M ${fromX} ${top} L ${fromX} ${bottom}`
  const [startY, endY] = segment.part === 'top' ? [top, middle] : [middle, bottom]
  if (fromX === toX) return `M ${fromX} ${startY} L ${toX} ${endY}`
  // A curve that leaves and arrives vertically, as GitKraken draws a fork or a merge.
  return `M ${fromX} ${startY} C ${fromX} ${endY} ${toX} ${startY} ${toX} ${endY}`
}

function WorkGraphHeader({
  onClose,
  onRefresh,
  loading,
  children,
}: {
  onClose: () => void
  onRefresh?: () => void
  loading?: boolean
  children?: React.ReactNode
}): JSX.Element {
  return (
    <div className="bc-header">
      <div className="bc-header-row" style={headerRow}>
        <span aria-hidden>⎇</span>
        <span className="bc-session-name" style={{ fontWeight: 600 }}>Work graph</span>
        {children}
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 8, alignItems: 'center' }}>
          {onRefresh && (
            <button type="button" className="bc-ctrl-btn" onClick={onRefresh} disabled={loading}>
              {loading ? 'Loading…' : 'Refresh'}
            </button>
          )}
          <button type="button" className="bc-ctrl-btn" onClick={onClose} aria-label="Close the work graph" title="Close the work graph">
            ✕
          </button>
        </span>
      </div>
    </div>
  )
}

function CommitRow({
  commit,
  repo,
  heads,
  defaultBranch,
  selected,
  highlightedSession,
  onSelect,
  textStart,
}: {
  commit: WorkGraphCommit
  repo: WorkGraphRepo
  heads: WorkGraphBranch[]
  /** Where the row's text begins, just right of the lines this row draws. */
  textStart: number
  defaultBranch: string
  selected: boolean
  highlightedSession: string | null
  onSelect: () => void
}): JSX.Element {
  const madeBy = commit.made_by
  const dimmed = highlightedSession !== null && madeBy?.session_id !== highlightedSession
    && !heads.some(branch => branchTouchedBy(branch, highlightedSession))
  const title = madeBy
    ? `${commit.sha}\nmade by ${madeBy.session_id} (git ${madeBy.git_subcommand})`
    : `${commit.sha}\nno recorded maker`
  return (
    <li
      className={`${styles.row} ${selected ? styles.rowSelected : ''} ${dimmed ? styles.rowDimmed : ''}`}
      style={{ height: ROW_HEIGHT, paddingLeft: textStart }}
      data-commit-sha={commit.sha}
      onClick={onSelect}
      title={title}
    >
      {heads.map(branch => (
        <BranchChip
          key={branch.ref}
          branch={branch}
          isDefault={!branch.is_remote && branch.name === defaultBranch}
          highlightedSession={highlightedSession}
        />
      ))}
      <span className={styles.subject}>{commit.subject}</span>
      <span className={styles.author}>{commit.author_name}</span>
      <CommitShaLink commit={commit} repo={repo} />
      <span className={styles.time}>{relativeTimeFromUnixSeconds(commit.committed_at)}</span>
    </li>
  )
}

function BranchChip({
  branch,
  isDefault,
  highlightedSession,
}: {
  branch: WorkGraphBranch
  isDefault: boolean
  highlightedSession: string | null
}): JSX.Element {
  const touched = highlightedSession !== null && branchTouchedBy(branch, highlightedSession)
  const state = branch.merged_into_default ? 'merged into the default branch' : 'not merged into the default branch'
  const worktree = branch.worktree_path ? `\nchecked out in ${branch.worktree_path}` : ''
  const sessionLines = branch.sessions.map(session => `\n${session.session_id || '(no session)'}: ${session.git_subcommands.join(', ')}`).join('')
  const classes = [
    styles.branch,
    branch.is_remote ? styles.branchRemote : styles.branchLocal,
    branch.merged_into_default ? '' : styles.branchUnmerged,
    isDefault ? styles.branchDefault : '',
    touched ? styles.branchTouched : '',
  ].join(' ')
  return (
    <span className={classes} title={`${branch.ref} — ${state}${worktree}${sessionLines}`}>
      {branch.sessions.filter(session => session.session_id !== '').map(session => (
        <span key={session.session_id} className={styles.branchSessionDot} style={{ background: sessionColor(session.session_id) }} />
      ))}
      {branch.name}
      {!branch.merged_into_default && <span className={styles.branchMark} aria-label="not merged">●</span>}
      {branch.worktree_path && <span className={styles.branchWorktree}>⌂ {worktreeName(branch.worktree_path)}</span>}
    </span>
  )
}

function SessionEntry({
  session,
  name,
  highlighted,
  onToggleHighlight,
  onOpenSession,
}: {
  session: WorkGraphSessionSummary
  name: string
  highlighted: boolean
  onToggleHighlight: () => void
  onOpenSession: () => void
}): JSX.Element {
  return (
    <li className={`${styles.session} ${highlighted ? styles.sessionHighlighted : ''}`}>
      <button
        type="button"
        className={styles.sessionPick}
        aria-pressed={highlighted}
        onClick={onToggleHighlight}
        title={highlighted ? 'Stop highlighting this session' : 'Highlight what this session did'}
      >
        <span className={styles.swatch} style={{ background: sessionColor(session.sessionId) }} />
        {/* useSessionNames falls back to the id, which is drawn in full below. */}
        {name !== session.sessionId && <span className={styles.sessionName}>{name}</span>}
        <span className={styles.muted}>{relativeTimeFromUnixNano(session.lastAtUnixNano)}</span>
      </button>
      <button type="button" className={styles.sessionId} onClick={onOpenSession} title="Open this session in the chat">
        {session.sessionId}
      </button>
      <div className={styles.sessionDid}>
        {session.commitShas.length > 0 && (
          <div>
            made {session.commitShas.length} commit{session.commitShas.length === 1 ? '' : 's'}
            {' ('}{session.commitSubcommands.map(entry => `${entry.subcommand} ×${entry.count}`).join(', ')})
          </div>
        )}
        {session.branches.map(({ branch, session: part }) => (
          <div key={branch.ref}>
            {branch.name}: moved ×{part.ref_updates}{part.checkouts > 0 ? `, checked out ×${part.checkouts}` : ''}
          </div>
        ))}
        <div className={styles.muted}>git {session.gitSubcommands.join(', ')}</div>
      </div>
    </li>
  )
}

function CommitDetails({
  commit,
  repo,
  heads,
  commitsBySha,
  sessionName,
  onSelectCommit,
  onOpenSession,
  onClose,
}: {
  commit: WorkGraphCommit
  repo: WorkGraphRepo
  heads: WorkGraphBranch[]
  commitsBySha: Map<string, WorkGraphCommit>
  sessionName: (sessionId: string) => string
  onSelectCommit: (sha: string) => void
  onOpenSession: (sessionId: string) => void
  onClose: () => void
}): JSX.Element {
  const madeBy = commit.made_by
  return (
    <section className={`${styles.sideSection} ${styles.details}`}>
      <div className={styles.detailsHeader}>
        <h3 className={styles.sideTitle}>Commit</h3>
        <button type="button" className="bc-ctrl-btn" onClick={onClose} aria-label="Close the commit details">✕</button>
      </div>
      <p className={styles.detailsSubject}>{commit.subject}</p>
      <dl className={styles.detailsList}>
        <dt>sha</dt>
        <dd><code className={styles.fullSha}>{commit.sha}</code></dd>
        <dt>GitHub</dt>
        <dd>
          {githubCommitUrl(repo, commit) ? (
            <a href={githubCommitUrl(repo, commit) ?? undefined} target="_blank" rel="noreferrer">Open this commit ↗</a>
          ) : (
            <span className={styles.muted}>
              {repo.github_url ? 'not on any branch of origin, so GitHub does not have it' : `${repo.name} has no GitHub remote`}
            </span>
          )}
        </dd>
        <dt>mention</dt>
        <dd><code title="Write this in a chat message to link this commit">{repo.name}@{shortSha(commit.sha)}</code></dd>
        <dt>parents</dt>
        <dd>
          {commit.parents.length === 0 && <span className={styles.muted}>none (root commit)</span>}
          {commit.parents.map(parent => (
            <button
              key={parent}
              type="button"
              className={`${styles.shaLink} ${styles.fullSha}`}
              onClick={() => onSelectCommit(parent)}
              disabled={!commitsBySha.has(parent)}
              title={commitsBySha.has(parent) ? 'Show this parent' : 'Older than the commits listed'}
            >
              {parent}
            </button>
          ))}
        </dd>
        <dt>author</dt>
        <dd>{commit.author_name} · {new Date(commit.committed_at * 1000).toLocaleString()} ({relativeTimeFromUnixSeconds(commit.committed_at)})</dd>
        <dt>made by</dt>
        <dd>
          {madeBy ? (
            <>
              <span className={styles.swatch} style={{ background: sessionColor(madeBy.session_id) }} />{' '}
              <button type="button" className={styles.sessionId} onClick={() => onOpenSession(madeBy.session_id)} title="Open this session in the chat">
                {madeBy.session_id}
              </button>
              {sessionName(madeBy.session_id) !== madeBy.session_id && <div>{sessionName(madeBy.session_id)}</div>}
              <div className={styles.muted}>git {madeBy.git_subcommand} · {localTimeFromUnixNano(madeBy.recorded_at_unix_nano)}</div>
            </>
          ) : (
            <span className={styles.muted}>no session recorded — made before recording began, or fetched from elsewhere</span>
          )}
        </dd>
        {heads.length > 0 && (
          <>
            <dt>branches</dt>
            <dd>{heads.map(branch => <div key={branch.ref}>{branch.name}{branch.worktree_path ? ` — ${branch.worktree_path}` : ''}</div>)}</dd>
          </>
        )}
      </dl>
    </section>
  )
}

/** The short sha, linked to the commit on GitHub when GitHub has it. The row
 *  itself selects the commit, so the link keeps its click to itself. */
function CommitShaLink({ commit, repo }: { commit: WorkGraphCommit; repo: WorkGraphRepo }): JSX.Element {
  const href = githubCommitUrl(repo, commit)
  if (!href) {
    return (
      <code className={styles.sha} title={repo.github_url ? 'Not on any branch of origin' : undefined}>
        {shortSha(commit.sha)}
      </code>
    )
  }
  return (
    <a
      className={`${styles.sha} ${styles.shaGitHub}`}
      href={href}
      target="_blank"
      rel="noreferrer"
      title="Open this commit on GitHub"
      onClick={event => event.stopPropagation()}
    >
      {shortSha(commit.sha)} ↗
    </a>
  )
}

function GitHubLink({ href, title }: { href: string | null; title: string }): JSX.Element | null {
  if (!href) return null
  return (
    <a className={styles.shaGitHub} href={href} target="_blank" rel="noreferrer" title={title}>↗</a>
  )
}

const headerRow: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }
