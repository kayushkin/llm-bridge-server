import { useCallback, useEffect, useState } from 'react'
import type { FormEvent, JSX, ReactNode } from 'react'
import { Link as RouterLink } from 'react-router-dom'
import type { SessionSummary, SignalRequest } from '@kayushkin/chat-core'
import type { ActivityEvent, LinkedNote, Project, RepoRollup, Rollup, Suggestion } from '@kayushkin/project-store-types'
import { useBridgeConfig } from '../../context'
import { formatCost, timeAgo } from '../../utils'
import {
  ACTIVITY_DAY_CHOICES,
  PROJECTS_PAGE_AUTHOR,
  STRANDED_BRANCH_AGE_SECONDS,
  WAITING_WORK_STATE,
  activityByDay,
  cardsByWorkState,
  commitMention,
  entityTypeHeading,
  finishedNextSteps,
  linkedNotesOtherThanTodos,
  noteboardItemPath,
  openNextSteps,
  ownedThingsByType,
  projectActivityPath,
  projectNextStepsPath,
  projectPath,
  projectSuggestionsPath,
  repoMention,
  suggestionDecisionPath,
  undeployedCommitCount,
  type AcceptedSuggestionAnswer,
  type ActivityAnswer,
  type ProjectVocabulary,
  type SuggestionListAnswer,
} from '../../projects'
import { RefChip } from './RefChip'
import { SignalRequestList } from './SessionSignals'
import { ProjectMonthStrip, StepProgressBar } from './ProjectsLandingViews'
import { questionCount } from '../../projectViews'
import styles from './ProjectsView.module.css'

// One project's page in the projects view: what it is for (editable), its next
// steps and the model's suggested ones, what is open on it, what happened in
// it, and what it owns. project-store works out everything shown; the rules
// for reading it are in `src/projects.ts`. Every id is drawn as a chip, so a
// glance at a session, todo, repo or commit needs no click away.

/** How many activity events the log draws before asking to show the rest. */
const ACTIVITY_EVENTS_SHOWN = 80

async function readJSON<T>(response: Response, what: string): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim()
    throw new Error(`${what}: ${response.status} ${response.statusText}${body ? ` — ${body}` : ''}`)
  }
  return (await response.json()) as T
}

function agoFromSeconds(seconds: number): string {
  return timeAgo(new Date(seconds * 1000).toISOString())
}

export type RollupSlot = { rollup: Rollup; error: null } | { rollup: null; error: string }

/** The open question groups of the sessions at work on a project, split as
 *  the chat splits them: the operator's, and background agents'. */
export interface ProjectQuestions {
  operator: SignalRequest[]
  background: SignalRequest[]
}

export interface ProjectPageProps {
  project: Project
  slot: RollupSlot | null
  projectsById: ReadonlyMap<string, Project>
  vocabulary: ProjectVocabulary | null
  summaries: ReadonlyMap<string, SessionSummary>
  onOpenProject: (projectId: string) => void
  onSelectSession: (sessionId: string) => void
  /** Read the project and its rollup again after a write. */
  onChanged: () => void
  /** null until the rollup says which sessions are the project's. */
  questions: ProjectQuestions | null
  /** The signals read's failure, verbatim. */
  questionsError: string | null
  /** Re-read the open signals after an answer lands. */
  onQuestionAnswered: () => void
}

export function ProjectPage({
  project,
  slot,
  projectsById,
  vocabulary,
  summaries,
  onOpenProject,
  onSelectSession,
  onChanged,
  questions,
  questionsError,
  onQuestionAnswered,
}: ProjectPageProps): JSX.Element {
  const rollup = slot?.rollup ?? null
  const parent = projectsById.get(project.parent_id)
  const openSession = useCallback((_kind: string, id: string) => onSelectSession(id), [onSelectSession])

  return (
    <div className={styles.projectPage} data-project-id={project.id}>
      <section className={styles.section}>
        <div className={styles.cardHead}>
          <h2 className={`${styles.pageTitle} page-title`}>{project.name}</h2>
          <StageChip stage={project.stage} />
          <span className={styles.muted}>{project.kind}</span>
          <RefChip kind="registered" refId={project.id} />
        </div>
        {parent && (
          <p className={styles.muted}>
            Part of <button type="button" className={styles.linkButton} onClick={() => onOpenProject(parent.id)}>{parent.name}</button>
          </p>
        )}
        <ProjectPurpose project={project} rollup={rollup} stages={vocabulary?.stages ?? null} onChanged={onChanged} />
        <ProjectMonthStrip projectId={project.id} />
      </section>

      {slot?.error && <p className={styles.error}>{slot.error}</p>}
      {!slot && <p className={styles.notice}>Loading…</p>}

      {rollup && (
        <div className={styles.pageColumns}>
          <div className={styles.mainColumn}>
            <Questions questions={questions} error={questionsError} onAnswered={onQuestionAnswered} />
            <NextSteps project={project} rollup={rollup} onChanged={onChanged} />
            <SuggestedSteps project={project} suggestions={rollup.suggestions ?? []} onChanged={onChanged} />
            <OpenIssues rollup={rollup} onActivateSession={openSession} />
            <Activity projectId={project.id} onActivateSession={openSession} />
          </div>
          <aside className={styles.sideColumn}>
            <Repos repos={rollup.repos} />
            <FiledSessions rollup={rollup} summaries={summaries} onActivateSession={openSession} />
            {rollup.children.length > 0 && (
              <Panel title="Sub-projects">
                <ul className={styles.plainList}>
                  {rollup.children.map((child) => (
                    <li key={child.id}>
                      <button type="button" className={styles.linkButton} onClick={() => onOpenProject(child.id)}>{child.name}</button>{' '}
                      <StageChip stage={child.stage} />
                      {child.goal && <div className={styles.muted}>{child.goal}</div>}
                    </li>
                  ))}
                </ul>
              </Panel>
            )}
            <Owned rollup={rollup} vocabulary={vocabulary} />
          </aside>
        </div>
      )}
    </div>
  )
}

export function StageChip({ stage }: { stage: string }): JSX.Element {
  return <span className={styles.stageChip} data-stage={stage}>{stage.replace(/_/g, ' ')}</span>
}

function Panel({ title, count, actions, children }: { title: string; count?: number; actions?: ReactNode; children: ReactNode }): JSX.Element {
  return (
    <section className={styles.panel}>
      <header className={styles.panelHead}>
        <h3 className={styles.subTitle}>
          {title}
          {count !== undefined && <span className={styles.countBadge}>{count}</span>}
        </h3>
        {actions && <span className={styles.panelActions}>{actions}</span>}
      </header>
      {children}
    </section>
  )
}

// ---------------------------------------------------------------------------
// Goal, done when, stage
// ---------------------------------------------------------------------------

function ProjectPurpose({
  project,
  rollup,
  stages,
  onChanged,
}: {
  project: Project
  rollup: Rollup | null
  stages: string[] | null
  onChanged: () => void
}): JSX.Element {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const [editing, setEditing] = useState(false)
  const [goal, setGoal] = useState(project.goal)
  const [doneWhen, setDoneWhen] = useState(project.done_when)
  const [stage, setStage] = useState(project.stage)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (editing) return
    setGoal(project.goal)
    setDoneWhen(project.done_when)
    setStage(project.stage)
  }, [editing, project.goal, project.done_when, project.stage])

  const save = async (event: FormEvent) => {
    event.preventDefault()
    if (!projectStoreBasePath) return
    // Send only what changed: project-store records every field it is sent
    // that differs, and an unchanged one would be noise in nothing, but
    // sending less says plainly what this edit was.
    const body: Record<string, string> = { changed_by: PROJECTS_PAGE_AUTHOR }
    if (goal !== project.goal) body.goal = goal
    if (doneWhen !== project.done_when) body.done_when = doneWhen
    if (stage !== project.stage) body.stage = stage
    setSaving(true)
    setError(null)
    try {
      await readJSON<Project>(
        await fetchFn(projectPath(projectStoreBasePath, project.id), {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }),
        'save the project',
      )
      setEditing(false)
      onChanged()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  if (!editing) {
    const now = rollup ? openNextSteps(rollup)[0] : undefined
    return (
      <div className={styles.direction}>
        <div className={styles.directionGoal}>
          <span className={styles.directionLabel}>Goal</span>
          <p className={styles.directionGoalText}>{project.goal || <span className={styles.muted}>No goal set yet.</span>}</p>
        </div>
        <div className={styles.directionFinish}>
          <span className={styles.directionLabel}>Done when</span>
          <p>{project.done_when || <span className={styles.muted}>Not set: say what done looks like, so progress can be judged.</span>}</p>
        </div>
        <div className={styles.directionFoot}>
          {rollup && <StepProgressBar rollup={rollup} />}
          {now && (
            <span className={styles.directionNow}>
              <span className={styles.directionLabel}>Now</span> <RefChip kind="todo" refId={now.note_id} />
            </span>
          )}
          <button type="button" className="bc-ctrl-btn" onClick={() => setEditing(true)}>Edit goal</button>
        </div>
      </div>
    )
  }
  return (
    <form className={styles.purposeForm} onSubmit={save}>
      <label className={styles.field}>
        <span className={styles.fieldLabel}>Goal</span>
        <textarea name="goal" className={styles.textInput} rows={2} value={goal} onChange={(e) => setGoal(e.target.value)} />
      </label>
      <label className={styles.field}>
        <span className={styles.fieldLabel}>Done when</span>
        <textarea name="done_when" className={styles.textInput} rows={2} value={doneWhen} onChange={(e) => setDoneWhen(e.target.value)} />
      </label>
      {stages && (
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Stage</span>
          <select name="stage" className={styles.textInput} value={stage} onChange={(e) => setStage(e.target.value)}>
            {stages.map((s) => <option key={s} value={s}>{s.replace(/_/g, ' ')}</option>)}
          </select>
        </label>
      )}
      {error && <p className={styles.error}>{error}</p>}
      <div className={styles.formButtons}>
        <button type="submit" className="bc-ctrl-btn" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
        <button type="button" className="bc-ctrl-btn" disabled={saving} onClick={() => setEditing(false)}>Cancel</button>
      </div>
    </form>
  )
}

// ---------------------------------------------------------------------------
// Next steps and suggested ones
// ---------------------------------------------------------------------------

function Questions({
  questions,
  error,
  onAnswered,
}: {
  questions: ProjectQuestions | null
  error: string | null
  onAnswered: () => void
}): JSX.Element | null {
  const [showBackground, setShowBackground] = useState(false)
  if (error) return <p className={styles.error}>Couldn’t load the sessions’ questions: {error}</p>
  if (!questions) return null
  const operatorCount = questionCount(questions.operator)
  const backgroundCount = questionCount(questions.background)
  if (operatorCount === 0 && backgroundCount === 0) return null
  return (
    <Panel title="Questions for you" count={operatorCount}>
      <p className={styles.muted}>Sessions at work on this project — filed under it, on its branches, or behind its recent commits — are waiting on these.</p>
      {questions.operator.map((request) => (
        <div key={request.requestId || request.signals[0]?.id} className={styles.questionGroup}>
          <div className={styles.questionFrom}>
            <span className={styles.muted}>from</span> <RefChip kind="session" refId={request.sessionId} />
          </div>
          <SignalRequestList requests={[request]} onResolved={onAnswered} />
        </div>
      ))}
      {backgroundCount > 0 && (
        <div className={styles.group}>
          <button type="button" className={styles.linkButton} onClick={() => setShowBackground((v) => !v)}>
            {showBackground ? 'Hide' : 'Show'} {backgroundCount} from background agents
          </button>
          {showBackground && questions.background.map((request) => (
            <div key={request.requestId || request.signals[0]?.id} className={styles.questionGroup}>
              <div className={styles.questionFrom}>
                <span className={styles.muted}>from</span> <RefChip kind="session" refId={request.sessionId} />
              </div>
              <SignalRequestList requests={[request]} compact onResolved={onAnswered} />
            </div>
          ))}
        </div>
      )}
    </Panel>
  )
}

function NextSteps({ project, rollup, onChanged }: { project: Project; rollup: Rollup; onChanged: () => void }): JSX.Element {
  const { fetch: fetchFn, projectStoreBasePath, noteboardBasePath } = useBridgeConfig()
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [showFinished, setShowFinished] = useState(false)
  const open = openNextSteps(rollup)
  const finished = finishedNextSteps(rollup)

  const run = async (key: string, what: string, call: () => Promise<Response>) => {
    setBusy(key)
    setError(null)
    try {
      await readJSON<unknown>(await call(), what)
      onChanged()
      return true
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return false
    } finally {
      setBusy(null)
    }
  }

  const add = async (event: FormEvent) => {
    event.preventDefault()
    if (!projectStoreBasePath || title.trim() === '') return
    const added = await run('add', 'add the step', () =>
      fetchFn(projectNextStepsPath(projectStoreBasePath, project.id), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: title.trim(), created_by: PROJECTS_PAGE_AUTHOR }),
      }),
    )
    if (added) setTitle('')
  }

  // A step's status is noteboard's, so finishing one is noteboard's write.
  const setStatus = (note: LinkedNote, status: 'open' | 'done') =>
    run(note.note_id, status === 'done' ? 'finish the step' : 'reopen the step', () =>
      fetchFn(noteboardItemPath(noteboardBasePath, note.note_id), {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status }),
      }),
    )

  return (
    <Panel title="Next steps" count={open.length}>
      {open.length === 0 && <p className={styles.muted}>No open step. Add one, or ask for suggestions below.</p>}
      <ul className={styles.stepList}>
        {open.map((note) => (
          <li key={note.note_id} className={styles.stepRow}>
            <input
              type="checkbox"
              name={`step-${note.note_id}`}
              checked={false}
              disabled={busy !== null}
              onChange={() => void setStatus(note, 'done')}
              aria-label={`Mark "${note.title}" done`}
              title="Mark done"
            />
            <RefChip kind="todo" refId={note.note_id} />
            {note.priority > 0 && <span className={styles.priority} title="noteboard priority; higher is more urgent">p{note.priority}</span>}
            {note.due_at && <span className={styles.muted}>due {timeAgo(note.due_at)}</span>}
          </li>
        ))}
      </ul>
      <form className={styles.addStep} onSubmit={add}>
        <input
          name="next_step_title"
          className={styles.textInput}
          placeholder="Add a next step…"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          disabled={busy === 'add'}
        />
        <button type="submit" className="bc-ctrl-btn" disabled={busy !== null || title.trim() === ''}>
          {busy === 'add' ? 'Adding…' : 'Add'}
        </button>
      </form>
      {error && <p className={styles.error}>{error}</p>}
      {finished.length > 0 && (
        <div className={styles.group}>
          <button type="button" className={styles.linkButton} onClick={() => setShowFinished((v) => !v)}>
            {showFinished ? 'Hide' : 'Show'} {finished.length} finished
          </button>
          {showFinished && (
            <ul className={styles.stepList}>
              {finished.map((note) => (
                <li key={note.note_id} className={`${styles.stepRow} ${styles.stepDone}`}>
                  <input
                    type="checkbox"
                    name={`step-${note.note_id}`}
                    checked
                    disabled={busy !== null}
                    onChange={() => void setStatus(note, 'open')}
                    aria-label={`Reopen "${note.title}"`}
                    title="Reopen"
                  />
                  <RefChip kind="todo" refId={note.note_id} />
                  {note.completed_at && <span className={styles.muted}>{timeAgo(note.completed_at)}</span>}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </Panel>
  )
}

function SuggestedSteps({ project, suggestions, onChanged }: { project: Project; suggestions: Suggestion[]; onChanged: () => void }): JSX.Element {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const call = async <T,>(key: string, what: string, path: string, body: object): Promise<T | null> => {
    setBusy(key)
    setError(null)
    try {
      const answer = await readJSON<T>(
        await fetchFn(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
        what,
      )
      onChanged()
      return answer
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return null
    } finally {
      setBusy(null)
    }
  }

  if (!projectStoreBasePath) return <></>
  const ask = () =>
    void call<SuggestionListAnswer>('ask', 'ask for suggestions', projectSuggestionsPath(projectStoreBasePath, project.id), {
      requested_by: PROJECTS_PAGE_AUTHOR,
    })
  const decide = (suggestion: Suggestion, decision: 'accept' | 'dismiss') =>
    void call<AcceptedSuggestionAnswer | Suggestion>(suggestion.id, `${decision} the suggestion`,
      suggestionDecisionPath(projectStoreBasePath, suggestion.id, decision), { decided_by: PROJECTS_PAGE_AUTHOR })

  return (
    <Panel
      title="Suggested next steps"
      count={suggestions.length > 0 ? suggestions.length : undefined}
      actions={
        <button type="button" className="bc-ctrl-btn" onClick={ask} disabled={busy !== null}
          title="A model reads the project's goal, steps, cards, pull requests and recent activity, and proposes steps. Nothing becomes a step until you accept it.">
          {busy === 'ask' ? 'Thinking… (up to a minute)' : suggestions.length > 0 ? 'Suggest again' : 'Suggest next steps'}
        </button>
      }
    >
      {suggestions.length === 0 && busy !== 'ask' && (
        <p className={styles.muted}>None waiting. A model's proposals wait here until you accept one, which makes it a next step, or dismiss it.</p>
      )}
      <ul className={styles.suggestionList}>
        {suggestions.map((suggestion) => (
          <li key={suggestion.id} className={styles.suggestion}>
            <div className={styles.suggestionText}>
              <div className={styles.suggestionTitle}>{suggestion.title}</div>
              {suggestion.why && <div className={styles.muted}>{suggestion.why}</div>}
              <div className={`${styles.muted} ${styles.mono}`}>{suggestion.id} · {suggestion.model}</div>
            </div>
            <div className={styles.suggestionButtons}>
              <button type="button" className="bc-ctrl-btn" disabled={busy !== null} onClick={() => decide(suggestion, 'accept')}>
                {busy === suggestion.id ? '…' : 'Accept'}
              </button>
              <button type="button" className="bc-ctrl-btn" disabled={busy !== null} onClick={() => decide(suggestion, 'dismiss')}>Dismiss</button>
            </div>
          </li>
        ))}
      </ul>
      {error && <p className={styles.error}>{error}</p>}
    </Panel>
  )
}

// ---------------------------------------------------------------------------
// What is open
// ---------------------------------------------------------------------------

function OpenIssues({ rollup, onActivateSession }: { rollup: Rollup; onActivateSession: (kind: string, id: string) => void }): JSX.Element {
  const { routes } = useBridgeConfig()
  const now = Math.floor(Date.now() / 1000)
  const openCards = cardsByWorkState({ cards: rollup.cards.filter((c) => c.status === 'open') })
  const pullRequests = rollup.repos.flatMap((repo) => (repo.open_pull_requests ?? []).map((pr) => ({ repo, pr })))
  const branches = rollup.repos
    .flatMap((repo) => repo.unmerged_branches.map((branch) => ({ repo, branch })))
    .sort((a, b) => b.branch.committed_at - a.branch.committed_at)
  const missing = rollup.missing_note_ids ?? []
  const total = openCards.reduce((n, g) => n + g.cards.length, 0) + pullRequests.length + branches.length + missing.length

  return (
    <Panel title="Open issues" count={total}>
      {total === 0 && <p className={styles.muted}>Nothing open: no card, pull request or unmerged branch.</p>}
      {openCards.map(({ workState, cards }) => (
        <div key={workState} className={styles.group}>
          <h4 className={`${styles.groupTitle} ${workState === WAITING_WORK_STATE ? styles.stateWaiting : ''}`}>
            {workState ? `Cards ${workState.replace(/_/g, ' ')}` : 'Cards in a column with no state'} ({cards.length})
          </h4>
          <ul className={styles.plainList}>
            {cards.map((card) => (
              <li key={card.card_id} className={styles.issueRow}>
                <RefChip kind="todo" refId={card.card_id} />
                {routes.card && (
                  <RouterLink className={styles.muted} to={`${routes.card}/${encodeURIComponent(card.card_id)}`}>open card</RouterLink>
                )}
              </li>
            ))}
          </ul>
        </div>
      ))}
      {pullRequests.length > 0 && (
        <div className={styles.group}>
          <h4 className={styles.groupTitle}>Pull requests ({pullRequests.length})</h4>
          <ul className={styles.plainList}>
            {pullRequests.map(({ repo, pr }) => (
              <li key={`${repo.repo_store_id}-${pr.number}`} className={styles.issueRow}>
                <RefChip kind="registered" refId={repoMention(repo.name)} />
                <a href={pr.url} target="_blank" rel="noreferrer">#{pr.number} {pr.title}</a>
                {pr.is_draft && <span className={styles.stageChip}>draft</span>}
                <span className={styles.muted}>by {pr.author}, opened {agoFromSeconds(pr.created_at)}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
      {branches.length > 0 && (
        <div className={styles.group}>
          <h4 className={styles.groupTitle}>Unmerged branches ({branches.length})</h4>
          <ul className={styles.plainList}>
            {branches.map(({ repo, branch }) => {
              const stranded = now - branch.committed_at > STRANDED_BRANCH_AGE_SECONDS
              return (
                <li key={`${repo.repo_store_id}-${branch.name}`} className={styles.issueRow}>
                  <RefChip kind="registered" refId={repoMention(repo.name)} />
                  <span className={`${styles.mono} ${styles.branch}`}>{branch.name}</span>
                  {branch.head_sha && <RefChip kind="registered" refId={commitMention(repo.name, branch.head_sha)} />}
                  <span className={stranded ? styles.stateWaiting : styles.muted}
                    title={stranded ? 'Last commit over a day ago and not merged: nobody seems to be at work on it' : undefined}>
                    {stranded ? 'stranded, ' : ''}last commit {agoFromSeconds(branch.committed_at)}
                  </span>
                  {branch.worktree_path && <span className={`${styles.mono} ${styles.muted}`}>{branch.worktree_path}</span>}
                  {branch.session_ids.map((id) => (
                    <RefChip key={id} kind="session" refId={id} onActivate={onActivateSession} />
                  ))}
                </li>
              )
            })}
          </ul>
        </div>
      )}
      {missing.length > 0 && (
        <div className={styles.group}>
          <h4 className={`${styles.groupTitle} ${styles.stateWaiting}`}>Links to notes noteboard no longer has ({missing.length})</h4>
          <ul className={styles.plainList}>
            {missing.map((id) => <li key={id} className={styles.mono}>{id}</li>)}
          </ul>
        </div>
      )}
    </Panel>
  )
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

function Activity({ projectId, onActivateSession }: { projectId: string; onActivateSession: (kind: string, id: string) => void }): JSX.Element {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const [days, setDays] = useState<number>(14)
  const [answer, setAnswer] = useState<ActivityAnswer | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [showAll, setShowAll] = useState(false)

  useEffect(() => {
    if (!projectStoreBasePath) return
    let canceled = false
    setError(null)
    fetchFn(projectActivityPath(projectStoreBasePath, projectId, days))
      .then((r) => readJSON<ActivityAnswer>(r, 'project activity'))
      .then((a) => { if (!canceled) setAnswer(a) })
      .catch((e: Error) => { if (!canceled) setError(e.message) })
    return () => { canceled = true }
  }, [fetchFn, projectStoreBasePath, projectId, days])

  const events = answer?.events ?? []
  const shown = showAll ? events : events.slice(0, ACTIVITY_EVENTS_SHOWN)
  return (
    <Panel
      title="Recent activity"
      count={answer ? events.length : undefined}
      actions={
        <span className={styles.segmented} role="group" aria-label="How far back">
          {ACTIVITY_DAY_CHOICES.map((choice) => (
            <button key={choice} type="button" className={`bc-ctrl-btn ${choice === days ? 'bc-ctrl-btn-active' : ''}`}
              aria-pressed={choice === days} onClick={() => setDays(choice)}>
              {choice}d
            </button>
          ))}
        </span>
      }
    >
      {error && <p className={styles.error}>{error}</p>}
      {!answer && !error && <p className={styles.muted}>Loading…</p>}
      {answer && events.length === 0 && <p className={styles.muted}>Nothing happened in the last {days} days.</p>}
      {activityByDay(shown).map((day) => (
        <div key={day.dayStart} className={styles.activityDay}>
          <h4 className={styles.activityDayTitle}>
            {new Date(day.dayStart * 1000).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' })}
          </h4>
          <ul className={styles.activityList}>
            {day.events.map((event, index) => (
              <li key={`${event.kind}-${event.at}-${index}`} className={styles.activityRow} data-kind={event.kind}>
                <span className={styles.activityTime}>
                  {new Date(event.at * 1000).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })}
                </span>
                <span className={styles.activityIcon} aria-hidden>{ACTIVITY_ICONS[event.kind] ?? '•'}</span>
                <span className={styles.activityBody}><ActivityLine event={event} onActivateSession={onActivateSession} /></span>
              </li>
            ))}
          </ul>
        </div>
      ))}
      {events.length > shown.length && (
        <button type="button" className={styles.linkButton} onClick={() => setShowAll(true)}>
          Show {events.length - shown.length} more
        </button>
      )}
    </Panel>
  )
}

/** One glyph per kind of event, so a day of activity reads at a glance. */
const ACTIVITY_ICONS: Record<string, string> = {
  project_changed: '✎',
  link_added: '＋',
  commit_landed: '●',
  deployed: '🚀',
  pull_request_opened: '⇡',
  pull_request_merged: '⇣',
  pull_request_closed: '✕',
  note_completed: '✓',
  suggestion_accepted: '✓',
  suggestion_dismissed: '–',
}

function ActivityLine({ event, onActivateSession }: { event: ActivityEvent; onActivateSession: (kind: string, id: string) => void }): JSX.Element {
  const session = event.session_id ? <RefChip kind="session" refId={event.session_id} onActivate={onActivateSession} /> : null
  const actor = event.actor ? <span className={styles.muted}>by {event.actor}</span> : null
  const commit = event.repo_name && event.commit_sha ? <RefChip kind="registered" refId={commitMention(event.repo_name, event.commit_sha)} /> : null
  const pullRequest = event.url ? <a href={event.url} target="_blank" rel="noreferrer">#{event.pull_request_number} {event.pull_request_title}</a> : null
  switch (event.kind) {
    case 'commit_landed':
      return <>{commit} <span>{event.subject}</span> {session ?? actor}</>
    case 'deployed':
      return <>Deployed <RefChip kind="registered" refId={repoMention(event.repo_name)} /> at {commit} {actor}</>
    case 'pull_request_opened':
      return <>Opened {pullRequest} on <RefChip kind="registered" refId={repoMention(event.repo_name)} /> {actor}</>
    case 'pull_request_merged':
      return <>Merged {pullRequest} on <RefChip kind="registered" refId={repoMention(event.repo_name)} /></>
    case 'pull_request_closed':
      return <>Closed {pullRequest} unmerged on <RefChip kind="registered" refId={repoMention(event.repo_name)} /></>
    case 'project_changed':
      return (
        <>
          Changed <b>{event.field.replace(/_/g, ' ')}</b>{' '}
          {event.old_value ? <><span className={styles.oldValue}>{event.old_value}</span> → </> : 'to '}
          <span>{event.new_value || <span className={styles.muted}>nothing</span>}</span> {actor}
        </>
      )
    case 'link_added':
      if (event.entity_type === 'session') return <>Filed {session}</>
      if (event.entity_type === 'note') return <>Linked <RefChip kind="todo" refId={event.entity_ref} /> {actor}</>
      if (event.entity_type === 'repo') return <>Took on <RefChip kind="registered" refId={repoMention(event.label)} /></>
      return <>Took on {entityTypeHeading(event.entity_type).replace(/s$/, '').toLowerCase()} {event.label || event.entity_ref} <span className={`${styles.mono} ${styles.muted}`}>{event.entity_ref}</span></>
    case 'note_completed':
      return <>Finished <RefChip kind="todo" refId={event.entity_ref} /></>
    case 'suggestion_accepted':
      return <>Accepted a suggested step: {event.entity_ref ? <RefChip kind="todo" refId={event.entity_ref} /> : event.label} {actor}</>
    case 'suggestion_dismissed':
      return <>Dismissed a suggested step: <span className={styles.oldValue}>{event.label}</span> {actor}</>
    default:
      return <>{event.kind.replace(/_/g, ' ')}</>
  }
}

// ---------------------------------------------------------------------------
// Side column
// ---------------------------------------------------------------------------

function Repos({ repos }: { repos: RepoRollup[] }): JSX.Element | null {
  if (repos.length === 0) return null
  return (
    <Panel title="Repos" count={repos.length}>
      <ul className={styles.repoList}>
        {repos.map((repo) => {
          const undeployed = undeployedCommitCount(repo)
          const commits = repo.recent_commits ?? []
          return (
            <li key={repo.repo_store_id} className={styles.repoRow}>
              <div className={styles.repoHead}>
                <RefChip kind="registered" refId={repoMention(repo.name)} />
                {undeployed !== null && undeployed > 0 && (
                  <span className={styles.stateWaiting} title="Commits on the default branch newer than the deployed one">
                    {undeployed} not deployed
                  </span>
                )}
              </div>
              <div className={styles.muted}>
                {repo.latest_deploy ? (
                  <>deployed {agoFromSeconds(repo.latest_deploy.created_at)} at{' '}
                    <RefChip kind="registered" refId={commitMention(repo.name, repo.latest_deploy.commit_sha)} /></>
                ) : (
                  'never deployed'
                )}
              </div>
              <div className={styles.muted}>
                {commits.length} {commits.length === 1 ? 'commit' : 'commits'} in 14 days
                {commits[0] && <>, last {agoFromSeconds(commits[0].committed_at)}</>}
                {(repo.open_pull_requests ?? []).length > 0 && <>, {repo.open_pull_requests.length} open PR</>}
              </div>
            </li>
          )
        })}
      </ul>
    </Panel>
  )
}

function FiledSessions({
  rollup,
  summaries,
  onActivateSession,
}: {
  rollup: Rollup
  summaries: ReadonlyMap<string, SessionSummary>
  onActivateSession: (kind: string, id: string) => void
}): JSX.Element {
  const links = rollup.links
    .filter((l) => l.entity_type === 'session')
    .sort((a, b) => {
      const at = (id: string) => Date.parse(summaries.get(id)?.updatedAt ?? '') || 0
      return at(b.entity_ref) - at(a.entity_ref)
    })
  return (
    <Panel title="Filed sessions" count={links.length}>
      {links.length === 0 ? (
        <p className={styles.muted}>No session is filed here. File one from its header.</p>
      ) : (
        <ul className={styles.plainList}>
          {links.map((link) => {
            const summary = summaries.get(link.entity_ref)
            return (
              <li key={link.id} className={styles.sessionRow}>
                <RefChip kind="session" refId={link.entity_ref} onActivate={onActivateSession} />
                {typeof summary?.spendUsd === 'number' && <span className={styles.muted}>{formatCost(summary.spendUsd)}</span>}
                {summary?.updatedAt && <span className={styles.muted}>{timeAgo(summary.updatedAt)}</span>}
              </li>
            )
          })}
        </ul>
      )}
    </Panel>
  )
}

function Owned({ rollup, vocabulary }: { rollup: Rollup; vocabulary: ProjectVocabulary | null }): JSX.Element | null {
  const notes = linkedNotesOtherThanTodos(rollup)
  // Repos, sessions and notes have panels of their own.
  const owned = ownedThingsByType(rollup.links, vocabulary?.entity_types ?? []).filter(
    ({ entityType }) => entityType !== 'repo' && entityType !== 'note',
  )
  if (notes.length === 0 && owned.length === 0) return null
  return (
    <Panel title="What it owns">
      {notes.length > 0 && (
        <div className={styles.group}>
          <h4 className={styles.groupTitle}>Notes ({notes.length})</h4>
          <ul className={styles.plainList}>
            {notes.map((note) => <li key={note.note_id}><RefChip kind="note" refId={note.note_id} /></li>)}
          </ul>
        </div>
      )}
      {owned.map(({ entityType, info, links }) => (
        <div key={entityType} className={styles.group}>
          <h4 className={styles.groupTitle} title={info?.meaning}>
            {entityTypeHeading(entityType)} <span className={styles.muted}>({info?.service ?? 'unknown store'})</span>
          </h4>
          <ul className={styles.plainList}>
            {links.map((link) => (
              <li key={link.id}>
                {/* The label is the owning store's name for the thing; the ref is
                    that store's id, said as such so a bare number reads as an id. */}
                {link.label && link.label !== link.entity_ref ? `${link.label} ` : ''}
                <span className={styles.muted}>
                  {info?.service ?? 'its store'} id <span className={styles.mono}>{link.entity_ref}</span>
                </span>
                {link.note && <span className={styles.muted}> — {link.note}</span>}
              </li>
            ))}
          </ul>
        </div>
      ))}
    </Panel>
  )
}
