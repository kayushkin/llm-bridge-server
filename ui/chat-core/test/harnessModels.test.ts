import { describe, expect, it } from 'vitest';
import { harnessCapabilities, modelsForHarness } from '../src/store/selectors.js';
import type { HarnessMeta, ModelOption } from '../src/net/types.js';

const HARNESSES: HarnessMeta[] = [
  {
    name: 'claudecode',
    label: 'Claude Code',
    emoji: '🤖',
    available: true,
    capabilities: ['model', 'effort', 'compact', 'fork', 'system_prompt', 'tools'],
    supportedProviders: ['anthropic'],
    pty: true,
  },
  {
    name: 'codex',
    label: 'Codex',
    emoji: '🧠',
    available: true,
    capabilities: ['model', 'compact'],
    // No supportedProviders declared — the picker must fall through to ALL models.
    pty: true,
  },
];

// `gpt-5` deliberately carries no nickname: the filter must pass a model through whole
// whether or not the registry has a short name for it.
const MODELS: ModelOption[] = [
  { value: 'claude-opus', label: 'Opus', provider: 'anthropic', shortName: 'opus-4.6' },
  { value: 'claude-sonnet', label: 'Sonnet', provider: 'anthropic', shortName: 'sonnet-4.6' },
  { value: 'gpt-5', label: 'GPT-5', provider: 'openai', shortName: '' },
];

describe('harnessCapabilities — from the canonical registry', () => {
  it('returns the harness capability set', () => {
    expect(harnessCapabilities(HARNESSES, 'codex')).toEqual(new Set(['model', 'compact']));
    expect(harnessCapabilities(HARNESSES, 'claudecode').has('fork')).toBe(true);
  });

  it('is an empty set for an unknown harness, a null id, or an unloaded registry', () => {
    expect(harnessCapabilities(HARNESSES, 'nope').size).toBe(0);
    expect(harnessCapabilities(HARNESSES, null).size).toBe(0);
    expect(harnessCapabilities(null, 'claudecode').size).toBe(0);
  });
});

describe('modelsForHarness — filters by the harness providers', () => {
  it("keeps only the harness's supported providers", () => {
    expect(modelsForHarness(MODELS, HARNESSES, 'claudecode')).toEqual([
      { value: 'claude-opus', label: 'Opus', provider: 'anthropic', shortName: 'opus-4.6' },
      { value: 'claude-sonnet', label: 'Sonnet', provider: 'anthropic', shortName: 'sonnet-4.6' },
    ]);
  });

  it('returns ALL models when the harness declares no providers', () => {
    expect(modelsForHarness(MODELS, HARNESSES, 'codex')).toEqual(MODELS);
  });

  it('returns ALL models when no harness id is given', () => {
    expect(modelsForHarness(MODELS, HARNESSES)).toEqual(MODELS);
    expect(modelsForHarness(MODELS, HARNESSES, null)).toEqual(MODELS);
  });

  it('returns [] until the model registry loads', () => {
    expect(modelsForHarness(null, HARNESSES, 'claudecode')).toEqual([]);
  });
});
