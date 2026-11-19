import type { SessionInfo } from '../../types'

export interface ToolsPanelProps {
  info: SessionInfo
}

export function ToolsPanel({ info }: ToolsPanelProps) {
  const tools = info.tools ?? []
  const slashCommands = info.slash_commands ?? []
  const agents = info.agents ?? []
  const skills = info.skills ?? []
  const mcpServers = info.mcp_servers ?? []

  if (tools.length === 0 && slashCommands.length === 0 && agents.length === 0 && skills.length === 0 && mcpServers.length === 0) {
    return <div className="bc-tools-panel"><div className="bc-info-empty">No tools reported yet. The harness will emit this after its first init.</div></div>
  }

  return (
    <div className="bc-tools-panel">
      {tools.length > 0 && (
        <div className="bc-tools-section">
          <div className="bc-tools-section-header">Tools ({tools.length})</div>
          <div className="bc-tools-grid">
            {tools.map(t => (
              <span key={t.name} className="bc-tool-chip" title={t.description || undefined}>{t.name}</span>
            ))}
          </div>
        </div>
      )}
      {slashCommands.length > 0 && (
        <div className="bc-tools-section">
          <div className="bc-tools-section-header">Slash commands ({slashCommands.length})</div>
          <div className="bc-tools-grid">
            {slashCommands.map(c => <span key={c} className="bc-tool-chip">/{c}</span>)}
          </div>
        </div>
      )}
      {agents.length > 0 && (
        <div className="bc-tools-section">
          <div className="bc-tools-section-header">Sub-agents ({agents.length})</div>
          <div className="bc-tools-grid">
            {agents.map(a => <span key={a} className="bc-tool-chip">{a}</span>)}
          </div>
        </div>
      )}
      {skills.length > 0 && (
        <div className="bc-tools-section">
          <div className="bc-tools-section-header">Skills ({skills.length})</div>
          <div className="bc-tools-grid">
            {skills.map(s => <span key={s} className="bc-tool-chip">{s}</span>)}
          </div>
        </div>
      )}
      {mcpServers.length > 0 && (
        <div className="bc-tools-section">
          <div className="bc-tools-section-header">MCP servers ({mcpServers.length})</div>
          <div className="bc-tools-grid">
            {mcpServers.map(m => (
              <span key={m.name} className="bc-tool-chip" title={m.status || undefined}>
                {m.name}{m.status ? ` · ${m.status}` : ''}
              </span>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
