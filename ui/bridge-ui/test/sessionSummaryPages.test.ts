import { describe, expect, it } from 'vitest'
import { listSessionSummariesUpdatedSince, type SessionSummaryRow } from '../src/sessionSummaryPages'

function row(id: string, updatedAt: string): SessionSummaryRow {
  return { sessionId: id, harness: 'claude_code', instanceId: 'i', purpose: '', createdAt: updatedAt, updatedAt, spendUsd: 0 }
}

describe('listSessionSummariesUpdatedSince', () => {
  it('pages newest first and stops at the first row older than the cutoff', async () => {
    const urls: string[] = []
    const pages: Record<string, unknown> = {
      '': { sessions: [row('a', '2026-09-16T10:00:00Z'), row('b', '2026-09-15T10:00:00Z')], next: 'c1' },
      c1: { sessions: [row('c', '2026-09-10T10:00:00Z'), row('old', '2026-08-01T00:00:00Z')], next: 'c2' },
    }
    const fetchFn = (async (url: string) => {
      urls.push(url)
      const before = new URLSearchParams(url.split('?')[1]).get('before') ?? ''
      if (!(before in pages)) throw new Error(`paged past the cutoff: ${url}`)
      return { ok: true, status: 200, json: async () => pages[before] } as Response
    }) as unknown as import('../src/types').FetchFn

    const got = await listSessionSummariesUpdatedSince(fetchFn, '/api/bridge', new Date('2026-09-01T00:00:00Z'))

    expect(got.map((r) => r.sessionId)).toEqual(['a', 'b', 'c'])
    expect(urls).toHaveLength(2)
    expect(urls[0]).toBe('/api/bridge/sessions/summary?limit=100')
  })

  it('says so when a page fails, rather than returning a short list', async () => {
    const fetchFn = (async () => ({ ok: false, status: 502 }) as Response) as unknown as import('../src/types').FetchFn
    await expect(listSessionSummariesUpdatedSince(fetchFn, '/b', new Date())).rejects.toThrow('502')
  })
})
