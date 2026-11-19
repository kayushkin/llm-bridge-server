import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { ManagedSessionDetail, SessionSummary } from '../src/net/types.js';
import type { SessionInfoWire, WireEvent } from '../src/net/wireEvents.js';

// The claudecode harness sends SessionInfo at init and again once Claude Code reports
// its effort. The second one moves no turn, and the detail was fetched once, so without
// the live fold the chat keeps whatever info it fetched until a reload.

const SUMMARY = {
  sessionId: 's',
  state: 'idle',
  harness: 'claude_code',
} as SessionSummary;

function detail(): ManagedSessionDetail {
  return { sessionId: 's', summary: SUMMARY, info: { model: 'claude-opus-5-5' }, harnessConfig: null };
}

function infoEvent(id: number, info: SessionInfoWire): WireEvent {
  return { id: String(id), type: 'session_info', data: { type: 'session_info', info } };
}

describe('a live session_info event', () => {
  it('replaces the fetched info, the newest in a batch winning', () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setSessionDetail('s', detail());
    actions.applyTailEvents('s', [
      infoEvent(1, { model: 'claude-opus-5-5' }),
      infoEvent(2, { model: 'claude-opus-5-5', effort: 'medium' }),
    ]);
    expect(store.getState().sessionDetail.get('s')?.info?.effort).toBe('medium');
  });

  it('is dropped for a session whose detail was never fetched', () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('s', infoEvent(1, { effort: 'high' }));
    expect(store.getState().sessionDetail.has('s')).toBe(false);
  });
});
