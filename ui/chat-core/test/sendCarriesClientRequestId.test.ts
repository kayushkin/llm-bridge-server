import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';

describe('sending a message', () => {
  it('sends the id the optimistic row was drawn under as client_request_id', async () => {
    const bodies: unknown[] = [];
    const fetchFn = (async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body)));
      return { ok: true, status: 200, statusText: '', json: async () => ({ status: 'sent', message_id: 'msg_1' }), text: async () => '' } as Response;
    }) as typeof fetch;
    const api = new ApiClient({ fetch: fetchFn, basePath: '/api/bridge' });
    await api.send('br_1', 'hello', 'c_1_abcd');
    await api.send('br_1', 'hello');
    expect(bodies).toEqual([{ message: 'hello', client_request_id: 'c_1_abcd' }, { message: 'hello' }]);
  });
});
