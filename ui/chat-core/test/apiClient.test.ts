import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';

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

describe('ApiClient.interrupt — fails loud', () => {
  it('throws (does NOT swallow) on a 409 "nothing was stopped"', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 409, statusText: 'Conflict', textBody: 'nothing was stopped' }),
      basePath: '/api/bridge',
    });
    // A 409 while a tool holds the turn must surface as a thrown error — never a
    // resolved/fake-idle result.
    await expect(api.interrupt('br_1')).rejects.toThrow(/409/);
  });

  it('POSTs to /sessions/{id}/interrupt and resolves on 2xx', async () => {
    const seen: string[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: {} }, (u) => seen.push(u)),
      basePath: '/api/bridge',
    });
    await expect(api.interrupt('br_1')).resolves.toBeDefined();
    expect(seen[0]).toBe('/api/bridge/sessions/br_1/interrupt');
  });
});

describe('ApiClient.getValidators — the id set rides a POST body, never the URL', () => {
  // ⚠️ Same regression family as getSummary's id lookups: one id per cached
  // session, and a query string that grows with the cache walks toward the URL
  // length (~11.5 KB, measured) at which nginx destroys the whole HTTP/2
  // connection — killing every other request in flight, not just this one.
  it('POSTs { ids } to /sessions/validators, keeping the URL fixed-length', async () => {
    const calls: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: {} }, (url, init) =>
        calls.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    const ids = Array.from({ length: 3000 }, (_, i) => `br_${i}`);
    await api.getValidators(ids);

    expect(calls).toHaveLength(1);
    expect(calls[0]!.url).toBe('/api/bridge/sessions/validators');
    expect(calls[0]!.init?.method).toBe('POST');
    expect(JSON.parse(String(calls[0]!.init?.body))).toEqual({ ids });
  });

  it('asks the server nothing at all for an empty id set', async () => {
    const calls: string[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: {} }, (url) =>
        calls.push(url),
      ),
      basePath: '/api/bridge',
    });
    expect(await api.getValidators([])).toEqual({});
    expect(calls).toHaveLength(0);
  });
});

describe('ApiClient.search', () => {
  it('GETs /sessions/search?q= and returns the hit ids', async () => {
    const seen: string[] = [];
    const api = new ApiClient({
      // The real log-store shape: a bare array of {session_id, match_count}. This
      // mock used to send `{ sessionIds: [...] }`, an envelope the backend has never
      // produced, so the test asserted the client could read a response that does
      // not exist and passed while content search was broken in the browser.
      fetch: fakeFetch(
        {
          ok: true,
          status: 200,
          statusText: 'OK',
          jsonBody: [
            { session_id: 'br_2', match_count: 9 },
            { session_id: 'br_9', match_count: 3 },
          ],
        },
        (u) => seen.push(u),
      ),
      basePath: '/api/bridge',
    });
    const r = await api.search('deploy sync');
    expect(r.sessionIds).toEqual(['br_2', 'br_9']);
    expect(seen[0]).toContain('/sessions/search?q=deploy');
  });

  it('throws on a non-2xx search response', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 500, statusText: 'Server Error' }),
      basePath: '/api/bridge',
    });
    await expect(api.search('x')).rejects.toThrow(/500/);
  });
});

describe('ApiClient.getSessionDetail — snake_case info → camelCase SessionInfo', () => {
  it('GETs /sessions/{id} and maps every info field explicitly', async () => {
    const seen: string[] = [];
    const wire = {
      session_id: 'br_7',
      state: 'idle',
      harness: 'claudecode',
      display_name: 'My session',
      updated_at: '2026-07-27T10:00:00-07:00',
      created_at: '2026-07-27T09:00:00-07:00',
      info: {
        system_prompt: 'You are helpful.',
        append_system_prompt: 'Extra.',
        working_dir: '/home/u/repo',
        model: 'claude-opus',
        permission_mode: 'acceptEdits',
        tools: [{ name: 'Read', description: 'reads' }, { name: 'Bash' }],
        slash_commands: ['/init', '/review'],
        agents: ['Explore'],
        skills: ['browser-automation'],
        mcp_servers: [{ name: 'gmail', status: 'connected' }, { name: 'drive' }],
      },
    };
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: wire }, (u) => seen.push(u)),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_7');
    expect(seen[0]).toBe('/api/bridge/sessions/br_7');
    // Summary projection reuses summaryFromManaged (single source of truth).
    expect(detail.summary.sessionId).toBe('br_7');
    expect(detail.summary.displayName).toBe('My session');
    // Every info field mapped to camelCase.
    expect(detail.info).toEqual({
      systemPrompt: 'You are helpful.',
      appendSystemPrompt: 'Extra.',
      workingDir: '/home/u/repo',
      model: 'claude-opus',
      permissionMode: 'acceptEdits',
      tools: [{ name: 'Read', description: 'reads' }, { name: 'Bash', description: undefined }],
      slashCommands: ['/init', '/review'],
      agents: ['Explore'],
      skills: ['browser-automation'],
      mcpServers: [{ name: 'gmail', status: 'connected' }, { name: 'drive', status: undefined }],
    });
  });

  it('returns info=null when the harness has reported none', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: true,
        status: 200,
        statusText: 'OK',
        jsonBody: { session_id: 'br_8', state: 'starting' },
      }),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_8');
    expect(detail.summary.sessionId).toBe('br_8');
    expect(detail.info).toBeNull();
  });

  it('throws loudly on a non-2xx detail response', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 404, statusText: 'Not Found' }),
      basePath: '/api/bridge',
    });
    await expect(api.getSessionDetail('nope')).rejects.toThrow(/404/);
  });
});

describe('ApiClient.getSessionDetail — the identity, lineage and spend block', () => {
  it('carries all nine fields the endpoint returns, none of them onto the summary', async () => {
    const wire = {
      session_id: 'br_11',
      state: 'running',
      harness: 'claudecode',
      updated_at: '2026-08-02T10:00:00-07:00',
      created_at: '2026-08-02T09:00:00-07:00',
      origin: 'frontend-dash',
      pid: 4242,
      harness_session_id: 'cc-9f3a',
      parent_id: 'cc-parent-uuid',
      forked_from_session_id: 'br_10',
      manager_session_id: 'br_1',
      working_dir: '/home/u/repos/dash',
      spend_usd: 1.25,
      max_budget_usd: 5,
    };
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: wire }),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_11');
    expect(detail.origin).toBe('frontend-dash');
    expect(detail.pid).toBe(4242);
    expect(detail.harnessSessionId).toBe('cc-9f3a');
    // The deprecated `parent_id` is a harness uuid, so it does NOT land on a field
    // called "parent" — the name has to say which id it holds.
    expect(detail.forkParentHarnessSessionId).toBe('cc-parent-uuid');
    expect(detail.forkedFromSessionId).toBe('br_10');
    expect(detail.managerSessionId).toBe('br_1');
    expect(detail.workingDir).toBe('/home/u/repos/dash');
    expect(detail.spendUsd).toBe(1.25);
    expect(detail.maxBudgetUsd).toBe(5);
    // The sidebar row must not grow beyond these keys: the original 12, the two
    // lineage ids (harnessSessionId, managerSessionId) the live-status surface joins
    // subagent sessions on, spendUsd, which the server's summary rows carry and
    // the projects view sums per project, and workingDir, which summary rows carry
    // since 2026-10-31 so a list reader can place a session by its repo. Everything
    // else detail-only.
    expect(Object.keys(detail.summary).sort()).toEqual([
      'agentId', 'createdAt', 'displayName', 'folderName', 'harness',
      'harnessSessionId', 'instanceId', 'managerSessionId', 'mode', 'principalId',
      'purpose', 'sessionId', 'spendUsd', 'state', 'type', 'updatedAt', 'workingDir',
    ]);
  });

  it('leaves an omitted field absent rather than defaulting it', async () => {
    // A server that predates the spend gate sends neither money field. Absent must
    // stay absent: a `spendUsd` of 0 would read as "measured $0.00" and a
    // `maxBudgetUsd` of 0 means NO ceiling, so defaulting either invents a fact.
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: true,
        status: 200,
        statusText: 'OK',
        jsonBody: { session_id: 'br_12', state: 'idle' },
      }),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_12');
    expect('spendUsd' in detail).toBe(false);
    expect('maxBudgetUsd' in detail).toBe(false);
    expect('origin' in detail).toBe(false);
    expect('pid' in detail).toBe(false);
    expect('workingDir' in detail).toBe(false);
  });

  it('keeps a reported zero, which is the opposite of an absent one', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: true,
        status: 200,
        statusText: 'OK',
        jsonBody: { session_id: 'br_13', state: 'idle', spend_usd: 0, max_budget_usd: 0 },
      }),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_13');
    expect(detail.spendUsd).toBe(0);
    expect(detail.maxBudgetUsd).toBe(0);
  });
});

describe('ApiClient.getSessionDetail — harness_config → camelCase harnessConfig', () => {
  it('maps the well-known keys and carries opaque knobs through', async () => {
    const wire = {
      session_id: 'br_5',
      state: 'idle',
      harness: 'codex',
      updated_at: '2026-07-27T10:00:00-07:00',
      created_at: '2026-07-27T09:00:00-07:00',
      harness_config: {
        permission_mode: 'custom',
        disable_network: true,
        permission_mode_custom: { approval: 'on-request', sandbox: 'workspace-write' },
        model: 'gpt-5',
        effort: 'high',
        // an unnamed harness-specific knob — must survive (opaque bag).
        reasoning_summaries: 'auto',
      },
    };
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: wire }),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_5');
    expect(detail.sessionId).toBe('br_5');
    expect(detail.harnessConfig).toEqual({
      permissionMode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'on-request', sandbox: 'workspace-write' },
      model: 'gpt-5',
      effort: 'high',
      reasoning_summaries: 'auto',
    });
  });

  it('harnessConfig is null when harness_config is absent', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({
        ok: true,
        status: 200,
        statusText: 'OK',
        jsonBody: { session_id: 'br_6', state: 'starting' },
      }),
      basePath: '/api/bridge',
    });
    const detail = await api.getSessionDetail('br_6');
    expect(detail.harnessConfig).toBeNull();
    expect(detail.sessionId).toBe('br_6');
  });
});

describe('ApiClient.createSession / fork — map canonical session_id → sessionId', () => {
  it('createSession reads session_id from the ManagedSession wire (not a phantom sessionId)', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch(
        { ok: true, status: 201, statusText: 'Created', jsonBody: { session_id: 'br_new', state: 'starting' } },
        (url, init) => seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    const created = await api.createSession({ instanceId: 'inst1', harness: 'claudecode' });
    expect(created.sessionId).toBe('br_new');
    expect(seen[0]!.url).toBe('/api/bridge/sessions');
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({
      type: 'interactive',
      purpose: 'chat',
      origin: 'frontend',
      instance_id: 'inst1',
      harness: 'claudecode',
    });
  });

  it('createSession sends principal_id on the create itself, and nothing when there is none', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch(
        { ok: true, status: 201, statusText: 'Created', jsonBody: { session_id: 'br_new', state: 'starting' } },
        (url, init) => seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    await api.createSession({ instanceId: 'inst1', harness: 'claudecode', principalId: 'principal_000001' });
    expect(JSON.parse(String(seen[0]!.init?.body))).toMatchObject({ principal_id: 'principal_000001' });
    await api.createSession({ instanceId: 'inst1', harness: 'claudecode' });
    expect('principal_id' in JSON.parse(String(seen[1]!.init?.body))).toBe(false);
  });

  it('fork POSTs display_name + type and maps the forked session_id', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch(
        { ok: true, status: 201, statusText: 'Created', jsonBody: { session_id: 'br_fork', state: 'starting' } },
        (url, init) => seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    const created = await api.fork('br_1', 'My fork');
    expect(created.sessionId).toBe('br_fork');
    expect(seen[0]!.url).toBe('/api/bridge/sessions/br_1/fork');
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({ display_name: 'My fork', type: 'interactive' });
  });

  it('fork throws loud on a non-2xx (e.g. 409 parent not initialized)', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 409, statusText: 'Conflict', textBody: 'no harness_session_id yet' }),
      basePath: '/api/bridge',
    });
    await expect(api.fork('br_1')).rejects.toThrow(/409/);
  });
});

describe('ApiClient.compact — loud, body shape', () => {
  it('POSTs {} with no summary and { summary } with one', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: {} }, (url, init) =>
        seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    await api.compact('br_1');
    await api.compact('br_1', 'keep the plan');
    expect(seen[0]!.url).toBe('/api/bridge/sessions/br_1/compact');
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({});
    expect(JSON.parse(String(seen[1]!.init?.body))).toEqual({ summary: 'keep the plan' });
  });

  it('throws loud on a non-2xx', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 404, statusText: 'Not Found' }),
      basePath: '/api/bridge',
    });
    await expect(api.compact('nope')).rejects.toThrow(/404/);
  });
});

describe('ApiClient.switchMode — loud, POST /mode { mode }', () => {
  it('POSTs the mode and resolves on 2xx', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: { attach_token: 'tok' } }, (url, init) =>
        seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    await expect(api.switchMode('br_1', 'pty')).resolves.toBeDefined();
    expect(seen[0]!.url).toBe('/api/bridge/sessions/br_1/mode');
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({ mode: 'pty' });
  });

  it('throws loud on a non-2xx (e.g. pty unsupported)', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 400, statusText: 'Bad Request' }),
      basePath: '/api/bridge',
    });
    await expect(api.switchMode('br_1', 'pty')).rejects.toThrow(/400/);
  });
});

describe('ApiClient.setConfig — camelCase → snake_case body, only provided fields', () => {
  it('maps model/effort/maxBudget/disabledTools and omits absent fields', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: {} }, (url, init) =>
        seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    await api.setConfig('br_1', { model: 'claude-opus', effort: 'high', maxBudget: 5, disabledTools: ['Bash'] });
    expect(seen[0]!.url).toBe('/api/bridge/sessions/br_1/config');
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({
      model: 'claude-opus',
      effort: 'high',
      max_budget: 5,
      disabled_tools: ['Bash'],
    });
    // A partial config sends ONLY the provided key — never a null for the rest.
    await api.setConfig('br_1', { effort: 'low' });
    expect(JSON.parse(String(seen[1]!.init?.body))).toEqual({ effort: 'low' });
  });

  it('throws loud on a non-2xx', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 400, statusText: 'Bad Request' }),
      basePath: '/api/bridge',
    });
    await expect(api.setConfig('br_1', { model: 'x' })).rejects.toThrow(/400/);
  });
});

describe('ApiClient.getHarnesses — snake_case HarnessInfo → camelCase HarnessMeta', () => {
  it('maps every field and defaults capabilities', async () => {
    const seen: string[] = [];
    const wire = [
      {
        name: 'claudecode',
        label: 'Claude Code',
        emoji: '🤖',
        tint: '#d97757',
        available: true,
        capabilities: ['model', 'effort', 'compact', 'fork', 'system_prompt', 'tools'],
        hook_events: ['PreToolUse'],
        supported_providers: ['anthropic'],
        supported_permission_modes: ['ask', 'bypass'],
        pty: true,
        supports_disable_network: false,
      },
    ];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: wire }, (u) => seen.push(u)),
      basePath: '/api/bridge',
    });
    const harnesses = await api.getHarnesses();
    expect(seen[0]).toBe('/api/bridge/harnesses');
    expect(harnesses[0]).toEqual({
      name: 'claudecode',
      label: 'Claude Code',
      emoji: '🤖',
      tint: '#d97757',
      available: true,
      capabilities: ['model', 'effort', 'compact', 'fork', 'system_prompt', 'tools'],
      hookEvents: ['PreToolUse'],
      supportedProviders: ['anthropic'],
      supportedPermissionModes: ['ask', 'bypass'],
      pty: true,
      supportsDisableNetwork: false,
    });
  });

  it('tolerates a null body (nil slice) → []', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: null }),
      basePath: '/api/bridge',
    });
    await expect(api.getHarnesses()).resolves.toEqual([]);
  });
});

describe('ApiClient.getModels — filters enabled + projects ModelOption', () => {
  it('drops disabled rows and builds { value, label, provider, shortName } with cost', async () => {
    const wire = [
      {
        id: 'claude-opus',
        name: 'Opus',
        short_name: 'opus-4.6',
        provider: 'anthropic',
        enabled: true,
        input_cost: 15,
        output_cost: 75,
      },
      { id: 'gpt-5', name: 'GPT-5', provider: 'openai', enabled: false, input_cost: 10, output_cost: 30 },
      { id: 'bare', provider: 'local', enabled: true },
    ];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: wire }),
      basePath: '/api/bridge',
    });
    const models = await api.getModels();
    expect(models).toEqual([
      { value: 'claude-opus', label: 'Opus ($15/$75)', provider: 'anthropic', shortName: 'opus-4.6' },
      // No cost reported → label is just the name (falls back to id) — never a fake cost.
      // No `short_name` either → the empty string, NOT the name and NOT the id: the mapper
      // reports that this model has no nickname and leaves it to the picker to decide what
      // to draw instead.
      { value: 'bare', label: 'bare', provider: 'local', shortName: '' },
    ]);
  });

  it('carries short_name through independently of the cost-bearing label', async () => {
    // A nickname and a cost are separate facts about a row, so pin the two combinations the
    // case above does not cover: a nicknamed model with no cost, and a priced model with no
    // nickname. Otherwise `shortName` could be accidentally wired to the cost branch and
    // every assertion above would still pass.
    const wire = [
      { id: 'claude-haiku', name: 'Claude Haiku 4.6', short_name: 'haiku-4.6', provider: 'anthropic', enabled: true },
      { id: 'gpt-5', name: 'GPT-5', provider: 'openai', enabled: true, input_cost: 10, output_cost: 30 },
    ];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: wire }),
      basePath: '/api/bridge',
    });
    await expect(api.getModels()).resolves.toEqual([
      { value: 'claude-haiku', label: 'Claude Haiku 4.6', provider: 'anthropic', shortName: 'haiku-4.6' },
      { value: 'gpt-5', label: 'GPT-5 ($10/$30)', provider: 'openai', shortName: '' },
    ]);
  });

  it('tolerates a null body → []', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: null }),
      basePath: '/api/bridge',
    });
    await expect(api.getModels()).resolves.toEqual([]);
  });
});

describe('ApiClient.setSessionPermissionState — loud, single-body PUT', () => {
  it('PUTs /sessions/{id}/permission-mode with { mode } and resolves on 2xx', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: { status: 'ok' } }, (url, init) =>
        seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    await expect(api.setSessionPermissionState('br_1', { mode: 'bypass' })).resolves.toBeDefined();
    expect(seen[0]!.url).toBe('/api/bridge/sessions/br_1/permission-mode');
    expect(seen[0]!.init?.method).toBe('PUT');
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({ mode: 'bypass' });
  });

  it('carries the mode, the network gate and the custom knobs in ONE body', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: { status: 'ok' } }, (url, init) =>
        seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    await api.setSessionPermissionState('br_1', {
      mode: 'custom',
      disableNetwork: true,
      permissionModeCustom: { approval: 'on-request', sandbox: 'workspace-write' },
    });
    expect(seen).toHaveLength(1);
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({
      mode: 'custom',
      disable_network: true,
      permission_mode_custom: { approval: 'on-request', sandbox: 'workspace-write' },
    });
  });

  it('sends an explicit false for the network gate — that is how it is turned OFF', async () => {
    const seen: { url: string; init?: RequestInit }[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: { status: 'ok' } }, (url, init) =>
        seen.push({ url, init }),
      ),
      basePath: '/api/bridge',
    });
    // `false` must reach the wire, not be dropped as falsy: the server deletes the
    // stored key on an explicit false and leaves it alone when the field is absent.
    await api.setSessionPermissionState('br_1', { mode: 'ask', disableNetwork: false });
    expect(JSON.parse(String(seen[0]!.init?.body))).toEqual({ mode: 'ask', disable_network: false });
  });

  it('throws (does NOT swallow) on a non-2xx — e.g. 400 invalid mode', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 400, statusText: 'Bad Request', textBody: 'mode must be one of ...' }),
      basePath: '/api/bridge',
    });
    await expect(api.setSessionPermissionState('br_1', { mode: 'nonsense' })).rejects.toThrow(/400/);
  });
});

describe('ApiClient.search — parses the array log-store actually sends', () => {
  // Regression: SearchResponse used to declare `{ sessionIds, hits }`, an envelope
  // the backend has never sent. GET /sessions/search answers with a bare array of
  // store.SearchHit — `{session_id, match_count}`. Reading `.sessionIds` off an
  // array yielded undefined, `new Set(undefined)` is an empty set rather than a
  // throw, and so content search matched nothing for every query, silently, while
  // the local display-name filter kept working and hid the failure.
  //
  // test/searchFolding.test.ts passed throughout: it calls setContentHits() with a
  // hand-written string[], so it asserted the store folds ids it is *given* and
  // never exercised this parse. The bug lived in the gap between them.
  const WIRE = [
    { session_id: 'br_low', match_count: 2 },
    { session_id: 'br_high', match_count: 40 },
    { session_id: 'br_mid', match_count: 7 },
  ];

  it('maps session_id/match_count and ranks by descending match count', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: WIRE }),
      basePath: '/api/bridge',
    });
    const res = await api.search('needle');
    expect(res.sessionIds).toEqual(['br_high', 'br_mid', 'br_low']);
    expect(res.hits[0]).toEqual({ sessionId: 'br_high', matchCount: 40 });
  });

  it('sends the query and returns an empty result set for no matches', async () => {
    const seen: string[] = [];
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: [] }, (u) => seen.push(u)),
      basePath: '/api/bridge',
    });
    const res = await api.search('a b');
    // The limit is sent EXPLICITLY. It used to be omitted, which left the bound as
    // log-store's own default (100) — the same number, but arrived at by nobody,
    // and with no way for the client to tell a full result from a truncated one.
    expect(seen[0]).toBe('/api/bridge/sessions/search?q=a+b&limit=100');
    expect(res.sessionIds).toEqual([]);
    expect(res.truncated).toBe(false);
  });

  it('reports truncated when the backend fills the whole page', async () => {
    const full = Array.from({ length: 3 }, (_, i) => ({ session_id: `br_${i}`, match_count: 1 }));
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: full }),
      basePath: '/api/bridge',
    });
    const res = await api.search('needle', { limit: 3 });
    expect(res.limit).toBe(3);
    expect(res.truncated).toBe(true);
  });

  it('does not report truncated on a short page', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: WIRE }),
      basePath: '/api/bridge',
    });
    const res = await api.search('needle', { limit: 50 });
    expect(res.hits).toHaveLength(3);
    expect(res.truncated).toBe(false);
  });

  it('throws rather than degrading to zero hits if the shape is not an array', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 200, statusText: 'OK', jsonBody: { sessionIds: ['br_1'] } }),
      basePath: '/api/bridge',
    });
    await expect(api.search('needle')).rejects.toThrow(/expected an array/);
  });
});

describe('ApiClient.shareSessionFile', () => {
  it('POSTs the bytes with their type and the name in the query', async () => {
    const seen: Array<{ url: string; init?: RequestInit }> = [];
    const record = { file_id: 'file_000001', path: '/x/a b.png' };
    const api = new ApiClient({
      fetch: fakeFetch({ ok: true, status: 201, statusText: 'Created', jsonBody: record }, (url, init) => seen.push({ url, init })),
      basePath: '/api/bridge',
    });
    const file = new Blob(['PNG'], { type: 'image/png' });
    await expect(api.shareSessionFile('br_1', file, 'a b.png')).resolves.toEqual(record);
    expect(seen[0].url).toBe('/api/bridge/sessions/br_1/files?filename=a%20b.png');
    expect(seen[0].init?.method).toBe('POST');
    expect((seen[0].init?.headers as Record<string, string>)['Content-Type']).toBe('image/png');
    expect(seen[0].init?.body).toBe(file);
  });

  it('throws the server refusal rather than resolving', async () => {
    const api = new ApiClient({
      fetch: fakeFetch({ ok: false, status: 413, statusText: 'Payload Too Large', textBody: 'at most 26214400 bytes' }),
      basePath: '/api/bridge',
    });
    await expect(api.shareSessionFile('br_1', new Blob(['x']), 'big.bin')).rejects.toThrow(/413.*26214400/);
  });
});
