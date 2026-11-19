import { describe, expect, it } from 'vitest';
import { carryForwardReasoning } from '../src/reduce/TurnReducer.js';
import type { Entry, TurnModel } from '../src/net/types.js';

// Reasoning survives a reconcile.
//
// ⚠️ The premise, measured rather than assumed: reasoning TEXT never reaches storage,
// and the loss is upstream of this whole stack. Claude Code's own transcript records
// thinking blocks as `thinking: ""` plus a signature — that reasoning happened, signed,
// never what it said. log-store holds 98,959 thinking blocks of which ZERO carry text,
// and text last appeared in quantity in 2026-04.
//
// So the live stream is the only place a session's reasoning exists, and `setTurns`
// replacing the live model with a materialized page deleted it. That is the reported
// bug in all three of its shapes: the aside vanishing when the final text lands, an open
// aside collapsing when more text streams in, and reasoning appearing to stop at the
// latest block.

function entry(over: Partial<Entry> & Pick<Entry, 'id' | 'kind' | 'turnId'>): Entry {
  return {
    role: 'assistant',
    source: 'harness',
    ts: '2026-08-23T05:00:00+00:00',
    eventId: 1,
    duplicate: false,
    primary: true,
    ...over,
  } as Entry;
}

function model(entries: Entry[], turnIds = ['t1']): TurnModel {
  const dict: Record<string, Entry> = {};
  for (const e of entries) dict[e.id] = e;
  return {
    sessionId: 's',
    turns: turnIds.map((id) => ({
      id,
      role: 'user',
      ts: '2026-08-23T05:00:00+00:00',
      entryIds: entries.filter((e) => e.turnId === id).map((e) => e.id),
    })),
    entries: dict,
    validator: { maxEventId: 9, eventCount: entries.length, updatedAt: '' },
    more: false,
  } as TurnModel;
}

describe('carryForwardReasoning', () => {
  it('keeps live reasoning the materialized page does not carry', () => {
    const live = model([
      entry({ id: 'think_1', kind: 'thinking', turnId: 't1', eventId: 2, text: 'weighing it' }),
      entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 3, text: 'the answer' }),
    ]);
    const materialized = model([
      entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 3, text: 'the answer' }),
    ]);

    const merged = carryForwardReasoning(live, materialized);
    expect(merged.entries['think_1']?.text).toBe('weighing it');
    expect(merged.turns[0]!.entryIds).toContain('think_1');
  });

  it('re-seats it by eventId, not at the end', () => {
    // The Turns view joins a turn's reasoning in `entryIds` order, so appending puts
    // earlier reasoning after later reasoning in one aside — right text, wrong order,
    // and nothing on screen to say so.
    const live = model([
      entry({ id: 'think_a', kind: 'thinking', turnId: 't1', eventId: 2, text: 'first' }),
      entry({ id: 'think_b', kind: 'thinking', turnId: 't1', eventId: 6, text: 'second' }),
      entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 9, text: 'answer' }),
    ]);
    const materialized = model([
      entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 9, text: 'answer' }),
    ]);

    const ids = carryForwardReasoning(live, materialized).turns[0]!.entryIds;
    expect(ids).toEqual(['think_a', 'think_b', 'text_1']);
  });

  it('does NOT preserve any other kind — the server is authoritative for those', () => {
    // A page that drops a text entry is reporting a real edit (a compaction, a
    // redaction) and the client must honor it. Only reasoning is unreportable.
    const live = model([
      entry({ id: 'text_gone', kind: 'text', turnId: 't1', eventId: 2, text: 'compacted away' }),
      entry({ id: 'tool_gone', kind: 'tool_call', turnId: 't1', eventId: 3, toolName: 'Bash' }),
    ]);
    const materialized = model([
      entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 9, text: 'kept' }),
    ]);

    const merged = carryForwardReasoning(live, materialized);
    expect(merged.entries['text_gone']).toBeUndefined();
    expect(merged.entries['tool_gone']).toBeUndefined();
  });

  it('does not preserve a reasoning entry with no text', () => {
    // The stored shape: a signed thinking block with `text: ""`. Carrying it forward
    // would put an empty aside on screen, which claims reasoning that cannot be shown.
    const live = model([entry({ id: 'think_empty', kind: 'thinking', turnId: 't1', eventId: 2, text: '' })]);
    const materialized = model([entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 9, text: 'a' })]);

    expect(carryForwardReasoning(live, materialized).entries['think_empty']).toBeUndefined();
  });

  it('lets the fresher page win when it DOES carry the entry', () => {
    // Never a merge of two texts: if the server reports the entry, its version is the
    // one that renders. Preservation is only for what the page cannot report at all.
    const live = model([entry({ id: 'think_1', kind: 'thinking', turnId: 't1', eventId: 2, text: 'stale' })]);
    const materialized = model([entry({ id: 'think_1', kind: 'thinking', turnId: 't1', eventId: 2, text: 'fresh' })]);

    expect(carryForwardReasoning(live, materialized).entries['think_1']?.text).toBe('fresh');
  });

  it('does not resurrect reasoning from a turn the page does not contain', () => {
    // That turn is off the loaded window and renders nothing either way; re-adding its
    // entries would grow the model without putting anything on screen.
    const live = model(
      [entry({ id: 'think_old', kind: 'thinking', turnId: 't_old', eventId: 2, text: 'older turn' })],
      ['t_old'],
    );
    const materialized = model([entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 9, text: 'a' })]);

    expect(carryForwardReasoning(live, materialized).entries['think_old']).toBeUndefined();
  });

  it('returns the fresher model untouched when there is nothing to preserve', () => {
    // Identity, so the common path allocates nothing and no subscriber re-renders.
    const materialized = model([entry({ id: 'text_1', kind: 'text', turnId: 't1', eventId: 9, text: 'a' })]);
    expect(carryForwardReasoning(undefined, materialized)).toBe(materialized);
    expect(carryForwardReasoning(model([]), materialized)).toBe(materialized);
  });
});
