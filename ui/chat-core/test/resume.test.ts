import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { isResumableState } from '../src/react/hooks.js';

// `useComposer().resume` is the first caller `ApiClient.resume` has ever had — the
// method shipped with the client and nothing invoked it, so the chat page rendered a passive
// "⏸ paused" label and a stopped session was a dead end.
//
// These tests cover the wire half (the hook half is driven end-to-end by
// dash/e2e/chat-resume.spec.ts, which renders the real hook in a browser). What
// matters here is that resume is LOUD: the server refuses it with a 409 whenever the
// session turns out to have a live process — `TestResumeSession_AlreadyRunning` in
// llm-bridge-server pins that — and a swallowed 409 would leave the user looking at a
// session the UI claims is back and the server never restarted.

interface FakeRes {
  ok: boolean;
  status: number;
  statusText: string;
  jsonBody?: unknown;
  textBody?: string;
}

function fakeFetch(res: FakeRes, record?: (url: string, init?: RequestInit) => void): typeof fetch {
  return (async (url: string, init?: RequestInit) => {
    record?.(String(url), init);
    return {
      ok: res.ok,
      status: res.status,
      statusText: res.statusText,
      json: async () => res.jsonBody ?? {},
      text: async () => res.textBody ?? '',
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

describe('ApiClient.resume — fails loud', () => {
  it('POSTs to /sessions/{id}/resume and resolves on 2xx', async () => {
    const seen: Array<{ url: string; method?: string }> = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: {} }, (url, init) =>
        seen.push({ url, method: init?.method }),
      ),
      basePath: '/api/bridge',
    });
    await expect(api.resume('br_1')).resolves.toBeDefined();
    expect(seen[0]?.url).toBe('/api/bridge/sessions/br_1/resume');
    expect(seen[0]?.method).toBe('POST');
  });

  it('throws on the 409 the server returns when the session is already running', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: false,
        status: 409,
        statusText: 'Conflict',
        textBody: 'session already running',
      }),
      basePath: '/api/bridge',
    });
    await expect(api.resume('br_1')).rejects.toThrow(/409/);
  });

  it('throws when the session is bound to no instance and cannot be respawned', async () => {
    // handleResumeSession returns 500 for this: there is no instance to start on, so
    // the session cannot come back at all. Surfacing it is the difference between the
    // user retrying forever and the user learning the session is unrecoverable.
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: false,
        status: 500,
        statusText: 'Internal Server Error',
        textBody: 'session has no instance bound',
      }),
      basePath: '/api/bridge',
    });
    await expect(api.resume('br_1')).rejects.toThrow(/500/);
  });
});

describe('which states offer Resume', () => {
  it('offers it for a session whose process is gone, including a reaped completed one', () => {
    // `completed` is what llm-bridge-server leaves on a finished session after it reaps
    // the process, once it stops overwriting that state with `aborted`.
    for (const state of ['aborted', 'disconnected', 'completed']) {
      expect(isResumableState(state), state).toBe(true);
    }
  });

  it('does not offer it for states that still have a live process', () => {
    for (const state of ['paused', 'idle', 'error', 'rate_limited', 'awaiting_user', 'model_generating', '']) {
      expect(isResumableState(state), state).toBe(false);
    }
  });
});
