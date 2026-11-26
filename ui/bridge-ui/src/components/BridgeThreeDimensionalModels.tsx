import { useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import {
  folderLabel,
  renderChangedSinceViewed,
  sessionTree,
  shortFileLabel,
  shortTime,
  threeDimensionalModelFileUrl,
  type ThreeDimensionalModelFile,
  type ThreeDimensionalModelRender,
  type ThreeDimensionalModelSession,
} from '../threeDimensionalModels'
import { useThreeDimensionalModels } from '../useThreeDimensionalModels'
import { formatAgeCompact } from '../utils'
import { formatBytes } from '../machineStats'
import { GlbViewer } from './GlbViewer'
import styles from './BridgeThreeDimensionalModels.module.css'

/**
 * The 3D models agents are making and have made, and the renders each agent
 * opened while making them.
 *
 * The host decides what is listed (dash: the .glb files in the folders a
 * session's tool calls name, changed while it ran, and the images it opened,
 * from log-store), and serves each file it listed; this page draws the list,
 * a session's models in a 3D view and its renders newest first. Subagents
 * and delegates sit under the session that started them.
 *
 * `?session=<id>` picks a session, `?repository=<folder>` a repository's
 * committed models, and `&model=<path>` the model in the view. A host that
 * carries no model list (`threeDimensionalModelsBasePath` empty) gets a line
 * saying so, because the hook then asks nothing and would never finish loading.
 */
export function BridgeThreeDimensionalModels() {
  const { threeDimensionalModelsBasePath } = useBridgeConfig()
  const { answer, error, loaded } = useThreeDimensionalModels()
  const [searchParams, setSearchParams] = useSearchParams()
  const sessions = answer?.sessions ?? []
  const repositories = answer?.repositories ?? []
  const tree = useMemo(() => sessionTree(sessions), [sessions])

  const pickedSessionId = searchParams.get('session')
  const pickedRepositoryFolder = searchParams.get('repository')
  const pickedSession = sessions.find(session => session.session_id === pickedSessionId)
    ?? (pickedSessionId || pickedRepositoryFolder ? undefined : sessions[0])
  const pickedRepository = pickedSession ? undefined : repositories.find(repository => repository.folder === pickedRepositoryFolder)

  const pick = (params: Record<string, string>) => setSearchParams(new URLSearchParams(params), { replace: true })

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h2 className={styles.title}>3D models</h2>
        <p className={styles.subtitle}>
          The .glb files agents are making, and the renders each agent opened while it worked. A session is listed when a
          model in a folder it works in changed while it ran, or when it opened renders while working among models.
          Refreshed every few seconds{answer ? `; it looks back ${answer.window}` : ''}. The renders are the files as they
          are now: an agent that writes the same file again replaces what it saw before.
        </p>
      </header>
      {error && <pre className={styles.error}>{error}</pre>}
      {answer && answer.problems.length > 0 && (
        <details className={styles.problems}>
          <summary>{answer.problems.length === 1 ? 'One read' : `${answer.problems.length} reads`} failed or stopped short, so some models may be missing</summary>
          <ul>{answer.problems.map(problem => <li key={problem}><code>{problem}</code></li>)}</ul>
        </details>
      )}
      {!threeDimensionalModelsBasePath && (
        <div className={styles.empty}>
          This host does not list 3D models (<code>threeDimensionalModelsBasePath</code> is empty), so there is nothing to show.
        </div>
      )}
      {threeDimensionalModelsBasePath && !loaded && <div className={styles.empty}>Loading…</div>}
      {loaded && answer && sessions.length === 0 && repositories.length === 0 && (
        <div className={styles.empty}>No agent has made a model in the last {answer.window}.</div>
      )}
      {(sessions.length > 0 || repositories.length > 0) && (
        <div className={styles.columns}>
          <nav className={styles.list} aria-label="Sessions and repositories">
            {tree.length > 0 && <h3 className={styles.listHeading}>Sessions</h3>}
            <ul className={styles.tree}>
              {tree.map(node => (
                <SessionTreeRow
                  key={node.session.session_id}
                  session={node.session}
                  children={node.children}
                  pickedSessionId={pickedSession?.session_id ?? null}
                  onPick={id => pick({ session: id })}
                />
              ))}
            </ul>
            {repositories.length > 0 && <h3 className={styles.listHeading}>Committed in repositories</h3>}
            <ul className={styles.tree}>
              {repositories.map(repository => (
                <li key={repository.folder}>
                  <button
                    type="button"
                    className={`${styles.row} ${pickedRepository?.folder === repository.folder ? styles.rowSelected : ''}`}
                    onClick={() => pick({ repository: repository.folder })}
                    title={repository.folder}
                  >
                    <span className={styles.rowName}>{folderLabel(repository.folder)}</span>
                    <span className={styles.rowFacts}>{repository.models.length} models</span>
                  </button>
                </li>
              ))}
            </ul>
          </nav>
          <section className={styles.detail}>
            {pickedSession && (
              <SessionDetail
                key={pickedSession.session_id}
                session={pickedSession}
                pickedModelPath={searchParams.get('model')}
                onPickModel={path => pick({ session: pickedSession.session_id, model: path })}
              />
            )}
            {pickedRepository && (
              <>
                <div className={styles.detailHeader}>
                  <h3 className={styles.detailTitle}>{folderLabel(pickedRepository.folder)}</h3>
                  <code className={styles.muted}>{pickedRepository.folder}</code>
                </div>
                <ModelsSection
                  models={pickedRepository.models}
                  pickedModelPath={searchParams.get('model')}
                  onPickModel={path => pick({ repository: pickedRepository.folder, model: path })}
                />
              </>
            )}
            {!pickedSession && !pickedRepository && (
              <div className={styles.empty}>
                {pickedSessionId ? `Session ${pickedSessionId} has no models in this window.` : 'Pick a session or a repository.'}
              </div>
            )}
          </section>
        </div>
      )}
    </div>
  )
}

function SessionTreeRow({ session, children, pickedSessionId, onPick }: {
  session: ThreeDimensionalModelSession
  children: ThreeDimensionalModelSession[]
  pickedSessionId: string | null
  onPick: (sessionId: string) => void
}) {
  const childPicked = children.some(child => child.session_id === pickedSessionId)
  const [open, setOpen] = useState(false)
  const showChildren = open || childPicked
  return (
    <li>
      <SessionRow session={session} selected={session.session_id === pickedSessionId} onPick={onPick} />
      {children.length > 0 && (
        <>
          <button type="button" className={styles.childToggle} onClick={() => setOpen(!showChildren)} aria-expanded={showChildren}>
            {showChildren ? '▾' : '▸'} {children.length === 1 ? 'one session' : `${children.length} sessions`} it started
          </button>
          {showChildren && (
            <ul className={styles.children}>
              {children.map(child => (
                <li key={child.session_id}>
                  <SessionRow session={child} selected={child.session_id === pickedSessionId} onPick={onPick} />
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </li>
  )
}

function SessionRow({ session, selected, onPick }: { session: ThreeDimensionalModelSession; selected: boolean; onPick: (sessionId: string) => void }) {
  const age = formatAgeCompact(session.updated_at)
  return (
    <button
      type="button"
      className={`${styles.row} ${selected ? styles.rowSelected : ''}`}
      onClick={() => onPick(session.session_id)}
      title={session.display_name}
    >
      <span className={styles.rowName}>{session.display_name || session.session_id}</span>
      <span className={styles.rowFacts}>
        <span className={styles.state} data-state={session.state}>{session.state}</span>
        <span>{session.harness}</span>
        <span>{session.models.length} models</span>
        <span>{session.renders.length} renders</span>
        {age && <span>{age}</span>}
      </span>
    </button>
  )
}

function SessionDetail({ session, pickedModelPath, onPickModel }: {
  session: ThreeDimensionalModelSession
  pickedModelPath: string | null
  onPickModel: (path: string) => void
}) {
  const { routes } = useBridgeConfig()
  return (
    <>
      <div className={styles.detailHeader}>
        <h3 className={styles.detailTitle}>{session.display_name || session.session_id}</h3>
        <span className={styles.rowFacts}>
          <Link to={`${routes.chat}?session=${encodeURIComponent(session.session_id)}`}>{session.session_id}</Link>
          <span className={styles.state} data-state={session.state}>{session.state}</span>
          <span>{session.harness}</span>
          {session.purpose && <span>{session.purpose}</span>}
          {session.manager_session_id && (
            <span>
              started by <Link to={`${routes.chat}?session=${encodeURIComponent(session.manager_session_id)}`}>{session.manager_session_id}</Link>
            </span>
          )}
        </span>
        {session.lineage_error && <span className={styles.muted}>Who started it is unknown: {session.lineage_error}</span>}
        {session.folders.length > 0 && (
          <details className={styles.folders}>
            <summary>{session.folders.length === 1 ? 'One folder' : `${session.folders.length} folders`} it worked in</summary>
            <ul>{session.folders.map(folder => <li key={folder}><code>{folder}</code></li>)}</ul>
          </details>
        )}
      </div>
      {session.models.length > 0
        ? <ModelsSection models={session.models} pickedModelPath={pickedModelPath} onPickModel={onPickModel} />
        : <div className={styles.empty}>No .glb file of its own: this session checked its models with the renders below.</div>}
      <RendersSection renders={session.renders} />
    </>
  )
}

function ModelsSection({ models, pickedModelPath, onPickModel }: {
  models: ThreeDimensionalModelFile[]
  pickedModelPath: string | null
  onPickModel: (path: string) => void
}) {
  const { threeDimensionalModelsBasePath } = useBridgeConfig()
  const picked = models.find(model => model.path === pickedModelPath) ?? models[0]
  return (
    <div className={styles.models}>
      <div className={styles.viewerColumn}>
        {picked && (
          <>
            <div className={styles.viewerBar}>
              <strong>{shortFileLabel(picked.path, picked.folder)}</strong>
              <ModelFacts model={picked} />
            </div>
            <GlbViewer
              key={picked.path}
              url={threeDimensionalModelFileUrl(threeDimensionalModelsBasePath, picked.path, picked.modified_at)}
              label={shortFileLabel(picked.path, picked.folder)}
            />
            <code className={styles.muted}>{picked.path}</code>
          </>
        )}
      </div>
      <ul className={styles.modelList} aria-label="Models">
        {models.map(model => (
          <li key={model.path}>
            <button
              type="button"
              className={`${styles.modelRow} ${model.path === picked?.path ? styles.rowSelected : ''}`}
              onClick={() => onPickModel(model.path)}
              title={model.path}
            >
              <span className={styles.rowName}>{shortFileLabel(model.path, model.folder)}</span>
              <span className={styles.rowFacts}>
                <span>{folderLabel(model.folder)}</span>
                {model.summary && <span>{model.summary.triangles.toLocaleString()} triangles</span>}
                <span title={model.modified_at}>{shortTime(model.modified_at)}</span>
                {model.in_git_repository && <span>{model.committed ? 'committed' : 'uncommitted'}</span>}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}

function ModelFacts({ model }: { model: ThreeDimensionalModelFile }) {
  return (
    <span className={styles.rowFacts}>
      {model.summary && (
        <>
          <span>{model.summary.triangles.toLocaleString()} triangles</span>
          <span>{model.summary.primitives} primitives</span>
          <span>{model.summary.materials} materials</span>
          {model.summary.root_nodes && model.summary.root_nodes.length > 0 && <span>root {model.summary.root_nodes.join(', ')}</span>}
        </>
      )}
      {model.summary_error && <span className={styles.warning}>Unreadable: {model.summary_error}</span>}
      <span>{formatBytes(model.size_bytes)}</span>
      <span title={model.modified_at}>changed {shortTime(model.modified_at)}</span>
      {model.last_commit && (
        <span title={model.last_commit.subject}>
          <code>{model.last_commit.sha.slice(0, 9)}</code> {model.last_commit.subject}
        </span>
      )}
      {model.in_git_repository && !model.committed && <span>not committed</span>}
    </span>
  )
}

function RendersSection({ renders }: { renders: ThreeDimensionalModelRender[] }) {
  const { threeDimensionalModelsBasePath } = useBridgeConfig()
  if (renders.length === 0) return null
  return (
    <div className={styles.renders}>
      <h4 className={styles.sectionHeading}>Renders it opened, newest first</h4>
      <ul className={styles.renderGrid}>
        {renders.map(render => {
          const url = threeDimensionalModelFileUrl(threeDimensionalModelsBasePath, render.path, render.modified_at)
          const changed = renderChangedSinceViewed(render)
          return (
            <li key={render.path} className={styles.render}>
              {render.exists
                ? <a href={url} target="_blank" rel="noreferrer"><img src={url} alt={shortFileLabel(render.path)} loading="lazy" /></a>
                : <div className={styles.renderGone}>No longer on disk</div>}
              <span className={styles.renderName} title={render.path}>{shortFileLabel(render.path)}</span>
              <span className={styles.rowFacts}>
                <span title={render.viewed_at.join('\n')}>
                  opened {shortTime(render.last_viewed_at)}{render.viewed_at.length > 1 ? `, ${render.viewed_at.length} times` : ''}
                </span>
                {render.size_bytes !== undefined && <span>{formatBytes(render.size_bytes)}</span>}
              </span>
              {changed && render.modified_at && (
                <span className={styles.warning}>Rewritten at {shortTime(render.modified_at)}, after the agent looked</span>
              )}
            </li>
          )
        })}
      </ul>
    </div>
  )
}
