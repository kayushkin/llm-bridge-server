import { describe, expect, it } from 'vitest';
import { ReferenceStoreClient, isResolvableReferenceType } from '../src/net/ReferenceStoreClient.js';

function fakeFetch(answers: Record<string, unknown>, asked: string[]): typeof fetch {
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    asked.push(key);
    if (!(key in answers)) return new Response('no', { status: 404, statusText: 'Not Found' });
    return new Response(JSON.stringify(answers[key]), { status: 200 });
  }) as typeof fetch;
}

describe('ReferenceStoreClient', () => {
  it('reads the list and looks ids up under its base path', async () => {
    const asked: string[] = [];
    const client = new ReferenceStoreClient({
      basePath: '/api/bridge/reference-store/',
      fetch: fakeFetch(
        {
          'GET /api/bridge/reference-store/types': { types: [{ type: 'prediction', service: 'prediction-store', description: '', get: '/predictions/{id}', id_patterns: ['prediction_\\d{6,}'], access: 'grant' }, { type: 'git_repo', service: '', description: '', access: 'grant' }] },
          'POST /api/bridge/reference-store/resolve': { results: { prediction_000012: [{ type: 'prediction', service: 'prediction-store', refused: true }] } },
        },
        asked,
      ),
    });
    const types = await client.types();
    expect(types.filter(isResolvableReferenceType).map((t) => t.type)).toEqual(['prediction']);
    const answer = await client.resolve(['prediction_000012']);
    expect(answer.results.prediction_000012).toEqual([{ type: 'prediction', service: 'prediction-store', refused: true }]);
    expect(asked).toEqual(['GET /api/bridge/reference-store/types', 'POST /api/bridge/reference-store/resolve']);
  });

  it('throws on a refusal of the call, so a chip can say it could not ask', async () => {
    const client = new ReferenceStoreClient({ basePath: '/x', fetch: fakeFetch({}, []) });
    await expect(client.resolve(['prediction_000012'])).rejects.toThrow('POST /x/resolve failed: 404');
  });
});
