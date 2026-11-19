import { describe, expect, it } from 'vitest'
import {
  getCardEffectiveDefaults, listAssignmentStrategies, getEffectiveDefaultsForTags, getTagRules, patchBoard, putPriorityLadder, putTagRules,
} from '../src/kanbanStoreClient'
import type { FetchFn } from '../src/types'

/** A fetch that records every call and answers each with `status` and `body`. */
function recordingFetch(status: number, body: unknown) {
  const calls: { url: string; method?: string; body?: unknown }[] = []
  const fetchFn = (async (url: string, init?: RequestInit) => {
    calls.push({ url, method: init?.method, body: init?.body === undefined ? undefined : JSON.parse(String(init.body)) })
    return new Response(JSON.stringify(body), { status })
  }) as unknown as FetchFn
  return { calls, fetchFn }
}

describe('kanbanStoreClient', () => {
  it('PATCHes exactly the patch it is given to the board’s URL', async () => {
    const { calls, fetchFn } = recordingFetch(200, { id: 'b1' })
    const result = await patchBoard(fetchFn, '/api/kanban', 'b1', { default_instance_id: '' })
    expect(result.ok).toBe(true)
    expect(calls).toEqual([{ url: '/api/kanban/api/boards/b1', method: 'PATCH', body: { default_instance_id: '' } }])
  })

  it('returns kanban-store’s refusal verbatim, so the page shows the owner’s words', async () => {
    const refusal = 'default_bundle_id: not found in bundle-store: bundle-store refused id "docker" ({"error":"invalid id"}) — send bundle-store\'s numeric id, not the bundle\'s name'
    const { fetchFn } = recordingFetch(400, { error: refusal })
    const result = await patchBoard(fetchFn, '/api/kanban', 'b1', { default_bundle_id: 'docker' })
    expect(result).toEqual({ ok: false, error: refusal })
  })

  it('reports an owner that could not be asked as an error, not as success', async () => {
    const { fetchFn } = recordingFetch(502, { error: 'llm-bridge-server check of default_instance_id failed: connection refused' })
    const result = await putPriorityLadder(fetchFn, '/api/kanban', 'b1', { levels: [] })
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toContain('connection refused')
  })
})

describe('tag rules and effective defaults', () => {
  it('reads the board’s tag rules from /tag-rules', async () => {
    const { calls, fetchFn } = recordingFetch(200, { board_id: 'b 1', rules: [] })
    const result = await getTagRules(fetchFn, '/api/kanban', 'b 1')
    expect(result).toEqual({ ok: true, value: { board_id: 'b 1', rules: [] } })
    expect(calls).toEqual([{ url: '/api/kanban/api/boards/b%201/tag-rules', method: undefined, body: undefined }])
  })

  it('PUTs the whole list, in order, to /tag-rules', async () => {
    const body = { rules: [
      { id: 'rule-a', tags: ['cat:product', 'urgency:high'], default_instance_id: 'inst-cc-local' },
      { tags: ['cat:product'], default_bundle_id: '6' },
    ] }
    const { calls, fetchFn } = recordingFetch(200, { board_id: 'b1', rules: [] })
    const result = await putTagRules(fetchFn, '/api/kanban', 'b1', body)
    expect(result.ok).toBe(true)
    expect(calls).toEqual([{ url: '/api/kanban/api/boards/b1/tag-rules', method: 'PUT', body }])
  })

  it('returns a tag-rules refusal verbatim, naming the rule the store refused', async () => {
    const refusal = 'rules[1] (tags cat:product) default_bundle_id: bundle-store refused id "docker"'
    const { fetchFn } = recordingFetch(400, { error: refusal })
    const result = await putTagRules(fetchFn, '/api/kanban', 'b1', { rules: [{ tags: ['cat:product'], default_bundle_id: 'docker' }] })
    expect(result).toEqual({ ok: false, error: refusal })
  })

  it('reports a 502 from a tag-rules save as an error', async () => {
    const { fetchFn } = recordingFetch(502, { error: 'principal-store check of rules[0] failed: connection refused' })
    const result = await putTagRules(fetchFn, '/api/kanban', 'b1', { rules: [] })
    expect(result).toEqual({ ok: false, error: 'principal-store check of rules[0] failed: connection refused' })
  })

  it('reads a card’s effective defaults from the per-card route', async () => {
    const answer = {
      board_id: 'b1', card_id: 'c/1', tags: ['cat:product'], matched_rule_ids: ['rule-b'],
      defaults: { default_bundle_id: { value: '6', source: { kind: 'tag_rule', rule_id: 'rule-b', rule_tags: ['cat:product'], rule_position: 1 } } },
    }
    const { calls, fetchFn } = recordingFetch(200, answer)
    const result = await getCardEffectiveDefaults(fetchFn, '/api/kanban', 'b1', 'c/1')
    expect(result).toEqual({ ok: true, value: answer })
    expect(calls[0].url).toBe('/api/kanban/api/boards/b1/cards/c%2F1/effective-defaults')
  })

  it('returns a card read’s failure verbatim', async () => {
    const { fetchFn } = recordingFetch(404, { error: 'noteboard item not found: c1' })
    expect(await getCardEffectiveDefaults(fetchFn, '/api/kanban', 'b1', 'c1')).toEqual({ ok: false, error: 'noteboard item not found: c1' })
  })

  it('asks for a tag list as repeated tag parameters, and for no tags with no query', async () => {
    const { calls, fetchFn } = recordingFetch(200, { board_id: 'b1', tags: [], matched_rule_ids: [], defaults: {} })
    await getEffectiveDefaultsForTags(fetchFn, '/api/kanban', 'b1', ['cat:product', 'urgency:high'])
    await getEffectiveDefaultsForTags(fetchFn, '/api/kanban', 'b1', [])
    const first = new URL(calls[0].url, 'http://host')
    expect(first.pathname).toBe('/api/kanban/api/boards/b1/effective-defaults')
    expect(first.searchParams.getAll('tag')).toEqual(['cat:product', 'urgency:high'])
    expect(calls[1].url).toBe('/api/kanban/api/boards/b1/effective-defaults')
  })
})

describe('assignment strategies', () => {
  it('reads the served list from the store rather than keeping a copy', async () => {
    const { calls, fetchFn } = recordingFetch(200, ['least_open_cards', 'round_robin'])
    const result = await listAssignmentStrategies(fetchFn, '/api/kanban')
    expect(result).toEqual({ ok: true, value: ['least_open_cards', 'round_robin'] })
    expect(calls).toEqual([{ url: '/api/kanban/api/assignment-strategies', method: undefined, body: undefined }])
  })

  it('reports a store without the route as an error, not as an empty list', async () => {
    const { fetchFn } = recordingFetch(404, { error: 'not found' })
    const result = await listAssignmentStrategies(fetchFn, '/api/kanban')
    expect(result.ok).toBe(false)
  })
})
