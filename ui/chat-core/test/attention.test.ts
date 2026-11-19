import { describe, expect, it } from 'vitest';
import {
  isAwaitingHumanState,
  isOperatorSessionType,
  isSessionAwaitingOperator,
  sessionTypeOfSignal,
  splitSignalsByAttention,
} from '../src/net/attention.js';
import { signalFromWire, type Signal } from '../src/net/signals.js';

// The one rule for "is this the operator's to answer". Every surface that marks or
// counts what needs the operator reads it, so a wrong answer here is wrong
// everywhere at once — which is the point of having one.

function signal(id: string, sessionId: string, sessionType: string | undefined, kind = 'question'): Signal {
  return signalFromWire({
    id,
    session_id: sessionId,
    session_type: sessionType,
    kind,
    source: 'tool',
    surface: sessionType === 'autonomous' ? 'kanban' : 'chat',
    title: `${id}?`,
    state: 'open',
    created_at: '2026-10-19T10:00:00Z',
  });
}

const noSessionRows = (): string | undefined => undefined;

describe('isOperatorSessionType', () => {
  it('is true for interactive and herald sessions only', () => {
    expect(isOperatorSessionType('interactive')).toBe(true);
    expect(isOperatorSessionType('herald')).toBe(true);
    for (const background of ['autonomous', 'system', 'external']) {
      expect(isOperatorSessionType(background), background).toBe(false);
    }
  });

  it('treats an empty or unheard-of type as background, never as the operator’s', () => {
    expect(isOperatorSessionType('')).toBe(false);
    expect(isOperatorSessionType('some-future-type')).toBe(false);
  });
});

describe('isSessionAwaitingOperator', () => {
  it('needs both an awaiting state and an operator session type', () => {
    expect(isSessionAwaitingOperator({ type: 'interactive', state: 'awaiting_user' })).toBe(true);
    expect(isSessionAwaitingOperator({ type: 'herald', state: 'awaiting_user' })).toBe(true);
    expect(isSessionAwaitingOperator({ type: 'interactive', state: 'awaiting_permission' })).toBe(true);
    // A reaped autoworker that ended its turn on a question keeps `awaiting_user`.
    expect(isSessionAwaitingOperator({ type: 'autonomous', state: 'awaiting_user' })).toBe(false);
    expect(isSessionAwaitingOperator({ type: 'system', state: 'awaiting_permission' })).toBe(false);
    expect(isSessionAwaitingOperator({ type: '', state: 'awaiting_user' })).toBe(false);
    expect(isSessionAwaitingOperator({ type: 'interactive', state: 'completed' })).toBe(false);
  });

  it('counts only the two awaiting states as waiting on a human', () => {
    expect(isAwaitingHumanState('awaiting_user')).toBe(true);
    expect(isAwaitingHumanState('awaiting_permission')).toBe(true);
    expect(isAwaitingHumanState('paused')).toBe(false);
    expect(isAwaitingHumanState('rate_limited')).toBe(false);
  });
});

describe('sessionTypeOfSignal', () => {
  it('uses the type the signal carries, even when the session row says otherwise', () => {
    const s = signal('sig-1', 'br_a', 'interactive');
    expect(sessionTypeOfSignal(s, () => 'autonomous')).toBe('interactive');
  });

  it('joins a typeless signal to its session row', () => {
    const s = signal('sig-1', 'br_a', undefined);
    expect(sessionTypeOfSignal(s, (id) => (id === 'br_a' ? 'herald' : undefined))).toBe('herald');
  });

  it('answers empty when neither the signal nor a session row knows', () => {
    expect(sessionTypeOfSignal(signal('sig-1', 'br_a', undefined), noSessionRows)).toBe('');
  });
});

describe('splitSignalsByAttention', () => {
  it('puts every signal in exactly one group and keeps the server’s order in each', () => {
    const signals = [
      signal('auto-1', 'autoworker-1', 'autonomous', 'notification'),
      signal('chat-1', 'br_chat', 'interactive'),
      signal('herald-1', 'herald-1', 'herald'),
      signal('sys-1', 'br_sub', 'system'),
      signal('ext-1', 'br_ext', 'external', 'notification'),
      signal('chat-2', 'br_chat', 'interactive', 'notification'),
      signal('unknown-1', 'br_gone', undefined),
    ];
    const { operatorSignals, backgroundSignals } = splitSignalsByAttention(signals, noSessionRows);
    expect(operatorSignals.map((s) => s.id)).toEqual(['chat-1', 'herald-1', 'chat-2']);
    expect(backgroundSignals.map((s) => s.id)).toEqual(['auto-1', 'sys-1', 'ext-1', 'unknown-1']);
    expect(operatorSignals.length + backgroundSignals.length).toBe(signals.length);
  });

  it('moves a typeless signal to the operator only when its session row is interactive or herald', () => {
    const signals = [signal('a', 'br_chat', undefined), signal('b', 'autoworker-1', undefined)];
    const types: Record<string, string> = { br_chat: 'interactive', 'autoworker-1': 'autonomous' };
    const { operatorSignals, backgroundSignals } = splitSignalsByAttention(signals, (id) => types[id]);
    expect(operatorSignals.map((s) => s.id)).toEqual(['a']);
    expect(backgroundSignals.map((s) => s.id)).toEqual(['b']);
  });
});
