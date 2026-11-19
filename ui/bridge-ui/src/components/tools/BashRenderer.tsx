import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import type { ToolRendererProps } from './types'
import { registerToolRenderer } from './registry'
import { useToolContext } from './context'
import { useBridgeConfig } from '../../context'
import { DiffView } from './DiffView'
import { localPortsMentioned, previewsMentioned } from '../../devServerPreviews'
import { useDevServerPreviews } from '../../useDevServerPreviews'
import {
  loadSide,
  sidesHint,
  type LoadedFileDiff,
  type SnapshotsResponse,
} from './snapshots'

// BashRenderer shows the command, its output, and — for any files the
// command created/modified/deleted — a per-file diff fetched from the
// bridge server's snapshot store. The set of tracked files is determined
// server-side from the bash AST (see bashfiles.go); this component just
// renders whatever comes back, so a command that touches no recognized
// files (cat, ls, grep, …) shows up identical to the old renderer.
function BashRenderer({ tool, running }: ToolRendererProps) {
  const { sessionId } = useToolContext()
  const { fetch: fetchFn, basePath, routes } = useBridgeConfig()

  const input = tool.input ?? {}
  const command = (input.command ?? input.cmd ?? '') as string

  // A local URL in the command or its output — a dev server's "Local:" line,
  // or a curl at one — gets a View link while an agent process still listens
  // on that port. Only a call that names one asks for the list at all.
  const commandAndOutput = `${command}\n${tool.output ?? ''}`
  const namesLocalPort = useMemo(() => localPortsMentioned(commandAndOutput).length > 0, [commandAndOutput])
  const { previews } = useDevServerPreviews(namesLocalPort)
  const viewable = useMemo(
    () => (namesLocalPort ? previewsMentioned(commandAndOutput, previews).filter(preview => preview.preview_port) : []),
    [namesLocalPort, commandAndOutput, previews],
  )

  const [diffs, setDiffs] = useState<LoadedFileDiff[] | null>(null)
  const [state, setState] = useState<'idle' | 'loading' | 'ready' | 'error'>('idle')
  const [errorHint, setErrorHint] = useState<string>('')

  const needsFetch = !running && tool.tool_id !== '' && !!sessionId

  useEffect(() => {
    if (!needsFetch) return
    let canceled = false
    setState('loading')
    ;(async () => {
      try {
        const res = await fetchFn(`${basePath}/sessions/${sessionId}/tools/${tool.tool_id}/snapshots`)
        if (!res.ok) throw new Error(`snapshots ${res.status}`)
        const meta = (await res.json()) as SnapshotsResponse

        const loaded: LoadedFileDiff[] = []
        for (const entry of meta.files) {
          const before = await loadSide(fetchFn, basePath, 'Before', entry.before, undefined)
          const after = await loadSide(fetchFn, basePath, 'After', entry.after, undefined)
          loaded.push({
            filePath: entry.file_path,
            before,
            after,
            hint: sidesHint(entry.before, entry.after),
          })
        }
        if (canceled) return
        setDiffs(loaded)
        setState('ready')
      } catch (err) {
        if (canceled) return
        setState('error')
        setErrorHint(String(err))
      }
    })()
    return () => { canceled = true }
  }, [needsFetch, sessionId, tool.tool_id, fetchFn, basePath])

  return (
    <div className={`bc-tool-item ${tool.error ? 'bc-tool-item-error' : ''} ${running ? 'bc-tool-item-running' : ''}`}>
      <div className="bc-tool-header">
        <span className="bc-tool-name">⌨ bash</span>
        {running && <span className="bc-tool-spinner">⟳</span>}
        {tool.error && !running && <span className="bc-tool-error-badge">error</span>}
        {viewable.map(preview => (
          <Link
            key={preview.port}
            className="bc-tool-preview-link"
            to={`${routes.previews}?port=${preview.port}`}
            title={`Show the server on port ${preview.port}`}
          >
            View :{preview.port}
          </Link>
        ))}
      </div>
      {command && <pre className="bc-tool-output-code">$ {command}</pre>}
      {tool.output && (
        <pre className="bc-tool-output-code" style={{ opacity: 0.8 }}>
          {tool.output.length > 800 ? tool.output.slice(0, 800) + '…' : tool.output}
        </pre>
      )}
      {state === 'error' && (
        <div className="bc-tool-output-code" style={{ opacity: 0.7 }}>Snapshot unavailable: {errorHint}</div>
      )}
      {state === 'ready' && diffs && diffs.length > 0 && (
        <div className="bc-bash-diffs">
          {diffs.map((d) => (
            <BashFileDiff key={d.filePath} diff={d} />
          ))}
        </div>
      )}
    </div>
  )
}

function BashFileDiff({ diff }: { diff: LoadedFileDiff }) {
  const filename = diff.filePath.split('/').pop() ?? diff.filePath
  return (
    <div className="bc-bash-file-diff">
      <div className="bc-tool-detail" title={diff.filePath}>
        <strong>{filename}</strong>
        {filename !== diff.filePath && (
          <span style={{ opacity: 0.6, marginLeft: 4 }}>{diff.filePath}</span>
        )}
      </div>
      <DiffView filePath={diff.filePath} before={diff.before.content} after={diff.after.content} />
      {diff.hint && (
        <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>{diff.hint}</div>
      )}
    </div>
  )
}

registerToolRenderer('Bash', BashRenderer)
registerToolRenderer('bash', BashRenderer)

export default BashRenderer
