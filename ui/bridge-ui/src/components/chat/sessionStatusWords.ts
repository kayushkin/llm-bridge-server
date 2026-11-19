import { isRunningState, type SessionStatus } from '@kayushkin/chat-core'

// The words a status is drawn with. Pure, and the only place they are chosen, so the
// status line, the header's dot and anything else that names what a session is doing
// say the same thing about the same status.
//
// Every fact here comes from `SessionStatus`, which llm-bridge-server decides. Nothing
// is inferred: a state with no tool listed is not given a tool, and a session that is
// not running is not given an activity.

/**
 * What a RUNNING session is doing, in a few words: the newest tool call in flight
 * (name, the server's one-line input summary, and a count when calls run in
 * parallel), else what the model is emitting, else what it is waiting on. Empty for
 * a session that is not running — there is no activity to name.
 *
 * "responding" rather than the wire's "text": the reader is a person watching a chat.
 */
export function statusActivityText(status: SessionStatus | null): string {
  if (!status) return ''
  const tools = status.tools ?? []
  const newest = tools[tools.length - 1]
  if (newest) {
    const name = newest.name || 'tool'
    const extra = tools.length > 1 ? `  (+${tools.length - 1} more)` : ''
    return `${name}${newest.summary ? ` — ${newest.summary}` : ''}${extra}`
  }
  switch (status.state) {
    case 'model_generating':
      if (status.generating === 'thinking') return 'thinking'
      if (status.generating === 'text') return 'responding'
      return 'working'
    case 'compacting':
      return 'compacting context'
    case 'awaiting_permission':
    case 'waiting_on_approval':
      return 'waiting for permission'
    case 'starting':
      return 'starting'
    case 'rate_limited':
      return 'rate limited'
    case 'background_tasks_running':
      return 'background tasks running'
    default:
      return isRunningState(status.state) ? 'working' : ''
  }
}

/** The short form for a label that has no room for an input summary — the header's
 *  status dot: the tool's name alone, or the same word `statusActivityText` gives. */
export function statusActivityWord(status: SessionStatus | null): string {
  const tools = status?.tools ?? []
  const newest = tools[tools.length - 1]
  if (newest) return newest.name || 'tool'
  return statusActivityText(status)
}

/** When the thing `statusActivityText` names began: the newest in-flight tool call,
 *  else the turn. Undefined when the status says neither. */
export function statusActivitySince(status: SessionStatus | null): string | undefined {
  const tools = status?.tools ?? []
  return tools[tools.length - 1]?.started_at ?? status?.turn_started_at
}

/** "rate limit hit — resets 14:00", when the provider's last verdict was a rejection.
 *  A rejected limit ends the turn in an error, so this is the reason beside the state. */
export function statusRateLimitText(status: SessionStatus | null, formatTime: (unixSeconds: number) => string): string {
  const limit = status?.rate_limit
  if (!limit || limit.status !== 'rejected') return ''
  const window = limit.limit_type ? ` (${limit.limit_type.replace(/_/g, ' ')})` : ''
  const resets = limit.resets_at ? ` — resets ${formatTime(limit.resets_at)}` : ''
  return `rate limit hit${window}${resets}`
}
