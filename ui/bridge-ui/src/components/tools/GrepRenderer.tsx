import type { ToolRendererProps } from './types'
import { registerToolRenderer } from './registry'

function GrepRenderer({ tool, running }: ToolRendererProps) {
  const input = tool.input ?? {}
  const pattern = (input.pattern ?? input.query ?? '') as string
  const path = (input.path ?? input.dir ?? '') as string

  return (
    <div className={`bc-tool-item ${tool.error ? 'bc-tool-item-error' : ''} ${running ? 'bc-tool-item-running' : ''}`}>
      <div className="bc-tool-header">
        <span className="bc-tool-name">🔍 {tool.tool.toLowerCase()}</span>
        {running && <span className="bc-tool-spinner">⟳</span>}
        {tool.error && !running && <span className="bc-tool-error-badge">error</span>}
      </div>
      {pattern && (
        <div className="bc-tool-detail">
          <code>{pattern}</code>
          {path && <span style={{ opacity: 0.6, marginLeft: 6 }}>in {path}</span>}
        </div>
      )}
      {tool.output && (
        <pre className="bc-tool-output-code" style={{ opacity: 0.8 }}>
          {tool.output.length > 600 ? tool.output.slice(0, 600) + '…' : tool.output}
        </pre>
      )}
    </div>
  )
}

registerToolRenderer('Grep', GrepRenderer)
registerToolRenderer('Glob', GrepRenderer)
registerToolRenderer('grep', GrepRenderer)
registerToolRenderer('glob', GrepRenderer)

export default GrepRenderer
