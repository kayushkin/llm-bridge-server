import type { SessionInfo } from '../../types'

export interface SystemPromptModalProps {
  info: SessionInfo
  onClose: () => void
}

export function SystemPromptModal({ info, onClose }: SystemPromptModalProps) {
  const hasPrompt = !!info.system_prompt || !!info.append_system_prompt
  return (
    <div className="bc-modal-overlay" onClick={onClose}>
      <div className="bc-modal" onClick={e => e.stopPropagation()}>
        <div className="bc-modal-header">
          <h3>System Prompt</h3>
          <button className="bc-modal-close" onClick={onClose} aria-label="Close">×</button>
        </div>
        <div className="bc-modal-body">
          {info.working_dir && (
            <div className="bc-info-row"><span className="bc-info-label">Working directory</span><code>{info.working_dir}</code></div>
          )}
          {info.model && (
            <div className="bc-info-row"><span className="bc-info-label">Model</span><code>{info.model}</code></div>
          )}
          {info.permission_mode && (
            <div className="bc-info-row"><span className="bc-info-label">Permission mode</span><code>{info.permission_mode}</code></div>
          )}
          {info.system_prompt && (
            <>
              <div className="bc-info-label">System prompt (replaces default)</div>
              <pre className="bc-prompt-block">{info.system_prompt}</pre>
            </>
          )}
          {info.append_system_prompt && (
            <>
              <div className="bc-info-label">Appended to default system prompt</div>
              <pre className="bc-prompt-block">{info.append_system_prompt}</pre>
            </>
          )}
          {!hasPrompt && (
            <div className="bc-info-empty">
              No custom system prompt was set at session start. The agent is running with its default prompt plus any CLAUDE.md files it discovers in the working directory.
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
