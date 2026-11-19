import { describe, expect, it, vi } from 'vitest';
import * as dedup from '../src/reduce/otelDedup.js';
import { sourcesForEntry, visibleEntryIdsFor } from '../src/store/selectors.js';
import type { Entry, TurnModel } from '../src/net/types.js';

function entry(partial: Partial<Entry> & Pick<Entry, 'id' | 'role' | 'kind' | 'source' | 'eventId'>): Entry {
  return { turnId: 't1', ts: '2026-09-16T00:00:00Z', duplicate: false, primary: true, ...partial };
}

function model(entries: Entry[]): TurnModel {
  return {
    sessionId: 's',
    turns: [{ id: 't1', role: 'user', ts: '', entryIds: entries.map((e) => e.id) }],
    entries: Object.fromEntries(entries.map((e) => [e.id, e])),
    validator: { maxEventId: 0, eventCount: 0, updatedAt: '' },
    more: false,
  };
}

const entries = [
  entry({ id: 'u_h', role: 'user', kind: 'text', source: 'harness', eventId: 1, text: 'hello' }),
  entry({ id: 'a', role: 'assistant', kind: 'result', source: 'harness', eventId: 2, text: 'hi' }),
  entry({ id: 'u_o', role: 'user', kind: 'text', source: 'otel', eventId: 3, text: 'hello' }),
];

describe('sourcesForEntry', () => {
  it('answers what grouping the annotation over the whole model answers', () => {
    const m = model(entries);
    const annotated = dedup.annotateOTelDuplicates(Object.values(m.entries));
    for (const e of entries) {
      expect(sourcesForEntry(m, e.id)).toEqual(dedup.groupMembers(annotated, e.id));
    }
    expect(sourcesForEntry(m, 'missing')).toEqual([]);
    expect(sourcesForEntry(undefined, 'u_h')).toEqual([]);
  });

  it('annotates a model once, however many entries ask', () => {
    const spy = vi.spyOn(dedup, 'annotateOTelDuplicates');
    const m = model(entries);
    for (let i = 0; i < 50; i++) for (const e of entries) sourcesForEntry(m, e.id);
    expect(spy).toHaveBeenCalledTimes(1);
    // A new model (the store replaces rather than edits) is annotated afresh.
    sourcesForEntry(model(entries), 'u_h');
    expect(spy).toHaveBeenCalledTimes(2);
    spy.mockRestore();
  });
});

describe('visibleEntryIdsFor', () => {
  it('finds the turn by id and filters duplicates in the turns view', () => {
    const m = model([entries[0]!, { ...entries[2]!, duplicate: true }]);
    expect(visibleEntryIdsFor(m, 't1', 'turns')).toEqual(['u_h']);
    expect(visibleEntryIdsFor(m, 't1', 'raw')).toEqual(['u_h', 'u_o']);
    expect(visibleEntryIdsFor(m, 'nope', 'raw')).toEqual([]);
  });
});
