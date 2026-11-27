import { useCallback, useEffect, useMemo, useState } from 'react'
import type { JSX } from 'react'
import type { Project, SessionFiling } from '@kayushkin/project-store-types'
import { useBridgeConfig } from '../../context'
import {
  PROJECTS_PAGE_AUTHOR,
  acceptSessionFilingBody,
  projectPathName,
  projectTree,
  proposedSessionFilingsPath,
  sessionFilingDecisionPath,
  sessionFilingOdds,
  sessionFilingsToDecide,
  type SessionFilingListAnswer,
} from '../../projects'
import styles from './ProjectsView.module.css'

// The chats project-store's session filer was not sure enough of to file by
// itself, on the projects landing page. Each row shows what the filer read
// (the chat's goal, or that it had only a name), the project it proposes with
// its odds, and its reason. The person files the chat under that project,
// picks another, or says to leave it unfiled; project-store records who
// decided. Closed until opened, since the list can run long.

async function readJSON<T>(response: Response, what: string): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim()
    throw new Error(`${what}: ${response.status} ${response.statusText}${body ? ` — ${body}` : ''}`)
  }
  return (await response.json()) as T
}

export interface SessionsToFileProps {
  projects: Project[]
  /** Open a session in the chat. */
  onSelectSession: (sessionId: string) => void
  /** A session was filed, so the filed sessions must be read again. */
  onChanged: () => void
  /** Changes when the page is refreshed. */
  reloadSignal: number
}

export function SessionsToFile({ projects, onSelectSession, onChanged, reloadSignal }: SessionsToFileProps): JSX.Element | null {
  const { fetch: fetchFn, projectsBasePath } = useBridgeConfig()
  const [filings, setFilings] = useState<SessionFiling[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [decided, setDecided] = useState(0)

  useEffect(() => {
    if (!projectsBasePath) return
    let canceled = false
    fetchFn(proposedSessionFilingsPath(projectsBasePath))
      .then((r) => readJSON<SessionFilingListAnswer>(r, 'sessions to file'))
      .then((answer) => {
        if (canceled) return
        setFilings(sessionFilingsToDecide(answer.session_filings))
        setError(null)
      })
      .catch((e: Error) => { if (!canceled) setError(e.message) })
    return () => { canceled = true }
  }, [fetchFn, projectsBasePath, reloadSignal, decided])

  const decide = useCallback(
    async (filing: SessionFiling, decision: 'accept' | 'dismiss', projectId: string | null) => {
      if (!projectsBasePath) return
      setBusy(filing.id)
      setError(null)
      try {
        const body = decision === 'accept' ? acceptSessionFilingBody(PROJECTS_PAGE_AUTHOR, projectId) : JSON.stringify({ decided_by: PROJECTS_PAGE_AUTHOR })
        await readJSON<unknown>(
          await fetchFn(sessionFilingDecisionPath(projectsBasePath, filing.id, decision), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body,
          }),
          decision === 'accept' ? 'file the session' : 'leave the session unfiled',
        )
        if (decision === 'accept') onChanged()
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setBusy(null)
        setDecided((n) => n + 1)
      }
    },
    [fetchFn, projectsBasePath, onChanged],
  )

  if (!projectsBasePath) return null
  if (filings === null) return error ? <p className={styles.error}>{error}</p> : null
  if (filings.length === 0 && !error) return null
  return (
    <SessionsToFileList
      filings={filings}
      projects={projects}
      busyFilingId={busy}
      error={error}
      onSelectSession={onSelectSession}
      onFile={(filing, projectId) => void decide(filing, 'accept', projectId)}
      onLeaveUnfiled={(filing) => void decide(filing, 'dismiss', null)}
    />
  )
}

export interface SessionsToFileListProps {
  /** The waiting filings, in the order to show them. */
  filings: SessionFiling[]
  projects: Project[]
  /** The filing a decision is being sent for; every button waits on it. */
  busyFilingId: string | null
  error: string | null
  onSelectSession: (sessionId: string) => void
  /** File under `projectId`, or under the project the filing proposes when null. */
  onFile: (filing: SessionFiling, projectId: string | null) => void
  onLeaveUnfiled: (filing: SessionFiling) => void
}

/** The list itself, drawn from what it is given. */
export function SessionsToFileList({ filings, projects, busyFilingId, error, onSelectSession, onFile, onLeaveUnfiled }: SessionsToFileListProps): JSX.Element {
  const projectsById = useMemo(() => new Map(projects.map((p) => [p.id, p])), [projects])
  const tree = useMemo(() => projectTree(projects), [projects])
  const busy = busyFilingId !== null
  return (
    <section className={styles.section}>
      <details className={styles.toFile}>
        <summary className={styles.toFileSummary}>
          <span className={`${styles.sectionTitle} page-title`}>Sessions to file</span>
          <span className={styles.toFileCount}>{filings.length}</span>
        </summary>
        <p className={styles.muted}>
          Chats the session filer was not sure enough of to file by itself. File each under a project or leave it
          unfiled. When you make a new project, the filer tries it against these again.
        </p>
        {error && <p className={styles.error}>{error}</p>}
        <ul className={styles.suggestionList}>
          {filings.map((filing) => {
            const proposed = projectsById.get(filing.project_id)
            const odds = sessionFilingOdds(filing)
            const name = filing.session_label || filing.session_id
            return (
              <li key={filing.id} className={`${styles.suggestion} ${styles.toFileRow}`} data-session-filing-id={filing.id}>
                <div className={styles.suggestionText}>
                  <button type="button" className={styles.cardName} onClick={() => onSelectSession(filing.session_id)} title="Open this chat">
                    {name}
                  </button>
                  <div>{filing.goal || 'No brief was written for this chat: judged from its name.'}</div>
                  {filing.why && <div className={styles.muted}>{filing.why}</div>}
                  <div className={`${styles.muted} ${styles.mono}`}>{filing.session_id} · {filing.id} · {filing.model}</div>
                </div>
                <div className={styles.toFileButtons}>
                  {proposed && (
                    <button type="button" className="bc-ctrl-btn" disabled={busy} onClick={() => onFile(filing, null)}
                      title={`The filer's pick, at ${odds} odds`}>
                      {busyFilingId === filing.id ? '…' : `File under ${projectPathName(proposed, projectsById)} (${odds})`}
                    </button>
                  )}
                  <select className={styles.textInput} value="" disabled={busy} aria-label={`File ${name} under a project`}
                    onChange={(e) => { if (e.target.value) onFile(filing, e.target.value) }}>
                    <option value="">{proposed ? 'Another project…' : 'File under project…'}</option>
                    {tree.map(({ project, depth }) => (
                      <option key={project.id} value={project.id}>
                        {'  '.repeat(depth)}
                        {project.name}
                      </option>
                    ))}
                  </select>
                  <button type="button" className="bc-ctrl-btn" disabled={busy} onClick={() => onLeaveUnfiled(filing)}
                    title="Leave this chat unfiled. The filer asks again only if the chat's goal changes.">
                    Leave unfiled
                  </button>
                </div>
              </li>
            )
          })}
        </ul>
      </details>
    </section>
  )
}
