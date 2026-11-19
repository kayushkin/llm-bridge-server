import { createContext, useContext } from 'react'

// ToolContext gives tool renderers access to the enclosing session id,
// which is needed when a renderer fetches per-tool resources from the
// bridge server (e.g. EditRenderer pulling file snapshots). sessionId is
// threaded from the chat view via a provider rather than via props so the
// intermediate components (Thread, LogRowView, ToolsSection, ToolItem)
// don't each need to plumb it through.
export const ToolContext = createContext<{ sessionId: string }>({ sessionId: '' })

export function useToolContext() {
  return useContext(ToolContext)
}
