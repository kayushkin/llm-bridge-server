// Images a tool returned, and files shared into a session, on both paths: folded
// live from the stream, and read from a log-store page. log-store's own tests pin the
// page side of the wire (entry_images_test.go); these pin what the client makes of it.
import { describe, expect, it } from 'vitest';
import { applyEvent, initTailState, type TailState } from '../src/reduce/TurnReducer.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import type { Entry, TurnModel } from '../src/net/types.js';

let nextId = 1;

function ev(type: string, data: Record<string, unknown> = {}): WireEvent {
  const id = nextId++;
  return {
    id: String(id),
    type,
    data: { event_id: id, type, turn_id: 'turn1', timestamp: '2026-09-27T12:00:00Z', ...data },
  };
}

function apply(events: WireEvent[]): TailState {
  let s = initTailState('br_1');
  for (const e of events) s = applyEvent(s, e);
  return s;
}

const entries = (s: TailState): Entry[] => Object.values(s.model.entries);

describe('a tool result that carried an image', () => {
  it('folds live with the bytes, on the tool row', () => {
    const s = apply([
      ev('tool_call', { tool_call: { tool_id: 't1', name: 'Read', input: { file_path: '/tmp/a.png' } } }),
      ev('tool_result', {
        tool_result: {
          tool_id: 't1',
          output: '',
          content: [{ type: 'image', image_block: { source: { kind: 'base64', media_type: 'image/png', data: 'iVBORw0KGgo=' } } }],
        },
      }),
    ]);
    const tool = entries(s).find((e) => e.toolId === 't1');
    expect(tool?.toolResultImages).toEqual([{ index: 0, mediaType: 'image/png', base64Data: 'iVBORw0KGgo=' }]);
  });

  it('does not draw an image a tool gave by URL', () => {
    const s = apply([
      ev('tool_result', {
        tool_result: {
          tool_id: 't2',
          content: [{ type: 'image', image_block: { source: { kind: 'url', media_type: 'image/png', data: 'https://example.com/x.png' } } }],
        },
      }),
    ]);
    expect(entries(s).find((e) => e.toolId === 't2')?.toolResultImages).toBeUndefined();
  });

  it('on a page, each image is stamped with the event its bytes are under', () => {
    const page: TurnModel = {
      sessionId: 'br_1',
      turns: [{ id: 'turn1', entryIds: ['e_41', 'e_42'] } as unknown as TurnModel['turns'][number]],
      entries: {
        e_41: { id: 'e_41', turnId: 'turn1', role: 'tool', kind: 'tool_call', source: 'harness', eventId: 41, ts: '', toolId: 't3' },
        e_42: {
          id: 'e_42', turnId: 'turn1', role: 'tool', kind: 'tool_result', source: 'harness', eventId: 42, ts: '', toolId: 't3',
          toolResultImages: [{ index: 0, mediaType: 'image/png' }],
        },
      },
      validator: { maxEventId: 42, eventCount: 2, updatedAt: '' },
      more: false,
    };
    const s = initTailState('br_1', page);
    expect(s.model.entries.e_42.toolResultImages).toEqual([{ index: 0, mediaType: 'image/png', eventId: 42 }]);
  });
});

describe('a file shared into the session', () => {
  const file = (sharedBy: 'user' | 'agent') => ({
    file_id: 'file_000001', session_id: 'br_1', filename: 'chart.png', media_type: 'image/png',
    size_bytes: 8, shared_by: sharedBy, path: '/x/chart.png', created_at: '2026-09-27T12:00:00Z',
  });

  it('is a file entry said by whoever shared it', () => {
    for (const [sharedBy, role] of [['user', 'user'], ['agent', 'assistant']] as const) {
      const s = apply([ev('session_file', { session_file: file(sharedBy) })]);
      const entry = entries(s).find((e) => e.kind === 'file');
      expect(entry?.role).toBe(role);
      expect(entry?.sessionFile?.file_id).toBe('file_000001');
    }
  });

  it('two shares are two entries', () => {
    const s = apply([
      ev('session_file', { session_file: { ...file('user'), file_id: 'file_000001' } }),
      ev('session_file', { session_file: { ...file('user'), file_id: 'file_000002' } }),
    ]);
    expect(entries(s).filter((e) => e.kind === 'file')).toHaveLength(2);
  });
});
