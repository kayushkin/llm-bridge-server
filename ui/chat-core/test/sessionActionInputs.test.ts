import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';

/** A confirm carries what the person typed into the button's boxes, and nothing when
 *  the button has none. */
describe('runSessionAction', () => {
  const recordingClient = () => {
    const bodies: string[] = [];
    const fetchFn = (async (_url: string, init?: RequestInit) => {
      bodies.push(String(init?.body ?? ''));
      return new Response(JSON.stringify({ action_id: 'session_action_000001', state: 'running' }), {
        status: 202,
        headers: { 'Content-Type': 'application/json' },
      });
    }) as typeof fetch;
    return { api: new ApiClient({ fetch: fetchFn, basePath: '/api/bridge' }), bodies };
  };

  it('sends the typed values under inputs', async () => {
    const { api, bodies } = recordingClient();
    await api.runSessionAction('br_1', 'session_action_000001', { PASSWORD: 'hunter22' });
    expect(JSON.parse(bodies[0])).toEqual({ inputs: { PASSWORD: 'hunter22' } });
  });

  it('sends an empty body for a button with no boxes', async () => {
    const { api, bodies } = recordingClient();
    await api.runSessionAction('br_1', 'session_action_000001');
    expect(JSON.parse(bodies[0])).toEqual({});
  });
});
