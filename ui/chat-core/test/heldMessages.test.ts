import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { applyEvent, initTailState } from '../src/reduce/TurnReducer.js';
import type { WireEvent } from '../src/net/wireEvents.js';

function answering(status: number, body: unknown): typeof fetch {
  return (async () =>
    ({
      ok: status >= 200 && status < 300,
      status,
      statusText: '',
      json: async () => body,
      text: async () => JSON.stringify(body),
    }) as Response) as typeof fetch;
}

describe('a message the server held before a restart', () => {
  it('reads a held send answer as held, with its id', async () => {
    const api = new ApiClient({
      fetch: answering(202, { status: 'held', held_message_id: 'held_01TEST' }),
      basePath: '/api/bridge',
    });
    expect(await api.send('br_1', 'hello')).toEqual({ status: 'held', messageId: undefined, heldMessageId: 'held_01TEST' });
  });

  it('reads an ordinary send answer as sent, with its message id', async () => {
    const api = new ApiClient({ fetch: answering(200, { status: 'sent', message_id: 'msg_1' }), basePath: '/api/bridge' });
    expect(await api.send('br_1', 'hello')).toEqual({ status: 'sent', messageId: 'msg_1', heldMessageId: undefined });
  });

  it('carries the held message id and text onto the system entry', () => {
    const held: WireEvent = {
      id: '1',
      type: 'system',
      data: {
        event_id: 1,
        timestamp: '2026-11-03T14:00:00-08:00',
        type: 'system',
        system: { subtype: 'message_held_for_restart', held_message_id: 'held_01TEST', message: 'please continue' },
      },
    };
    const entry = Object.values(applyEvent(initTailState('s'), held).model.entries)[0]!;
    expect(entry.subtype).toBe('message_held_for_restart');
    expect(entry.heldMessageId).toBe('held_01TEST');
    expect(entry.text).toBe('please continue');
  });
});
