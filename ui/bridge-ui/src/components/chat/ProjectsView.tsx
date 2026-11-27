import { useCallback, useEffect, useMemo, useState } from 'react'
import type { JSX, ReactNode } from 'react'
import { useChatContext, useOpenSignalsByAttention, type SessionSummary, type SignalRequest } from '@kayushkin/chat-core'
import type { Project, RepoRollup, Rollup } from '@kayushkin/project-store-types'
import { useBridgeConfig } from '../../context'
import { formatAgeCompact, formatCost } from '../../utils'
import { useProjectStore } from '../../useProjectStore'
import {
  WAITING_WORK_STATE,
  attentionTotal,
  commitsPerDay,
  filedSessionSpend,
  lastActivitySeconds,
  openCardCounts,
  openNextSteps,
  projectAttention,
  projectPathName,
  projectRollupPath,
  projectSections,
  sessionsFiledByProject,
  unmergedBranchCount,
  vocabularyPath,
  repoMention,
  type ProjectAttention,
  type ProjectTreeRow,
  type ProjectVocabulary,
} from '../../projects'
import {
  LANDING_VIEWS,
  LANDING_VIEW_LABELS,
  LANDING_VIEW_STORAGE_KEY,
  parseLandingView,
  projectSessionIds,
  questionCount,
  questionRequestsOfSessions,
  type LandingView,
} from '../../projectViews'
import { ProjectPage, StageChip, type ProjectQuestions, type RollupSlot } from './ProjectPage'
import { EffortChart, ProjectTimeline, StageBoard, StepProgressBar } from './ProjectsLandingViews'
import { RefChip } from './RefChip'
import { SessionsToFile } from './SessionsToFile'
import styles from './ProjectsView.module.css'

// The projects, opened in the chat's workspace in place of the thread, as the
// work graph is (`?view=projects`, and `&project=` for one project's page).
// The landing page is a card per project, sub-projects under their parent and
// streams apart; a card opens the project's page. Everything a card counts is
// worked out by project-store's rollup on request; the rules for reading it
// are `src/projects.ts`, and this file fetches and draws.
//
// Ids are written in full everywhere, so the chat's reference detection and a
// copy of the text both carry the whole id.

/** How many rollups the landing page asks for at once. Each reaches
 *  kanban-store, work-graph-store and repo-store behind project-store. */
const ROLLUP_CONCURRENCY = 4

async function readJSON<T>(response: Response, what: string): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim()
    throw new Error(`${what}: ${response.status} ${response.statusText}${body ? ` — ${body}` : ''}`)
  }
  return (await response.json()) as T
}

function ageFromSeconds(seconds: number): string | null {
  return seconds > 0 ? formatAgeCompact(new Date(seconds * 1000).toISOString()) : null
}

function spendText(spendUsd: number, sessionsWithoutSpend: number): string {
  return sessionsWithoutSpend > 0 ? `${formatCost(spendUsd)} (+${sessionsWithoutSpend} unknown)` : formatCost(spendUsd)
}

export interface ProjectsViewProps {
  /** The project whose page is open, or null for the landing page. */
  projectId: string | null
  onOpenProject: (projectId: string | null) => void
  /** Open a session in the chat, closing this view. */
  onSelectSession: (sessionId: string) => void
  onClose: () => void
}

export function ProjectsView({ projectId, onOpenProject, onSelectSession, onClose }: ProjectsViewProps): JSX.Element {
  const { fetch: fetchFn, projectsBasePath } = useBridgeConfig()
  const { api } = useChatContext()
  const { projects, sessionLinks, error: storeError, loaded, refresh } = useProjectStore()

  const [rollups, setRollups] = useState<ReadonlyMap<string, RollupSlot>>(new Map())
  const [vocabulary, setVocabulary] = useState<ProjectVocabulary | null>(null)
  const [summaries, setSummaries] = useState<ReadonlyMap<string, SessionSummary>>(new Map())
  const [summaryError, setSummaryError] = useState<string | null>(null)
  const [reloadSignal, setReloadSignal] = useState(0)
  const [loading, setLoading] = useState(false)

  const projectsById = useMemo(() => new Map(projects.map((p) => [p.id, p])), [projects])
  const filedByProject = useMemo(() => sessionsFiledByProject(sessionLinks), [sessionLinks])
  const filedSessionIds = useMemo(
    () => [...new Set(sessionLinks.map((l) => l.entity_ref))].sort(),
    [sessionLinks],
  )
  const filedKey = filedSessionIds.join(',')

  useEffect(() => {
    if (!projectsBasePath) return
    let canceled = false
    fetchFn(vocabularyPath(projectsBasePath))
      .then((r) => readJSON<ProjectVocabulary>(r, 'project-store vocabulary'))
      .then((v) => { if (!canceled) setVocabulary(v) })
      .catch((error: Error) => console.error(error))
    return () => { canceled = true }
  }, [fetchFn, projectsBasePath])

  // Every rollup on the landing page; only the open project's on its page.
  const wantedRollups = useMemo(
    () => (projectId ? [projectId] : projects.map((p) => p.id)),
    [projectId, projects],
  )
  const wantedKey = wantedRollups.join(',')
  useEffect(() => {
    if (!projectsBasePath || wantedKey === '') return
    let canceled = false
    const ids = wantedKey.split(',')
    setLoading(true)
    let next = 0
    const worker = async (): Promise<void> => {
      while (!canceled && next < ids.length) {
        const id = ids[next++]
        let slot: RollupSlot
        try {
          const rollup = await fetchFn(projectRollupPath(projectsBasePath, id)).then((r) =>
            readJSON<Rollup>(r, `rollup of ${id}`),
          )
          slot = { rollup, error: null }
        } catch (error) {
          slot = { rollup: null, error: error instanceof Error ? error.message : String(error) }
        }
        if (canceled) return
        setRollups((previous) => new Map(previous).set(id, slot))
      }
    }
    void Promise.all(Array.from({ length: Math.min(ROLLUP_CONCURRENCY, ids.length) }, worker)).finally(() => {
      if (!canceled) setLoading(false)
    })
    return () => { canceled = true }
  }, [fetchFn, projectsBasePath, wantedKey, reloadSignal])

  // The filed sessions' rows, for names, states and spend: one lookup by id,
  // because most of them are long past the sidebar's loaded page.
  useEffect(() => {
    if (filedKey === '') return
    let canceled = false
    const ids = filedKey.split(',')
    api
      .getSummary({ sessionIds: ids, limit: ids.length })
      .then((answer) => {
        if (canceled) return
        setSummaries(new Map(answer.sessions.map((s) => [s.sessionId, s])))
        setSummaryError(null)
      })
      .catch((error: Error) => { if (!canceled) setSummaryError(error.message) })
    return () => { canceled = true }
  }, [api, filedKey, reloadSignal])

  const reload = useCallback(() => {
    void refresh()
    setReloadSignal((n) => n + 1)
  }, [refresh])

  // Every session's open signals, from the one read the chat shares, split
  // into the operator's and background agents'. Each project takes the
  // question groups of the sessions at work on it.
  const openSignals = useOpenSignalsByAttention()
  const questionsByProject = useMemo(() => {
    const out = new Map<string, ProjectQuestions>()
    for (const [id, slot] of rollups) {
      if (!slot.rollup) continue
      const sessionIds = projectSessionIds(slot.rollup)
      out.set(id, {
        operator: questionRequestsOfSessions<SignalRequest>(openSignals.operator.requests, sessionIds),
        background: questionRequestsOfSessions<SignalRequest>(openSignals.background.requests, sessionIds),
      })
    }
    return out
  }, [rollups, openSignals.operator.requests, openSignals.background.requests])

  if (!projectsBasePath) {
    return (
      <div className="bc-workspace bc-workspace-focused">
        <ProjectsHeader title="Projects" onClose={onClose} />
        <p className={styles.notice}>
          This host does not proxy project-store, so there are no projects to show (the host sets
          <code> projectsBasePath</code>).
        </p>
      </div>
    )
  }

  const open = projectId ? projectsById.get(projectId) ?? null : null
  const errors = [storeError, summaryError].filter((e): e is string => !!e)

  return (
    <div className={`bc-workspace bc-workspace-focused ${styles.page}`} role="region" aria-label="Projects">
      <ProjectsHeader
        title={open ? projectPathName(open, projectsById) : 'Projects'}
        onBack={projectId ? () => onOpenProject(null) : undefined}
        onRefresh={reload}
        loading={loading}
        onClose={onClose}
      />
      {errors.map((e) => <p key={e} className={styles.error}>{e}</p>)}
      <div className={styles.scroll}>
        {!loaded ? (
          <p className={styles.notice}>Loading…</p>
        ) : projectId ? (
          open ? (
            <ProjectPage
              project={open}
              slot={rollups.get(open.id) ?? null}
              projectsById={projectsById}
              vocabulary={vocabulary}
              summaries={summaries}
              onOpenProject={onOpenProject}
              onSelectSession={onSelectSession}
              onChanged={reload}
              questions={questionsByProject.get(open.id) ?? null}
              questionsError={openSignals.error}
              onQuestionAnswered={openSignals.reload}
            />
          ) : (
            <p className={styles.notice}>project-store has no live project {projectId}.</p>
          )
        ) : (
          <Landing
            projects={projects}
            rollups={rollups}
            filedByProject={filedByProject}
            summaries={summaries}
            vocabulary={vocabulary}
            questionsByProject={questionsByProject}
            onOpenProject={onOpenProject}
            onSelectSession={onSelectSession}
            onChanged={reload}
            reloadSignal={reloadSignal}
          />
        )}
      </div>
    </div>
  )
}

function ProjectsHeader({
  title,
  onBack,
  onRefresh,
  loading,
  onClose,
}: {
  title: string
  onBack?: () => void
  onRefresh?: () => void
  loading?: boolean
  onClose: () => void
}): JSX.Element {
  return (
    <div className="bc-header">
      <div className={`bc-header-row ${styles.headerRow}`}>
        {onBack ? (
          <button type="button" className="bc-ctrl-btn" onClick={onBack} title="All projects">← All projects</button>
        ) : (
          <span aria-hidden>▦</span>
        )}
        <span className="bc-session-name" style={{ fontWeight: 600 }}>{title}</span>
        <span className={styles.headerEnd}>
          {onRefresh && (
            <button type="button" className="bc-ctrl-btn" onClick={onRefresh} disabled={loading}>
              {loading ? 'Loading…' : 'Refresh'}
            </button>
          )}
          <button type="button" className="bc-ctrl-btn" onClick={onClose} aria-label="Close the projects" title="Close the projects">✕</button>
        </span>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Landing
// ---------------------------------------------------------------------------

function Landing({
  projects,
  rollups,
  filedByProject,
  summaries,
  vocabulary,
  questionsByProject,
  onOpenProject,
  onSelectSession,
  onChanged,
  reloadSignal,
}: {
  projects: Project[]
  rollups: ReadonlyMap<string, RollupSlot>
  filedByProject: ReadonlyMap<string, string[]>
  summaries: ReadonlyMap<string, SessionSummary>
  vocabulary: ProjectVocabulary | null
  questionsByProject: ReadonlyMap<string, ProjectQuestions>
  onOpenProject: (projectId: string) => void
  onSelectSession: (sessionId: string) => void
  onChanged: () => void
  reloadSignal: number
}): JSX.Element {
  const sections = useMemo(() => projectSections(projects), [projects])
  const [view, setView] = useState<LandingView>(() => parseLandingView(localStorage.getItem(LANDING_VIEW_STORAGE_KEY)))
  const chooseView = (next: LandingView) => {
    setView(next)
    localStorage.setItem(LANDING_VIEW_STORAGE_KEY, next)
  }
  const nowSeconds = Math.floor(Date.now() / 1000)
  // Only the operator's questions count as needing you; a background agent's
  // are on the project's page, as they are in the chat.
  const attentionOf = (projectId: string): ProjectAttention | null => {
    const rollup = rollups.get(projectId)?.rollup
    if (!rollup) return null
    return projectAttention(rollup, nowSeconds, questionCount(questionsByProject.get(projectId)?.operator ?? []))
  }
  const needsYou = projects
    .map((project) => {
      const attention = attentionOf(project.id)
      return attention ? { project, attention } : null
    })
    .filter((row): row is { project: Project; attention: ProjectAttention } => row !== null && attentionTotal(row.attention) > 0)
    .sort((a, b) => attentionTotal(b.attention) - attentionTotal(a.attention))
  if (projects.length === 0) return <p className={styles.notice}>project-store holds no live project.</p>
  const cards = (rows: ProjectTreeRow[]) => (
    <div className={styles.cardList}>
      {rows.map((row) => (
        <ProjectCard
          key={row.project.id}
          row={row}
          slot={rollups.get(row.project.id) ?? null}
          filedSessionIds={filedByProject.get(row.project.id) ?? []}
          summaries={summaries}
          attention={attentionOf(row.project.id)}
          onOpen={() => onOpenProject(row.project.id)}
        />
      ))}
    </div>
  )
  return (
    <>
      <div className={styles.viewTabs} role="tablist" aria-label="How to show the projects">
        {LANDING_VIEWS.map((choice) => (
          <button key={choice} type="button" role="tab" aria-selected={choice === view}
            className={`bc-ctrl-btn ${choice === view ? 'bc-ctrl-btn-active' : ''}`} onClick={() => chooseView(choice)}>
            {LANDING_VIEW_LABELS[choice]}
          </button>
        ))}
      </div>
      {needsYou.length > 0 && (
        <section className={styles.section}>
          <h2 className={`${styles.sectionTitle} page-title`}>Needs you</h2>
          <ul className={styles.needsYou}>
            {needsYou.map(({ project, attention }) => (
              <li key={project.id} className={styles.needsYouRow}>
                <button type="button" className={styles.cardName} onClick={() => onOpenProject(project.id)}>{project.name}</button>
                <AttentionChips attention={attention} />
              </li>
            ))}
          </ul>
        </section>
      )}
      <SessionsToFile projects={projects} onSelectSession={onSelectSession} onChanged={onChanged} reloadSignal={reloadSignal} />
      {view === 'timeline' && <ProjectTimeline projects={projects} onOpenProject={onOpenProject} />}
      {view === 'stages' && (
        <StageBoard projects={projects} stages={vocabulary?.stages ?? null} rollups={rollups} attentionOf={attentionOf}
          onOpenProject={onOpenProject} onChanged={onChanged} />
      )}
      {view === 'effort' && (
        <EffortChart projects={projects} rollups={rollups} filedByProject={filedByProject} summaries={summaries} onOpenProject={onOpenProject} />
      )}
      {view === 'cards' && sections.projects.length > 0 && (
        <section className={styles.section}>
          <h2 className={`${styles.sectionTitle} page-title`}>Projects</h2>
          {cards(sections.projects)}
        </section>
      )}
      {view === 'cards' && sections.streams.length > 0 && (
        <section className={styles.section}>
          <h2 className={`${styles.sectionTitle} page-title`}>Streams</h2>
          <p className={styles.muted}>Work that never finishes and needs a steady trickle of attention.</p>
          {cards(sections.streams)}
        </section>
      )}
    </>
  )
}

function ProjectCard({
  row,
  slot,
  filedSessionIds,
  summaries,
  attention,
  onOpen,
}: {
  row: ProjectTreeRow
  slot: RollupSlot | null
  filedSessionIds: string[]
  summaries: ReadonlyMap<string, SessionSummary>
  attention: ProjectAttention | null
  onOpen: () => void
}): JSX.Element {
  const { project, depth } = row
  const rollup = slot?.rollup ?? null
  const spend = filedSessionSpend(filedSessionIds, summaries)
  const lastActivity = rollup ? ageFromSeconds(lastActivitySeconds(rollup, summaries)) : null
  const nowSeconds = Math.floor(Date.now() / 1000)
  const nextSteps = rollup ? openNextSteps(rollup) : []
  return (
    <article
      className={`${styles.card} ${depth > 0 ? styles.cardChild : ''}`}
      style={{ marginLeft: depth * 24 }}
      data-project-id={project.id}
    >
      <header className={styles.cardHead}>
        <button type="button" className={styles.cardName} onClick={onOpen}>{project.name}</button>
        <StageChip stage={project.stage} />
        {lastActivity && <span className={styles.muted} title="Last activity">{lastActivity} ago</span>}
        {rollup && <CommitBars repos={rollup.repos} nowSeconds={nowSeconds} />}
      </header>
      {project.goal && <p className={styles.goal}>{project.goal}</p>}
      {rollup && <StepProgressBar rollup={rollup} />}
      {slot?.error && <p className={styles.error}>{slot.error}</p>}
      {!slot && <p className={styles.muted}>Loading…</p>}
      {rollup && (
        <div className={styles.stats}>
          <Stat label="open cards">
            {openCardCounts(rollup).length === 0 ? (
              '0'
            ) : (
              openCardCounts(rollup).map(({ workState, count }) => (
                <span
                  key={workState}
                  className={`${styles.stateChip} ${workState === WAITING_WORK_STATE ? styles.stateWaiting : ''}`}
                  title={`${count} open ${count === 1 ? 'card' : 'cards'} ${workState || 'in a column with no state'}`}
                >
                  {count} {workState.replace(/_/g, ' ')}
                </span>
              ))
            )}
          </Stat>
          <Stat label={nextSteps.length === 1 ? 'next step' : 'next steps'}>{nextSteps.length}</Stat>
          <Stat label="unmerged branches">{unmergedBranchCount(rollup)}</Stat>
          <Stat label={`filed ${spend.count === 1 ? 'session' : 'sessions'}`}>
            {spend.count}
            {spend.count > 0 && <span className={styles.muted}> · {spendText(spend.spendUsd, spend.sessionsWithoutSpend)}</span>}
          </Stat>
        </div>
      )}
      {nextSteps.length > 0 && (
        <p className={styles.nextStepLine}>
          <span className={styles.muted}>Next:</span> <RefChip kind="todo" refId={nextSteps[0].note_id} />
          {nextSteps.length > 1 && <span className={styles.muted}> and {nextSteps.length - 1} more</span>}
        </p>
      )}
      {attention && <AttentionChips attention={attention} />}
      {rollup && rollup.repos.length > 0 && (
        <p className={styles.deploys} title="Last deploy of each repo">
          {rollup.repos.map((repo) => {
            const age = repo.latest_deploy ? ageFromSeconds(repo.latest_deploy.created_at) : null
            return (
              <span key={repo.repo_store_id} className={styles.deployItem}>
                <RefChip kind="registered" refId={repoMention(repo.name)} /> {age ? `deployed ${age} ago` : 'never deployed'}
              </span>
            )
          })}
        </p>
      )}
    </article>
  )
}

function Stat({ label, children }: { label: string; children: ReactNode }): JSX.Element {
  return (
    <div className={styles.stat}>
      <div className={styles.statValue}>{children}</div>
      <div className={styles.statLabel}>{label}</div>
    </div>
  )
}

/** Fourteen bars, one per local day, today on the right: commits on the
 *  project's default branches. */
function CommitBars({ repos, nowSeconds }: { repos: RepoRollup[]; nowSeconds: number }): JSX.Element | null {
  const counts = commitsPerDay(repos, 14, nowSeconds)
  const total = counts.reduce((n, c) => n + c, 0)
  if (repos.length === 0) return null
  const tallest = Math.max(1, ...counts)
  return (
    <span className={styles.commitBars} title={`${total} commits on the default branches in 14 days`} aria-label={`${total} commits in 14 days`}>
      {counts.map((count, index) => (
        <span key={index} className={styles.commitBar} style={{ height: `${count === 0 ? 2 : 3 + (count / tallest) * 15}px` }}
          data-empty={count === 0 || undefined} />
      ))}
    </span>
  )
}

const ATTENTION_LABELS: Array<[keyof ProjectAttention, string, string]> = [
  ['openQuestions', 'question for you', 'questions for you'],
  ['waitingCards', 'waiting card', 'waiting cards'],
  ['suggestionsToDecide', 'suggestion to decide', 'suggestions to decide'],
  ['openPullRequests', 'open pull request', 'open pull requests'],
  ['undeployedCommits', 'commit not deployed', 'commits not deployed'],
  ['strandedBranches', 'stranded branch', 'stranded branches'],
  ['missingNotes', 'missing note', 'missing notes'],
]

function AttentionChips({ attention }: { attention: ProjectAttention }): JSX.Element | null {
  const shown = ATTENTION_LABELS.filter(([key]) => attention[key] > 0)
  if (shown.length === 0) return null
  return (
    <span className={styles.attention}>
      {shown.map(([key, one, many]) => (
        <span key={key} className={`${styles.stateChip} ${styles.stateWaiting}`} data-attention={key}>
          {attention[key]} {attention[key] === 1 ? one : many}
        </span>
      ))}
    </span>
  )
}
