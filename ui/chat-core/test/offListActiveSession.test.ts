import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import {
  activeSummary,
  selectSessionStatus,
  sessionSummaryFor,
} from '../src/store/selectors.js';
import type { ManagedSessionDetail, SessionSummary } from '../src/net/types.js';

// A session opened BY ID — a `[session:…]` reference chip, a `?session=` deeplink
// older than the loaded list window — has no row in the `sessions` list Map, and
// `setSessions` rebuilds that Map from whatever page each refresh returns. The
// summary warmed on open therefore must live somewhere a refresh does not rebuild:
// the `sessionDetail` cache, with `activeSummary` falling back to it.
//
// The bug this pins was measured on dash 2026-09-01: click an orchestrator chip to
// an old session, the transcript loads and the header shows the name — until the
// list refresh lands, at which point the header blanks to `—` and every summary-fed
// control (rename, nav arrows, status dot, cost chip) goes with it. The warm used
// to write `upsertSession`, i.e. into exactly the Map the refresh rebuilds.

function summary(over: Partial<SessionSummary> & Pick<SessionSummary, 'sessionId'>): SessionSummary {
  return {
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'inst1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: over.sessionId,
    agentId: '',
    updatedAt: '2026-07-27T10:00:00-07:00',
    createdAt: '2026-07-27T10:00:00-07:00',
    ...over,
  };
}

function detailOf(s: SessionSummary): ManagedSessionDetail {
  return { sessionId: s.sessionId, summary: s, info: null, harnessConfig: null };
}

const OFF_LIST = summary({ sessionId: 'br_old', displayName: 'missing CC message' });

describe('active session outside the loaded list — summary survives the list refresh', () => {
  it('activeSummary falls back to the cached detail when the list Map lacks the session', () => {
    const s = createChatStore();
    s.getState().actions.setActive('br_old');
    s.getState().actions.setSessionDetail('br_old', detailOf(OFF_LIST));
    expect(activeSummary(s.getState())?.displayName).toBe('missing CC message');
  });

  it('setSessions with a page that omits the active session does not blank it', () => {
    const s = createChatStore();
    s.getState().actions.setActive('br_old');
    s.getState().actions.setSessionDetail('br_old', detailOf(OFF_LIST));
    // The refresh: a full page of OTHER sessions, exactly what a live sidebar loads.
    s.getState().actions.setSessions([summary({ sessionId: 'br_recent' })]);
    expect(activeSummary(s.getState())?.displayName).toBe('missing CC message');
    expect(activeSummary(s.getState())?.displayName).toBe('missing CC message');
    expect(selectSessionStatus(s.getState(), 'br_old')?.state).toBe('idle');
  });

  it('a list row wins over the cached detail while it exists — live upserts stay fresh', () => {
    const s = createChatStore();
    s.getState().actions.setActive('br_old');
    s.getState().actions.setSessionDetail('br_old', detailOf(OFF_LIST));
    // An SSE upsert lands in the list Map with a newer state; the point-in-time
    // detail must not shadow it.
    s.getState().actions.upsertSession({ ...OFF_LIST, state: 'tool_running' });
    expect(activeSummary(s.getState())?.state).toBe('tool_running');
    // The refresh evicts the row again; the fallback is stale on `state` but
    // present, which is the honest trade — a name and controls over a blank pane.
    s.getState().actions.setSessions([summary({ sessionId: 'br_recent' })]);
    expect(sessionSummaryFor(s.getState(), 'br_old')?.state).toBe('idle');
  });

  it('removeSession drops the cached detail too — no fallback to a deleted session', () => {
    const s = createChatStore();
    s.getState().actions.setActive('br_old');
    s.getState().actions.setSessionDetail('br_old', detailOf(OFF_LIST));
    s.getState().actions.removeSession('br_old');
    expect(sessionSummaryFor(s.getState(), 'br_old')).toBeNull();
  });
});
