// A button an agent offered, and the steps of its one run: each is its own
// session_action event carrying the whole record.
import { describe, expect, it } from 'vitest';
import { applyEvent, initTailState, type TailState } from '../src/reduce/TurnReducer.js';
import { newestSessionActions } from '../src/reduce/sessionActions.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import type { Entry, SessionAction, SessionActionState } from '../src/net/types.js';

let nextId = 1;

function ev(type: string, data: Record<string, unknown> = {}): WireEvent {
  const id = nextId++;
  return {
    id: String(id),
    type,
    data: { event_id: id, type, turn_id: 'turn1', timestamp: '2026-09-28T12:00:00Z', ...data },
  };
}

function apply(events: WireEvent[]): TailState {
  let s = initTailState('br_1');
  for (const e of events) s = applyEvent(s, e);
  return s;
}

const entries = (s: TailState): Entry[] => Object.values(s.model.entries);

const action = (actionId: string, state: SessionActionState): SessionAction => ({
  action_id: actionId,
  session_id: 'br_1',
  offer: { label: 'Deploy dash', type: 'deploy', repo_id: 12 },
  command: 'run `bash -l -c ./deploy.sh` in /repos/dash (repo-store repo 12, dash)',
  state,
  offered_at: '2026-09-28T12:00:00Z',
});

describe('a session action', () => {
  it('folds each event into its own action entry, the offer said by the agent', () => {
    const s = apply([
      ev('session_action', { session_action: action('session_action_000001', 'offered') }),
      ev('session_action', { session_action: action('session_action_000001', 'running') }),
      ev('session_action', { session_action: action('session_action_000001', 'succeeded') }),
    ]);
    const actionEntries = entries(s).filter((e) => e.kind === 'action');
    expect(actionEntries.map((e) => [e.role, e.sessionAction?.state])).toEqual([
      ['assistant', 'offered'],
      ['system', 'running'],
      ['system', 'succeeded'],
    ]);
  });

  it('reads its current state from its newest record, whatever order entries come in', () => {
    const s = apply([
      ev('session_action', { session_action: action('session_action_000001', 'offered') }),
      ev('session_action', { session_action: action('session_action_000002', 'offered') }),
      ev('session_action', { session_action: action('session_action_000001', 'running') }),
      ev('session_action', { session_action: action('session_action_000001', 'failed') }),
    ]);
    const newest = newestSessionActions(entries(s).reverse());
    expect(newest.get('session_action_000001')?.state).toBe('failed');
    expect(newest.get('session_action_000002')?.state).toBe('offered');
  });
});

describe('a session action id written in text', () => {
  it('becomes an action chip, and the agent prose that carries it places the button', async () => {
    const { parseRefChips } = await import('../src/reduce/refChips.js');
    const { sessionActionIdsPlacedInProse } = await import('../src/reduce/sessionActions.js');
    expect(parseRefChips('Ready: session_action_000007 or `session_action_000008`.').filter((s) => s.type === 'chip')).toEqual([
      { type: 'chip', kind: 'action', refId: 'session_action_000007' },
      { type: 'chip', kind: 'action', refId: 'session_action_000008' },
    ]);
    // Too short to be an id, and part of a longer word: plain text.
    expect(parseRefChips('session_action_12 and xsession_action_000009').every((s) => s.type === 'text')).toBe(true);

    const s = apply([
      ev('result', { message_id: 'm1', result: { text: 'Press session_action_000007 to deploy.' } }),
      ev('user_message', { message_id: 'm2', result: { text: 'what is session_action_000008?' } }),
    ]);
    const placed = sessionActionIdsPlacedInProse(entries(s));
    expect([...placed]).toEqual(['session_action_000007']);
  });
});
