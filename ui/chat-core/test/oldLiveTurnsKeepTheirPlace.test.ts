// A turn the stream built stays "live" for good when it holds an entry no page ever
// reports: the per-second thinking-token ticks and status changes are id-less and the
// projected page carries none of them. Once the session grows past the 30-turn page,
// a refetch no longer covers that turn, and the merge used to file it with the live
// tail — below the page. Measured on br_1791424129618218898 (2026-10-08): at 33 turns
// its first three exchanges were drawn under the newest ones.
import { describe, expect, it } from 'vitest';
import { initTailState, mergeMaterializedPage } from '../src/reduce/TurnReducer.js';
import type { Entry, Turn, TurnModel } from '../src/net/types.js';

const SID = 'br_test';

function entry(id: string, turnId: string, ts: string, origin: 'live' | 'page', extra: Partial<Entry> = {}): Entry {
  return {
    id,
    turnId,
    role: 'user',
    kind: 'text',
    source: 'harness',
    eventId: 1,
    ts,
    origin,
    duplicate: false,
    primary: true,
    ...extra,
  } as Entry;
}

function model(turns: Turn[], entries: Entry[]): TurnModel {
  return {
    sessionId: SID,
    turns,
    entries: Object.fromEntries(entries.map((e) => [e.id, e])),
    validator: { maxEventId: 9, eventCount: entries.length, updatedAt: '' },
    more: true,
  };
}

describe('a live turn older than the refetched page', () => {
  it('stays above the page instead of sinking below it', () => {
    // Prior: three turns the stream built. Turn A carries a thinking-token tick
    // the page will never report.
    const prior = initTailState(
      SID,
      model(
        [
          { id: 'turn_A', role: 'user', ts: '2026-10-08T01:48:49.100Z', entryIds: ['live_A_prompt', 'live_A_tick'] },
          { id: 'turn_B', role: 'user', ts: '2026-10-08T02:00:00.100Z', entryIds: ['live_B_prompt'] },
          { id: 'turn_C', role: 'user', ts: '2026-10-08T08:41:39.100Z', entryIds: ['live_C_prompt'] },
        ],
        [
          entry('live_A_prompt', 'turn_A', '2026-10-08T01:48:49.100Z', 'live', { text: 'first question', messageId: 'msg_A' }),
          entry('live_A_tick', 'turn_A', '2026-10-08T01:48:50.300Z', 'live', {
            role: 'system',
            kind: 'system',
            subtype: 'thinking_tokens',
          }),
          entry('live_B_prompt', 'turn_B', '2026-10-08T02:00:00.100Z', 'live', { text: 'second question', messageId: 'msg_B' }),
          entry('live_C_prompt', 'turn_C', '2026-10-08T08:41:39.100Z', 'live', { text: 'newest question', messageId: 'msg_C' }),
        ],
      ),
    );

    // The refetched window no longer reaches turn A.
    const page = model(
      [
        { id: 'turn_B', role: 'user', ts: '2026-10-08T02:00:00Z', entryIds: ['e_B'] },
        { id: 'turn_C', role: 'user', ts: '2026-10-08T08:41:39Z', entryIds: ['e_C'] },
      ],
      [
        entry('e_B', 'turn_B', '2026-10-08T02:00:00Z', 'page', { text: 'second question', messageId: 'msg_B' }),
        entry('e_C', 'turn_C', '2026-10-08T08:41:39Z', 'page', { text: 'newest question', messageId: 'msg_C' }),
      ],
    );

    const merged = mergeMaterializedPage(prior, page);
    expect(merged.model.turns.map((t) => t.id)).toEqual(['turn_A', 'turn_B', 'turn_C']);
  });

  it('still puts a live turn newer than the page at the bottom', () => {
    const prior = initTailState(
      SID,
      model(
        [{ id: 'turn_D', role: 'user', ts: '2026-10-08T09:00:00.100Z', entryIds: ['live_D_prompt'] }],
        [entry('live_D_prompt', 'turn_D', '2026-10-08T09:00:00.100Z', 'live', { text: 'just sent', messageId: 'msg_D' })],
      ),
    );
    const page = model(
      [{ id: 'turn_C', role: 'user', ts: '2026-10-08T08:41:39Z', entryIds: ['e_C'] }],
      [entry('e_C', 'turn_C', '2026-10-08T08:41:39Z', 'page', { text: 'newest question', messageId: 'msg_C' })],
    );

    const merged = mergeMaterializedPage(prior, page);
    expect(merged.model.turns.map((t) => t.id)).toEqual(['turn_C', 'turn_D']);
  });
});
