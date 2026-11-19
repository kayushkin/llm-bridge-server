import { describe, expect, it, beforeEach } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';

// The id lookup on `/sessions/summary`, and why it is not a filter chip.
//
// Every other surface in this package already holds a `SessionSummary` — a sidebar row
// renders one, a header renders one — so none of them ever needed to turn an id into a
// name. The cross-session signals inbox is the first that does: a signal carries
// `session_id` and nothing else.
//
// Paging cannot answer it. The sidebar loads a newest-first prefix, and a session that
// asked something and then went quiet sinks out of that prefix while its question stays
// open — which is exactly the case the inbox exists for. Measured on this host: of the
// 17 sessions holding an open chat signal, 11 were nowhere in the sidebar's first page.

const BASE = '/api/bridge';

interface Call {
  url: string;
  init?: RequestInit;
}

function client(): { api: ApiClient; calls: Call[] } {
  const calls: Call[] = [];
  const fetchFn = async (url: string, init?: RequestInit): Promise<Response> => {
    calls.push({ url: String(url), init });
    return {
      ok: true,
      status: 200,
      statusText: '200',
      json: async () => ({ sessions: [], next: null, revision: 'r' }),
      text: async () => '{}',
    } as unknown as Response;
  };
  return { api: new ApiClient({ fetch: fetchFn as unknown as typeof fetch, basePath: BASE }), calls };
}

describe('looking sessions up by id', () => {
  let ctx: ReturnType<typeof client>;
  beforeEach(() => {
    ctx = client();
  });

  it('sends every id in one POST body, in one request', async () => {
    // A body, never the URL: this list is unbounded — the inbox may name every session
    // holding a signal — and the query-string encoding of the sibling lookup reached
    // 93 KB on the real sidebar, which nginx answers by destroying the whole HTTP/2
    // connection. See subagents.test.ts for the full regression pin.
    await ctx.api.getSummary({ sessionIds: ['br_a', 'br_b', 'br_c'] });

    expect(ctx.calls).toHaveLength(1);
    expect(ctx.calls[0]!.init?.method).toBe('POST');
    expect(ctx.calls[0]!.url).toBe(`${BASE}/sessions/summary`);
    expect(JSON.parse(String(ctx.calls[0]!.init?.body))).toEqual({
      session_ids: ['br_a', 'br_b', 'br_c'],
    });
  });

  it('is not a filter axis — it composes with them and with paging', async () => {
    await ctx.api.getSummary({
      sessionIds: ['br_a'],
      filter: { harness: ['claude_code'] },
      limit: 1,
    });
    expect(JSON.parse(String(ctx.calls[0]!.init?.body))).toEqual({
      session_ids: ['br_a'],
      harnesses: ['claude_code'],
      limit: 1,
    });
  });

  it('sends nothing at all when no ids are asked for, and stays a GET', async () => {
    // An empty id list would narrow to nothing on a server that treats
    // present-but-empty as a constraint, turning an ordinary sidebar page into a blank
    // one. Absent has to stay absent — and with no lookup there is nothing that
    // outgrows a URL, so the request keeps the GET encoding and its conditional-GET
    // caching.
    await ctx.api.getSummary({ limit: 20 });
    expect(ctx.calls[0]!.init?.method ?? 'GET').toBe('GET');
    expect(ctx.calls[0]!.url).not.toContain('session_id');

    await ctx.api.getSummary({ sessionIds: [], limit: 20 });
    expect(ctx.calls[1]!.init?.method ?? 'GET').toBe('GET');
    expect(ctx.calls[1]!.url).not.toContain('session_id');
  });
});
