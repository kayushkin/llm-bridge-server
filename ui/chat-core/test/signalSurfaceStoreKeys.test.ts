import { describe, expect, it } from 'vitest';
import { parseSessionRowTypesKey, sessionRowTypesKey } from '../src/react/signals.js';
import { loadedSessionNamesKey, parseLoadedSessionNamesKey } from '../src/react/sessionNames.js';
import { signalFromWire, type Signal } from '../src/net/signals.js';

// The signal surfaces subscribe to these keys instead of the session map, so a
// change to a session they do not show must leave the key equal, and a change to
// one they do show must not. Before the keys, every session-row update re-rendered
// every card on the signals page.

function signal(id: string, sessionId: string, sessionType: string): Signal {
  return signalFromWire({
    id,
    session_id: sessionId,
    session_type: sessionType,
    kind: 'question',
    source: 'tool',
    surface: 'chat',
    title: `${id}?`,
    state: 'open',
    created_at: '2026-10-20T10:00:00Z',
  });
}

describe('sessionRowTypesKey', () => {
  const signals = [signal('s1', 'typed', 'autonomous'), signal('s2', 'untyped', '')];

  it('reads only sessions whose signal carries no type of its own', () => {
    const rows = new Map([
      ['typed', { type: 'interactive' }],
      ['untyped', { type: 'herald' }],
    ]);
    expect(parseSessionRowTypesKey(sessionRowTypesKey(signals, rows))).toEqual(new Map([['untyped', 'herald']]));
  });

  it('stays equal when an unrelated session changes, and moves when a read one does', () => {
    const before = sessionRowTypesKey(signals, new Map([['untyped', { type: 'herald' }]]));
    const unrelated = sessionRowTypesKey(
      signals,
      new Map([['untyped', { type: 'herald' }], ['other', { type: 'autonomous' }]]),
    );
    const retyped = sessionRowTypesKey(signals, new Map([['untyped', { type: 'autonomous' }]]));
    expect(unrelated).toBe(before);
    expect(retyped).not.toBe(before);
  });
});

describe('loadedSessionNamesKey', () => {
  it('keeps only wanted ids the store holds, and survives odd characters in a name', () => {
    const rows = new Map([
      ['a', { displayName: 'tab\there\nand newline' }],
      ['b', { displayName: '' }],
      ['unwanted', { displayName: 'x' }],
    ]);
    const key = loadedSessionNamesKey('a,b,missing', rows);
    expect(parseLoadedSessionNamesKey(key)).toEqual(new Map([['a', 'tab\there\nand newline'], ['b', '']]));
    rows.set('unwanted', { displayName: 'renamed' });
    expect(loadedSessionNamesKey('a,b,missing', rows)).toBe(key);
  });

  it('is empty for no wanted ids', () => {
    expect(parseLoadedSessionNamesKey(loadedSessionNamesKey('', new Map())).size).toBe(0);
  });
});
