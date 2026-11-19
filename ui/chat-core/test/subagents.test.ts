import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { compareSubagents } from '../src/react/subagents.js';
import type { SessionSummary } from '../src/net/types.js';

// The parent→child join behind the sidebar's subagent tree.
//
// It rests on `managerSessionId`, the server's own parent pointer. No id is invented
// here and no count exists anywhere: a child names its parent, and a caller that holds
// session ids can ask what they spawned.
//
// Why it is a server lookup and not client-side grouping: a child is ordered by its OWN
// updated_at, not its parent's, so a session that spawned 106 subagents last week has
// them scattered thousands of rows deep in a listing the sidebar reads one page of.
// Grouping only the loaded rows would show four of that 106 and look complete.

const BASE = '/api/bridge';

function client(): { api: ApiClient; urls: string[]; requests: { url: string; init?: RequestInit }[] } {
  const urls: string[] = [];
  const requests: { url: string; init?: RequestInit }[] = [];
  const fetchFn = async (url: string, init?: RequestInit): Promise<Response> => {
    urls.push(String(url));
    requests.push({ url: String(url), init });
    return {
      ok: true,
      status: 200,
      statusText: '200',
      json: async () => ({ sessions: [], next: null, revision: 'r' }),
      text: async () => '{}',
    } as unknown as Response;
  };
  return {
    api: new ApiClient({ fetch: fetchFn as unknown as typeof fetch, basePath: BASE }),
    urls,
    requests,
  };
}

function bodyOf(request: { init?: RequestInit }): Record<string, unknown> {
  return JSON.parse(String(request.init?.body)) as Record<string, unknown>;
}

function summary(over: Partial<SessionSummary> & Pick<SessionSummary, 'sessionId'>): SessionSummary {
  return {
    state: 'idle',
    harness: 'claude_code',
    instanceId: 'inst',
    type: 'system',
    purpose: 'subagent',
    mode: 'events',
    folderName: '',
    displayName: over.sessionId,
    agentId: '',
    updatedAt: '2026-08-20T05:00:00+00:00',
    createdAt: '2026-08-20T05:00:00+00:00',
    harnessSessionId: '',
    managerSessionId: 'br_parent',
    ...over,
  } as SessionSummary;
}

describe('asking what a session spawned', () => {
  it('sends every parent in one POST body, in one request', async () => {
    // One request for a whole page of rows. That is also what lets the caller learn
    // WHICH rows have children — no count column exists, and none was added.
    const ctx = client();
    await ctx.api.getSummary({ managerSessionIds: ['br_a', 'br_b', 'br_c'] });

    expect(ctx.requests).toHaveLength(1);
    expect(ctx.requests[0]!.init?.method).toBe('POST');
    expect(bodyOf(ctx.requests[0]!)).toEqual({ manager_session_ids: ['br_a', 'br_b', 'br_c'] });
  });

  it('keeps the ids OFF the URL, however many there are', async () => {
    // ⚠️ Regression pin for a bug that broke every new chat. This lookup used to go as
    // repeated query parameters, one per loaded session — 93 KB of URL on the real
    // sidebar — and nginx answers a long-enough URL by destroying the whole HTTP/2
    // connection, killing the new session's /send, its /events stream and the /signals
    // read whose failure was the visible banner. The body encoding has no such cliff.
    const ctx = client();
    const parents = Array.from({ length: 2000 }, (_, i) => `br_${i}`);
    await ctx.api.getSummary({ managerSessionIds: parents });

    expect(ctx.urls[0]!).toBe(`${BASE}/sessions/summary`);
    expect(bodyOf(ctx.requests[0]!)).toEqual({ manager_session_ids: parents });
  });

  it('composes with the id lookup and the axes rather than replacing them', async () => {
    // It is a LOOKUP, not a seventh chip — the server keeps it off `axes()` for the
    // same reason, so the two must be able to travel together.
    const ctx = client();
    await ctx.api.getSummary({
      managerSessionIds: ['br_parent'],
      sessionIds: ['br_a'],
      filter: { harness: ['claude_code'] },
      limit: 5,
    });
    expect(bodyOf(ctx.requests[0]!)).toEqual({
      manager_session_ids: ['br_parent'],
      session_ids: ['br_a'],
      harnesses: ['claude_code'],
      limit: 5,
    });
  });

  it('sends nothing at all when no parent is named, and stays a GET', async () => {
    // ⚠️ The server answers a PRESENT-but-empty id list with a 400, on purpose: it
    // means a caller assembled an empty list and would otherwise be handed the newest
    // hundred sessions on the box as "what this spawned". The client must therefore
    // omit the field rather than send it blank — and with no lookup in the request
    // there is nothing that outgrows a URL, so it keeps the GET encoding and the
    // conditional-GET caching that rides it.
    const ctx = client();
    await ctx.api.getSummary({ managerSessionIds: [] });
    expect(ctx.requests[0]!.init?.method ?? 'GET').toBe('GET');
    expect(ctx.urls[0]!).not.toContain('manager_session_id');
  });
});

describe('the order children are listed in', () => {
  it('puts running children above finished ones', () => {
    // What the list is opened to find. A parent with 106 children has maybe two you can
    // still act on; ordering purely by recency buries them under 104 finished ones.
    const running = summary({ sessionId: 'br_run', state: 'tool_running', updatedAt: '2026-08-01T00:00:00+00:00' });
    const finished = summary({ sessionId: 'br_done', state: 'idle', updatedAt: '2026-08-20T00:00:00+00:00' });

    expect([finished, running].sort(compareSubagents).map((s) => s.sessionId)).toEqual([
      'br_run',
      'br_done',
    ]);
  });

  it('orders the rest newest first', () => {
    const older = summary({ sessionId: 'br_old', updatedAt: '2026-08-01T00:00:00+00:00' });
    const newer = summary({ sessionId: 'br_new', updatedAt: '2026-08-20T00:00:00+00:00' });

    expect([older, newer].sort(compareSubagents).map((s) => s.sessionId)).toEqual([
      'br_new',
      'br_old',
    ]);
  });

  it('breaks a timestamp tie totally, so the order cannot shuffle between renders', () => {
    // The server writes updated_at to the nanosecond and ties still happen — 30 groups
    // of rows share one on the live table. A comparator returning 0 for them leaves the
    // order to the engine's stability and the rows can swap on a re-render.
    const a = summary({ sessionId: 'br_a' });
    const b = summary({ sessionId: 'br_b' });
    expect(compareSubagents(a, b)).not.toBe(0);
    expect([a, b].sort(compareSubagents)).toEqual([b, a].sort(compareSubagents));
  });

  it('does not try to sort on terminal state, which subagents do not report', () => {
    // ⚠️ Subagent sessions settle to `idle`, not `completed` — measured on this host,
    // 1,303 idle / 21 error / 1 completed out of 1,325. So "finished" and "never
    // started" are the SAME value, and a sort that leaned on it would order by nothing.
    // Recency is what separates them.
    const errored = summary({ sessionId: 'br_err', state: 'error', updatedAt: '2026-08-01T00:00:00+00:00' });
    const idle = summary({ sessionId: 'br_idle', state: 'idle', updatedAt: '2026-08-20T00:00:00+00:00' });

    // Neither is running, so recency decides — the errored one does NOT float.
    expect([errored, idle].sort(compareSubagents).map((s) => s.sessionId)).toEqual([
      'br_idle',
      'br_err',
    ]);
  });
});
