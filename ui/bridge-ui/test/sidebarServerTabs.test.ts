import { describe, expect, it } from 'vitest'
import { EMPTY_FILTER, type SessionSummary } from '@kayushkin/chat-core'
import {
  mergeSessionsNewestFirst,
  mirroredFilterPatch,
  sessionDayHeading,
  shownServerTab,
  type MergedSessionEntry,
} from '../src/sidebarServerTabs'

const session = (sessionId: string, updatedAt: string) => ({ sessionId, updatedAt }) as SessionSummary

describe('mergeSessionsNewestFirst', () => {
  it('interleaves the servers by last activity, newest first', () => {
    const entries: MergedSessionEntry[] = [
      { server: 'this', session: session('a', '2026-10-29T10:00:00Z') },
      { server: 'this', session: session('b', '2026-10-27T10:00:00Z') },
      { server: 'connected', connectionId: 'c1', serverLabel: 'laptop', session: session('c', '2026-10-28T10:00:00Z') },
    ]
    expect(mergeSessionsNewestFirst(entries).map(e => e.session.sessionId)).toEqual(['a', 'c', 'b'])
  })

  it('keeps the incoming order on a tie and puts an unreadable time last', () => {
    const entries: MergedSessionEntry[] = [
      { server: 'this', session: session('bad', 'not a time') },
      { server: 'this', session: session('x', '2026-10-29T10:00:00Z') },
      { server: 'connected', connectionId: 'c1', serverLabel: 'laptop', session: session('y', '2026-10-29T10:00:00Z') },
    ]
    expect(mergeSessionsNewestFirst(entries).map(e => e.session.sessionId)).toEqual(['x', 'y', 'bad'])
  })
})

describe('sessionDayHeading', () => {
  const now = new Date(2026, 9, 29, 15, 0)
  it('names today and yesterday by local day', () => {
    expect(sessionDayHeading(new Date(2026, 9, 29, 0, 5).toISOString(), now)).toBe('Today')
    expect(sessionDayHeading(new Date(2026, 9, 28, 23, 55).toISOString(), now)).toBe('Yesterday')
  })
  it('dates anything older, and says so for a time it cannot read', () => {
    expect(sessionDayHeading(new Date(2026, 9, 20, 12).toISOString(), now)).not.toMatch(/Today|Yesterday/)
    expect(sessionDayHeading(new Date(2025, 9, 20, 12).toISOString(), now)).toMatch(/2025/)
    expect(sessionDayHeading('nope', now)).toBe('Unknown day')
  })
})

describe('shownServerTab', () => {
  it('shows the stored tab only while its server is listed', () => {
    expect(shownServerTab('c1', ':this', ['c1', 'c2'])).toBe('c1')
    expect(shownServerTab('gone', ':this', ['c1'])).toBe(':this')
    expect(shownServerTab(':this', ':this', [])).toBe(':this')
  })
})

describe('mirroredFilterPatch', () => {
  it('copies the search and the shared axes, never machine or folder', () => {
    const from = { ...EMPTY_FILTER, search: 'deploy', status: ['running'], machine: ['instance_1'], folder: 'x' }
    expect(mirroredFilterPatch(from, EMPTY_FILTER)).toEqual({ search: 'deploy', status: ['running'] })
  })
  it('is null when the two already agree', () => {
    const from = { ...EMPTY_FILTER, search: 'a', harness: ['claude_code'] }
    expect(mirroredFilterPatch(from, { ...from, machine: ['other'] })).toBeNull()
  })
})
