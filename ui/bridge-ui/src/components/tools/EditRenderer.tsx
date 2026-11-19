import { useEffect, useState } from 'react'
import type { ToolRendererProps } from './types'
import { registerToolRenderer } from './registry'
import { useToolContext } from './context'
import { useBridgeConfig } from '../../context'
import { DiffView } from './DiffView'
import {
  loadSide,
  sidesHint,
  type LoadedSide,
  type SnapshotsResponse,
} from './snapshots'

// EditRenderer handles `Edit`, `Write`, and `MultiEdit` tool calls — every
// tool whose tool_input names a single `file_path`. (Bash multi-file diffs
// are handled by BashRenderer.) There are two paths:
//   1. Fast path: the tool input carries the full diff payload already
//      (old_string / new_string for Edit; content for Write when the before
//      was missing). No fetch required.
//   2. Fetch path: ask the bridge server for before/after snapshots keyed by
//      tool_use_id and render the diff from the blob contents.
function EditRenderer({ tool, running }: ToolRendererProps) {
  const { sessionId } = useToolContext()
  const { fetch: fetchFn, basePath } = useBridgeConfig()

  const input = (tool.input ?? {}) as Record<string, unknown>
  // NotebookEdit uses `notebook_path`; everything else uses `file_path`.
  const filePath = (input.file_path ?? input.notebook_path ?? input.path ?? '') as string
  const isEdit = tool.tool === 'Edit'
  const isWrite = tool.tool === 'Write'

  // A shortened input is not the edit: its strings stop at 2 KB. Leave the fast path to
  // a full input, so the diff comes from the snapshots or waits for the whole entry.
  const inputComplete = !tool.input_truncated
  const fastBefore = isEdit && inputComplete ? (input.old_string as string | undefined) : undefined
  const fastAfter = !inputComplete
    ? undefined
    : isEdit
      ? (input.new_string as string | undefined)
      : isWrite
        ? (input.content as string | undefined)
        : undefined
  const haveFastPath = typeof fastBefore === 'string' && typeof fastAfter === 'string'
  // For Write, before-state only exists in snapshots — the tool input has
  // no old content field. Same for MultiEdit which doesn't expose its diff.
  const needsFetch = !running && tool.tool_id !== '' && (!haveFastPath || isWrite)

  const [sides, setSides] = useState<{ before: LoadedSide; after: LoadedSide } | null>(null)
  const [state, setState] = useState<'idle' | 'loading' | 'ready' | 'error'>('idle')
  const [hint, setHint] = useState<string>('')

  useEffect(() => {
    if (!needsFetch || !sessionId) return
    let canceled = false
    setState('loading')
    ;(async () => {
      try {
        const res = await fetchFn(`${basePath}/sessions/${sessionId}/tools/${tool.tool_id}/snapshots`)
        if (!res.ok) throw new Error(`snapshots ${res.status}`)
        const meta = (await res.json()) as SnapshotsResponse

        // Edit/Write/MultiEdit only ever snapshot one file; pick the entry
        // matching tool_input.file_path, or the first one if there's only
        // one. Server is authoritative — never trust filePath when the
        // server reports something different.
        const entry =
          meta.files.find((f) => f.file_path === filePath) ?? meta.files[0] ?? null
        const before = entry?.before ?? null
        const after = entry?.after ?? null

        const beforeSide = await loadSide(fetchFn, basePath, 'Before', before, fastBefore)
        const afterSide = await loadSide(fetchFn, basePath, 'After', after, fastAfter)
        if (canceled) return

        setSides({ before: beforeSide, after: afterSide })
        setHint(sidesHint(before, after))
        setState('ready')
      } catch (err) {
        if (canceled) return
        setState('error')
        setHint(String(err))
      }
    })()
    return () => { canceled = true }
  }, [needsFetch, sessionId, tool.tool_id, fetchFn, basePath, fastBefore, fastAfter, filePath])

  const label = tool.tool
  const filename = filePath ? filePath.split('/').pop() : ''

  // Pick whichever content we have. Fast path wins when fetch wasn't needed
  // (or hasn't landed yet) because it's instant and still accurate.
  const before = sides?.before.content ?? fastBefore ?? ''
  const after = sides?.after.content ?? fastAfter ?? ''
  const showDiff = (haveFastPath && !isWrite) || state === 'ready'

  return (
    <div className={`bc-tool-item ${tool.error ? 'bc-tool-item-error' : ''} ${running ? 'bc-tool-item-running' : ''}`}>
      <div className="bc-tool-header">
        <span className="bc-tool-name">📝 {label}</span>
        {running && <span className="bc-tool-spinner">⟳</span>}
        {tool.error && !running && <span className="bc-tool-error-badge">error</span>}
      </div>
      {filePath && (
        <div className="bc-tool-detail" title={filePath}>
          {filename && <strong>{filename}</strong>}
          {filename !== filePath && <span style={{ opacity: 0.6, marginLeft: 4 }}>{filePath}</span>}
        </div>
      )}
      {running && <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>Applying…</div>}
      {!running && state === 'loading' && !haveFastPath && (
        <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>Loading snapshot…</div>
      )}
      {!running && state === 'error' && (
        <div className="bc-tool-output-code" style={{ opacity: 0.7 }}>Snapshot unavailable: {hint}</div>
      )}
      {!running && showDiff && (
        <DiffView filePath={filePath} before={before} after={after} />
      )}
      {hint && state === 'ready' && (
        <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>{hint}</div>
      )}
      {tool.output && (
        <div className="bc-tool-output">
          <span className="bc-tool-output-label">→</span>
          <span className="bc-tool-output-text">{tool.output}</span>
        </div>
      )}
    </div>
  )
}

registerToolRenderer('Edit', EditRenderer)
registerToolRenderer('Write', EditRenderer)
registerToolRenderer('MultiEdit', EditRenderer)
registerToolRenderer('NotebookEdit', EditRenderer)

export default EditRenderer
