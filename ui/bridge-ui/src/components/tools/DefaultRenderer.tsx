import type { ToolRendererProps } from './types'
import CappedText from './CappedText'

function formatDetail(input?: Record<string, unknown>): string {
  if (!input) return ''
  const keys = Object.keys(input).slice(0, 2)
  return keys.map(k => `${k}=${JSON.stringify(input[k]).slice(0, 30)}`).join(', ')
}

export default function DefaultRenderer({ tool, running }: ToolRendererProps) {
  const detail = formatDetail(tool.input)

  return (
    <div className={`bc-tool-item ${tool.error ? 'bc-tool-item-error' : ''} ${running ? 'bc-tool-item-running' : ''}`}>
      <div className="bc-tool-header">
        <span className="bc-tool-name">{tool.tool}</span>
        {tool.tool_id && (
          <code className="bc-row-id bc-row-id-tu" title="harness tool_use id">tu:{tool.tool_id.slice(-10)}</code>
        )}
        {detail && <span className="bc-tool-detail">{detail}</span>}
        {running && <span className="bc-tool-spinner">⟳</span>}
        {tool.error && !running && <span className="bc-tool-error-badge">error</span>}
      </div>
      {tool.output && (
        <div className="bc-tool-output">
          <span className="bc-tool-output-label">→</span>
          <CappedText as="span" className="bc-tool-output-text" text={tool.output} />
        </div>
      )}
    </div>
  )
}
