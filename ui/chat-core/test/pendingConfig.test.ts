import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import { pendingSessionConfig } from '../src/store/pendingConfig.js';

// `POST /sessions` takes the target only, so every runtime setting a new chat starts
// with arrives on a second call, `POST /sessions/{id}/config`. `pendingSessionConfig`
// is that call's whole body, and these tests pin the three things about it that a UI
// test cannot reach: that 0 and [] survive, that absent stays absent, and that a pane
// with nothing to say produces no call at all.

describe('pendingSessionConfig — the body a lazily-created session is configured with', () => {
  it('returns null for a pane with no settings, so no config call is made', () => {
    expect(pendingSessionConfig(null)).toBeNull();
    expect(pendingSessionConfig(undefined)).toBeNull();
    expect(pendingSessionConfig({})).toBeNull();
    // A target is not a setting: instance and harness went in on `POST /sessions`.
    expect(pendingSessionConfig({ instanceId: 'inst-cc', harness: 'claude_code' })).toBeNull();
  });

  it('carries all four settings in ONE call, so the server never sees a half-configured session', () => {
    expect(
      pendingSessionConfig({
        instanceId: 'inst-cc',
        harness: 'claude_code',
        model: 'claude-sonnet-4-6',
        effort: 'high',
        maxBudget: 5,
        disabledTools: ['Bash'],
      }),
    ).toEqual({
      model: 'claude-sonnet-4-6',
      effort: 'high',
      maxBudget: 5,
      disabledTools: ['Bash'],
    });
  });

  it('keeps maxBudget 0 — on this server 0 means NO CEILING, the opposite of absent', () => {
    // The trap: `if (opts.maxBudget)` reads 0 as nothing to send, the config call is
    // skipped, and a session the user explicitly uncapped comes up under whatever
    // ceiling the server defaults to. A halt the user disabled is worse than a missing
    // one, so this is asserted on the VALUE, not just on the key being present.
    const config = pendingSessionConfig({ maxBudget: 0 });
    expect(config).toEqual({ maxBudget: 0 });
    expect(config?.maxBudget).toBe(0);
  });

  it('keeps an empty disabledTools — "disable nothing" is a decision, absent is not', () => {
    expect(pendingSessionConfig({ disabledTools: [] })).toEqual({ disabledTools: [] });
  });

  it('omits absent fields rather than sending null, so nothing is cleared by accident', () => {
    const config = pendingSessionConfig({ model: 'gpt-5' });
    expect(config).toEqual({ model: 'gpt-5' });
    expect(config && 'effort' in config).toBe(false);
    expect(config && 'maxBudget' in config).toBe(false);
    expect(config && 'disabledTools' in config).toBe(false);
  });

  it('drops an empty model/effort string — a cleared select means "let the server pick"', () => {
    expect(pendingSessionConfig({ model: '', effort: '' })).toBeNull();
  });
});

describe('openPending — the pane records what the config call will read back', () => {
  const store = () => createChatStore();

  it('round-trips all four settings through the pending pane', () => {
    const s = store();
    s.getState().actions.openPending({
      instanceId: 'inst-cc',
      harness: 'claude_code',
      model: 'claude-sonnet-4-6',
      effort: 'medium',
      maxBudget: 2.5,
      disabledTools: ['WebSearch'],
    });
    expect(pendingSessionConfig(s.getState().pending)).toEqual({
      model: 'claude-sonnet-4-6',
      effort: 'medium',
      maxBudget: 2.5,
      disabledTools: ['WebSearch'],
    });
  });

  it('records maxBudget 0 and an empty tool list instead of dropping them as falsy', () => {
    const s = store();
    s.getState().actions.openPending({ harness: 'codex', maxBudget: 0, disabledTools: [] });
    const pending = s.getState().pending;
    expect(pending?.maxBudget).toBe(0);
    expect(pending?.disabledTools).toEqual([]);
  });

  it('leaves the keys ABSENT when the caller has no default, never `undefined`', () => {
    // `'maxBudget' in pending` is the difference between "the user has no saved ceiling"
    // and "the user saved a ceiling of undefined", and only the first is a real state.
    const s = store();
    s.getState().actions.openPending({ instanceId: 'inst-cc', harness: 'claude_code' });
    const pending = s.getState().pending;
    expect(pending && 'maxBudget' in pending).toBe(false);
    expect(pending && 'disabledTools' in pending).toBe(false);
  });
});
