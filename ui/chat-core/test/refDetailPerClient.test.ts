import { afterEach, describe, expect, it } from 'vitest';
import { clearRefDetailCache, loadSessionRefDetail } from '../src/index.js';
import type { ManagedSessionDetail } from '../src/index.js';

// Two stores on one page — this server's and a connected server's — number
// their sessions independently. A chip under one must never be answered from
// what the other's client loaded for the same id.

function clientAnswering(name: string) {
  const calls: string[] = [];
  return {
    calls,
    getSessionDetail: async (sessionId: string) => {
      calls.push(sessionId);
      return { summary: { sessionId, displayName: `${name} ${sessionId}` } } as unknown as ManagedSessionDetail;
    },
  };
}

describe('reference detail caches are per client', () => {
  afterEach(() => clearRefDetailCache());

  it('loads the same id once per client and never hands one client the other answer', async () => {
    const here = clientAnswering('here');
    const there = clientAnswering('there');
    const fromHere = await loadSessionRefDetail(here, 'br_1');
    const fromThere = await loadSessionRefDetail(there, 'br_1');
    expect(fromHere.summary.displayName).toBe('here br_1');
    expect(fromThere.summary.displayName).toBe('there br_1');
    expect(here.calls).toEqual(['br_1']);
    expect(there.calls).toEqual(['br_1']);
  });

  it('still shares one flight for repeated chips under one client', async () => {
    const here = clientAnswering('here');
    await Promise.all([loadSessionRefDetail(here, 'br_2'), loadSessionRefDetail(here, 'br_2')]);
    expect(here.calls).toEqual(['br_2']);
  });
});
