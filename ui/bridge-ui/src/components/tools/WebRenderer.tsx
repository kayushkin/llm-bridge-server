import type { ToolRendererProps } from './types'
import { registerToolRenderer } from './registry'

function WebRenderer({ tool, running }: ToolRendererProps) {
  const input = tool.input ?? {}
  const query = (input.query ?? input.search ?? '') as string
  const url = (input.url ?? '') as string

  const isSearch = tool.tool === 'WebSearch' || tool.tool === 'web_search'

  return (
    <div className={`bc-tool-item ${tool.error ? 'bc-tool-item-error' : ''} ${running ? 'bc-tool-item-running' : ''}`}>
      <div className="bc-tool-header">
        <span className="bc-tool-name">🌐 {isSearch ? 'web search' : 'web fetch'}</span>
        {running && <span className="bc-tool-spinner">⟳</span>}
        {tool.error && !running && <span className="bc-tool-error-badge">error</span>}
      </div>
      {query && <div className="bc-tool-detail">"{query}"</div>}
      {url && <div className="bc-tool-detail" style={{ opacity: 0.8 }}>{url}</div>}
      {tool.output && (
        <div className="bc-tool-output">
          <span className="bc-tool-output-label">→</span>
          <span className="bc-tool-output-text">
            {tool.output.length > 400 ? tool.output.slice(0, 400) + '…' : tool.output}
          </span>
        </div>
      )}
    </div>
  )
}

registerToolRenderer('WebSearch', WebRenderer)
registerToolRenderer('WebFetch', WebRenderer)
registerToolRenderer('web_search', WebRenderer)
registerToolRenderer('web_fetch', WebRenderer)

export default WebRenderer
