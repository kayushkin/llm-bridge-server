import { describe, expect, it } from 'vitest';
import { applyEvent, applyEvents, initTailState } from '../src/reduce/TurnReducer.js';
import type { WireEvent } from '../src/net/wireEvents.js';

function userMsg(eventId: number, messageId: string, turnId: string, text: string, source?: 'otel'): WireEvent {
  return {
    id: String(eventId),
    type: 'user_message',
    data: {
      event_id: eventId,
      message_id: messageId,
      turn_id: turnId,
      timestamp: '2026-07-27T14:00:00-07:00',
      result: { text },
      ...(source ? { extensions: { source } } : {}),
    },
  };
}

function streamText(eventId: number, messageId: string, turnId: string, text: string): WireEvent {
  return {
    id: String(eventId),
    type: 'stream',
    data: {
      event_id: eventId,
      message_id: messageId,
      turn_id: turnId,
      timestamp: '2026-07-27T14:00:01-07:00',
      stream: { delta: { type: 'text_delta', text } },
    },
  };
}

function resultMsg(eventId: number, messageId: string, turnId: string, text: string): WireEvent {
  return {
    id: String(eventId),
    type: 'result',
    data: {
      event_id: eventId,
      message_id: messageId,
      turn_id: turnId,
      timestamp: '2026-07-27T14:00:02-07:00',
      result: { text },
    },
  };
}

describe('TurnReducer — live-tail, O(1)-indexed, idempotent', () => {
  it('coalesces stream deltas into one entry and groups by turn', () => {
    let s = initTailState('br_1');
    s = applyEvent(s, userMsg(1, 'm1', 't1', 'hi'));
    s = applyEvent(s, streamText(2, 'm2', 't2', 'Hel'));
    s = applyEvent(s, streamText(3, 'm2', 't2', 'lo'));
    s = applyEvent(s, resultMsg(4, 'm2', 't2', 'Hello'));

    // Two turns: the user prompt (t1) and the assistant turn (t2).
    expect(s.model.turns.map((t) => t.id).sort()).toEqual(['t1', 't2']);

    const streamEntry = s.model.entries['m2_text'];
    expect(streamEntry?.text).toBe('Hello'); // deltas accumulated
    const resultEntry = s.model.entries['m2_result'];
    expect(resultEntry?.text).toBe('Hello');

    // validator tracks the max event id + distinct event count.
    expect(s.model.validator.maxEventId).toBe(4);
    expect(s.model.validator.eventCount).toBe(4);
  });

  it('is idempotent on a repeated event_id (Last-Event-ID replay is a no-op)', () => {
    let s = initTailState('br_1');
    s = applyEvent(s, streamText(2, 'm2', 't2', 'Hel'));
    s = applyEvent(s, streamText(3, 'm2', 't2', 'lo'));
    const before = s;
    // Re-apply the exact same event ids — must not double-append text.
    const after = applyEvent(applyEvent(before, streamText(2, 'm2', 't2', 'Hel')), streamText(3, 'm2', 't2', 'lo'));
    expect(after).toBe(before); // same reference — a true no-op
    expect(after.model.entries['m2_text']?.text).toBe('Hello');
    expect(after.model.validator.eventCount).toBe(2);
  });

  it('indexes turns in a Map (O(1)) so a new entry finds its turn without a scan', () => {
    let s = initTailState('br_1');
    s = applyEvent(s, streamText(1, 'm1', 't1', 'a'));
    // turnIndex is the O(1) lookup structure.
    expect(s.turnIndex.get('t1')).toBe(0);
    s = applyEvent(s, resultMsg(2, 'm1', 't1', 'a'));
    // Same turn — no new turn appended, index still points at 0.
    expect(s.model.turns).toHaveLength(1);
    expect(s.turnIndex.get('t1')).toBe(0);
    expect(s.model.turns[0]!.entryIds.sort()).toEqual(['m1_result', 'm1_text']);
  });

  it('annotates a live-tail dual-emit as it arrives out of order', () => {
    // Prompt (harness), reply, then the LATE OTel prompt copy.
    const s = applyEvents(initTailState('br_1'), [
      userMsg(1, 'm1', 't1', 'question'),
      resultMsg(2, 'm2', 't2', 'answer'),
      userMsg(3, 'm3', 't3', 'question', 'otel'),
    ]);
    const harnessPrompt = s.model.entries['m1_user'];
    const otelPrompt = s.model.entries['m3_user'];
    expect(harnessPrompt?.duplicate).toBe(false);
    expect(otelPrompt?.duplicate).toBe(true);
    expect(otelPrompt?.groupId).toBe(harnessPrompt?.groupId);
    // Nothing dropped: both entries still in the model.
    expect(Object.keys(s.model.entries)).toContain('m3_user');
  });

  it('carries canonical ErrorEvent fields (code/retryable/statusCode) onto the entry', () => {
    const s = applyEvent(initTailState('br_1'), {
      id: '7',
      type: 'error',
      data: {
        event_id: 7,
        message_id: 'me',
        turn_id: 'te',
        timestamp: '2026-07-27T14:00:03-07:00',
        extensions: { source: 'otel' },
        error: { code: 'api_error', message: 'Overloaded', retryable: true, status_code: 529 },
      },
    });
    const e = s.model.entries['me_error'];
    expect(e?.kind).toBe('error');
    expect(e?.text).toBe('Overloaded');
    expect(e?.code).toBe('api_error');
    expect(e?.retryable).toBe(true);
    expect(e?.statusCode).toBe(529);
  });

  it('marks a recovered OTel assistant block with recovered=true and keeps it visible', () => {
    const s = applyEvent(initTailState('br_1'), {
      id: '8',
      type: 'block',
      data: {
        event_id: 8,
        turn_id: 'tr',
        timestamp: '2026-07-27T14:00:04-07:00',
        extensions: { source: 'otel', recovered: true },
        block: { block: { type: 'text', text_block: { text: 'final answer' } } },
      },
    });
    // No message_id → keyed by event id; single copy stays primary/visible (recovery).
    const e = s.model.entries['evt_8'];
    expect(e?.recovered).toBe(true);
    expect(e?.text).toBe('final answer');
    expect(e?.duplicate).toBe(false);
    expect(e?.primary).toBe(true);
  });

  it('does not mutate the input state (pure)', () => {
    const s0 = initTailState('br_1');
    const s1 = applyEvent(s0, streamText(1, 'm1', 't1', 'x'));
    expect(s0.model.turns).toHaveLength(0);
    expect(s1.model.turns).toHaveLength(1);
    expect(s1).not.toBe(s0);
  });
});

describe('subagent task fields cross the wire boundary', () => {
  // WireEventData.system used to be typed as { subtype, message } only, so the
  // correlators and the outcome were reachable only by digging through the
  // untyped raw blob. The timeline dug for task_id and nothing dug for the
  // status at all, which is why a finished subagent never reported.
  //
  // Frame shape is verbatim from a live CC 2.1.220 capture under the flags the
  // bridge runs, marshalled as the canonical msg.Event the server sends.
  function taskNotification(eventId: number, turnId: string): WireEvent {
    return {
      id: String(eventId),
      type: 'system',
      data: {
        event_id: eventId,
        turn_id: turnId,
        timestamp: '2026-07-27T14:00:02-07:00',
        type: 'system',
        system: {
          subtype: 'task_notification',
          task_id: 'a66ecc868c9c5c8db',
          tool_use_id: 'toolu_01PdEBPefCBfgx62jXmkfBvP',
          task_status: 'completed',
          task_summary: 'The command output was: hi',
          task_output_file: '/tmp/claude-1000/x/tasks/a66ecc868c9c5c8db.output',
        },
      },
    };
  }

  it('maps the canonical system fields onto the entry', () => {
    const state = applyEvent(initTailState('s'), taskNotification(1, 't1'));
    const entry = Object.values(state.model.entries)[0]!;
    expect(entry.subtype).toBe('task_notification');
    expect(entry.taskId).toBe('a66ecc868c9c5c8db');
    expect(entry.toolUseId).toBe('toolu_01PdEBPefCBfgx62jXmkfBvP');
    expect(entry.taskStatus).toBe('completed');
    expect(entry.taskSummary).toBe('The command output was: hi');
    expect(entry.taskOutputFile).toBe('/tmp/claude-1000/x/tasks/a66ecc868c9c5c8db.output');
  });

  it('marks a tool event with no tool_id unpairable, because nothing can ever join it', () => {
    // OTel-derived tool stand-ins arrive with no tool id by design. Keyed by
    // event id, a call and its result become two entries, so the call would
    // render "running" forever.
    const noId: WireEvent = {
      id: '2',
      type: 'tool_call',
      data: {
        event_id: 2,
        turn_id: 't1',
        timestamp: '2026-07-27T14:00:03-07:00',
        type: 'tool_call',
        tool_call: { name: 'Bash' },
      },
    };
    const withId: WireEvent = {
      id: '3',
      type: 'tool_call',
      data: {
        event_id: 3,
        turn_id: 't1',
        timestamp: '2026-07-27T14:00:04-07:00',
        type: 'tool_call',
        tool_call: { tool_id: 'toolu_real', name: 'Bash' },
      },
    };
    const state = applyEvents(initTailState('s'), [noId, withId]);
    const entries = Object.values(state.model.entries);
    expect(entries.find((e) => e.id.startsWith('evt_'))?.unpairable).toBe(true);
    expect(entries.find((e) => e.id === 'tool_toolu_real')?.unpairable).toBe(false);
  });
});
