import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { openLooseEnds, sessionBriefChanges, type SessionBriefViewWire } from '../src/net/sessionBriefs.js';

const BASE = '/api/bridge';

interface Call {
  url: string;
  method: string;
  body: unknown;
}

function respond(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: async () => body,
    text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
  } as unknown as Response;
}


/** A fetch that routes on the URL and records every call, so a test can assert
 *  BOTH what went on the wire and what did NOT (a decline must never fetch the
 *  pending hooks; a derived answer must never touch the resolve route). */
function recordingFetch(
  route: (url: string) => Response | undefined,
): { fetch: typeof fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fn = async (url: string, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? 'GET';
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
    calls.push({ url: String(url), method, body });
    const res = route(String(url));
    if (!res) throw new Error(`no route for ${method} ${String(url)}`);
    return res;
  };
  return { fetch: fn as unknown as typeof fetch, calls };
}

function client(route: (url: string) => Response | undefined): { api: ApiClient; calls: Call[] } {
  const { fetch, calls } = recordingFetch(route);
  return { api: new ApiClient({ fetch, basePath: BASE }), calls };
}

const view: SessionBriefViewWire = {
  brief: {
    session_id: 'br_a',
    overall_goal: 'Run the nightly backup at 02:00',
    current_step: 'Waiting for a disk choice',
    revision_count: 3,
    created_at: '2026-11-02T18:00:00Z',
    updated_at: '2026-11-02T19:00:00Z',
  },
  revisions: [
    { id: 'sbrev_1', session_id: 'br_a', overall_goal: 'g', current_step: 's', goal_changed: false, turn_change: 'job 81 moved', created_at: '2026-11-02T18:00:00Z' },
    { id: 'sbrev_2', session_id: 'br_a', overall_goal: 'g', current_step: 's', goal_changed: false, created_at: '2026-11-02T18:30:00Z' },
    { id: 'sbrev_3', session_id: 'br_a', overall_goal: 'g2', current_step: 's', goal_changed: true, created_at: '2026-11-02T19:00:00Z' },
  ],
  loose_ends: [
    { id: 'loose_1', session_id: 'br_a', kind: 'offer', text: 'look at the disk?', state: 'open', created_at: '2026-11-02T18:00:00Z' },
    { id: 'loose_2', session_id: 'br_a', kind: 'question', text: 'which port?', state: 'settled', resolution: 'picked 8080', created_at: '2026-11-02T18:00:00Z' },
  ],
};

describe('session briefs', () => {
  it('asks for open loose ends and maps the answer, dropping a view with no brief', async () => {
    const { api, calls } = client(() => respond(200, [view, { brief: null, revisions: [], loose_ends: [] }]));
    const briefs = await api.getSessionBriefs({ looseEndState: 'open' });
    expect(calls[0]!.url).toBe(`${BASE}/session-briefs?loose_end_state=open`);
    expect(briefs).toHaveLength(1);
    const brief = briefs![0]!;
    expect(brief.overallGoal).toBe('Run the nightly backup at 02:00');
    expect(openLooseEnds(brief).map((looseEnd) => looseEnd.id)).toEqual(['loose_1']);
    // Newest first, and the turn that only talked is left out.
    expect(sessionBriefChanges(brief).map((revision) => revision.id)).toEqual(['sbrev_3', 'sbrev_1']);
  });

  it('answers null on a server with no briefs route, and throws on anything else', async () => {
    await expect(client(() => respond(404, 'not found')).api.getSessionBriefs()).resolves.toBeNull();
    await expect(client(() => respond(500, 'boom')).api.getSessionBriefs()).rejects.toThrow(/500/);
  });

  it('answers and dismisses a loose end through its session', async () => {
    const { api, calls } = client(() => respond(200, {}));
    await api.answerLooseEnd('br_a', 'loose_1', 'yes, look');
    await api.dismissLooseEnd('br_a', 'loose_1');
    expect(calls.map((call) => [call.method, call.url, call.body])).toEqual([
      ['POST', `${BASE}/sessions/br_a/loose-ends/loose_1/answer`, { text: 'yes, look' }],
      ['POST', `${BASE}/sessions/br_a/loose-ends/loose_1/dismiss`, undefined],
    ]);
  });
});
