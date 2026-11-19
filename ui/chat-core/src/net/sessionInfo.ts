import type { SessionInfo } from './types.js';
import type { SessionInfoWire } from './wireEvents.js';

/** Map the snake_case `msg.SessionInfo` wire blob to the camelCase `SessionInfo`.
 *  The single source of truth for this mapping — every field is copied explicitly
 *  (never spread), so a wire rename fails the type-check here instead of leaking a
 *  snake_case key into the client. Absent fields stay absent; nothing is invented. */
export function sessionInfoFromWire(w: SessionInfoWire): SessionInfo {
  return {
    systemPrompt: w.system_prompt,
    appendSystemPrompt: w.append_system_prompt,
    workingDir: w.working_dir,
    model: w.model,
    permissionMode: w.permission_mode,
    effort: w.effort,
    tools: w.tools?.map((t) => ({ name: t.name, description: t.description })),
    slashCommands: w.slash_commands,
    agents: w.agents,
    skills: w.skills,
    mcpServers: w.mcp_servers?.map((s) => ({ name: s.name, status: s.status })),
  };
}
