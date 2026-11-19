// Presentation-edge adapters: chat-core exposes its session-info and per-entry
// usage in camelCase; bridge-ui's exported panels consume the canonical
// llm-bridge snake_case shapes (SessionInfo / TokenUsage). These pure mappers
// live at the render edge so the chat page can feed chat-core's data straight
// into bridge-ui's components without either library duplicating the other's
// naming. No data is invented — absent fields stay absent.

import { ARCHIVE_FOLDER } from '@kayushkin/chat-core'
import type { SessionInfo as ChatSessionInfo, EntryUsage } from '@kayushkin/chat-core'
import type { TokenUsage } from '../../types'
import type { ToolsPanelProps } from './ToolsPanel'

// bridge-ui does not re-export the canonical SessionInfo type by name; derive the
// exact shape its panels consume from the exported ToolsPanelProps so the adapter
// stays pinned to the real prop contract.
type BridgeSessionInfo = ToolsPanelProps['info']

/** Map chat-core's camelCase SessionInfo onto bridge-ui's snake_case SessionInfo
 *  (the shape ToolsPanel / SystemPromptModal read). */
export function toBridgeSessionInfo(info: ChatSessionInfo): BridgeSessionInfo {
  return {
    system_prompt: info.systemPrompt,
    append_system_prompt: info.appendSystemPrompt,
    working_dir: info.workingDir,
    model: info.model,
    permission_mode: info.permissionMode,
    tools: info.tools,
    slash_commands: info.slashCommands,
    agents: info.agents,
    skills: info.skills,
    mcp_servers: info.mcpServers,
  }
}

/** hh:mm:ss for a turn/entry/timeline timestamp, the one formatter TurnList and
 *  Timeline share. */
export function formatHMS(ts: string): string {
  try {
    const d = new Date(ts)
    const hh = String(d.getHours()).padStart(2, '0')
    const mm = String(d.getMinutes()).padStart(2, '0')
    const ss = String(d.getSeconds()).padStart(2, '0')
    return `${hh}:${mm}:${ss}`
  } catch {
    return ts
  }
}

/** Map chat-core's per-entry EntryUsage onto bridge-ui's TokenUsage (the shape
 *  UsageLine reads). total_tokens is derived from the parts chat-core carries;
 *  UsageLine only renders the non-zero fields, so the derived total is inert. */
export function toBridgeUsage(usage: EntryUsage): TokenUsage {
  const input = usage.inputTokens ?? 0
  const output = usage.outputTokens ?? 0
  return {
    input_tokens: input,
    output_tokens: output,
    total_tokens: input + output,
    cache_read_tokens: usage.cacheReadTokens,
    cache_write_tokens: usage.cacheWriteTokens,
  }
}

/** Is this session filed in the canonical Archive folder?
 *
 *  `ARCHIVE_FOLDER` ('Archive') is the server's own constant, re-exported by
 *  chat-core — join on that, never on a literal typed here.
 *
 *  The lowercase spelling is tolerated on purpose and is NOT a second source of
 *  truth. chat-core used to write `folderName: 'archive'` optimistically while
 *  POSTing to `/sessions/{id}/archive`, a route the gateway has never had. The POST
 *  always 404'd, so the lowercase value never reached the server and no row on this
 *  box carries it (checked 2026-08-02: the only folders that exist are Scheduled,
 *  Archive, Auto-rename, Conformance, Subagents, Kanban). But the optimistic row
 *  could be written to the IndexedDB SessionCache before the revert landed, so a
 *  browser with a warm cache from before that fix can still paint one. Reading both
 *  costs nothing and stops such a row rendering as un-archived.
 *
 *  Delete the lowercase arm once caches have turned over. */
export function isArchivedFolder(folderName: string): boolean {
  return folderName === ARCHIVE_FOLDER || folderName === 'archive'
}
