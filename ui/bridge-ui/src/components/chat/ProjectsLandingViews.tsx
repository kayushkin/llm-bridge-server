import { useEffect, useMemo, useState } from 'react'
import type { DragEvent, JSX } from 'react'
import type { Project, Rollup } from '@kayushkin/project-store-types'
import type { SessionSummary } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import { formatCost } from '../../utils'
import {
  PROJECTS_PAGE_AUTHOR,
  attentionTotal,
  filedSessionSpend,
  projectActivityPath,
  projectPath,
  projectTree,
  type ActivityAnswer,
  type ProjectAttention,
} from '../../projects'
import {
  TIMELINE_DAYS,
  busiestDayCommits,
  commitShade,
  effortRows,
  projectsByStage,
  recentCommitCount,
  stepProgress,
  timelineDayStarts,
  timelineDays,
  timelineDaySummary,
  type TimelineDay,
} from '../../projectViews'
import { StageChip, type RollupSlot } from './ProjectPage'
import styles from './ProjectsView.module.css'

// The landing's three pictures of every project at once. The timeline lays a
// month of each project's activity side by side, so it shows where work went
// and where it stopped; the stage board shows the direction of every project,
// and moves one to another stage by dragging it; the effort chart sets each
// project's share of the work beside the share it was meant to get.

async function readJSON<T>(response: Response, what: string): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim()
    throw new Error(`${what}: ${response.status} ${response.statusText}${body ? ` — ${body}` : ''}`)
  }
  return (await response.json()) as T
}

/** How many activity reads the timeline makes at once. Each reaches
 *  work-graph-store, repo-store and noteboard behind project-store. */
const ACTIVITY_CONCURRENCY = 4

function commitsText(count: number): string {
  return `${count} ${count === 1 ? 'commit' : 'commits'}`
}

type ActivitySlot = { events: ActivityAnswer['events']; error: null } | { events: null; error: string }

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

/** "3 of 7 steps done" as a bar. Nothing when the project has no steps. */
export function StepProgressBar({ rollup }: { rollup: Pick<Rollup, 'notes'> }): JSX.Element | null {
  const { done, total } = stepProgress(rollup)
  if (total === 0) return null
  return (
    <span className={styles.progress} title={`${done} of ${total} linked todos done`}>
      <span className={styles.progressTrack}>
        <span className={styles.progressFill} style={{ width: `${(done / total) * 100}%` }} />
      </span>
      <span className={styles.muted}>{done} of {total} steps done</span>
    </span>
  )
}

// ---------------------------------------------------------------------------
// Timeline
// ---------------------------------------------------------------------------

export function ProjectTimeline({
  projects,
  onOpenProject,
}: {
  projects: Project[]
  onOpenProject: (projectId: string) => void
}): JSX.Element {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const [activity, setActivity] = useState<ReadonlyMap<string, ActivitySlot>>(new Map())
  const rows = useMemo(() => projectTree(projects), [projects])
  const idsKey = rows.map((r) => r.project.id).join(',')

  useEffect(() => {
    if (!projectStoreBasePath || idsKey === '') return
    let canceled = false
    const ids = idsKey.split(',')
    let next = 0
    const worker = async (): Promise<void> => {
      while (!canceled && next < ids.length) {
        const id = ids[next++]
        let slot: ActivitySlot
        try {
          const answer = await fetchFn(projectActivityPath(projectStoreBasePath, id, TIMELINE_DAYS)).then((r) =>
            readJSON<ActivityAnswer>(r, `activity of ${id}`),
          )
          slot = { events: answer.events, error: null }
        } catch (error) {
          slot = { events: null, error: error instanceof Error ? error.message : String(error) }
        }
        if (canceled) return
        setActivity((previous) => new Map(previous).set(id, slot))
      }
    }
    void Promise.all(Array.from({ length: Math.min(ACTIVITY_CONCURRENCY, ids.length) }, worker))
    return () => { canceled = true }
  }, [fetchFn, projectStoreBasePath, idsKey])

  const nowSeconds = Math.floor(Date.now() / 1000)
  const dayStarts = timelineDayStarts(TIMELINE_DAYS, nowSeconds)
  const laidOut = rows.map((row) => {
    const slot = activity.get(row.project.id)
    return { row, slot, days: slot?.events ? timelineDays(slot.events, dayStarts) : null }
  })
  const busiest = busiestDayCommits(laidOut.flatMap((r) => (r.days ? [r.days] : [])))
  const gridTemplate = `minmax(140px, 220px) repeat(${TIMELINE_DAYS}, minmax(14px, 1fr)) 5.5em`

  return (
    <section className={styles.section}>
      <p className={styles.muted}>
        The last {TIMELINE_DAYS} days, one row per project. A cell is darker the more commits landed on the project's
        default branches that day; ▲ is a deploy, ◆ a merged pull request, ✓ a finished step and ⚑ a change of stage.
        Hover a cell for the day.
      </p>
      <div className={styles.timeline} role="table" aria-label={`Activity of every project over ${TIMELINE_DAYS} days`}>
        <div className={styles.timelineRow} style={{ gridTemplateColumns: gridTemplate }} role="row">
          <span className={styles.timelineName} role="columnheader">Project</span>
          {dayStarts.map((start, index) => {
            const date = new Date(start * 1000)
            const isMonday = date.getDay() === 1
            const isToday = index === dayStarts.length - 1
            return (
              <span key={start} role="columnheader" className={`${styles.timelineHeadCell} ${isToday ? styles.timelineToday : ''}`}
                title={date.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' })}>
                {isToday ? 'today' : isMonday || date.getDate() === 1 ? date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) : ''}
              </span>
            )
          })}
          <span className={styles.timelineTotal} role="columnheader">commits</span>
        </div>
        {laidOut.map(({ row, slot, days }) => (
          <div key={row.project.id} className={styles.timelineRow} style={{ gridTemplateColumns: gridTemplate }} role="row">
            <span className={styles.timelineName} role="rowheader" style={{ paddingLeft: row.depth * 14 }}>
              <button type="button" className={styles.linkButton} onClick={() => onOpenProject(row.project.id)} title={row.project.goal}>
                {row.project.name}
              </button>{' '}
              <StageChip stage={row.project.stage} />
            </span>
            {slot?.error ? (
              <span className={styles.timelineError} style={{ gridColumn: `span ${TIMELINE_DAYS + 1}` }} title={slot.error}>
                {slot.error}
              </span>
            ) : days ? (
              <>
                {days.map((day, index) => (
                  <TimelineCell key={day.dayStart} day={day} busiest={busiest} isToday={index === days.length - 1} />
                ))}
                <span className={styles.timelineTotal}>{days.reduce((n, d) => n + d.commits, 0)}</span>
              </>
            ) : (
              <span className={styles.timelineLoading} style={{ gridColumn: `span ${TIMELINE_DAYS + 1}` }}>Loading…</span>
            )}
          </div>
        ))}
      </div>
    </section>
  )
}

function TimelineCell({ day, busiest, isToday }: { day: TimelineDay; busiest: number; isToday: boolean }): JSX.Element {
  const shade = commitShade(day.commits, busiest)
  const date = new Date(day.dayStart * 1000).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' })
  const summary = timelineDaySummary(day)
  return (
    <span
      role="cell"
      className={`${styles.timelineCell} ${isToday ? styles.timelineToday : ''}`}
      title={summary ? `${date}\n${summary}` : `${date}: nothing`}
      data-commits={day.commits}
    >
      <span className={styles.timelineHeat} style={{ opacity: shade }} />
      <span className={styles.timelineMarks}>
        {day.stageMoves.length > 0 && <span className={styles.markStage}>⚑</span>}
        {day.deploys > 0 && <span className={styles.markDeploy}>▲</span>}
        {day.mergedPullRequests > 0 && <span className={styles.markMerge}>◆</span>}
        {day.finishedSteps > 0 && <span className={styles.markStep}>✓</span>}
      </span>
    </span>
  )
}

/** One project's month as a strip, for the top of its own page. */
export function ProjectMonthStrip({ projectId }: { projectId: string }): JSX.Element | null {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const [slot, setSlot] = useState<ActivitySlot | null>(null)
  useEffect(() => {
    if (!projectStoreBasePath) return
    let canceled = false
    fetchFn(projectActivityPath(projectStoreBasePath, projectId, TIMELINE_DAYS))
      .then((r) => readJSON<ActivityAnswer>(r, 'project activity'))
      .then((answer) => { if (!canceled) setSlot({ events: answer.events, error: null }) })
      .catch((error: Error) => { if (!canceled) setSlot({ events: null, error: error.message }) })
    return () => { canceled = true }
  }, [fetchFn, projectStoreBasePath, projectId])
  if (!slot) return null
  if (slot.error !== null) return <p className={styles.error}>{slot.error}</p>
  const days = timelineDays(slot.events, timelineDayStarts(TIMELINE_DAYS, Math.floor(Date.now() / 1000)))
  const busiest = busiestDayCommits([days])
  return (
    <div className={styles.monthStrip} aria-label={`The last ${TIMELINE_DAYS} days`}>
      <span className={styles.monthStripLabel}>{TIMELINE_DAYS} days ago</span>
      <span className={styles.monthStripCells} style={{ gridTemplateColumns: `repeat(${TIMELINE_DAYS}, 1fr)` }}>
        {days.map((day, index) => <TimelineCell key={day.dayStart} day={day} busiest={busiest} isToday={index === days.length - 1} />)}
      </span>
      <span className={styles.monthStripLabel}>today</span>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Stage board
// ---------------------------------------------------------------------------

export function StageBoard({
  projects,
  stages,
  rollups,
  attentionOf,
  onOpenProject,
  onChanged,
}: {
  projects: Project[]
  stages: string[] | null
  rollups: ReadonlyMap<string, RollupSlot>
  attentionOf: (projectId: string) => ProjectAttention | null
  onOpenProject: (projectId: string) => void
  onChanged: () => void
}): JSX.Element {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const [dragging, setDragging] = useState<string | null>(null)
  const [over, setOver] = useState<string | null>(null)
  const [moving, setMoving] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  if (!stages) return <p className={styles.muted}>Loading the stages…</p>
  const columns = projectsByStage(projects, stages)

  const move = async (projectId: string, stage: string) => {
    const project = projects.find((p) => p.id === projectId)
    if (!project || project.stage === stage || !projectStoreBasePath) return
    setMoving(projectId)
    setError(null)
    try {
      await readJSON<Project>(
        await fetchFn(projectPath(projectStoreBasePath, projectId), {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ stage, changed_by: PROJECTS_PAGE_AUTHOR }),
        }),
        `move ${project.name} to ${stage}`,
      )
      onChanged()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setMoving(null)
    }
  }

  const onDrop = (stage: string) => (event: DragEvent) => {
    event.preventDefault()
    const projectId = event.dataTransfer.getData('text/x-project-id')
    setOver(null)
    setDragging(null)
    if (projectId) void move(projectId, stage)
  }

  return (
    <section className={styles.section}>
      <p className={styles.muted}>Every project by stage, from idea to done. Drag a card to another stage to move it; project-store records who moved it and when.</p>
      {error && <p className={styles.error}>{error}</p>}
      <div className={styles.stageBoard}>
        {columns.map(({ stage, projects: inStage }) => (
          <div
            key={stage}
            className={`${styles.stageColumn} ${over === stage ? styles.stageColumnOver : ''}`}
            data-stage={stage}
            onDragOver={(event) => { event.preventDefault(); setOver(stage) }}
            onDragLeave={() => setOver((current) => (current === stage ? null : current))}
            onDrop={onDrop(stage)}
          >
            <h3 className={styles.stageColumnTitle}>
              {stage.replace(/_/g, ' ')} <span className={styles.countBadge}>{inStage.length}</span>
            </h3>
            {inStage.map((project) => {
              const rollup = rollups.get(project.id)?.rollup ?? null
              const attention = attentionOf(project.id)
              const waiting = attention ? attentionTotal(attention) : 0
              return (
                <article
                  key={project.id}
                  className={`${styles.stageCard} ${dragging === project.id ? styles.stageCardDragging : ''}`}
                  draggable
                  onDragStart={(event) => {
                    event.dataTransfer.setData('text/x-project-id', project.id)
                    event.dataTransfer.effectAllowed = 'move'
                    setDragging(project.id)
                  }}
                  onDragEnd={() => { setDragging(null); setOver(null) }}
                  aria-busy={moving === project.id}
                >
                  <div className={styles.stageCardHead}>
                    <button type="button" className={styles.cardName} onClick={() => onOpenProject(project.id)}>{project.name}</button>
                    {project.kind === 'stream' && <span className={styles.muted}>stream</span>}
                    {waiting > 0 && <span className={`${styles.stateChip} ${styles.stateWaiting}`} title="Things that need you">{waiting}</span>}
                  </div>
                  {project.goal && <p className={styles.stageCardGoal}>{project.goal}</p>}
                  {rollup && <StepProgressBar rollup={rollup} />}
                  {rollup && <span className={styles.muted}>{commitsText(recentCommitCount(rollup))} in 14 days</span>}
                  {moving === project.id && <span className={styles.muted}>Moving…</span>}
                </article>
              )
            })}
          </div>
        ))}
      </div>
    </section>
  )
}

// ---------------------------------------------------------------------------
// Effort
// ---------------------------------------------------------------------------

export function EffortChart({
  projects,
  rollups,
  filedByProject,
  summaries,
  onOpenProject,
}: {
  projects: Project[]
  rollups: ReadonlyMap<string, RollupSlot>
  filedByProject: ReadonlyMap<string, string[]>
  summaries: ReadonlyMap<string, SessionSummary>
  onOpenProject: (projectId: string) => void
}): JSX.Element {
  const rows = effortRows(
    projects.map((project) => {
      const rollup = rollups.get(project.id)?.rollup
      return {
        project,
        commits: rollup ? recentCommitCount(rollup) : 0,
        spendUsd: filedSessionSpend(filedByProject.get(project.id) ?? [], summaries).spendUsd,
      }
    }),
  )
  const widest = Math.max(0.01, ...rows.flatMap((r) => [r.commitShare, r.spendShare, r.targetShare ?? 0]))
  const percent = (share: number) => `${Math.round(share * 100)}%`
  return (
    <section className={styles.section}>
      <p className={styles.muted}>
        Where the work went. <span className={styles.legendCommits}>■</span> share of commits on default branches in the last 14 days
        (a repo two projects own counts for both). <span className={styles.legendSpend}>■</span> share of what the project's filed
        sessions have cost. <span className={styles.legendTarget}>│</span> the share of spend set as the project's target.
      </p>
      <div className={styles.effort} role="table" aria-label="Share of commits and spend by project">
        {rows.map((row) => (
          <div key={row.project.id} className={styles.effortRow} role="row">
            <span role="rowheader" className={styles.effortName}>
              <button type="button" className={styles.linkButton} onClick={() => onOpenProject(row.project.id)}>{row.project.name}</button>
            </span>
            <span role="cell" className={styles.effortBars}>
              <span className={styles.effortTrack} title={`${commitsText(row.commits)}, ${percent(row.commitShare)} of all`}>
                <span className={styles.effortCommits} style={{ width: `${(row.commitShare / widest) * 100}%` }} />
              </span>
              <span className={styles.effortTrack} title={`${formatCost(row.spendUsd)}, ${percent(row.spendShare)} of all`}>
                <span className={styles.effortSpend} style={{ width: `${(row.spendShare / widest) * 100}%` }} />
              </span>
              {row.targetShare !== null && (
                <span className={styles.effortTarget} style={{ left: `${(row.targetShare / widest) * 100}%` }}
                  title={`target ${percent(row.targetShare)} of spend`} />
              )}
            </span>
            <span role="cell" className={styles.effortNumbers}>
              {commitsText(row.commits)} · {formatCost(row.spendUsd)}
              {row.targetShare !== null && (
                <span className={row.spendShare > row.targetShare ? styles.stateWaiting : styles.muted}>
                  {' '}· target {percent(row.targetShare)}, at {percent(row.spendShare)}
                </span>
              )}
            </span>
          </div>
        ))}
      </div>
    </section>
  )
}
