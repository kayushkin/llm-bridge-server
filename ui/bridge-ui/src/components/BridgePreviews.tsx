import { Link, useSearchParams } from 'react-router-dom'
import { useBridgeConfig } from '../context'
import { previewUrl, workingDirectoryLabel, type DevServerPreview } from '../devServerPreviews'
import { useDevServerPreviews } from '../useDevServerPreviews'
import styles from './BridgePreviews.module.css'

/**
 * The dev servers agents are running, each shown in a frame.
 *
 * The host decides what is listed (dash: a port an agent's process listens
 * on, found from the operating system, never a service of the host) and gives
 * each a public port on its own hostname; this page only draws the list and
 * frames the one picked. `?port=<n>` picks by the port the dev server listens
 * on, which is what a Bash call's View button links to.
 */
export function BridgePreviews() {
  const { previews, unreadableProcessIds, error, loaded } = useDevServerPreviews()
  const [searchParams, setSearchParams] = useSearchParams()
  const pickedPort = Number(searchParams.get('port')) || null
  const picked = previews.find(preview => preview.port === pickedPort) ?? null

  const pick = (port: number) => {
    const next = new URLSearchParams(searchParams)
    next.set('port', String(port))
    setSearchParams(next, { replace: true })
  }

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h2 className={styles.title}>Previews</h2>
        <p className={styles.subtitle}>
          Every port an agent&apos;s process is listening on, refreshed every few seconds. A server disappears from the
          list when it stops. Services of this host are never listed here.
        </p>
      </header>
      {error && <pre className={styles.error}>{error}</pre>}
      {unreadableProcessIds.length > 0 && (
        <div className={styles.muted}>
          {unreadableProcessIds.length === 1 ? 'One agent process belongs' : `${unreadableProcessIds.length} agent processes belong`} to
          another user (usually root, from <code>sudo</code>), so dash cannot see whether{' '}
          {unreadableProcessIds.length === 1 ? 'it listens' : 'they listen'} on a port: process{' '}
          {unreadableProcessIds.join(', ')}.
        </div>
      )}
      {loaded && !error && previews.length === 0 && (
        <div className={styles.empty}>No agent is running a server right now.</div>
      )}
      {previews.length > 0 && (
        <div className={styles.columns}>
          <ul className={styles.list}>
            {previews.map(preview => (
              <li key={preview.port}>
                <PreviewRow preview={preview} selected={preview.port === pickedPort} onPick={() => pick(preview.port)} />
              </li>
            ))}
          </ul>
          <div className={styles.viewer}>
            {picked ? <PreviewFrame preview={picked} /> : (
              <div className={styles.empty}>
                {pickedPort ? `Nothing is listening on port ${pickedPort} any more.` : 'Pick a server to show it here.'}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function PreviewRow({ preview, selected, onPick }: { preview: DevServerPreview; selected: boolean; onPick: () => void }) {
  const { routes } = useBridgeConfig()
  return (
    <div className={`${styles.row} ${selected ? styles.rowSelected : ''}`}>
      <button type="button" className={styles.rowPick} onClick={onPick} disabled={!preview.preview_port}>
        <span className={styles.port}>:{preview.port}</span>
        <span className={styles.directory} title={preview.working_directory}>{workingDirectoryLabel(preview.working_directory)}</span>
      </button>
      <code className={styles.command} title={preview.command}>{preview.command}</code>
      {!preview.preview_port && <span className={styles.muted}>Every preview port is taken; stop another server to show this one.</span>}
      {preview.sessions && preview.sessions.length > 0 && (
        <span className={styles.sessions}>
          {preview.sessions.map(session => (
            <Link key={session.session_id} to={`${routes.chat}?session=${encodeURIComponent(session.session_id)}`} title={`${session.event_count} events, last active ${session.last_active}`}>
              {session.session_id}
            </Link>
          ))}
        </span>
      )}
      {preview.sessions_error && <span className={styles.muted}>Sessions unknown: {preview.sessions_error}</span>}
    </div>
  )
}

function PreviewFrame({ preview }: { preview: DevServerPreview }) {
  const url = previewUrl(preview, window.location)
  if (!url) return <div className={styles.empty}>Every preview port is taken; stop another server to show this one.</div>
  return (
    <>
      <div className={styles.viewerBar}>
        <span className={styles.port}>:{preview.port}</span>
        <a href={url} target="_blank" rel="noreferrer">Open in a new tab</a>
      </div>
      {/* Keyed by URL so switching servers loads the new one rather than
          keeping the old frame's navigation. */}
      <iframe key={url} className={styles.frame} src={url} title={`Dev server on port ${preview.port}`} />
    </>
  )
}
