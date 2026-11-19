import { describe, expect, it } from 'vitest'
import type { SessionStatus } from '@kayushkin/chat-core'
import {
  statusActivitySince,
  statusActivityText,
  statusActivityWord,
  statusRateLimitText,
} from '../src/components/chat/sessionStatusWords'

const bash = { tool_id: 't1', name: 'Bash', summary: 'cat thing.txt', started_at: '2026-09-17T12:00:05Z' }
const read = { tool_id: 't2', name: 'Read', summary: '/a/b.go', started_at: '2026-09-17T12:00:09Z' }

function status(p: Partial<SessionStatus>): SessionStatus {
  return { state: 'idle', as_of: 1, ...p }
}

describe('what a status line says a session is doing', () => {
  it('names the newest tool in flight, with its input and a count of the rest', () => {
    const s = status({ state: 'tool_running', tools: [bash, read] })
    expect(statusActivityText(s)).toBe('Read — /a/b.go  (+1 more)')
    expect(statusActivityWord(s)).toBe('Read')
    expect(statusActivitySince(s)).toBe(read.started_at)
  })

  it('says thinking or responding while the model generates, from the server\'s word', () => {
    expect(statusActivityText(status({ state: 'model_generating', generating: 'thinking' }))).toBe('thinking')
    expect(statusActivityText(status({ state: 'model_generating', generating: 'text' }))).toBe('responding')
    expect(statusActivityText(status({ state: 'model_generating' }))).toBe('working')
  })

  it('times a turn with no tool in flight from the turn\'s start', () => {
    const s = status({ state: 'model_generating', turn_started_at: '2026-09-17T12:00:00Z' })
    expect(statusActivitySince(s)).toBe('2026-09-17T12:00:00Z')
  })

  it('says nothing for a session that is not running — a paused one has no activity', () => {
    // The Bash-then-Stop case: the server reports paused with no tools, and the line
    // must not invent a tool from anywhere else.
    expect(statusActivityText(status({ state: 'paused' }))).toBe('')
    expect(statusActivityText(status({ state: 'idle' }))).toBe('')
    expect(statusActivityText(null)).toBe('')
  })

  it('names a permission prompt and a compaction', () => {
    expect(statusActivityText(status({ state: 'awaiting_permission' }))).toBe('waiting for permission')
    expect(statusActivityText(status({ state: 'compacting' }))).toBe('compacting context')
  })

  it('reports a rejected rate limit with its window and reset, and nothing for a warning', () => {
    const at = (unixSeconds: number) => `t${unixSeconds}`
    const rejected = status({ state: 'error', rate_limit: { status: 'rejected', limit_type: 'seven_day', resets_at: 1789686000 } })
    expect(statusRateLimitText(rejected, at)).toBe('rate limit hit (seven day) — resets t1789686000')
    expect(statusRateLimitText(status({ rate_limit: { status: 'allowed_warning' } }), at)).toBe('')
  })
})
