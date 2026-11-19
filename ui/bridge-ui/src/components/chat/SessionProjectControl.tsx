import { useMemo, useState } from 'react'
import type { JSX } from 'react'
import { useProjectStore } from '../../useProjectStore'
import { projectPathName, projectTree, sessionFilings } from '../../projects'
import styles from './Chat.module.css'

// The session header's "Project: <name> ▾": which project the open session is
// filed under in project-store, and a picker that files it, moves it, or takes
// it out. Moving writes the new link before deleting the old one, so a refusal
// leaves the session where it was; the refusal is shown verbatim. Drawn only
// when the host proxies project-store.

/** The picker's value for "filed nowhere". No project id is empty. */
const NOT_FILED = ''

export function SessionProjectControl({ sessionId }: { sessionId: string }): JSX.Element | null {
  const { enabled, loaded, projects, sessionLinks, moveSession } = useProjectStore()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const projectsById = useMemo(() => new Map(projects.map((p) => [p.id, p])), [projects])
  const tree = useMemo(() => projectTree(projects), [projects])
  const filings = useMemo(() => sessionFilings(sessionLinks).get(sessionId) ?? [], [sessionLinks, sessionId])

  if (!enabled || !loaded) return null

  const filedUnder = filings.map((l) => projectsById.get(l.project_id)).filter((p) => p !== undefined)
  const current = filedUnder[0]?.id ?? NOT_FILED
  const title = filedUnder.length
    ? `Filed under ${filedUnder.map((p) => `${projectPathName(p, projectsById)} (${p.id})`).join(', ')}. Pick another project to move it.`
    : 'File this session under a project'

  const onChange = (next: string) => {
    setSaving(true)
    setError(null)
    moveSession(sessionId, next === NOT_FILED ? null : next)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setSaving(false))
  }

  return (
    <span className={styles.projectFiling} data-filed={filedUnder.length > 0 || undefined}>
      <label className={styles.projectFilingLabel} title={title}>
        {filedUnder.length > 0 && <span className={styles.projectFilingPrefix}>Project:</span>}
        <select
          className={styles.projectFilingSelect}
          value={current}
          disabled={saving}
          onChange={(e) => onChange(e.target.value)}
          aria-label="Project this session is filed under"
        >
          <option value={NOT_FILED}>{filedUnder.length ? 'Not filed (take it out)' : 'File under project…'}</option>
          {tree.map(({ project, depth }) => (
            <option key={project.id} value={project.id}>
              {'  '.repeat(depth)}
              {project.name}
            </option>
          ))}
        </select>
        {filedUnder.length > 1 && <span className={styles.projectFilingPrefix}>+{filedUnder.length - 1}</span>}
      </label>
      {error && (
        <span className={styles.projectFilingError} role="alert" title={error}>
          {error}
        </span>
      )}
    </span>
  )
}
