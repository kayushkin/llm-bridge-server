import { ApiError } from '../net/ApiClient.js';
import type { WireEvent } from '../net/wireEvents.js';

/** The error code llm-bridge stamps on both halves of the spend gate.
 *
 *  Single source of truth: `msg.ErrCodeBudgetExceeded` in `llm-bridge/msg/event.go`,
 *  mirrored in `llm-bridge/ts/msg.ts`. It appears on the 402 refusal body AND on the
 *  mid-turn error event, which is why one constant serves both readers below. */
export const ERR_CODE_BUDGET_EXCEEDED = 'budget_exceeded';

/** A session stopped at its spend ceiling.
 *
 *  llm-bridge-server halts a session two ways and this record covers both:
 *
 *  - **mid-turn** — the gate interrupts a running turn and persists an error event
 *    carrying `ErrCodeBudgetExceeded`. That event has a sentence and no numbers.
 *  - **between turns** — every later send, resume and mode switch is refused with a
 *    402 whose JSON body names both dollar figures
 *    (`internal/server/sessions.go` `writeRefusalIfOverBudget`).
 *
 *  `spendUSD` and `maxBudgetUSD` are therefore optional: only the 402 half carries
 *  them. A halt raised by the error event leaves them undefined and the surface falls
 *  back to `message` — the server's own words — rather than inventing a figure. */
export interface BudgetHalt {
  /** The session the halt belongs to. A halt never outlives its own session. */
  sessionId: string;
  /** The server's own description of the halt. Always present. */
  message: string;
  /** Spend recorded against the ceiling at the moment of refusal, in USD.
   *  Undefined when the halt came from the mid-turn error event. */
  spendUSD?: number;
  /** The ceiling that was breached, in USD. Undefined when the halt came from the
   *  mid-turn error event. */
  maxBudgetUSD?: number;
}

/** The refusal body's shape. Declared rather than asserted so every field below is
 *  checked before it is read. */
interface RefusalBody {
  error?: {
    code?: unknown;
    message?: unknown;
    spend_usd?: unknown;
    max_budget_usd?: unknown;
  };
}

/** Read a spend halt out of a thrown request error, or report null.
 *
 *  Reports null for everything that is not this specific refusal: a different error
 *  type, a non-402 status, a 402 that is not a budget refusal, a body that does not
 *  parse. The caller then falls through to its ordinary error path, so a refusal shape
 *  this code does not recognize still reaches the user as text instead of being folded
 *  into a banner that would describe it wrongly.
 *
 *  Takes the thrown value rather than a (status, body) pair so a caller can hand it
 *  whatever it caught without first having to know what it caught. */
export function budgetHaltFromRefusal(sessionId: string, thrown: unknown): BudgetHalt | null {
  if (!(thrown instanceof ApiError)) return null;
  if (thrown.status !== 402) return null;
  let parsed: RefusalBody;
  try {
    parsed = JSON.parse(thrown.body) as RefusalBody;
  } catch {
    return null;
  }
  const err = parsed?.error;
  if (!err || err.code !== ERR_CODE_BUDGET_EXCEEDED) return null;
  return {
    sessionId,
    message:
      typeof err.message === 'string' && err.message
        ? err.message
        : 'this session has reached its spend ceiling',
    ...(typeof err.spend_usd === 'number' ? { spendUSD: err.spend_usd } : {}),
    ...(typeof err.max_budget_usd === 'number' ? { maxBudgetUSD: err.max_budget_usd } : {}),
  };
}

/** Read a spend halt out of a live session event, or report null.
 *
 *  This is the mid-turn half: the gate interrupted a running turn, so no request was
 *  refused and nothing threw — the only trace is an error event on the session stream.
 *  It carries a sentence and no dollar figures, so the halt this builds has none
 *  either; the surface says what the server said.
 *
 *  Reports null for every other event, which is nearly all of them. */
export function budgetHaltFromEvent(sessionId: string, event: WireEvent): BudgetHalt | null {
  const err = event.data?.error;
  if (!err || err.code !== ERR_CODE_BUDGET_EXCEEDED) return null;
  return {
    sessionId,
    message: err.message || 'this session has reached its spend ceiling',
  };
}
