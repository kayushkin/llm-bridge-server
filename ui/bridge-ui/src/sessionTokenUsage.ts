// What a session's token totals are, read from the bridge's per-session
// aggregate (GET /sessions/aggregates, llm-bridge's msg.SessionAggregate).
//
// The aggregate carries two records of the same session, and this module
// decides which one a page shows and how it is split:
//
//   - the api_call record, one total per query source: every model call the
//     harness reported, a subagent's included. Only a harness that reports
//     each call has it (Claude Code does).
//   - the turn-result record: what each finished turn of the main
//     conversation reported. Every harness has it, but it holds no subagent
//     call and nothing from a turn that never finished.
//
// A session with the api_call record is drawn from it, split into the main
// conversation and its subagents. A session without it is drawn from its turn
// results, as the main conversation, and says so, because subagents it may
// have run are not in that number.
//
// Input counts only tokens that were not read from the prompt cache. A long
// session's input is nearly all cache reads, so a page that leaves the cache
// kinds out shows a small fraction of what the session used: one session's
// 2.1 billion cache-read tokens over 5,300 calls once showed as 192 thousand.

import type { SessionAggregate } from '@kayushkin/llm-bridge-types'
import { formatTokens } from './utils'

/** Tokens of the four kinds a model call is counted in. */
export interface TokenCounts {
  input: number
  output: number
  cacheRead: number
  cacheWrite: number
}

/** Which of the aggregate's two records a session's numbers were read from. */
export type TokenUsageRecord = 'api-calls' | 'turn-results'

export interface SessionTokenUsage {
  countedFrom: TokenUsageRecord
  mainConversation: TokenCounts
  subagents: TokenCounts
  /** Model calls behind each half. Zero when counted from turn results, which
   *  do not say how many calls a turn made. */
  mainConversationCalls: number
  subagentCalls: number
}

export function emptyTokenCounts(): TokenCounts {
  return { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }
}

export function emptySessionTokenUsage(): SessionTokenUsage {
  return {
    countedFrom: 'turn-results',
    mainConversation: emptyTokenCounts(),
    subagents: emptyTokenCounts(),
    mainConversationCalls: 0,
    subagentCalls: 0,
  }
}

/** Adds `counts` into `into`, in place. */
export function addTokenCounts(into: TokenCounts, counts: TokenCounts): void {
  into.input += counts.input
  into.output += counts.output
  into.cacheRead += counts.cacheRead
  into.cacheWrite += counts.cacheWrite
}

export function sumOfTokenCounts(a: TokenCounts, b: TokenCounts): TokenCounts {
  const sum = emptyTokenCounts()
  addTokenCounts(sum, a)
  addTokenCounts(sum, b)
  return sum
}

/** Every token of every kind: input, output, cache read and cache write. */
export function allTokens(counts: TokenCounts): number {
  return counts.input + counts.output + counts.cacheRead + counts.cacheWrite
}

/** The main conversation's and the subagents' tokens together. */
export function wholeSessionTokenCounts(usage: SessionTokenUsage): TokenCounts {
  return sumOfTokenCounts(usage.mainConversation, usage.subagents)
}

/**
 * True when a model call's query source says a subagent made it.
 *
 * The query source is the harness's own word for why it made a call. Claude
 * Code writes `agent:<kind>` for a subagent (`agent:builtin:general-purpose`,
 * `agent:builtin:fork`, `agent:custom`) and other words for everything the
 * main conversation does: its own turns (`sdk`, `repl_main_thread`), a
 * compaction, the model call inside a web search. Those all count as the main
 * conversation here, so the rule names the one prefix and no list of words
 * that a new harness version would outgrow.
 */
export function querySourceIsSubagent(querySource: string): boolean {
  return querySource.startsWith('agent:')
}

/** A session's token usage from its aggregate row. */
export function sessionTokenUsageOf(aggregate: SessionAggregate): SessionTokenUsage {
  const apiCallUsage = aggregate.api_call_usage ?? []
  if (apiCallUsage.length === 0) {
    return {
      countedFrom: 'turn-results',
      mainConversation: {
        input: aggregate.input_tokens,
        output: aggregate.output_tokens,
        cacheRead: aggregate.cache_read_tokens ?? 0,
        cacheWrite: aggregate.cache_write_tokens ?? 0,
      },
      subagents: emptyTokenCounts(),
      mainConversationCalls: 0,
      subagentCalls: 0,
    }
  }
  const usage: SessionTokenUsage = { ...emptySessionTokenUsage(), countedFrom: 'api-calls' }
  for (const source of apiCallUsage) {
    const counts: TokenCounts = {
      input: source.input_tokens,
      output: source.output_tokens,
      cacheRead: source.cache_read_tokens,
      cacheWrite: source.cache_write_tokens,
    }
    if (querySourceIsSubagent(source.query_source)) {
      addTokenCounts(usage.subagents, counts)
      usage.subagentCalls += source.calls
    } else {
      addTokenCounts(usage.mainConversation, counts)
      usage.mainConversationCalls += source.calls
    }
  }
  return usage
}

/** Token totals over many sessions, the two halves kept apart. */
export interface TokenUsageTotals {
  mainConversation: TokenCounts
  subagents: TokenCounts
}

export function emptyTokenUsageTotals(): TokenUsageTotals {
  return { mainConversation: emptyTokenCounts(), subagents: emptyTokenCounts() }
}

/** Adds one session's or one group's usage into `into`, in place. */
export function addTokenUsage(into: TokenUsageTotals, usage: TokenUsageTotals): void {
  addTokenCounts(into.mainConversation, usage.mainConversation)
  addTokenCounts(into.subagents, usage.subagents)
}

/** Every token of every kind in both halves. */
export function allTokensOfUsage(usage: TokenUsageTotals): number {
  return allTokens(usage.mainConversation) + allTokens(usage.subagents)
}

/**
 * Token counts as one line: `1.1k in · 215.0k out · 215.0M cache read ·
 * 2.7M cache write`. Input and output are always there; a cache kind is left
 * out when it is zero, which is every session of a harness that has no cache.
 */
export function tokenCountsText(counts: TokenCounts): string {
  const parts = [`${formatTokens(counts.input)} in`, `${formatTokens(counts.output)} out`]
  if (counts.cacheRead > 0) parts.push(`${formatTokens(counts.cacheRead)} cache read`)
  if (counts.cacheWrite > 0) parts.push(`${formatTokens(counts.cacheWrite)} cache write`)
  return parts.join(' · ')
}

/** `1 call`, `4,934 calls`. */
export function modelCallsText(calls: number): string {
  return `${calls.toLocaleString('en-US')} call${calls === 1 ? '' : 's'}`
}

/** What the record a session was counted from means, for a title or a note. */
export function tokenUsageRecordNote(record: TokenUsageRecord): string {
  return record === 'api-calls'
    ? 'Counted from every model call the harness reported, subagents included.'
    : 'Counted from finished turns only. This harness reports no per-call usage, so subagent calls and unfinished turns are not in these numbers.'
}

// ── A window of time ────────────────────────────────────────────────────────
//
// The Usage page's tabs are windows: the last 24 hours, 7 days, 30 days. The
// aggregate route answers a window with `?since=`: only the sessions that used
// anything from then on, each with what it did from then on and how much its
// cost grew (session_cost_usd). A session started last week that worked all
// of today is in today's window with today's numbers. The page used to list
// the sessions *started* in the window with their whole-life totals, which
// left that session out of today and showed all of its work on the day it
// began.

export type UsagePeriod = 'day' | 'week' | 'month'

const USAGE_PERIOD_MILLISECONDS: Record<UsagePeriod, number> = {
  day: 24 * 60 * 60 * 1000,
  week: 7 * 24 * 60 * 60 * 1000,
  month: 30 * 24 * 60 * 60 * 1000,
}

/** When a period's window began, counted back from `now`. */
export function usagePeriodStart(period: UsagePeriod, now: Date): Date {
  return new Date(now.getTime() - USAGE_PERIOD_MILLISECONDS[period])
}

/** The aggregate route for what every session did from `since` on. The server
 *  counts in whole clock hours and includes the hour holding `since`. */
export function sessionAggregatesSincePath(basePath: string, since: Date): string {
  return `${basePath}/sessions/aggregates?since=${encodeURIComponent(since.toISOString())}`
}

/** What the page needs from the bridge's session list to place a session's
 *  usage under its harness and source. */
export interface SessionPlacement {
  sessionId: string
  harness: string
  instanceId: string
  purpose: string
}

/** One session's usage inside a window. */
export interface SessionUsageInWindow {
  sessionId: string
  /** The session's row in the bridge's session list. Absent when that list
   *  does not hold a session log-store counted; its usage is still shown,
   *  apart, because leaving it out would make the totals wrong. */
  placement?: SessionPlacement
  tokens: SessionTokenUsage
  /** How much the session's cost estimate grew inside the window. */
  costUsd: number
  durationMs: number
  model: string
  turns: number
}

/** Joins a windowed aggregate with the session list, by session id. */
export function sessionUsageInWindow(
  aggregates: SessionAggregate[],
  placements: SessionPlacement[],
): SessionUsageInWindow[] {
  const placementById = new Map<string, SessionPlacement>()
  for (const p of placements) placementById.set(p.sessionId, p)
  return aggregates.map(a => ({
    sessionId: a.session_id,
    placement: placementById.get(a.session_id),
    tokens: sessionTokenUsageOf(a),
    costUsd: a.session_cost_usd ?? 0,
    durationMs: a.duration_ms,
    model: a.model ?? '',
    turns: a.turns,
  }))
}
