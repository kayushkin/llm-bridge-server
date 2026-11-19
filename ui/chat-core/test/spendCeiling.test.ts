import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore } from '../src/store/ChatStore.js';
import { recheckSessionBudgetHalt, setSessionSpendCeiling } from '../src/store/spendCeiling.js';

// The header's ceiling field calls this on a session that may or may not be halted.
// What has to hold: the cache ends up with the server's read-back (nothing streams a
// ceiling change, so without it the chip keeps the old figure), and a halt is lifted
// only when that read-back says the session is under its ceiling or has none.

interface Seen { method: string; url: string; body?: unknown }

function fakeServer(
  readBack: { spend_usd: number; max_budget_usd: number },
  configStatus = 200,
  seen: Seen[] = [],
): ApiClient {
  const fetchFn = (async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    seen.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const isConfig = method === 'POST' && url.endsWith('/config');
    const status = isConfig ? configStatus : 200;
    return {
      ok: status < 300,
      status,
      statusText: '',
      json: async () =>
        isConfig
          ? {}
          : {
              session_id: 'br_1',
              state: 'idle',
              harness: 'claude_code',
              updated_at: '2026-10-15T00:00:00Z',
              created_at: '2026-10-15T00:00:00Z',
              ...readBack,
            },
      text: async () => (status < 300 ? '' : 'invalid max_budget'),
    } as unknown as Response;
  }) as unknown as typeof fetch;
  return new ApiClient({ fetch: fetchFn, basePath: '/api/bridge' });
}

function haltedStore() {
  const store = createChatStore();
  store.getState().actions.setBudgetHalt({ sessionId: 'br_1', message: 'halted', spendUSD: 10, maxBudgetUSD: 10 });
  return store;
}

describe('setSessionSpendCeiling', () => {
  it('posts max_budget, then caches the read-back', async () => {
    const seen: Seen[] = [];
    const store = createChatStore();
    await setSessionSpendCeiling({ store, api: fakeServer({ spend_usd: 3, max_budget_usd: 40 }, 200, seen) }, 'br_1', 40);
    expect(seen[0]).toEqual({ method: 'POST', url: '/api/bridge/sessions/br_1/config', body: { max_budget: 40 } });
    expect(seen[1]!.url).toBe('/api/bridge/sessions/br_1');
    const detail = store.getState().sessionDetail.get('br_1');
    expect(detail?.maxBudgetUsd).toBe(40);
    expect(detail?.spendUsd).toBe(3);
  });

  it('lifts a halt once the session is under its new ceiling', async () => {
    const store = haltedStore();
    await setSessionSpendCeiling({ store, api: fakeServer({ spend_usd: 10, max_budget_usd: 25 }) }, 'br_1', 25);
    expect(store.getState().budgetHalts.has('br_1')).toBe(false);
  });

  it('lifts a halt when the ceiling is cleared with 0', async () => {
    const store = haltedStore();
    await setSessionSpendCeiling({ store, api: fakeServer({ spend_usd: 10, max_budget_usd: 0 }) }, 'br_1', 0);
    expect(store.getState().budgetHalts.has('br_1')).toBe(false);
  });

  it('keeps the halt when the new ceiling is still at or below the spend', async () => {
    const store = haltedStore();
    await setSessionSpendCeiling({ store, api: fakeServer({ spend_usd: 10, max_budget_usd: 8 }) }, 'br_1', 8);
    expect(store.getState().budgetHalts.has('br_1')).toBe(true);
  });

  it('rethrows a refused write and leaves the cache and the halt alone', async () => {
    const store = haltedStore();
    await expect(
      setSessionSpendCeiling({ store, api: fakeServer({ spend_usd: 10, max_budget_usd: 10 }, 400) }, 'br_1', -1),
    ).rejects.toThrow();
    expect(store.getState().sessionDetail.has('br_1')).toBe(false);
    expect(store.getState().budgetHalts.has('br_1')).toBe(true);
  });
});

describe('recheckSessionBudgetHalt', () => {
  it('writes nothing, caches the read-back, and lifts a halt the server says is over', async () => {
    const seen: Seen[] = [];
    const store = haltedStore();
    await recheckSessionBudgetHalt({ store, api: fakeServer({ spend_usd: 10, max_budget_usd: 160 }, 200, seen) }, 'br_1');
    expect(seen.map((s) => s.method)).toEqual(['GET']);
    expect(store.getState().sessionDetail.get('br_1')?.maxBudgetUsd).toBe(160);
    expect(store.getState().budgetHalts.has('br_1')).toBe(false);
  });

  it('keeps the halt when the ceiling another service set is still at the spend', async () => {
    const store = haltedStore();
    await recheckSessionBudgetHalt({ store, api: fakeServer({ spend_usd: 10, max_budget_usd: 10 }) }, 'br_1');
    expect(store.getState().budgetHalts.has('br_1')).toBe(true);
  });
});
