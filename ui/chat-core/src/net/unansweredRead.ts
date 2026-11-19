import { ApiError } from './ApiClient.js';

// A read this host's server never answered, told apart from one it answered
// with a refusal, and the retries the first kind gets.
//
// A phone on mobile data drops a request now and then, and a deploy restarts a
// service for a few seconds; both mend within seconds. So a read that got no
// answer is tried again after 1, 2 and 4 seconds, and only when every try
// fails does a surface hear of it. A read the server answered — a refusal, an
// unreadable body, or a JSON 503 in which the server reports the state of
// something it watches — is reported at once and never retried here: calls to
// other machines are the server's to pace, never the browser's.

/** How soon to ask again after a read that got no answer, one delay per retry. */
export const UNANSWERED_READ_RETRY_DELAYS_MS: readonly number[] = [1000, 2000, 4000];

/** How often a surface that has reported "no answer" asks again on its own. */
export const UNANSWERED_READ_RECHECK_MS = 30_000;

/** `unanswered`: this host's server never answered. `refused`: it answered. */
export type ReadFailure = 'unanswered' | 'refused';

const GATEWAY_STATUSES: ReadonlySet<number> = new Set([502, 503, 504]);

function isJSON(text: string): boolean {
  try {
    JSON.parse(text);
    return true;
  } catch {
    return false;
  }
}

/**
 * Which kind of failure `err` is. A fetch that throws a `TypeError` never got
 * an answer. An `ApiError` with a gateway status got one from the web server in
 * front of the bridge, not from the bridge, unless its body is JSON: every
 * service here answers its errors in JSON, and the web server's own error page
 * is HTML or empty. Anything else — a refusal, a body of the wrong shape — was
 * answered.
 */
export function readFailureOf(err: unknown): ReadFailure {
  if (err instanceof ApiError) {
    return GATEWAY_STATUSES.has(err.status) && !isJSON(err.body) ? 'unanswered' : 'refused';
  }
  if (err instanceof TypeError) return 'unanswered';
  return 'refused';
}

/** Thrown once every retry of an unanswered read has failed. */
export class UnansweredReadError extends Error {
  override readonly name = 'UnansweredReadError';
  /** When the first try failed. */
  readonly since: Date;
  readonly lastError: unknown;

  constructor(since: Date, lastError: unknown) {
    const detail = lastError instanceof Error ? lastError.message : String(lastError);
    const time = since.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' });
    super(`no answer since ${time}, still trying (${detail})`);
    this.since = since;
    this.lastError = lastError;
  }
}

/**
 * Run `read`, trying again after each delay while it gets no answer. Resolves
 * with the first answer. Rejects at once with a refusal, or with an
 * `UnansweredReadError` once the last retry has failed too.
 */
export async function readRetryingUnanswered<T>(
  read: () => Promise<T>,
  delaysMs: readonly number[] = UNANSWERED_READ_RETRY_DELAYS_MS,
  wait: (ms: number) => Promise<void> = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
): Promise<T> {
  let since: Date | null = null;
  for (let attempt = 0; ; attempt++) {
    try {
      return await read();
    } catch (err) {
      if (readFailureOf(err) === 'refused') throw err;
      since ??= new Date();
      const delay = delaysMs[attempt];
      if (delay === undefined) throw new UnansweredReadError(since, err);
      await wait(delay);
    }
  }
}
