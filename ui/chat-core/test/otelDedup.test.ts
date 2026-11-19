import { describe, expect, it } from 'vitest';
import { annotateOTelDuplicates, groupMembers } from '../src/reduce/otelDedup.js';
import type { Entry } from '../src/net/types.js';

// Build a minimal Entry. Annotations default to the "shown, ungrouped" state;
// the annotator is what sets duplicate/primary/groupId.
function entry(partial: Partial<Entry> & Pick<Entry, 'id' | 'role' | 'kind' | 'source' | 'eventId'>): Entry {
  return {
    turnId: partial.turnId ?? `turn_${partial.id}`,
    ts: partial.ts ?? '2026-07-27T14:00:00-07:00',
    duplicate: false,
    primary: true,
    ...partial,
  };
}

describe('annotateOTelDuplicates — non-destructive dedup', () => {
  it('tags the ~1s-LATE user_message OTel copy as duplicate, dropping nothing', () => {
    // The OTel exporter batches ~1s, so the OTel copy of the prompt lands AFTER
    // the assistant reply — NOT adjacent to the prompt it duplicates. Source+count
    // dedup (not positional) must still pair them.
    const userHarness = entry({ id: 'u_harness', role: 'user', kind: 'text', source: 'harness', eventId: 1, text: 'hello there' });
    const assistantReply = entry({ id: 'a1', role: 'assistant', kind: 'result', source: 'harness', eventId: 2, text: 'hi! how can I help?' });
    const userOtelLate = entry({ id: 'u_otel', role: 'user', kind: 'text', source: 'otel', eventId: 3, text: 'hello there' });

    const input = [userHarness, assistantReply, userOtelLate];
    const out = annotateOTelDuplicates(input);

    // NOTHING dropped — every input entry is present.
    expect(out).toHaveLength(3);
    expect(new Set(out.map((e) => e.id))).toEqual(new Set(['u_harness', 'a1', 'u_otel']));

    const uh = out.find((e) => e.id === 'u_harness')!;
    const uo = out.find((e) => e.id === 'u_otel')!;
    const a1 = out.find((e) => e.id === 'a1')!;

    // Exactly one primary in the prompt group; the OTel copy is the duplicate.
    expect(uh.primary).toBe(true);
    expect(uh.duplicate).toBe(false);
    expect(uo.primary).toBe(false);
    expect(uo.duplicate).toBe(true);
    expect(uh.groupId).toBeDefined();
    expect(uo.groupId).toBe(uh.groupId);

    // The reply is untouched.
    expect(a1.primary).toBe(true);
    expect(a1.duplicate).toBe(false);
    expect(a1.groupId).toBeUndefined();

    // The collapsed Turns view (!duplicate) shows one prompt; the raw view shows both.
    const collapsed = out.filter((e) => !e.duplicate);
    expect(collapsed.map((e) => e.id).sort()).toEqual(['a1', 'u_harness']);
    expect(out).toHaveLength(3); // raw view = every entry.

    // The input array is untouched (pure).
    expect(userOtelLate.duplicate).toBe(false);
  });

  it('tags an assistant dual-emit: exactly one primary, the other duplicate, nothing dropped', () => {
    const userHarness = entry({ id: 'u1', role: 'user', kind: 'text', source: 'harness', eventId: 1, text: 'do it' });
    const asstHarness = entry({ id: 'a_harness', role: 'assistant', kind: 'text', source: 'harness', eventId: 2, text: 'Done.' });
    const asstOtel = entry({ id: 'a_otel', role: 'assistant', kind: 'text', source: 'otel', eventId: 3, text: 'Done.' });

    const out = annotateOTelDuplicates([userHarness, asstHarness, asstOtel]);

    expect(out).toHaveLength(3);
    const grp = out.filter((e) => e.role === 'assistant');
    const primaries = grp.filter((e) => e.primary && !e.duplicate);
    const dupes = grp.filter((e) => e.duplicate);
    expect(primaries).toHaveLength(1);
    expect(dupes).toHaveLength(1);
    expect(primaries[0]!.source).toBe('harness');
    expect(dupes[0]!.source).toBe('otel');
    expect(primaries[0]!.groupId).toBe(dupes[0]!.groupId);
  });

  it('keeps an UNMATCHED OTel copy primary (PTY / recovery — the only source)', () => {
    // In PTY mode the OTel log is the ONLY record of the prompt; a surplus OTel
    // copy must still render.
    const userOtelOnly = entry({ id: 'u_otel', role: 'user', kind: 'text', source: 'otel', eventId: 1, text: 'typed in pty' });
    const out = annotateOTelDuplicates([userOtelOnly]);
    expect(out).toHaveLength(1);
    expect(out[0]!.primary).toBe(true);
    expect(out[0]!.duplicate).toBe(false);
    expect(out[0]!.groupId).toBeUndefined();
  });

  it('absorbs N OTel copies against N harness copies; surplus OTel still renders', () => {
    const h1 = entry({ id: 'h1', role: 'assistant', kind: 'text', source: 'harness', eventId: 1, text: 'X' });
    const o1 = entry({ id: 'o1', role: 'assistant', kind: 'text', source: 'otel', eventId: 2, text: 'X' });
    const o2 = entry({ id: 'o2', role: 'assistant', kind: 'text', source: 'otel', eventId: 3, text: 'X' });
    const out = annotateOTelDuplicates([h1, o1, o2]);
    const byId = Object.fromEntries(out.map((e) => [e.id, e]));
    expect(byId.h1!.duplicate).toBe(false);
    expect(byId.o1!.duplicate).toBe(true); // absorbed by h1
    expect(byId.o2!.duplicate).toBe(false); // surplus — still shown
  });

  it('groupMembers returns both copies for a badge, or just the entry when ungrouped', () => {
    const uh = entry({ id: 'u_harness', role: 'user', kind: 'text', source: 'harness', eventId: 1, text: 'q' });
    const uo = entry({ id: 'u_otel', role: 'user', kind: 'text', source: 'otel', eventId: 2, text: 'q' });
    const lone = entry({ id: 'lone', role: 'assistant', kind: 'result', source: 'harness', eventId: 3, text: 'a' });
    const out = annotateOTelDuplicates([uh, uo, lone]);
    expect(groupMembers(out, 'u_harness').map((e) => e.id).sort()).toEqual(['u_harness', 'u_otel']);
    expect(groupMembers(out, 'lone').map((e) => e.id)).toEqual(['lone']);
  });

  it('a prompt and a reply with the SAME TEXT never absorb each other', () => {
    // The dedup key carries the class as well as the text, and until this case
    // nothing said so: replacing the whole key with the bare text left all 370
    // tests green. Every other case here uses text that differs between the
    // roles, so the class half of the key is never asked to do anything.
    //
    // The two classes share one queue map. Keyed on text alone, the user's
    // harness copy and the assistant's harness copy sit in ONE queue in input
    // order, and the assistant's late OTel copy pops whichever is at the front —
    // the user's prompt. The visible result is the user's own message marked as
    // the primary of a group whose other member is part of the assistant's reply,
    // and an assistant reply left looking un-echoed.
    //
    // Identical prompt and reply text is not contrived: one-word exchanges
    // ('continue', 'yes', a pasted path) collide constantly, and a harness that
    // echoes the prompt back produces it every turn.
    const userHarness = entry({ id: 'u_h', role: 'user', kind: 'text', source: 'harness', eventId: 1, text: 'continue' });
    const assistantHarness = entry({ id: 'a_h', role: 'assistant', kind: 'text', source: 'harness', eventId: 2, text: 'continue' });
    const assistantOtel = entry({ id: 'a_o', role: 'assistant', kind: 'text', source: 'otel', eventId: 3, text: 'continue' });

    const out = annotateOTelDuplicates([userHarness, assistantHarness, assistantOtel]);
    const byId = Object.fromEntries(out.map((e) => [e.id, e]));

    // The OTel copy pairs with the reply it is a copy OF, not with the prompt
    // that happens to read the same.
    expect(groupMembers(out, 'a_o').map((e) => e.id).sort()).toEqual(['a_h', 'a_o']);
    expect(byId.a_o!.duplicate).toBe(true);
    expect(byId.a_h!.duplicate).toBe(false);

    // And the user's prompt is untouched: no group, still shown, not made the
    // primary of the assistant's group.
    expect(byId.u_h!.groupId).toBeUndefined();
    expect(byId.u_h!.duplicate).toBe(false);
    expect(groupMembers(out, 'u_h').map((e) => e.id)).toEqual(['u_h']);
  });

  it('the same text in the same class still absorbs — the class is a discriminator, not a blanket', () => {
    // The cry-wolf control for the case above. Splitting by class must not stop
    // a genuine same-class duplicate from being absorbed, which is what an
    // over-eager key (say, one that also folded in the entry id) would do.
    const h = entry({ id: 'h', role: 'assistant', kind: 'text', source: 'harness', eventId: 1, text: 'continue' });
    const o = entry({ id: 'o', role: 'assistant', kind: 'text', source: 'otel', eventId: 2, text: 'continue' });
    const out = annotateOTelDuplicates([h, o]);
    const byId = Object.fromEntries(out.map((e) => [e.id, e]));
    expect(byId.o!.duplicate).toBe(true);
    expect(byId.h!.duplicate).toBe(false);
  });

  it('an entry in NEITHER dual-emit class is never grouped, however its text reads', () => {
    // `dedupClass` returns null for anything that is not assistant prose or a
    // user prompt, and that null was unpinned: making it fall through to
    // 'assistant' left every case here green, because every case here uses
    // entries that ARE in a dual-emit class.
    //
    // Only those two classes are emitted twice. A tool call, a tool result or an
    // error has exactly one source, so two of them sharing text are two real
    // events -- and hiding the second as a duplicate would drop a genuine
    // occurrence out of the collapsed view. Repeated tool calls with identical
    // text are the normal case, not a corner: the same command run twice, two
    // reads of one path.
    const first = entry({ id: 'tc1', role: 'assistant', kind: 'tool_call', source: 'harness', eventId: 1, text: 'ls -la' });
    const second = entry({ id: 'tc2', role: 'assistant', kind: 'tool_call', source: 'otel', eventId: 2, text: 'ls -la' });

    const out = annotateOTelDuplicates([first, second]);
    const byId = Object.fromEntries(out.map((e) => [e.id, e]));

    expect(byId.tc1!.duplicate).toBe(false);
    expect(byId.tc2!.duplicate).toBe(false);
    expect(byId.tc1!.groupId).toBeUndefined();
    expect(byId.tc2!.groupId).toBeUndefined();
  });

  it('an assistant `result` and its OTel copy DO pair — both prose kinds count', () => {
    // The other half of the assistant class. `dedupClass` accepts `text` OR
    // `result`, and narrowing it to `text` alone left the suite green: the one
    // existing case with a `result` entry has no OTel copy of it to pair with,
    // so the `result` arm was never exercised.
    const h = entry({ id: 'r_h', role: 'assistant', kind: 'result', source: 'harness', eventId: 1, text: 'done' });
    const o = entry({ id: 'r_o', role: 'assistant', kind: 'result', source: 'otel', eventId: 2, text: 'done' });

    const out = annotateOTelDuplicates([h, o]);
    const byId = Object.fromEntries(out.map((e) => [e.id, e]));

    expect(byId.r_o!.duplicate).toBe(true);
    expect(byId.r_h!.duplicate).toBe(false);
    expect(groupMembers(out, 'r_h').map((e) => e.id).sort()).toEqual(['r_h', 'r_o']);
  });

  it('groupMembers answers nothing for an id that is not in the list', () => {
    // The failure value of `groupMembers`. Every existing call passes an id that
    // is present, so the guard could be deleted (a TypeError on `target.groupId`)
    // or made to return the WHOLE list, and nothing noticed either.
    //
    // The badge calls this with an id it holds from a previous render, so an
    // entry dropped by a refresh reaches it routinely. Returning every entry
    // there would render a sources badge claiming the whole transcript.
    const a = entry({ id: 'a', role: 'assistant', kind: 'text', source: 'harness', eventId: 1, text: 'x' });
    const b = entry({ id: 'b', role: 'assistant', kind: 'text', source: 'otel', eventId: 2, text: 'x' });
    const out = annotateOTelDuplicates([a, b]);

    expect(groupMembers(out, 'no-such-entry')).toEqual([]);
    // And the grouped pair is still found, so the case above is not passing
    // because groupMembers answers nothing for everything.
    expect(groupMembers(out, 'a').map((e) => e.id).sort()).toEqual(['a', 'b']);
  });
});
