import type { ToolRendererProps } from './types'
import { registerToolRenderer } from './registry'

function FileRenderer({ tool, running }: ToolRendererProps) {
  const input = tool.input ?? {}
  const path = (input.path ?? input.file_path ?? input.file ?? '') as string
  const content = (input.content ?? input.new_string ?? input.new_text ?? '') as string

  const label = tool.tool.replace(/_/g, ' ')
  const filename = path ? path.split('/').pop() : ''

  return (
    <div className={`bc-tool-item ${tool.error ? 'bc-tool-item-error' : ''} ${running ? 'bc-tool-item-running' : ''}`}>
      <div className="bc-tool-header">
        <span className="bc-tool-name">📄 {label}</span>
        {running && <span className="bc-tool-spinner">⟳</span>}
        {tool.error && !running && <span className="bc-tool-error-badge">error</span>}
      </div>
      {path && (
        <div className="bc-tool-detail" title={path}>
          {filename && <strong>{filename}</strong>}
          {filename !== path && <span style={{ opacity: 0.6, marginLeft: 4 }}>{path}</span>}
        </div>
      )}
      {content && !running && (
        <pre className="bc-tool-output-code">{content.length > 500 ? content.slice(0, 500) + '…' : content}</pre>
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

// Edit and Write are handled by EditRenderer (diff view with snapshot
// support). read_file/edit_file/write_file remain here for older harness
// variants that still emit those names.
registerToolRenderer('write_file', FileRenderer)
registerToolRenderer('edit_file', FileRenderer)
registerToolRenderer('read_file', FileRenderer)
registerToolRenderer('Read', FileRenderer)

export default FileRenderer
