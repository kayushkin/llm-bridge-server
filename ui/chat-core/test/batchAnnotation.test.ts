import { describe, expect, it, vi } from 'vitest';

// `annotateOTelDuplicates` rebuilds EVERY entry in the model (`entries.map(e => ({...e}))`)
// and it used to run once per folded frame. On a cold-loaded session that model holds
// about a thousand entries, and opening a session replays hundreds of frames — so the
// cost was O(frames x entries) for a result only the final pass can be right about.
//
// Folding a batch now defers it: fold each frame without annotating, annotate once at
// the end. That is safe only because the fold never READS the annotations it writes,
// which is a property that could quietly stop being true — so the equivalence case
// below is the important one. A batch must produce EXACTLY what folding one at a time
// produces; being faster is worth nothing if the grouping comes out different.

const annotateSpy = vi.fn();

vi.mock('../src/reduce/otelDedup.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../src/reduce/otelDedup.js')>();
  return {
    ...actual,
    annotateOTelDuplicates: (entries: unknown[]) => {
      annotateSpy(entries.length);
      return actual.annotateOTelDuplicates(entries as never);
    },
  };
});

const { applyEvent, applyEvents, initTailState } = await import('../src/reduce/TurnReducer.js');
type WireEvent = import('../src/net/wireEvents.js').WireEvent;

/** An assistant text block. `otel` marks it as the duplicate copy of the same content,
 *  which is the only thing the annotator actually groups. */
function block(eventId: number, text: string, otel = false): WireEvent {
  return {
    id: String(eventId),
    type: 'block',
    data: {
      type: 'block',
      event_id: eventId,
      turn_id: 't1',
      // DISTINCT message ids for the two copies, same text. That is what a real dual
      // emit looks like and it is what the annotator groups on — sharing one id folds
      // the copies into a single entry instead, which produces no duplicates at all and
      // made the equivalence case below compare two empty annotation sets.
      message_id: otel ? `msg_otel_${text}` : `msg_harness_${text}`,
      ...(otel ? { extensions: { source: 'otel' } } : {}),
      block: { block: { type: 'text', text_block: { text } } },
    },
  };
}

/** A dual-emitted turn: each reply reported by the harness and again by OTel. */
function dualEmittedTurn(replies: number): WireEvent[] {
  const events: WireEvent[] = [];
  let eventId = 1;
  for (let i = 0; i < replies; i++) {
    events.push(block(eventId++, `reply ${i}`));
    events.push(block(eventId++, `reply ${i}`, true));
  }
  return events;
}

/** The annotations only, keyed by entry id — what the batch must reproduce exactly. */
function annotationsOf(model: { entries: Record<string, { duplicate: boolean; primary: boolean; groupId?: string; text?: string }> }) {
  return Object.entries(model.entries)
    .map(([id, e]) => `${id}|${e.text ?? ''}|dup=${e.duplicate}|prim=${e.primary}|grp=${e.groupId ?? ''}`)
    .sort();
}

describe('folding a batch annotates once', () => {
  it('runs the annotator ONCE for a batch, not once per frame', () => {
    const events = dualEmittedTurn(25); // 50 frames
    annotateSpy.mockClear();

    applyEvents(initTailState('sess'), events);

    expect(annotateSpy).toHaveBeenCalledTimes(1);
  });

  it('still runs it every time for the single-event path', () => {
    // `applyEvent` is what everything other than the batched stream uses, and it has to
    // return an annotated tail — its callers render straight off the result.
    annotateSpy.mockClear();

    let tail = initTailState('sess');
    for (const ev of dualEmittedTurn(3)) tail = applyEvent(tail, ev);

    expect(annotateSpy).toHaveBeenCalledTimes(6);
  });

  it('produces EXACTLY what folding one at a time produces', () => {
    // The safety property. The batch defers annotation on the strength of the fold never
    // reading `duplicate`/`primary`/`groupId` — if that ever stops being true, the two
    // paths diverge and this is what says so.
    const events = dualEmittedTurn(10);

    const batched = applyEvents(initTailState('sess'), events);
    let oneAtATime = initTailState('sess');
    for (const ev of events) oneAtATime = applyEvent(oneAtATime, ev);

    expect(annotationsOf(batched.model)).toEqual(annotationsOf(oneAtATime.model));
  });

  it('groups the dual-emitted copies rather than leaving them all primary', () => {
    // Guards the case above against passing vacuously: two identical empty annotations
    // are equal too. A dual-emitted turn must actually produce duplicates.
    const batched = applyEvents(initTailState('sess'), dualEmittedTurn(4));
    const entries = Object.values(batched.model.entries);

    expect(entries.filter((e) => e.duplicate).length).toBeGreaterThan(0);
    expect(entries.filter((e) => e.groupId).length).toBeGreaterThan(0);
  });

  it('leaves the tail untouched when every frame is a no-op', () => {
    // A replay re-delivering frames already folded must not allocate a fresh model —
    // `applyEvents` returns the SAME object, so subscribers see no change at all.
    const events = dualEmittedTurn(3);
    const folded = applyEvents(initTailState('sess'), events);

    const again = applyEvents(folded, events);

    expect(again).toBe(folded);
  });

  it('an empty batch changes nothing', () => {
    const tail = applyEvents(initTailState('sess'), dualEmittedTurn(2));
    expect(applyEvents(tail, [])).toBe(tail);
  });
});
