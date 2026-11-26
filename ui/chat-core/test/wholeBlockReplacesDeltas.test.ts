import { describe, expect, it } from 'vitest';
import { applyEvent, initTailState } from '../src/reduce/TurnReducer.js';
import type { WireEvent } from '../src/net/wireEvents.js';

function streamText(eventId: number, text: string): WireEvent {
  return {
    id: String(eventId),
    type: 'stream',
    data: { event_id: eventId, message_id: 'm2', turn_id: 't2', timestamp: '2026-07-27T14:00:01-07:00', stream: { delta: { type: 'text_delta', text } } },
  };
}

function wholeTextBlock(eventId: number, text: string): WireEvent {
  return {
    id: String(eventId),
    type: 'block',
    data: { event_id: eventId, message_id: 'm2', turn_id: 't2', timestamp: '2026-07-27T14:00:02-07:00', block: { block: { type: 'text', text_block: { text } } } },
  };
}

describe('a whole text block after its deltas', () => {
  it('replaces the text the deltas built, so the reply shows once', () => {
    let s = initTailState('br_1');
    s = applyEvent(s, streamText(1, 'Hel'));
    s = applyEvent(s, streamText(2, 'lo'));
    s = applyEvent(s, wholeTextBlock(3, 'Hello'));
    expect(s.model.entries['m2_text']?.text).toBe('Hello');
  });

  it('keeps earlier whole blocks and replaces only the deltas since the last one', () => {
    let s = initTailState('br_1');
    s = applyEvent(s, streamText(1, 'One.'));
    s = applyEvent(s, wholeTextBlock(2, 'One.'));
    s = applyEvent(s, streamText(3, ' Tw'));
    expect(s.model.entries['m2_text']?.text).toBe('One. Tw');
    s = applyEvent(s, streamText(4, 'o.'));
    s = applyEvent(s, wholeTextBlock(5, ' Two.'));
    expect(s.model.entries['m2_text']?.text).toBe('One. Two.');
  });

  it('adds a whole block that came with no deltas', () => {
    let s = initTailState('br_1');
    s = applyEvent(s, wholeTextBlock(1, 'A.'));
    s = applyEvent(s, wholeTextBlock(2, ' B.'));
    expect(s.model.entries['m2_text']?.text).toBe('A. B.');
  });
});
