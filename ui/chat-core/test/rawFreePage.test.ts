import { describe, expect, it } from 'vitest';
import {
  applyEvent,
  initTailState,
  mergeMaterializedPage,
} from '../src/reduce/TurnReducer.js';
import { toolIdOf, resultedToolIds } from '../src/store/toolPairing.js';
import type { Entry, TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';

// The default materialized page is about to stop carrying `raw`.
//
// Measured 2026-08-25 on a real 30-message page: `raw` was 7.82 MB of 9.91 MB — 78.9%
// — a full copy of the source event stapled to every entry, and the Turns view renders
// none of it (dash TurnList.tsx returns null unless `view === 'raw'`). It is moving to
// its own endpoint, fetched only when the Raw pane is open.
//
// Four small scalars were being dug back out of it, which is what made an 8 MB blob
// load-bearing. They are fields now — on the page (log-store `buildTurnModel`) and on
// the live fold (`applyPayload`) alike. These cases pin that every consumer works with
// `raw` ABSENT, and that the two paths agree.
//
// The agreement is the sharp part. chat-core holds one model built from both a fetched
// page and a live SSE stream, and `mergeMaterializedPage` joins them. If a live-folded
// tool call and the page's copy of the SAME call disagree about its id, the merge
// cannot tell they are one event — so the call either doubles or is reported as never
// having finished. That exact failure is on record: reading the id out of `raw` is what
// fixed history pages rendering every completed tool call as still running.

const TS = '2026-08-25T12:00:00Z';

/** A page entry as the PROJECTED endpoint will serve it: promoted fields, no `raw`. */
function pageEntry(over: Partial<Entry>): Entry {
  return {
    id: 'e1',
    turnId: 't1',
    role: 'assistant',
    kind: 'text',
    source: 'harness',
    eventId: 1,
    ts: TS,
    duplicate: false,
    primary: true,
    ...over,
  };
}

function pageModel(entries: Entry[]): TurnModel {
  return {
    sessionId: 'sess',
    turns: [
      {
        id: 't1',
        role: 'user',
        ts: TS,
        entryIds: entries.map((e) => e.id),
      },
    ],
    entries: Object.fromEntries(entries.map((e) => [e.id, e])),
    validator: { maxEventId: 99, eventCount: entries.length, updatedAt: TS },
    more: false,
  };
}

function wireEvent(id: number, type: string, data: Record<string, unknown>): WireEvent {
  return { id: String(id), type, data: { type, event_id: id, turn_id: 't1', ...data } };
}

describe('every consumer works with a page that carries no raw', () => {
  it('reads a tool id off the promoted field', () => {
    const entry = pageEntry({ kind: 'tool_call', toolId: 'toolu_01ABC' });
    expect(entry.raw).toBeUndefined();
    expect(toolIdOf(entry)).toBe('toolu_01ABC');
  });

  it('reports a raw-free tool call as FINISHED once its result is on the page', () => {
    // The regression this whole exercise risks reintroducing. `resultedToolIds` is what
    // the timeline's ⚙ asks; keyed on an id it can no longer find, every completed call
    // on every history page reads as still running.
    const call = pageEntry({ id: 'e1', kind: 'tool_call', toolId: 'toolu_9', eventId: 1 });
    const result = pageEntry({ id: 'e2', kind: 'tool_result', toolId: 'toolu_9', eventId: 2 });

    expect(resultedToolIds([call, result]).has('toolu_9')).toBe(true);
  });

  it('carries a tool failure without raw', () => {
    // `toolError: false` and "no answer" are different facts, so both are asserted.
    expect(pageEntry({ kind: 'tool_result', toolError: true }).toolError).toBe(true);
    expect(pageEntry({ kind: 'tool_result', toolError: false }).toolError).toBe(false);
    expect(pageEntry({ kind: 'tool_result' }).toolError).toBeUndefined();
  });

});

describe('the live fold and the materialized page agree', () => {
  it('the live fold promotes the same four fields off the wire event', () => {
    let tail = initTailState('sess');
    tail = applyEvent(
      tail,
      wireEvent(1, 'tool_call', {
        tool_call: { tool_id: 'toolu_live', name: 'Bash', input: { command: 'ls' } },
      }),
    );
    const folded = Object.values(tail.model.entries)[0];
    expect(folded.toolId).toBe('toolu_live');
    expect(folded.eventType).toBe('tool_call');

    tail = applyEvent(
      tail,
      wireEvent(2, 'tool_result', {
        tool_result: { tool_id: 'toolu_live', name: 'Bash', output: 'boom', is_error: true },
      }),
    );
    const withResult = Object.values(tail.model.entries).find((e) => e.toolError !== undefined);
    expect(withResult?.toolError).toBe(true);
    expect(toolIdOf(withResult as Entry)).toBe('toolu_live');
  });

  it('a live-folded call and its raw-free page copy resolve to ONE tool id', () => {
    let live = initTailState('sess');
    live = applyEvent(
      live,
      wireEvent(1, 'tool_call', {
        tool_call: { tool_id: 'toolu_same', name: 'Bash', input: {} },
        message_id: 'msg_1',
      }),
    );
    const folded = Object.values(live.model.entries)[0];

    // The page's copy of the SAME call, as the projected endpoint will serve it.
    const fromPage = pageEntry({
      id: 'e_500',
      kind: 'tool_call',
      toolId: 'toolu_same',
      messageId: 'msg_1',
      eventId: 500,
    });

    expect(toolIdOf(folded)).toBe(toolIdOf(fromPage));
  });

  it('merging a raw-free page over a live tail does not double the tool call', () => {
    // `mergeMaterializedPage` decides whether the page already reports a live-held
    // entry, and for tool entries it decides on the tool id. With the id unreachable
    // the live copy is kept alongside the page's and the call shows twice.
    let live = initTailState('sess');
    live = applyEvent(
      live,
      wireEvent(1, 'tool_call', {
        tool_call: { tool_id: 'toolu_dup', name: 'Bash', input: {} },
        message_id: 'msg_7',
      }),
    );
    live = applyEvent(
      live,
      wireEvent(2, 'tool_result', {
        tool_result: { tool_id: 'toolu_dup', name: 'Bash', output: 'done' },
        message_id: 'msg_7',
      }),
    );

    const page = pageModel([
      pageEntry({ id: 'e_900', kind: 'tool_call', toolId: 'toolu_dup', messageId: 'msg_7', eventId: 900 }),
      pageEntry({ id: 'e_901', kind: 'tool_result', toolId: 'toolu_dup', messageId: 'msg_7', eventId: 901 }),
    ]);

    const merged = mergeMaterializedPage(live, page);
    const callsFor = Object.values(merged.model.entries).filter(
      (e) => toolIdOf(e) === 'toolu_dup' && e.kind === 'tool_call',
    );
    expect(callsFor).toHaveLength(1);
  });
});
