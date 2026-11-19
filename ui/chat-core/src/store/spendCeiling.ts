import type { ApiClient } from '../net/ApiClient.js';
import type { ChatStoreApi } from './ChatStore.js';

/**
 * Set a session's spend ceiling, then read the session back so the cache holds the
 * server's numbers.
 *
 * `maxBudgetUSD` is dollars; 0 removes the ceiling. bridge-server refuses a negative
 * one with a 400, and this does not pre-check it: the server is the judge.
 *
 * POST `/sessions/{id}/config` with `max_budget` writes the ceiling to the session row,
 * and the gate reads that row on every send. It works on any session, halted or not,
 * running or not.
 *
 * The read-back is the point. `GET /sessions/{id}` is the only place the cached
 * `spendUsd` / `maxBudgetUsd` pair comes from, and nothing streams a change to it, so
 * without the read the header would show the old ceiling until a reload. The halt, if
 * any, is cleared only when the read-back says the session is now under its ceiling or
 * has none — a ceiling set at or below the spend leaves the session halted, and saying
 * otherwise would be a lie the next send exposes.
 *
 * LOUD: a refused write or a failed read rethrows, and the cache is left as it was.
 */
export async function setSessionSpendCeiling(
  deps: { store: ChatStoreApi; api: ApiClient },
  sessionId: string,
  maxBudgetUSD: number,
): Promise<void> {
  await deps.api.setConfig(sessionId, { maxBudget: maxBudgetUSD });
  await recheckSessionBudgetHalt(deps, sessionId);
}

/**
 * Read the session back, cache it, and lift its halt if it is now under its ceiling
 * or has none.
 *
 * For a ceiling that something other than this client moved. The producer's home
 * session is the case: its ceiling follows the producer's weekly limit, and the
 * producer writes it to bridge-server itself, so the banner changes the limit and
 * then asks bridge-server what the ceiling became.
 *
 * LOUD: a failed read rethrows, and the cache is left as it was.
 */
export async function recheckSessionBudgetHalt(
  deps: { store: ChatStoreApi; api: ApiClient },
  sessionId: string,
): Promise<void> {
  const { store, api } = deps;
  const detail = await api.getSessionDetail(sessionId);
  const { actions } = store.getState();
  actions.setSessionDetail(sessionId, detail);
  const ceiling = detail.maxBudgetUsd ?? 0;
  const spend = detail.spendUsd ?? 0;
  if (ceiling <= 0 || spend < ceiling) actions.clearBudgetHalt(sessionId);
}
