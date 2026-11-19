import { describe, expect, it } from 'vitest';
import { ApiClient, ApiError } from '../src/net/ApiClient.js';
import { budgetHaltFromEvent, budgetHaltFromRefusal } from '../src/store/budgetHalt.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { WireEvent } from '../src/net/wireEvents.js';

// llm-bridge-server halts a session that has spent its ceiling and then refuses every
// send, resume and mode switch with a 402 (`writeRefusalIfOverBudget`,
// internal/server/sessions.go). the chat page showed none of it: `useComposer.send` had a
// `.catch(() => {})`, so the one refusal that tells the user exactly what to do about
// it was discarded before anything could read it.
//
// Two things have to hold for the banner to be trustworthy, and both are the reason
// these tests exist rather than a snapshot of the banner:
//
//  1. The refusal reader must be NARROW. Every other 402, and every unparseable body,
//     has to fall through to the ordinary error path — a banner that describes a
//     refusal wrongly is worse than raw text.
//  2. The error carrying the refusal has to keep the status and the body as fields.
//     Picking the JSON back out of the English message string is guessing.

interface FakeRes {
  ok: boolean;
  status: number;
  statusText: string;
  jsonBody?: unknown;
  textBody?: string;
}

function fakeFetch(res: FakeRes): typeof fetch {
  return (async () =>
    ({
      ok: res.ok,
      status: res.status,
      statusText: res.statusText,
      json: async () => res.jsonBody ?? {},
      text: async () => res.textBody ?? '',
    }) as unknown as Response) as unknown as typeof fetch;
}

const REFUSAL_BODY = JSON.stringify({
  error: {
    code: 'budget_exceeded',
    message: 'session has spent $5.25 of its $5.00 ceiling; raise max_budget to continue',
    spend_usd: 5.25,
    max_budget_usd: 5.0,
  },
});

function refusal(status: number, body: string): ApiError {
  return new ApiError({
    message: `POST /sessions/br_1/send failed: ${status} x ${body}`,
    status,
    body,
    method: 'POST',
    path: '/sessions/br_1/send',
  });
}

describe('ApiError carries the status and the body', () => {
  it('throws an ApiError whose status and body are readable as fields', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: false,
        status: 402,
        statusText: 'Payment Required',
        textBody: REFUSAL_BODY,
      }),
      basePath: '/api/bridge',
    });
    const thrown = await api.send('br_1', 'hello').then(
      () => null,
      (e: unknown) => e,
    );
    expect(thrown).toBeInstanceOf(ApiError);
    const err = thrown as ApiError;
    expect(err.status).toBe(402);
    expect(err.body).toBe(REFUSAL_BODY);
    expect(err.method).toBe('POST');
    expect(err.path).toBe('/sessions/br_1/send');
  });

  it('keeps the message text a non-2xx always had, so existing readers are unchanged', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 409, statusText: 'Conflict', textBody: 'already running' }),
      basePath: '/api/bridge',
    });
    await expect(api.resume('br_1')).rejects.toThrow(
      'POST /sessions/br_1/resume failed: 409 Conflict already running',
    );
  });
});

describe('budgetHaltFromRefusal', () => {
  it('reads both dollar figures out of the 402', () => {
    const halt = budgetHaltFromRefusal('br_1', refusal(402, REFUSAL_BODY));
    expect(halt).toEqual({
      sessionId: 'br_1',
      message: 'session has spent $5.25 of its $5.00 ceiling; raise max_budget to continue',
      spendUSD: 5.25,
      maxBudgetUSD: 5.0,
    });
  });

  it('reports null for a 402 that is not a budget refusal', () => {
    const body = JSON.stringify({ error: { code: 'quota_exhausted', message: 'no credit' } });
    expect(budgetHaltFromRefusal('br_1', refusal(402, body))).toBeNull();
  });

  it('reports null for a body that does not parse', () => {
    expect(budgetHaltFromRefusal('br_1', refusal(402, '<html>502 Bad Gateway</html>'))).toBeNull();
  });

  it('reports null for every status but 402', () => {
    expect(budgetHaltFromRefusal('br_1', refusal(409, REFUSAL_BODY))).toBeNull();
    expect(budgetHaltFromRefusal('br_1', refusal(500, REFUSAL_BODY))).toBeNull();
  });

  it('reports null for a thrown value that is not an ApiError', () => {
    expect(budgetHaltFromRefusal('br_1', new Error('network down'))).toBeNull();
    expect(budgetHaltFromRefusal('br_1', 'network down')).toBeNull();
    expect(budgetHaltFromRefusal('br_1', null)).toBeNull();
  });

  // The two tests below exist because `toBeNull()` names a value, not a reason, and this
  // function has four separate producers of that one null. Every other test here happens to
  // trip exactly one of them, so each of those pins its own guard. These two inputs are the
  // ones where a *different* guard was silently doing the work:
  //
  //   - a 402 whose body parses to something with no `error` at all was reaching the
  //     `err.code !== ...` comparison, so the `!err` half of that guard was unpinned;
  //   - a thrown value that merely looks like an ApiError was being rejected by the status
  //     read crashing on it, not by the type check, so `instanceof ApiError` was unpinned
  //     the moment the status read became defensive.
  //
  // Both were measured by mutation, not guessed: deleting either half left the whole suite
  // green. See `budgetHaltFromEvent`'s 'ignores an event with no error at all' for the same
  // guard on the other half of the gate, which was pinned all along.

  it('reports null for a 402 whose body parses but carries no error object', () => {
    // Reaches the `!err` half of the guard specifically: the code comparison cannot fire
    // here, because there is no code to compare.
    expect(budgetHaltFromRefusal('br_1', refusal(402, '{}'))).toBeNull();
    expect(budgetHaltFromRefusal('br_1', refusal(402, '{"error":null}'))).toBeNull();
    expect(budgetHaltFromRefusal('br_1', refusal(402, 'null'))).toBeNull();
  });

  it('reports null for a thrown value that only looks like an ApiError', () => {
    // Requirement 1 at the top of this file — the reader must be NARROW — is about the type,
    // not about the shape. A bare object carrying the right status and a real refusal body
    // is the case that tells those two apart.
    const lookalike = { status: 402, body: REFUSAL_BODY, method: 'POST', path: '/x' };
    expect(budgetHaltFromRefusal('br_1', lookalike)).toBeNull();
  });

  it('leaves the figures undefined when the body names the code and no numbers', () => {
    const body = JSON.stringify({ error: { code: 'budget_exceeded', message: 'over ceiling' } });
    const halt = budgetHaltFromRefusal('br_1', refusal(402, body));
    expect(halt).toEqual({ sessionId: 'br_1', message: 'over ceiling' });
    expect(halt && 'spendUSD' in halt).toBe(false);
  });
});

describe('budgetHaltFromEvent — the mid-turn half', () => {
  const errorEvent = (code: string, message: string): WireEvent => ({
    type: 'error',
    data: { type: 'error', error: { code, message } },
  });

  it('records a halt from the budget_exceeded error event, with no figures', () => {
    const halt = budgetHaltFromEvent('br_1', errorEvent('budget_exceeded', 'stopped at its ceiling'));
    expect(halt).toEqual({ sessionId: 'br_1', message: 'stopped at its ceiling' });
  });

  it('ignores every other error event', () => {
    expect(budgetHaltFromEvent('br_1', errorEvent('api_error', 'overloaded'))).toBeNull();
    expect(budgetHaltFromEvent('br_1', errorEvent('PROCESS_DIED', 'harness exited'))).toBeNull();
  });

  it('ignores an event with no error at all', () => {
    expect(budgetHaltFromEvent('br_1', { type: 'user_message', data: { type: 'user_message' } })).toBeNull();
  });
});

describe('the store keeps a halt per session', () => {
  it('folds a budget_exceeded event off the live stream into budgetHalts', () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('br_1', {
      type: 'error',
      data: { type: 'error', event_id: 7, error: { code: 'budget_exceeded', message: 'over' } },
    });
    expect(store.getState().budgetHalts.get('br_1')?.message).toBe('over');
    // A second session is untouched — a halt belongs to the session that hit it.
    expect(store.getState().budgetHalts.get('br_2')).toBeUndefined();
  });

  it('replaces a halt rather than keeping the first, so raising and re-breaching re-seeds', () => {
    const store = createChatStore();
    const { setBudgetHalt } = store.getState().actions;
    setBudgetHalt({ sessionId: 'br_1', message: 'first', spendUSD: 5.25, maxBudgetUSD: 5 });
    setBudgetHalt({ sessionId: 'br_1', message: 'second', spendUSD: 10.4, maxBudgetUSD: 10 });
    expect(store.getState().budgetHalts.get('br_1')).toEqual({
      sessionId: 'br_1',
      message: 'second',
      spendUSD: 10.4,
      maxBudgetUSD: 10,
    });
  });

  it('clears a halt, and clearing an unhalted session changes nothing', () => {
    const store = createChatStore();
    const { setBudgetHalt, clearBudgetHalt } = store.getState().actions;
    setBudgetHalt({ sessionId: 'br_1', message: 'over' });
    const before = store.getState().budgetHalts;
    clearBudgetHalt('br_2');
    expect(store.getState().budgetHalts).toBe(before); // same reference: no re-render
    clearBudgetHalt('br_1');
    expect(store.getState().budgetHalts.size).toBe(0);
  });
});

describe('dropOptimisticUser — a refused send does not stay on screen', () => {
  it('removes the optimistic row and its turn', () => {
    const store = createChatStore();
    const { appendOptimisticUser, dropOptimisticUser } = store.getState().actions;
    appendOptimisticUser('br_1', 'this will be refused', 'c_1');
    expect(store.getState().turnsBySession.get('br_1')?.turns).toHaveLength(1);
    dropOptimisticUser('br_1', 'c_1');
    expect(store.getState().turnsBySession.get('br_1')?.turns).toHaveLength(0);
    expect(Object.keys(store.getState().turnsBySession.get('br_1')?.entries ?? {})).toHaveLength(0);
  });

  it('leaves the other pending rows alone', () => {
    const store = createChatStore();
    const { appendOptimisticUser, dropOptimisticUser } = store.getState().actions;
    appendOptimisticUser('br_1', 'first', 'c_1');
    appendOptimisticUser('br_1', 'second', 'c_2');
    dropOptimisticUser('br_1', 'c_1');
    const model = store.getState().turnsBySession.get('br_1');
    expect(model?.turns).toHaveLength(1);
    expect(Object.values(model?.entries ?? {})[0]?.text).toBe('second');
  });

  it('is a no-op for a client id that already reconciled', () => {
    const store = createChatStore();
    const { appendOptimisticUser, dropOptimisticUser } = store.getState().actions;
    appendOptimisticUser('br_1', 'landed', 'c_1');
    const before = store.getState().tails.get('br_1');
    dropOptimisticUser('br_1', 'c_never_existed');
    expect(store.getState().tails.get('br_1')).toBe(before);
  });
});
