import type { FetchFn } from './types'

// Reading the session list a page at a time, newest first, down to a cutoff.
//
// The bridge's GET /sessions has no bound: every column of every session, info blobs
// included — 64 MB in 2.6 s on 2026-09-16. A page that only needs recent sessions
// pages GET /sessions/summary instead, which is ordered by last update, and stops at
// the first row older than it cares about.

/** The summary row, as GET /sessions/summary sends it. Only what callers here read. */
export interface SessionSummaryRow {
  sessionId: string
  harness: string
  instanceId: string
  purpose: string
  createdAt: string
  updatedAt: string
  /** The session's cost estimate so far, in US dollars (llm-bridge-server's
   *  spend_usd: per-call spend and per-turn result costs combined). */
  spendUsd: number
}

interface SummaryPage {
  sessions: SessionSummaryRow[]
  next?: string | null
}

/** Every session last updated at or after `since`, newest first. */
export async function listSessionSummariesUpdatedSince(
  fetchFn: FetchFn,
  basePath: string,
  since: Date,
  pageSize = 100,
): Promise<SessionSummaryRow[]> {
  const out: SessionSummaryRow[] = []
  let cursor: string | null = null
  for (;;) {
    const params = new URLSearchParams({ limit: String(pageSize) })
    if (cursor) params.set('before', cursor)
    const res = await fetchFn(`${basePath}/sessions/summary?${params.toString()}`)
    if (!res.ok) throw new Error(`GET /sessions/summary failed: ${res.status}`)
    const page = (await res.json()) as SummaryPage
    for (const row of page.sessions) {
      if (new Date(row.updatedAt).getTime() < since.getTime()) return out
      out.push(row)
    }
    if (!page.next) return out
    cursor = page.next
  }
}
