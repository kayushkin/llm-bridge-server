import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/net/ApiClient.js';
import { readFailureOf, readRetryingUnanswered, UnansweredReadError } from '../src/net/unansweredRead.js';

const apiError = (status: number, body: string) =>
  new ApiError({ message: `GET /signals failed: ${status}`, status, body, method: 'GET', path: '/signals' });

const noWait = async () => {};

describe('readFailureOf', () => {
  it('a fetch that threw got no answer', () => {
    expect(readFailureOf(new TypeError('Failed to fetch'))).toBe('unanswered');
  });
  it("the web server's own 502 page got no answer from the bridge", () => {
    expect(readFailureOf(apiError(502, '<html>Bad Gateway</html>'))).toBe('unanswered');
    expect(readFailureOf(apiError(504, ''))).toBe('unanswered');
  });
  it("the bridge's JSON 503 is its report of state, not a missing answer", () => {
    expect(readFailureOf(apiError(503, '{"error":{"code":"peer_not_connected"}}'))).toBe('refused');
  });
  it('a refusal and a body of the wrong shape were answered', () => {
    expect(readFailureOf(apiError(403, 'forbidden'))).toBe('refused');
    expect(readFailureOf(new Error('GET /signals returned object, expected an array of signals'))).toBe('refused');
  });
});

describe('readRetryingUnanswered', () => {
  it('a dropped request is retried and its answer returned', async () => {
    let tries = 0;
    const result = await readRetryingUnanswered(async () => {
      tries++;
      if (tries === 1) throw new TypeError('Failed to fetch');
      return 'answer';
    }, [1, 1, 1], noWait);
    expect(result).toBe('answer');
    expect(tries).toBe(2);
  });

  it('once every retry fails it rejects with when the failures began', async () => {
    let tries = 0;
    const read = readRetryingUnanswered(async () => {
      tries++;
      throw new TypeError('Failed to fetch');
    }, [1, 1], noWait);
    await expect(read).rejects.toBeInstanceOf(UnansweredReadError);
    await expect(read).rejects.toThrow(/^no answer since .*, still trying \(Failed to fetch\)$/);
    expect(tries).toBe(3);
  });

  it('a refusal is thrown at once and not retried', async () => {
    let tries = 0;
    const refusal = apiError(503, '{"error":{"code":"peer_not_connected"}}');
    await expect(readRetryingUnanswered(async () => {
      tries++;
      throw refusal;
    }, [1, 1, 1], noWait)).rejects.toBe(refusal);
    expect(tries).toBe(1);
  });
});
