import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore } from '../src/store/ChatStore.js';
import { changeSessionPermissionState } from '../src/store/permissionState.js';
import type { HarnessConfig, ManagedSessionDetail } from '../src/net/types.js';

function fakeApi(
  res: { ok: boolean; status: number; statusText?: string },
  seen?: { url: string; init?: RequestInit }[],
): ApiClient {
  const fetchFn = (async (url: string, init?: RequestInit) => {
    seen?.push({ url, init });
    return {
      ok: res.ok,
      status: res.status,
      statusText: res.statusText ?? '',
      json: async () => ({ status: 'ok' }),
      text: async () => '',
    } as unknown as Response;
  }) as unknown as typeof fetch;
  return new ApiClient({ fetch: fetchFn, basePath: '/api/bridge' });
}

function detail(mode: string): ManagedSessionDetail {
  return {
    sessionId: 'br_1',
    summary: {
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
      updatedAt: '2026-07-27T10:00:00-07:00',
      createdAt: '2026-07-27T10:00:00-07:00',
    },
    info: null,
    harnessConfig: { permissionMode: mode },
  };
}

function configOf(store: ReturnType<typeof createChatStore>): HarnessConfig | null | undefined {
  return store.getState().sessionDetail.get('br_1')?.harnessConfig;
}

function modeOf(store: ReturnType<typeof createChatStore>): string | undefined {
  return configOf(store)?.permissionMode;
}

function bodyOf(seen: { url: string; init?: RequestInit }[]): Record<string, unknown> {
  return JSON.parse(String(seen[0]!.init?.body)) as Record<string, unknown>;
}

describe('changeSessionPermissionState — optimistic + revert', () => {
  it('optimistically patches the cached mode and keeps it on a 2xx PUT', async () => {
    const store = createChatStore();
    store.getState().actions.setSessionDetail('br_1', detail('ask'));
    const api = fakeApi({ ok: true, status: 200 });

    const p = changeSessionPermissionState({ store, api }, 'br_1', { mode: 'bypass' });
    // Optimistic: the cached mode flips synchronously, before the PUT resolves.
    expect(modeOf(store)).toBe('bypass');
    await p;
    expect(modeOf(store)).toBe('bypass');
  });

  it('reverts the cached detail and rethrows on a non-2xx PUT', async () => {
    const store = createChatStore();
    store.getState().actions.setSessionDetail('br_1', detail('ask'));
    const api = fakeApi({ ok: false, status: 400, statusText: 'Bad Request' });

    await expect(
      changeSessionPermissionState({ store, api }, 'br_1', { mode: 'bypass' }),
    ).rejects.toThrow(/400/);
    // Reverted to the prior mode — a failed change is never silently kept.
    expect(modeOf(store)).toBe('ask');
  });

  it('sends every axis in ONE body, so a same-batch multi-axis edit lands together', async () => {
    const store = createChatStore();
    store.getState().actions.setSessionDetail('br_1', detail('ask'));
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = fakeApi({ ok: true, status: 200 }, seen);

    await changeSessionPermissionState({ store, api }, 'br_1', {
      mode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'never', sandbox: 'workspace-write' },
    });

    expect(seen).toHaveLength(1);
    expect(seen[0]!.url).toBe('/api/bridge/sessions/br_1/permission-mode');
    expect(seen[0]!.init?.method).toBe('PUT');
    expect(bodyOf(seen)).toEqual({
      mode: 'custom',
      disable_network: true,
      permission_mode_custom: { approval: 'never', sandbox: 'workspace-write' },
    });
  });

  it('optimistically patches the side-axes too, not just the mode', async () => {
    const store = createChatStore();
    store.getState().actions.setSessionDetail('br_1', detail('custom'));
    const api = fakeApi({ ok: true, status: 200 });

    const p = changeSessionPermissionState({ store, api }, 'br_1', {
      mode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'untrusted', sandbox: 'read-only' },
    });
    expect(configOf(store)).toEqual({
      permissionMode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'untrusted', sandbox: 'read-only' },
    });
    await p;
  });

  it('reverts EVERY axis on failure, leaving no half-applied state', async () => {
    const store = createChatStore();
    const d = detail('custom');
    d.harnessConfig = {
      permissionMode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'untrusted', sandbox: 'read-only' },
    };
    store.getState().actions.setSessionDetail('br_1', d);
    const api = fakeApi({ ok: false, status: 500, statusText: 'Server Error' });

    await expect(
      changeSessionPermissionState({ store, api }, 'br_1', {
        mode: 'custom',
        disableNetwork: false,
        permissionModeCustom: { approval: 'never', sandbox: 'danger-full-access' },
      }),
    ).rejects.toThrow(/500/);
    expect(configOf(store)).toEqual({
      permissionMode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'untrusted', sandbox: 'read-only' },
    });
  });

  it('omits an axis the caller left out — a mode change never clears the network gate', async () => {
    const store = createChatStore();
    const d = detail('ask');
    d.harnessConfig = { permissionMode: 'ask', disableNetwork: true };
    store.getState().actions.setSessionDetail('br_1', d);
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = fakeApi({ ok: true, status: 200 }, seen);

    await changeSessionPermissionState({ store, api }, 'br_1', { mode: 'bypass' });

    // No `disable_network` key at all — the server reads that as "leave it alone".
    // Sending `false` would DELETE the stored gate, which is a different request.
    expect(bodyOf(seen)).toEqual({ mode: 'bypass' });
    expect(configOf(store)?.disableNetwork).toBe(true);
  });

  it('sends an empty custom struct verbatim — that is how the server is told to clear it', async () => {
    const store = createChatStore();
    store.getState().actions.setSessionDetail('br_1', detail('ask'));
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = fakeApi({ ok: true, status: 200 }, seen);

    await changeSessionPermissionState({ store, api }, 'br_1', {
      mode: 'custom',
      permissionModeCustom: { approval: '', sandbox: '' },
    });

    expect(bodyOf(seen)).toEqual({
      mode: 'custom',
      permission_mode_custom: { approval: '', sandbox: '' },
    });
  });

  it('fills a half-specified custom struct with empty strings, never dropping the axis', async () => {
    const store = createChatStore();
    store.getState().actions.setSessionDetail('br_1', detail('custom'));
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = fakeApi({ ok: true, status: 200 }, seen);

    await changeSessionPermissionState({ store, api }, 'br_1', {
      mode: 'custom',
      permissionModeCustom: { sandbox: 'read-only' },
    });

    // `approval` absent on the way in must not become "keep the old approval" on the
    // way out — the server merges nothing inside the struct, it replaces it whole.
    expect(bodyOf(seen)).toEqual({
      mode: 'custom',
      permission_mode_custom: { approval: '', sandbox: 'read-only' },
    });
  });

  it('patchHarnessConfig merges into (not replaces) the existing config', () => {
    const store = createChatStore();
    const d = detail('ask');
    d.harnessConfig = { permissionMode: 'ask', disableNetwork: true, model: 'claude-opus' };
    store.getState().actions.setSessionDetail('br_1', d);

    const prior = store.getState().actions.patchHarnessConfig('br_1', { permissionMode: 'auto' });
    expect(prior).toEqual({ permissionMode: 'ask', disableNetwork: true, model: 'claude-opus' });
    // The patch merges — the other knobs survive.
    expect(store.getState().sessionDetail.get('br_1')?.harnessConfig).toEqual({
      permissionMode: 'auto',
      disableNetwork: true,
      model: 'claude-opus',
    });
  });

  it('patchHarnessConfig is a no-op (returns null) when the detail is not cached', () => {
    const store = createChatStore();
    expect(store.getState().actions.patchHarnessConfig('nope', { permissionMode: 'auto' })).toBeNull();
  });

  it('a refused change for an UNCACHED session writes no detail back', async () => {
    // The snapshot taken before the PUT is `... ?? null`, and the revert is
    // guarded on it. Every other case here starts from a cached detail, so the
    // guard is always true and its false branch was never taken: removing the
    // `if (prior)` test left the whole suite green.
    //
    // With no cached detail there is nothing to revert TO, and the revert must
    // not run. Reverting anyway would write the snapshot itself into the cache
    // as though it were a detail, and `sessionDetail` would then hold an entry
    // for a session whose detail was never fetched -- which every reader treats
    // as "loaded", so nothing would fetch it again.
    const store = createChatStore();
    const api = fakeApi({ ok: false, status: 500, statusText: 'Server Error' });

    await expect(
      changeSessionPermissionState({ store, api }, 'br_uncached', { mode: 'auto' }),
    ).rejects.toThrow();

    expect(store.getState().sessionDetail.has('br_uncached')).toBe(false);
  });
});
