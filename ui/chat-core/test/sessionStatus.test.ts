import { describe, expect, it } from 'vitest';
import { createChatStore, type ChatStoreApi } from '../src/store/ChatStore.js';
import { selectSessionStatus } from '../src/store/selectors.js';
import type { SessionStatus, SessionSummary } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

// A session's status reaches the client on two streams in no guaranteed order. These
// tests are the rule that makes that harmless: the larger `as_of` wins, wherever it
// came from and whenever it arrived.

function summary(p: Partial<SessionSummary> & { sessionId: string }): SessionSummary {
  return {
    state: 'idle',
    harness: 'claude_code',
    instanceId: '',
    type: 'interactive',
    purpose: 'chat',
    mode: '',
    folderName: '',
    displayName: p.sessionId,
    agentId: '',
    principalId: '',
    updatedAt: '2026-09-17T12:00:00+00:00',
    createdAt: '2026-09-17T11:00:00+00:00',
    harnessSessionId: '',
    managerSessionId: '',
    ...p,
  };
}

function statusEvent(status: SessionStatus): WireEvent {
  return { id: String(status.as_of), type: 'session_status', data: { type: 'session_status', status } };
}

const BASH_RUNNING: SessionStatus = {
  state: 'tool_running',
  tools: [{ tool_id: 't1', name: 'Bash', summary: 'sleep 600', started_at: '2026-09-17T12:00:01+00:00' }],
  turn_started_at: '2026-09-17T12:00:00+00:00',
  as_of: 100,
};
const PAUSED: SessionStatus = { state: 'paused', changed_at: '2026-09-17T12:00:30+00:00', as_of: 101 };

function seeded(status: SessionStatus): ChatStoreApi {
  const store = createChatStore();
  store.getState().actions.setSessions([summary({ sessionId: 's', state: status.state, status })]);
  return store;
}

describe('a Bash call is in flight and the user presses Stop', () => {
  it('list upsert first, then the stream: paused, no tools', () => {
    const store = seeded(BASH_RUNNING);
    const { actions } = store.getState();
    actions.upsertSession(summary({ sessionId: 's', state: 'paused', status: PAUSED }));
    actions.applyTailEvent('s', statusEvent(PAUSED));
    const status = selectSessionStatus(store.getState(), 's');
    expect(status).toEqual(PAUSED);
    expect(store.getState().sessions.get('s')?.state).toBe('paused');
  });

  it('a LATE copy of the tool_running status cannot undo the pause', () => {
    // The open session's stream replays the current turn on every open, so an older
    // status arriving after a newer one is the ordinary case, not an edge.
    const store = seeded(BASH_RUNNING);
    const { actions } = store.getState();
    actions.applyTailEvent('s', statusEvent(PAUSED));
    actions.applyTailEvent('s', statusEvent(BASH_RUNNING));
    actions.upsertSession(summary({ sessionId: 's', state: 'tool_running', status: BASH_RUNNING }));
    expect(selectSessionStatus(store.getState(), 's')).toEqual(PAUSED);
    expect(store.getState().sessions.get('s')?.state).toBe('paused');
  });

  it('when the pause is the OLDER fact, the tool that started after it wins', () => {
    const resumed: SessionStatus = { ...BASH_RUNNING, as_of: 102 };
    const store = seeded(PAUSED);
    const { actions } = store.getState();
    actions.applyTailEvent('s', statusEvent(resumed));
    actions.upsertSession(summary({ sessionId: 's', state: 'paused', status: PAUSED }));
    expect(selectSessionStatus(store.getState(), 's')).toEqual(resumed);
  });
});

describe('switching away and back', () => {
  it('changes nothing about the status, and a session never opened has one too', () => {
    const store = createChatStore();
    const other: SessionStatus = { state: 'model_generating', generating: 'thinking', as_of: 40 };
    store.getState().actions.setSessions([
      summary({ sessionId: 's', state: 'tool_running', status: BASH_RUNNING }),
      summary({ sessionId: 'never-opened', state: 'model_generating', status: other }),
    ]);
    const { actions } = store.getState();
    actions.setActive('s');
    actions.setActive('never-opened');
    // While 's' is NOT open, the list stream is the only thing that can speak for it.
    actions.upsertSession(summary({ sessionId: 's', state: 'paused', status: PAUSED }));
    actions.setActive('s');
    expect(selectSessionStatus(store.getState(), 's')).toEqual(PAUSED);
    expect(selectSessionStatus(store.getState(), 'never-opened')).toEqual(other);
  });
});

describe('a batch of tail events', () => {
  it('keeps the newest status in the batch whatever order it is in', () => {
    const store = seeded({ state: 'idle', as_of: 1 });
    store.getState().actions.applyTailEvents('s', [statusEvent(PAUSED), statusEvent(BASH_RUNNING)]);
    expect(selectSessionStatus(store.getState(), 's')).toEqual(PAUSED);
  });

  it('a replayed status the row already carries leaves the session rows untouched', () => {
    // The event still folds into the transcript (it is an event like any other);
    // what must not happen is a new `sessions` Map, which re-renders the sidebar.
    const store = seeded(PAUSED);
    const before = store.getState().sessions;
    store.getState().actions.applyTailEvent('s', statusEvent(PAUSED));
    expect(store.getState().sessions).toBe(before);
  });
});

describe('rows with no status of their own', () => {
  it('a row cached before the status existed reads as its bare state, as_of 0', () => {
    const store = createChatStore();
    store.getState().actions.setSessions([summary({ sessionId: 's', state: 'tool_running' })]);
    expect(selectSessionStatus(store.getState(), 's')).toEqual({ state: 'tool_running', as_of: 0 });
    // ...and the first real status replaces it.
    store.getState().actions.applyTailEvent('s', statusEvent(BASH_RUNNING));
    expect(selectSessionStatus(store.getState(), 's')).toEqual(BASH_RUNNING);
  });

  it('an optimistic state change shows at once, and the next server status overrides it', () => {
    const store = seeded(BASH_RUNNING);
    const { actions } = store.getState();
    const held = store.getState().sessions.get('s')!;
    // How every optimistic mutation is written: spread the row, change `state`.
    actions.upsertSession({ ...held, state: 'completed' });
    expect(store.getState().sessions.get('s')?.state).toBe('completed');
    expect(selectSessionStatus(store.getState(), 's')).toEqual({ state: 'completed', as_of: 100 });
    // The server's own answer arrives later with a larger as_of, and is kept.
    const settled: SessionStatus = { state: 'idle', as_of: 105 };
    actions.upsertSession(summary({ sessionId: 's', state: 'idle', status: settled }));
    expect(selectSessionStatus(store.getState(), 's')).toEqual(settled);
  });

  it('a bare upsert naming the SAME state keeps the held status, tools and all', () => {
    const store = seeded(BASH_RUNNING);
    store.getState().actions.upsertSession(summary({ sessionId: 's', state: 'tool_running', displayName: 'renamed' }));
    expect(selectSessionStatus(store.getState(), 's')).toEqual(BASH_RUNNING);
    expect(store.getState().sessions.get('s')?.displayName).toBe('renamed');
  });
});

describe('a session opened by id, which the list does not hold', () => {
  it('takes a status from its own stream through its cached detail', () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.setSessionDetail('off-list', {
      sessionId: 'off-list',
      summary: summary({ sessionId: 'off-list', state: 'idle', status: { state: 'idle', as_of: 5 } }),
      info: null,
      harnessConfig: null,
    });
    actions.applyTailEvent('off-list', statusEvent(BASH_RUNNING));
    expect(selectSessionStatus(store.getState(), 'off-list')).toEqual(BASH_RUNNING);
  });
});

describe('a status that arrives before the session has a row', () => {
  // The open session's stream and the first list page start together, and the stream
  // is often first. The list row can also be the OLDER of the two: it lags a
  // seconds-old session's whole first turn.
  const running: SessionStatus = { state: 'model_generating', generating: 'text', as_of: 498 };

  it('waits, and outranks the lagging row that arrives after it', () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.applyTailEvent('s', statusEvent(running));
    expect(selectSessionStatus(store.getState(), 's')).toBeNull();

    actions.setSessions([summary({ sessionId: 's', state: 'idle' })]);
    expect(selectSessionStatus(store.getState(), 's')).toEqual(running);
    expect(store.getState().sessions.get('s')?.state).toBe('model_generating');
  });

  it('loses to a row whose own status is newer', () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.applyTailEvent('s', statusEvent(running));
    const settled: SessionStatus = { state: 'idle', as_of: 600 };
    actions.upsertSession(summary({ sessionId: 's', state: 'idle', status: settled }));
    expect(selectSessionStatus(store.getState(), 's')).toEqual(settled);
  });

  it('reaches a session opened by id through the detail that arrives after it', () => {
    const store = createChatStore();
    const { actions } = store.getState();
    actions.applyTailEvent('off-list', statusEvent(running));
    actions.setSessionDetail('off-list', {
      sessionId: 'off-list',
      summary: summary({ sessionId: 'off-list', state: 'idle' }),
      info: null,
      harnessConfig: null,
    });
    expect(selectSessionStatus(store.getState(), 'off-list')).toEqual(running);
  });
});
