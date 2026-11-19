import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore, type ChatStoreApi } from '../src/store/ChatStore.js';
import { foldHookEvent, pendingHookFromWire, resolvePendingHook } from '../src/store/pendingHooks.js';
import type { PendingHook } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

function hookEvent(
  phase: string,
  requestId: string,
  extra: Record<string, unknown> = {},
): WireEvent {
  return {
    type: 'hook',
    data: {
      type: 'hook',
      hook: { event: 'PreToolUse', phase, request_id: requestId, ...extra },
    },
  };
}

function fakeFetch(
  res: { ok: boolean; status?: number; statusText?: string; jsonBody?: unknown; textBody?: string },
  record?: (url: string, init?: RequestInit) => void,
): typeof fetch {
  return (async (url: string, init?: RequestInit) => {
    record?.(String(url), init);
    return {
      ok: res.ok,
      status: res.status ?? 200,
      statusText: res.statusText ?? 'OK',
      json: async () => res.jsonBody ?? {},
      text: async () => res.textBody ?? '',
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

function parkedIn(store: ChatStoreApi, sessionId: string): PendingHook[] {
  return [...(store.getState().pendingHooks.get(sessionId)?.values() ?? [])];
}

describe('pendingHookFromWire — what cannot become a decision', () => {
  // Tested through the projector directly, because `foldHookEvent` cannot tell
  // these apart: it discards a null hook and a hook with an unrecognized phase
  // by the same `return current`. Projecting an absent payload into a blank
  // PendingHook instead of refusing it left every folding case green -- the
  // blank's phase is '' and falls out of the fold anyway. The refusal is only
  // visible here.

  it('an event carrying no hook payload at all is refused', () => {
    expect(pendingHookFromWire(undefined)).toBeNull();
  });

  it('a hook with no request_id is refused — the resolve endpoint is keyed on it', () => {
    expect(pendingHookFromWire({ event: 'PreToolUse', phase: 'awaiting_resolution' })).toBeNull();
    expect(
      pendingHookFromWire({ event: 'PreToolUse', phase: 'awaiting_resolution', request_id: '' }),
    ).toBeNull();
  });

  it('a hook WITH a request id projects, so the refusals above are not blanket', () => {
    // The cry-wolf control: the two cases above must not be passing because the
    // projector refuses everything.
    const hook = pendingHookFromWire({
      event: 'PreToolUse',
      phase: 'awaiting_resolution',
      request_id: 'req-9',
      tool_name: 'Bash',
    });
    expect(hook).not.toBeNull();
    expect(hook?.requestId).toBe('req-9');
    expect(hook?.toolName).toBe('Bash');
  });

  it('absent optional fields become empty strings, not undefined', () => {
    // `event`, `phase` and `source` are `?? ''` on the way in; the optional
    // camelCase ones are omitted entirely rather than set to undefined.
    const hook = pendingHookFromWire({ request_id: 'req-9' });
    expect(hook).toEqual({ requestId: 'req-9', event: '', phase: '', source: '' });
  });
});

describe('foldHookEvent — the awaiting/completed protocol', () => {
  it('inserts on awaiting_resolution and deletes on the matching completed', () => {
    const opened = foldHookEvent(new Map(), hookEvent('awaiting_resolution', 'req-1'));
    expect([...opened.keys()]).toEqual(['req-1']);

    const closed = foldHookEvent(opened, hookEvent('completed', 'req-1'));
    expect(closed.size).toBe(0);
  });

  it('ignores observation phases and events with no request id', () => {
    const start = foldHookEvent(new Map(), hookEvent('started', 'req-1'));
    expect(start.size).toBe(0);

    const noId = foldHookEvent(new Map(), hookEvent('awaiting_resolution', ''));
    expect(noId.size).toBe(0);
  });

  it('is idempotent both ways, so an SSE resume replay does not churn the banner', () => {
    const opened = foldHookEvent(new Map(), hookEvent('awaiting_resolution', 'req-1'));
    // Same reference back — a replayed insert must not produce a new map, or every
    // reconnect would re-render the banner and reset any in-progress card state.
    expect(foldHookEvent(opened, hookEvent('awaiting_resolution', 'req-1'))).toBe(opened);
    const closed = foldHookEvent(opened, hookEvent('completed', 'req-1'));
    expect(foldHookEvent(closed, hookEvent('completed', 'req-1'))).toBe(closed);
  });

  it('carries the tool name and raw input through unchanged', () => {
    const input = { command: 'rm -rf /tmp/x', description: 'clean' };
    const map = foldHookEvent(
      new Map(),
      hookEvent('awaiting_resolution', 'req-1', {
        tool_name: 'Bash',
        source: 'permission_prompt',
        input,
      }),
    );
    const hook = map.get('req-1')!;
    expect(hook.toolName).toBe('Bash');
    expect(hook.source).toBe('permission_prompt');
    expect(hook.input).toEqual(input);
  });
});

describe('applyTailEvent — the hook fold runs ahead of the reducer guard', () => {
  it('parks a hook the live stream delivers', () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('br_1', hookEvent('awaiting_resolution', 'req-1'));
    expect(parkedIn(store, 'br_1').map((h) => h.requestId)).toEqual(['req-1']);
  });

  // The regression this pins: applyTailEvent returns EARLY when the turn reducer reports
  // no change, and the reducer reports no change for any event id it has already seen.
  // With the hook fold behind that guard, a replayed awaiting_resolution is dropped and
  // the card never comes back — a tool call frozen with nothing on screen, which is the
  // exact failure the banner exists to prevent.
  it('re-parks a replayed hook the reducer treats as a no-op', () => {
    const store = createChatStore();
    const replayed = hookEvent('awaiting_resolution', 'req-1');
    replayed.data.event_id = 42;

    store.getState().actions.applyTailEvent('br_1', replayed);
    // Stand in for an optimistic resolve that the server then failed to confirm.
    store.getState().actions.clearPendingHook('br_1', 'req-1');
    expect(parkedIn(store, 'br_1')).toEqual([]);

    store.getState().actions.applyTailEvent('br_1', replayed);
    expect(parkedIn(store, 'br_1').map((h) => h.requestId)).toEqual(['req-1']);
  });

  it('clears the hook when the completed event lands', () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('br_1', hookEvent('awaiting_resolution', 'req-1'));
    store.getState().actions.applyTailEvent('br_1', hookEvent('completed', 'req-1'));
    expect(parkedIn(store, 'br_1')).toEqual([]);
  });

  it('keeps one session\'s parked hooks out of another\'s', () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('br_1', hookEvent('awaiting_resolution', 'req-1'));
    store.getState().actions.applyTailEvent('br_2', hookEvent('awaiting_resolution', 'req-2'));
    expect(parkedIn(store, 'br_1').map((h) => h.requestId)).toEqual(['req-1']);
    expect(parkedIn(store, 'br_2').map((h) => h.requestId)).toEqual(['req-2']);
  });
});

describe('ApiClient.getPendingHooks — hydration', () => {
  it('unwraps msg.Event.hook and keeps only resolvable awaiting entries', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: true,
        jsonBody: [
          { hook: { event: 'PreToolUse', phase: 'awaiting_resolution', request_id: 'req-1' } },
          { hook: { event: 'PreToolUse', phase: 'completed', request_id: 'req-2' } },
          { hook: { event: 'PreToolUse', phase: 'awaiting_resolution' } },
          {},
        ],
      }),
      basePath: '/api/bridge',
    });
    const pending = await api.getPendingHooks('br_1');
    expect(pending.map((h) => h.requestId)).toEqual(['req-1']);
  });

  // A 2xx whose body is not a list. The server contract says an array, but this
  // read is the parked-hook hydration that runs on every attach, and the body is
  // whatever the other end actually sent -- an error envelope from a proxy, a
  // `null` from a handler that forgot its empty case, a stray object. Until these
  // cases existed the `Array.isArray` guard was unpinned in both directions:
  // deleting it, and loosening it to a truthiness test, both left the suite green.
  //
  // The consequence of losing it is not a wrong list, it is a TypeError thrown out
  // of `getPendingHooks` on attach. The caller cannot tell that apart from a failed
  // request, so the permission banner stays blank and the tool call stays frozen
  // with nothing on screen -- the exact failure the docstring says hydration exists
  // to prevent.
  //
  // `'[]'` is the case a truthiness test lets through and an Array.isArray test does
  // not: a non-empty string is iterable, so `for (const ev of events)` would walk it
  // character by character and ask `pendingHookFromWire` about each character.
  it('answers an empty set for a 2xx body that is not an array, rather than throwing', async () => {
    for (const body of [null, {}, { hooks: [] }, '[]', 7, true]) {
      const api = new ApiClient({
        fetch: fakeFetch({ ok: true, jsonBody: body }),
        basePath: '/api/bridge',
      });
      await expect(api.getPendingHooks('br_1')).resolves.toEqual([]);
    }
  });

  it('throws on a non-2xx rather than reporting an empty set', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 404, statusText: 'Not Found' }),
      basePath: '/api/bridge',
    });
    await expect(api.getPendingHooks('br_1')).rejects.toThrow(/404/);
  });
});

describe('ApiClient.resolveHook — the decision POST', () => {
  it('posts to the request-id path with the snake_case body the server reads', async () => {
    const seen: Array<{ url: string; init?: RequestInit }> = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true }, (url, init) => seen.push({ url, init })),
      basePath: '/api/bridge',
    });
    await api.resolveHook('br_1', {
      requestId: 'req 1/x',
      behavior: 'allow',
      updatedInput: { answers: { q: 'a' } },
    });
    expect(seen[0].url).toBe('/api/bridge/sessions/br_1/hooks/req%201%2Fx/resolve');
    expect(JSON.parse(String(seen[0].init?.body))).toEqual({
      behavior: 'allow',
      resolved_by: 'user',
      updated_input: { answers: { q: 'a' } },
    });
  });

  it('omits updated_input and message when the caller gave none', async () => {
    const seen: Array<{ url: string; init?: RequestInit }> = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true }, (url, init) => seen.push({ url, init })),
      basePath: '/api/bridge',
    });
    await api.resolveHook('br_1', { requestId: 'req-1', behavior: 'deny' });
    expect(JSON.parse(String(seen[0].init?.body))).toEqual({
      behavior: 'deny',
      resolved_by: 'user',
    });
  });
});

describe('resolvePendingHook — optimistic clear, restore on refusal', () => {
  it('clears the card immediately and leaves it clear on a 2xx', async () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('br_1', hookEvent('awaiting_resolution', 'req-1'));
    const api = new ApiClient({ fetch: fakeFetch({ ok: true }), basePath: '/api/bridge' });

    await resolvePendingHook({ store, api }, 'br_1', { requestId: 'req-1', behavior: 'allow' });
    expect(parkedIn(store, 'br_1')).toEqual([]);
  });

  it('restores the card and rethrows when the server refuses', async () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent(
      'br_1',
      hookEvent('awaiting_resolution', 'req-1', { tool_name: 'Bash' }),
    );
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 500, statusText: 'Internal Server Error' }),
      basePath: '/api/bridge',
    });

    // The tool call is still parked on the server, so the only honest UI is the card
    // back on screen with the failure shown — never a silently emptied banner.
    await expect(
      resolvePendingHook({ store, api }, 'br_1', { requestId: 'req-1', behavior: 'allow' }),
    ).rejects.toThrow(/500/);
    expect(parkedIn(store, 'br_1').map((h) => h.toolName)).toEqual(['Bash']);
  });

  it('refuses a decision with no request id — there is nothing to address', async () => {
    const store = createChatStore();
    const api = new ApiClient({ fetch: fakeFetch({ ok: true }), basePath: '/api/bridge' });
    await expect(
      resolvePendingHook({ store, api }, 'br_1', { requestId: '', behavior: 'allow' }),
    ).rejects.toThrow(/requestId/);
  });

  it('a REFUSED resolve for a hook that was never parked restores nothing', () => {
    // The prior-entry snapshot is `... ?? null` and the restore is guarded on
    // it. Every case above parks a hook first, so the guard is always true and
    // its false branch was never taken -- the `?? null` could be deleted and the
    // suite stayed green.
    //
    // Resolving a request id the map does not hold is not exotic: two surfaces
    // can answer the same question, and the second one to arrive finds the
    // entry already cleared. Restoring the snapshot unconditionally would put an
    // `undefined` into the session's hook map, and the banner counts entries --
    // so a card with no question on it would appear and could never be cleared.
    const store = createChatStore();
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 500, statusText: 'Internal Server Error' }),
      basePath: '/api/bridge',
    });

    return expect(
      resolvePendingHook({ store, api }, 'br_1', { requestId: 'never-parked', behavior: 'allow' }),
    ).rejects.toThrow(/500/).then(() => {
      expect(parkedIn(store, 'br_1')).toEqual([]);
      expect(store.getState().pendingHooks.get('br_1')?.has('never-parked') ?? false).toBe(false);
    });
  });
});

describe('removeSession — parked hooks go with the session', () => {
  it('drops the session\'s hook map so a reused id cannot inherit a stale card', () => {
    const store = createChatStore();
    store.getState().actions.upsertSession({
      sessionId: 'br_1',
      state: 'idle',
      harness: 'claudecode',
      instanceId: 'inst1',
      type: 'interactive',
      purpose: 'chat',
      mode: 'events',
      folderName: '',
      displayName: 'br_1',
      agentId: '',
      updatedAt: '2026-08-01T10:00:00-07:00',
      createdAt: '2026-08-01T10:00:00-07:00',
    });
    store.getState().actions.applyTailEvent('br_1', hookEvent('awaiting_resolution', 'req-1'));
    store.getState().actions.removeSession('br_1');
    expect(store.getState().pendingHooks.has('br_1')).toBe(false);
  });
});
