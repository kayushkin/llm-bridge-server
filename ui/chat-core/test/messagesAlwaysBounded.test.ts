import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';

// GET /sessions/{id}/messages serves two different shapes off one path. With a
// `limit` or a `before` it answers the bounded `{ model }` TurnModel this client
// is typed for; with neither it answers the legacy unbounded array — every event
// in the session, measured at 306MB and 52s for one real session.
//
// So a request that carries no bound is not merely expensive, it comes back in a
// shape `MessagesResponse` does not describe, and `resp.model` reads as undefined
// with nothing raising. These pin that the client cannot produce that request.

function recordingClient() {
  const urls: string[] = [];
  const api = new ApiClient({
    basePath: '/api/bridge',
    fetch: (async (url: string) => {
      urls.push(String(url));
      return { ok: true, status: 200, json: async () => ({ model: {} }) };
    }) as unknown as typeof fetch,
  });
  return { api, urls };
}

describe('the messages request always carries a bound', () => {
  it('a caller naming no limit still asks for one', async () => {
    const { api, urls } = recordingClient();
    await api.getMessages('sess-a');
    expect(urls).toHaveLength(1);
    expect(new URL(urls[0], 'http://x').searchParams.get('limit'))
      .toBe(String(ApiClient.DEFAULT_MESSAGE_TURNS));
  });

  it('paging older still carries a limit alongside before', async () => {
    const { api, urls } = recordingClient();
    await api.getMessages('sess-a', { before: 4200 });
    const params = new URL(urls[0], 'http://x').searchParams;
    expect(params.get('before')).toBe('4200');
    expect(params.get('limit')).toBe(String(ApiClient.DEFAULT_MESSAGE_TURNS));
  });

  it('an explicit limit is the one sent', async () => {
    const { api, urls } = recordingClient();
    await api.getMessages('sess-a', { limit: 5 });
    expect(new URL(urls[0], 'http://x').searchParams.get('limit')).toBe('5');
  });

  it('no call shape reaches the unbounded path', async () => {
    const { api, urls } = recordingClient();
    await api.getMessages('sess-a');
    await api.getMessages('sess-a', {});
    await api.getMessages('sess-a', { before: 1 });
    await api.getMessages('sess-a', { limit: 30, before: 1 });
    // The bare path IS the unbounded shape. Asserted on the whole URL rather
    // than on "contains limit=", so appending a limit to a path that already
    // lost its query separator cannot pass.
    for (const url of urls) {
      expect(url.endsWith('/messages')).toBe(false);
      expect(url).toMatch(/\/messages\?[^?]*\blimit=\d+/);
    }
  });
});

describe('the reading page asks for tool payload previews', () => {
  it('every reading-page request carries payload=preview', async () => {
    const { api, urls } = recordingClient();
    await api.getMessages('sess-a');
    await api.getMessages('sess-a', { limit: 5, before: 9 });
    for (const url of urls) {
      expect(new URL(url, 'http://x').searchParams.get('payload')).toBe('preview');
    }
  });

  it('the raw page does not: it is the audit view of what was stored', async () => {
    const { api, urls } = recordingClient();
    await api.getMessagesRaw('sess-a');
    expect(new URL(urls[0], 'http://x').searchParams.get('payload')).toBeNull();
  });

  it('getEntry asks for one entry by its event id', async () => {
    const { api, urls } = recordingClient();
    await api.getEntry('br_1', 4242);
    expect(urls[0]).toBe('/api/bridge/sessions/br_1/entries/4242');
  });
});
