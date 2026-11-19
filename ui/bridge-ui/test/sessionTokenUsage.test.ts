import { describe, expect, it } from 'vitest'
import type { SessionAggregate } from '@kayushkin/llm-bridge-types'
import {
  addTokenUsage,
  allTokens,
  allTokensOfUsage,
  emptyTokenUsageTotals,
  modelCallsText,
  querySourceIsSubagent,
  sessionAggregatesSincePath,
  sessionTokenUsageOf,
  sessionUsageInWindow,
  tokenCountsText,
  usagePeriodStart,
  wholeSessionTokenCounts,
} from '../src/sessionTokenUsage'
import { formatTokens } from '../src/utils'

const aggregate = (fields: Partial<SessionAggregate>): SessionAggregate => ({
  session_id: 'br_1',
  turns: 1,
  input_tokens: 0,
  output_tokens: 0,
  duration_ms: 0,
  ...fields,
})

describe('querySourceIsSubagent', () => {
  it('reads a subagent from the agent: prefix', () => {
    expect(querySourceIsSubagent('agent:builtin:general-purpose')).toBe(true)
    expect(querySourceIsSubagent('agent:builtin:fork')).toBe(true)
    expect(querySourceIsSubagent('agent:custom')).toBe(true)
  })

  it('counts every other source as the main conversation', () => {
    for (const source of ['sdk', 'repl_main_thread', 'compact', 'web_search_tool', 'web_fetch_apply', '']) {
      expect(querySourceIsSubagent(source)).toBe(false)
    }
  })
})

describe('sessionTokenUsageOf', () => {
  it('splits the api_call record into the main conversation and its subagents', () => {
    const usage = sessionTokenUsageOf(aggregate({
      // The turn results of the same session: far short of the calls below,
      // and not added to them.
      input_tokens: 1134,
      output_tokens: 191327,
      cache_read_tokens: 6172212,
      api_call_usage: [
        { query_source: 'agent:builtin:fork', calls: 40, input_tokens: 100, output_tokens: 20000, cache_read_tokens: 60_000_000, cache_write_tokens: 400_000 },
        { query_source: 'agent:builtin:general-purpose', calls: 4894, input_tokens: 10148, output_tokens: 4_600_000, cache_read_tokens: 1_780_000_000, cache_write_tokens: 87_000_000 },
        { query_source: 'compact', calls: 2, input_tokens: 10, output_tokens: 6000, cache_read_tokens: 0, cache_write_tokens: 0 },
        { query_source: 'sdk', calls: 398, input_tokens: 1124, output_tokens: 209_000, cache_read_tokens: 215_000_000, cache_write_tokens: 2_700_000 },
      ],
    }))
    expect(usage.countedFrom).toBe('api-calls')
    expect(usage.mainConversation).toEqual({ input: 1134, output: 215_000, cacheRead: 215_000_000, cacheWrite: 2_700_000 })
    expect(usage.mainConversationCalls).toBe(400)
    expect(usage.subagents).toEqual({ input: 10248, output: 4_620_000, cacheRead: 1_840_000_000, cacheWrite: 87_400_000 })
    expect(usage.subagentCalls).toBe(4934)
    expect(wholeSessionTokenCounts(usage).cacheRead).toBe(2_055_000_000)
  })

  it('reads a session with no api_call record from its turn results, cache tokens included', () => {
    const usage = sessionTokenUsageOf(aggregate({
      input_tokens: 500,
      output_tokens: 900,
      cache_read_tokens: 70_000,
      cache_write_tokens: 3_000,
    }))
    expect(usage.countedFrom).toBe('turn-results')
    expect(usage.mainConversation).toEqual({ input: 500, output: 900, cacheRead: 70_000, cacheWrite: 3_000 })
    expect(allTokens(usage.subagents)).toBe(0)
    expect(usage.mainConversationCalls).toBe(0)
    expect(usage.subagentCalls).toBe(0)
  })

  it('reads absent cache fields as zero', () => {
    const usage = sessionTokenUsageOf(aggregate({ input_tokens: 10, output_tokens: 20 }))
    expect(usage.mainConversation).toEqual({ input: 10, output: 20, cacheRead: 0, cacheWrite: 0 })
  })

  it('treats an empty api_call record as none', () => {
    expect(sessionTokenUsageOf(aggregate({ input_tokens: 5, api_call_usage: [] })).countedFrom).toBe('turn-results')
  })
})

describe('totals', () => {
  it('keeps the two halves apart while adding sessions up', () => {
    const totals = emptyTokenUsageTotals()
    addTokenUsage(totals, sessionTokenUsageOf(aggregate({
      api_call_usage: [
        { query_source: 'sdk', calls: 1, input_tokens: 1, output_tokens: 2, cache_read_tokens: 3, cache_write_tokens: 4 },
        { query_source: 'agent:custom', calls: 1, input_tokens: 10, output_tokens: 20, cache_read_tokens: 30, cache_write_tokens: 40 },
      ],
    })))
    addTokenUsage(totals, sessionTokenUsageOf(aggregate({ input_tokens: 100, output_tokens: 200 })))
    expect(totals.mainConversation).toEqual({ input: 101, output: 202, cacheRead: 3, cacheWrite: 4 })
    expect(totals.subagents).toEqual({ input: 10, output: 20, cacheRead: 30, cacheWrite: 40 })
    expect(allTokensOfUsage(totals)).toBe(410)
  })
})

describe('text', () => {
  it('names each kind and leaves a zero cache kind out', () => {
    expect(tokenCountsText({ input: 1134, output: 215_000, cacheRead: 215_000_000, cacheWrite: 2_700_000 }))
      .toBe('1.1k in · 215.0k out · 215.0M cache read · 2.7M cache write')
    expect(tokenCountsText({ input: 10, output: 20, cacheRead: 0, cacheWrite: 0 })).toBe('10 in · 20 out')
  })

  it('writes billions as billions', () => {
    expect(formatTokens(2_055_182_536)).toBe('2.06B')
    expect(formatTokens(90_428_705)).toBe('90.4M')
  })

  it('counts calls in words', () => {
    expect(modelCallsText(1)).toBe('1 call')
    expect(modelCallsText(4934)).toBe('4,934 calls')
  })
})

describe('a window of time', () => {
  const now = new Date('2026-03-10T15:30:00.000Z')

  it('counts each period back from now', () => {
    expect(usagePeriodStart('day', now).toISOString()).toBe('2026-03-09T15:30:00.000Z')
    expect(usagePeriodStart('week', now).toISOString()).toBe('2026-03-03T15:30:00.000Z')
    expect(usagePeriodStart('month', now).toISOString()).toBe('2026-02-08T15:30:00.000Z')
  })

  it('asks the aggregate route for the window, the time escaped', () => {
    expect(sessionAggregatesSincePath('/api/bridge', usagePeriodStart('day', now)))
      .toBe('/api/bridge/sessions/aggregates?since=2026-03-09T15%3A30%3A00.000Z')
  })

  it('places each session by the session list and takes its cost from the window', () => {
    const usage = sessionUsageInWindow(
      [
        aggregate({
          session_id: 'br_long',
          turns: 3,
          duration_ms: 9000,
          model: 'test-model',
          session_cost_usd: 12.5,
          api_call_usage: [
            { query_source: 'agent:custom', calls: 7, input_tokens: 1, output_tokens: 2, cache_read_tokens: 3, cache_write_tokens: 4 },
          ],
        }),
        aggregate({ session_id: 'br_unlisted', output_tokens: 40 }),
      ],
      [
        { sessionId: 'br_long', harness: 'claude_code', instanceId: 'instance_1', purpose: 'chat' },
        { sessionId: 'br_idle', harness: 'codex', instanceId: 'instance_2', purpose: '' },
      ],
    )
    // One row per session in the window: br_idle used nothing and is not there.
    expect(usage.map(u => u.sessionId)).toEqual(['br_long', 'br_unlisted'])
    expect(usage[0].placement).toEqual({ sessionId: 'br_long', harness: 'claude_code', instanceId: 'instance_1', purpose: 'chat' })
    expect(usage[0].costUsd).toBe(12.5)
    expect(usage[0].turns).toBe(3)
    expect(usage[0].durationMs).toBe(9000)
    expect(usage[0].model).toBe('test-model')
    expect(usage[0].tokens.subagentCalls).toBe(7)
    // A session the list does not hold keeps its usage and has no placement.
    expect(usage[1].placement).toBeUndefined()
    expect(usage[1].costUsd).toBe(0)
    expect(usage[1].tokens.mainConversation.output).toBe(40)
  })
})
