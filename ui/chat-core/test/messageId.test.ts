// messageId on the live path — the wire half of the per-message Turns design
// (dash docs/chat-turns-per-message.md §2). Every entry that belongs to a
// chat message carries its canonical message_id as a FIELD, including tool
// entries whose key (`tool_<toolId>`) does not encode it. The materialized
// path (log-store Entry.messageId) is pinned by log-store's own
// TestBuildTurnModel_MessageIDOnTheWire; together they are the "both paths
// agree" contract.
import { describe, expect, it } from 'vitest';
import { applyEvent, initTailState, type TailState } from '../src/reduce/TurnReducer.js';
import type { WireEvent } from '../src/net/wireEvents.js';

let nextId = 1;

function ev(type: string, data: Record<string, unknown> = {}): WireEvent {
  const id = nextId++;
  return {
    id: String(id),
    type,
    data: {
      event_id: id,
      type,
      turn_id: 'turn1',
      timestamp: `2026-08-24T14:00:${String(id % 60).padStart(2, '0')}-07:00`,
      ...data,
    },
  };
}

function apply(events: WireEvent[]): TailState {
  let s = initTailState('br_1');
  for (const e of events) s = applyEvent(s, e);
  return s;
}

const entryWithMessageId = (s: TailState, messageId: string, kind: string) =>
  Object.values(s.model.entries).find((e) => e.messageId === messageId && e.kind === kind);

describe('messageId on live entries', () => {
  it('text, thinking and user entries carry it', () => {
    const s = apply([
      ev('user_message', { message_id: 'msg_u', result: { text: 'go' } }),
      ev('block', {
        message_id: 'msg_a',
        block: { block: { type: 'thinking', thinking_block: { text: 'hm' } } },
      }),
      ev('block', {
        message_id: 'msg_a',
        block: { block: { type: 'text', text_block: { text: 'Now the change:' } } },
      }),
      ev('result', { message_id: 'msg_b', result: { text: 'done' } }),
    ]);
    expect(entryWithMessageId(s, 'msg_u', 'text')).toBeDefined();
    expect(entryWithMessageId(s, 'msg_a', 'thinking')).toBeDefined();
    expect(entryWithMessageId(s, 'msg_a', 'text')?.text).toBe('Now the change:');
    expect(entryWithMessageId(s, 'msg_b', 'result')).toBeDefined();
  });

  it('a TOOL entry carries the CALL message id, and the result does not overwrite it', () => {
    // The key is `tool_<toolId>` — it does not encode the message — and the
    // result event arrives under a DIFFERENT message id. "Did this message
    // tool?" is asked of the call's message, so the call's id must win.
    const s = apply([
      ev('tool_call', {
        message_id: 'msg_call',
        tool_call: { tool_id: 't1', name: 'Edit', input: {} },
      }),
      ev('tool_result', {
        message_id: 'msg_other',
        tool_result: { tool_id: 't1', name: 'Edit', output: 'ok' },
      }),
    ]);
    const tool = s.model.entries['tool_t1'];
    expect(tool).toBeDefined();
    expect(tool?.messageId).toBe('msg_call');
  });

  it('the call id wins even when the result arrived first', () => {
    const s = apply([
      ev('tool_result', {
        message_id: 'msg_other',
        tool_result: { tool_id: 't1', name: 'Edit', output: 'ok' },
      }),
      ev('tool_call', {
        message_id: 'msg_call',
        tool_call: { tool_id: 't1', name: 'Edit', input: {} },
      }),
    ]);
    expect(s.model.entries['tool_t1']?.messageId).toBe('msg_call');
  });

  it('an event with no message_id yields an entry with none — never invented', () => {
    const s = apply([ev('system', { system: { subtype: 'compact_boundary', message: 'x' } })]);
    for (const e of Object.values(s.model.entries)) {
      expect(e.messageId).toBeUndefined();
    }
  });
});

describe('turn attachment for turn-less events', () => {
  it('a turn-less event attaches to the CURRENT turn, matching the materializer', () => {
    // ⚠️ One turn-less `system` event arrives ~100ms into every real turn. The old
    // rule gave it a solo turn that sat AFTER the real turn forever, so everything
    // keyed on "the last turn" — streaming indicator, live narration aside —
    // pointed at bookkeeping instead of the turn actually running. log-store's
    // buildTurns carries the current turn forward; the live path must agree.
    const s = apply([
      ev('user_message', { message_id: 'msg_u', result: { text: 'go' } }),
      { id: '3', type: 'system', data: { event_id: 3, type: 'system', timestamp: '2026-08-25T00:00:03Z', system: { subtype: 'init' } } } as never,
      ev('block', {
        message_id: 'msg_a',
        block: { block: { type: 'text', text_block: { text: 'working' } } },
      }),
    ]);
    expect(s.model.turns).toHaveLength(1);
    expect(s.model.turns[0]!.id).toBe('turn1');
  });

  it('a turn-less event with NO open turn still gets a solo turn', () => {
    const s = apply([
      { id: '9', type: 'system', data: { event_id: 9, type: 'system', timestamp: '2026-08-25T00:00:09Z', system: { subtype: 'init' } } } as never,
    ]);
    expect(s.model.turns).toHaveLength(1);
    expect(s.model.turns[0]!.id).toMatch(/^solo_/);
  });
});
