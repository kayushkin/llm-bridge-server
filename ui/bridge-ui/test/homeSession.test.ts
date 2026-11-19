import { describe, expect, it } from 'vitest'
import { fetchHomeSessionId } from '../src/components/chat/useHomeSession'

function answering(status: number, body: unknown) {
  const asked: string[] = []
  const fetch = async (url: string) => {
    asked.push(url)
    return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
  }
  return { fetch, asked }
}

describe('fetchHomeSessionId', () => {
  it('asks orchestratord for the home session and returns its id', async () => {
    const { fetch, asked } = answering(200, { session_id: 'br_home' })
    expect(await fetchHomeSessionId(fetch, '/api/orchestrator')).toBe('br_home')
    expect(asked).toEqual(['/api/orchestrator/home-session'])
  })
  it('throws orchestratord’s own reason on a 502', async () => {
    const { fetch } = answering(502, { error: 'llm-bridge refused the session' })
    await expect(fetchHomeSessionId(fetch, '/api/orchestrator')).rejects.toThrow('llm-bridge refused the session')
  })
  it('throws on a 200 that names no session', async () => {
    const { fetch } = answering(200, {})
    await expect(fetchHomeSessionId(fetch, '/api/orchestrator')).rejects.toThrow('names no session_id')
  })
})
