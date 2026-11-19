import { describe, expect, it } from 'vitest'
import { dispatchAgentOnCard } from '../src/agentDispatch'
import type { FetchFn } from '../src/types'

function recordingFetch() {
  const calls: { url: string; method: string; body: Record<string, unknown> | undefined }[] = []
  const fetchFn = (async (url: string, init?: RequestInit) => {
    calls.push({ url, method: init?.method ?? 'GET', body: init?.body ? JSON.parse(String(init.body)) : undefined })
    return new Response(JSON.stringify({ session_id: 'br_1' }), { status: 200, headers: { 'Content-Type': 'application/json' } })
  }) as unknown as FetchFn
  return { calls, fetchFn }
}

const common = {
  basePath: '/api/bridge', title: 'Fix the flaky test', prompt: 'do it',
  addLink: async () => true, instance: { id: 'inst-cc-local', harness_type: 'claude_code' },
}

describe('dispatchAgentOnCard', () => {
  it('sends the board’s bundle as bundle_id on the session create', async () => {
    const { calls, fetchFn } = recordingFetch()
    await dispatchAgentOnCard({ ...common, fetchFn, bundleID: '6' })
    expect(calls[0].url).toBe('/api/bridge/sessions')
    expect(calls[0].method).toBe('POST')
    expect(calls[0].body).toMatchObject({ bundle_id: '6', instance_id: 'inst-cc-local', harness: 'claude_code' })
  })

  it('sends no bundle_id at all for a board with none', async () => {
    const { calls, fetchFn } = recordingFetch()
    await dispatchAgentOnCard({ ...common, fetchFn })
    expect(calls[0].body).not.toHaveProperty('bundle_id')
  })

  it('surfaces the server’s refusal in its own words', async () => {
    const refusal = 'bundle_id "docker" is not a bundle bundle-store has'
    const fetchFn = (async () => new Response(JSON.stringify({ error: refusal }), { status: 400 })) as unknown as FetchFn
    await expect(dispatchAgentOnCard({ ...common, fetchFn, bundleID: 'docker' })).rejects.toThrow(refusal)
  })
})
