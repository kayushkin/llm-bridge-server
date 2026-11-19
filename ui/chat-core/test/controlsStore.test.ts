import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { HarnessMeta, ModelOption } from '../src/net/types.js';

describe('openPending — carries the pre-start model/effort', () => {
  it('stores model + effort on the pending pane (0 network until first send)', () => {
    const store = createChatStore();
    store.getState().actions.openPending({ instanceId: 'inst1', harness: 'codex', model: 'gpt-5', effort: 'high' });
    const pending = store.getState().pending;
    expect(pending?.instanceId).toBe('inst1');
    expect(pending?.harness).toBe('codex');
    expect(pending?.model).toBe('gpt-5');
    expect(pending?.effort).toBe('high');
  });

  it('omits model/effort when not chosen', () => {
    const store = createChatStore();
    store.getState().actions.openPending({ harness: 'claudecode' });
    const pending = store.getState().pending;
    expect(pending?.harness).toBe('claudecode');
    expect(pending?.model).toBeUndefined();
    expect(pending?.effort).toBeUndefined();
  });
});

describe('harness / model registry actions', () => {
  it('start as null (not fetched) and cache on set', () => {
    const store = createChatStore();
    expect(store.getState().harnesses).toBeNull();
    expect(store.getState().models).toBeNull();

    const harnesses: HarnessMeta[] = [
      { name: 'codex', label: 'Codex', emoji: '🧠', available: true, capabilities: ['model'], pty: true },
    ];
    const models: ModelOption[] = [
      { value: 'gpt-5', label: 'GPT-5', provider: 'openai', shortName: 'gpt-5' },
    ];

    store.getState().actions.setHarnessesLoading(true);
    expect(store.getState().harnessesLoading).toBe(true);
    store.getState().actions.setHarnesses(harnesses);
    expect(store.getState().harnesses).toEqual(harnesses);
    expect(store.getState().harnessesLoading).toBe(false);

    store.getState().actions.setModelsLoading(true);
    expect(store.getState().modelsLoading).toBe(true);
    store.getState().actions.setModels(models);
    expect(store.getState().models).toEqual(models);
    expect(store.getState().modelsLoading).toBe(false);
  });
});
