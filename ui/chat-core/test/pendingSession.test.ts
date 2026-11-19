import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';

// `usePendingSession` is a bare `useStore(store, s => s.pending)` selector, so what is
// worth testing is the store state it reads. The point of exposing it at all is that
// `openPending` sets `activeId` to null: through `useActiveSession` alone, "a new chat is
// open and unsent" and "nothing is selected" are the same value, and the chat page drew both as
// an empty pane. These tests pin the four facts a consumer of the hook depends on.

const store = () => createChatStore();

describe('pending session — what usePendingSession() reads', () => {
  it('starts null: a cold store has no pending pane until something opens one', () => {
    expect(store().getState().pending).toBeNull();
  });

  it('openPending(opts) carries the instance/harness the first send will create on', () => {
    const s = store();
    s.getState().actions.openPending({ instanceId: 'inst-cc-local', harness: 'claude_code' });
    const pending = s.getState().pending;
    expect(pending?.instanceId).toBe('inst-cc-local');
    expect(pending?.harness).toBe('claude_code');
    expect(pending?.clientId).toMatch(/^pending_/);
  });

  it('openPending(opts) carries the principal the first send will create the session as', () => {
    const s = store();
    s.getState().actions.openPending({ instanceId: 'inst-cc-local', principalId: 'principal_000001' });
    expect(s.getState().pending?.principalId).toBe('principal_000001');
  });

  it('openPending() with nothing recorded still opens a pane — a first run has no prefs', () => {
    const s = store();
    s.getState().actions.openPending();
    const pending = s.getState().pending;
    expect(pending).not.toBeNull();
    // No invented target: the keys are absent rather than undefined, so `createSession`
    // is called with no instance instead of one the user never chose.
    expect(pending && 'instanceId' in pending).toBe(false);
    expect(pending && 'harness' in pending).toBe(false);
  });

  it('openPending nulls activeId — which is why the pending pane needs its own selector', () => {
    const s = store();
    s.getState().actions.setActive('br_old');
    s.getState().actions.openPending({ instanceId: 'inst-cc-local' });
    expect(s.getState().activeId).toBeNull();
    expect(s.getState().pending).not.toBeNull();
  });

  it('setActive alone does NOT clear pending: selecting a session must clear it too', () => {
    // useActiveSession().select() calls setActive AND clearPending. A UI that swaps the
    // active id by any other route leaves a stale pending target behind, and the next
    // lazy create would aim at the instance of a new chat the user walked away from.
    const s = store();
    s.getState().actions.openPending({ instanceId: 'inst-cc-local' });
    s.getState().actions.setActive('br_real');
    expect(s.getState().pending).not.toBeNull();

    s.getState().actions.clearPending();
    expect(s.getState().pending).toBeNull();
    expect(s.getState().activeId).toBe('br_real');
  });
});

// `patchPending` is what lets a controls bar offer model / effort BEFORE the first send.
// The whole reason it exists rather than a second `openPending` call is the clientId.
describe('patchPending — pre-start settings on an open pane', () => {
  it('keeps the clientId — a moved select does not make the pane a different pane', () => {
    const s = store();
    const opened = s.getState().actions.openPending({ harness: 'claude_code' });
    const patched = s.getState().actions.patchPending({ model: 'claude-sonnet-4-6' });
    expect(patched?.clientId).toBe(opened.clientId);
    expect(patched?.harness).toBe('claude_code');
    expect(patched?.model).toBe('claude-sonnet-4-6');
  });

  it('MERGES where openPending replaces — this is why it is a separate action', () => {
    // The load-bearing difference. `openPending({ model })` returns a pane built from
    // that one key, so routing a model pick through it drops the instance the chat was
    // aimed at, the ceiling and the disabled-tool list. Stated here against openPending
    // directly so the contrast cannot rot.
    const s = store();
    const full = { instanceId: 'inst-cc-local', harness: 'claude_code', maxBudget: 12.5 };
    s.getState().actions.openPending(full);
    s.getState().actions.openPending({ model: 'gpt-5' });
    expect(s.getState().pending?.instanceId).toBeUndefined();
    expect(s.getState().pending?.maxBudget).toBeUndefined();

    s.getState().actions.openPending(full);
    s.getState().actions.patchPending({ model: 'gpt-5' });
    expect(s.getState().pending?.instanceId).toBe('inst-cc-local');
    expect(s.getState().pending?.maxBudget).toBe(12.5);
    expect(s.getState().pending?.model).toBe('gpt-5');
  });

  it('leaves a key the patch did not mention alone', () => {
    const s = store();
    s.getState().actions.openPending({
      instanceId: 'inst-cc-local',
      harness: 'claude_code',
      model: 'claude-sonnet-4-6',
      maxBudget: 0,
      disabledTools: [],
    });
    s.getState().actions.patchPending({ effort: 'high' });
    const pending = s.getState().pending;
    expect(pending?.instanceId).toBe('inst-cc-local');
    expect(pending?.model).toBe('claude-sonnet-4-6');
    expect(pending?.effort).toBe('high');
    // The two whose falsy values are real instructions survive an unrelated patch.
    expect(pending?.maxBudget).toBe(0);
    expect(pending?.disabledTools).toEqual([]);
  });

  it('an empty string CLEARS the key rather than doing nothing', () => {
    // '' is the value a select's placeholder row carries. Folding it into "not
    // mentioned" — which is what openPending does — would make a pre-start pick
    // impossible to take back: the user picks "Model", nothing happens, and the
    // config call still sends the model they just cleared.
    const s = store();
    s.getState().actions.openPending({ harness: 'claude_code', model: 'gpt-5', effort: 'max' });
    s.getState().actions.patchPending({ model: '' });
    const pending = s.getState().pending;
    expect(pending && 'model' in pending).toBe(false);
    expect(pending?.effort).toBe('max');
    expect(pending?.harness).toBe('claude_code');
  });

  it('carries maxBudget: 0 and disabledTools: [] rather than dropping them', () => {
    // 0 means NO CEILING and [] means "disable nothing"; a truthiness check reads both
    // as absent and the server silently applies its own default instead.
    const s = store();
    s.getState().actions.openPending({ harness: 'claude_code' });
    s.getState().actions.patchPending({ maxBudget: 0, disabledTools: [] });
    const pending = s.getState().pending;
    expect(pending?.maxBudget).toBe(0);
    expect(pending?.disabledTools).toEqual([]);
  });

  it('does nothing when no pane is pending — a select must not open a chat', () => {
    const s = store();
    expect(s.getState().actions.patchPending({ model: 'gpt-5' })).toBeNull();
    expect(s.getState().pending).toBeNull();
  });

  it('does not select anything: activeId stays where it was', () => {
    // `openPending` nulls activeId because opening a new chat replaces the focus.
    // Configuring one does not, and a patch that re-nulled it would knock the user out
    // of a session they had since clicked into.
    const s = store();
    s.getState().actions.openPending({ harness: 'claude_code' });
    s.getState().actions.setActive('br_real');
    s.getState().actions.patchPending({ model: 'gpt-5' });
    expect(s.getState().activeId).toBe('br_real');
    expect(s.getState().pending?.model).toBe('gpt-5');
  });

  it('a patch that changes nothing keeps the same object, so subscribers do not wake', () => {
    // usePendingSession subscribes to the value's identity. A re-selected identical
    // option would otherwise re-render every consumer of the pane, and a caller
    // patching from an effect would loop.
    const s = store();
    s.getState().actions.openPending({ harness: 'claude_code', model: 'gpt-5', disabledTools: ['Bash'] });
    const before = s.getState().pending;
    s.getState().actions.patchPending({ model: 'gpt-5', disabledTools: ['Bash'] });
    expect(s.getState().pending).toBe(before);
  });
});
