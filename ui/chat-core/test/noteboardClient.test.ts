import { describe, expect, it } from 'vitest';
import { NoteboardClient, type NoteboardItem } from '../src/net/NoteboardClient.js';
import { ApiError } from '../src/net/ApiClient.js';

const UUID = '0d195aec-5ad0-4399-9b5d-75fe03262145';

function item(over: Partial<NoteboardItem> = {}): NoteboardItem {
  return {
    id: UUID,
    type: 'todo',
    title: 'Call plumber',
    body: '',
    tags: [],
    priority: 0,
    rank: 0,
    status: 'open',
    list_id: '',
    links: [],
    created_by: '',
    created_at: '2026-08-09T22:00:05Z',
    updated_at: '2026-08-09T22:00:05Z',
    ...over,
  };
}

/** Records the urls it was called with and answers with a canned response. */
function fakeFetch(
  responder: (url: string) => { status: number; body: unknown },
): { fn: typeof fetch; urls: string[] } {
  const urls: string[] = [];
  const fn = ((url: string) => {
    urls.push(url);
    const { status, body } = responder(url);
    return Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      statusText: status === 404 ? 'Not Found' : 'OK',
      json: () => Promise.resolve(body),
      text: () => Promise.resolve(typeof body === 'string' ? body : JSON.stringify(body)),
    } as Response);
  }) as unknown as typeof fetch;
  return { fn, urls };
}

describe('NoteboardClient.getItem', () => {
  it('builds the path under the configured base and encodes the id', () => {
    const { fn, urls } = fakeFetch(() => ({ status: 200, body: item() }));
    const client = new NoteboardClient({ fetch: fn, basePath: '/api/noteboard' });
    return client.getItem(UUID).then(() => {
      expect(urls).toEqual([`/api/noteboard/api/items/${UUID}`]);
    });
  });

  it('strips a trailing slash from the base rather than doubling it', () => {
    const { fn, urls } = fakeFetch(() => ({ status: 200, body: item() }));
    const client = new NoteboardClient({ fetch: fn, basePath: '/api/noteboard/' });
    return client.getItem(UUID).then(() => {
      expect(urls[0]).toBe(`/api/noteboard/api/items/${UUID}`);
    });
  });

  it('returns the row verbatim, including fields the chip does not read', () => {
    const row = item({ type: 'note', body: '## Steps', tags: ['ops'], priority: 2 });
    const { fn } = fakeFetch(() => ({ status: 200, body: row }));
    const client = new NoteboardClient({ fetch: fn, basePath: '/api/noteboard' });
    return client.getItem(UUID).then((got) => {
      expect(got).toEqual(row);
    });
  });

  it('resolves a deleted item instead of failing', () => {
    // noteboard's delete is reversible and GET-by-id still answers, so the panel
    // can say "deleted" — which is the useful answer for an id quoted in an old
    // message, and better than an error that reads as "no such item".
    const { fn } = fakeFetch(() => ({
      status: 200,
      body: item({ deleted_at: '2026-08-09T22:30:00Z' }),
    }));
    const client = new NoteboardClient({ fetch: fn, basePath: '/api/noteboard' });
    return client.getItem(UUID).then((got) => {
      expect(got.deleted_at).toBe('2026-08-09T22:30:00Z');
      expect(got.status).toBe('open');
    });
  });

  it('throws a loud ApiError on a non-2xx, carrying the status and body', () => {
    const { fn } = fakeFetch(() => ({ status: 404, body: 'item not found' }));
    const client = new NoteboardClient({ fetch: fn, basePath: '/api/noteboard' });
    return client.getItem(UUID).then(
      () => {
        throw new Error('expected getItem to reject');
      },
      (e: unknown) => {
        expect(e).toBeInstanceOf(ApiError);
        const err = e as ApiError;
        expect(err.status).toBe(404);
        expect(err.body).toBe('item not found');
        expect(err.method).toBe('GET');
        expect(err.message).toContain('404');
      },
    );
  });
});
