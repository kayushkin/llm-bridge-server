// Dev server previews: the ports agents' processes listen on, as the host
// lists them, and the rules that turn one into a URL the browser can open.
//
// The wire type is written here by hand: the list comes from the HOST (dash's
// GET /api/previews, server/dev_server_previews.go), not from a store with a
// generated types package. Keep it in step with devServerPreviewView there.

/** One bridge session log-store says ran under a harness session, verbatim
 *  from its `GET /api/v1/sessions/by-harness-id`. */
export interface DevServerPreviewSession {
  session_id: string
  event_count: number
  last_active: string
}

/** One port an agent's process listens on. */
export interface DevServerPreview {
  port: number
  listen_address: string
  process_id: number
  command: string
  working_directory: string
  /** The harness's own session id from the process's environment; absent for
   *  a harness that sets none. */
  harness_session_id?: string
  /** The public port on the host's hostname that shows this server. Absent
   *  when every preview port is taken. */
  preview_port?: number
  /** The bridge sessions that ran under `harness_session_id`. Absent when the
   *  process named no harness session. */
  sessions?: DevServerPreviewSession[]
  /** Why `sessions` could not be read. */
  sessions_error?: string
}

export interface DevServerPreviewList {
  previews: DevServerPreview[]
  /** Processes among the agents' that the host may not look inside — most
   *  often root's, from an agent's `sudo` — so any port they listen on is
   *  missing from `previews`. */
  unreadable_process_ids: number[]
}

/** Where the browser opens a preview: the page's own hostname on the preview
 *  port. Null when the server has no preview port. */
export function previewUrl(preview: DevServerPreview, page: { protocol: string; hostname: string }): string | null {
  if (!preview.preview_port) return null
  return `${page.protocol}//${page.hostname}:${preview.preview_port}/`
}

// A URL on this machine's loopback or wildcard address, as a dev server prints
// it ("Local: http://localhost:5173/") and as an agent types it into curl.
const LOCAL_URL = /\b(?:https?|wss?):\/\/(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1?\]):(\d{2,5})\b/g

/** The ports of every local URL in the text, once each, in order of first
 *  mention. */
export function localPortsMentioned(text: string): number[] {
  const ports: number[] = []
  for (const match of text.matchAll(LOCAL_URL)) {
    const port = Number(match[1])
    if (port > 0 && port <= 65535 && !ports.includes(port)) ports.push(port)
  }
  return ports
}

/** The previews for the ports a text mentions, in the text's order. A port no
 *  agent process listens on — a service of the host, or a server that has
 *  stopped — has no preview and is left out. */
export function previewsMentioned(text: string, previews: readonly DevServerPreview[]): DevServerPreview[] {
  const byPort = new Map(previews.map(preview => [preview.port, preview]))
  return localPortsMentioned(text).flatMap(port => byPort.get(port) ?? [])
}

/** The last path segment of the working directory — for a worktree under
 *  ~/repos, the repo and task — for a short label. */
export function workingDirectoryLabel(workingDirectory: string): string {
  const trimmed = workingDirectory.replace(/\/+$/, '')
  return trimmed.slice(trimmed.lastIndexOf('/') + 1) || workingDirectory
}

/** The previews whose process ran under this bridge session, as the host
 *  reports it (`sessions`, from the process's harness session id). */
export function previewsOfSession(previews: readonly DevServerPreview[], sessionId: string): DevServerPreview[] {
  return previews.filter(preview => preview.sessions?.some(session => session.session_id === sessionId))
}
