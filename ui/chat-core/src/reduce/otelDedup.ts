import type { Entry } from '../net/types.js';

// Ported from bridge-ui `components/chat/TurnsView.tsx`:
//   - isOTelSourced (~:46-51)
//   - dedupOTelAssistantRows (~:90-119)
//   - the user_message dual-emit dedup inside rowsToTurns (~:153-195)
//
// CRITICAL DIFFERENCE FROM THE SOURCE: bridge-ui *drops* the redundant OTel copy
// (it `filter`s / `continue`s past it). Per the chat architecture's decision D2b/D9 (dash/docs/chat-architecture.md), chat-core is
// NON-DESTRUCTIVE — this is an ANNOTATOR. It tags each Entry with
// `duplicate` / `primary` / `groupId` and drops NOTHING. The collapsed "Turns"
// view is a selector over `!duplicate`; the raw "Timeline" view shows every
// entry. A dedup bug can therefore only ever double-show or collapse-hide — never
// silently lose a trace.
//
// The dedup is SOURCE+COUNT, never positional: Claude Code's OTel exporter
// batches on a ~1s interval, so when the assistant starts replying before that
// batch flushes, the OTel copy lands AFTER the reply and is no longer adjacent to
// the message it duplicates. Counting (not a Set) keeps the two sources genuinely
// redundant: N harness copies absorb N OTel copies, and any surplus OTel copy
// still renders primary — which is the recovery path (stream-json dropped the
// final turn) and the PTY path (the OTel log is the only record of the prompt).

/** True iff this entry is the OTel-sourced copy (bridge-ui reads the same
 *  `extensions.source === 'otel'` tag; chat-core normalizes it onto `source`). */
export function isOTelSourced(entry: Entry): boolean {
  return entry.source === 'otel';
}

// Which dual-emit class an entry belongs to, or null if it is not subject to
// source+count dedup. Assistant prose (`text`/`result`) and user prompts
// (`text` with role `user`) are the two dual-emitted classes.
function dedupClass(entry: Entry): 'assistant' | 'user' | null {
  if (entry.role === 'assistant' && (entry.kind === 'text' || entry.kind === 'result')) {
    return 'assistant';
  }
  if (entry.role === 'user' && entry.kind === 'text') {
    return 'user';
  }
  return null;
}

// The two dual-emit classes share one count map, so the key has to carry the
// class as well as the text. The separator is NUL because it is the one
// character that cannot occur in either half: a class name is a literal here
// and entry text is UTF-16 off the wire, so a printable separator could be
// produced by the text itself and let 'assistant' + ' x' collide with
// 'assistant ' + 'x'.
//
// ⚠️ WRITTEN AS THE ESCAPE `\u0000`, NEVER AS A RAW NUL BYTE. A raw NUL in
// the source makes the whole FILE binary: `file(1)` reports `data`, and every
// search tool that skips binary files -- ripgrep, ugrep, git-grep without
// `-a`, and the `grep` every agent on this box runs -- then returns ZERO
// matches for it, with no error and no warning. This file spent its whole life
// that way (a raw NUL landed with the original port on 2026-07-27), so it
// dropped silently out of every fleet-wide census ever taken over this repo.
// The escape is the identical string at runtime and leaves the file plain text.
function contentKey(cls: 'assistant' | 'user', text: string): string {
  return cls + '\u0000' + text;
}

/**
 * Annotate OTel duplicates across a flat, ordered list of entries. Returns a NEW
 * array of NEW entry objects (input untouched); every input entry appears in the
 * output. Annotations:
 *  - `duplicate=true`  → hidden in the collapsed Turns view, always shown in raw.
 *  - `primary=true`    → the single copy shown for its group in the collapsed view.
 *  - `groupId`         → members that share identical logical content across
 *                        sources (a harness copy + its ~1s-late OTel copy).
 *
 * A harness copy is always primary and always visible. An OTel copy is tagged
 * `duplicate` ONLY when an unabsorbed harness copy of the same text exists to
 * stand in for it; otherwise it stays primary (the recovery / PTY case).
 */
export function annotateOTelDuplicates(entries: Entry[]): Entry[] {
  // Fresh copies with annotations reset to the "shown, ungrouped" default.
  const result: Entry[] = entries.map((e) => ({
    ...e,
    duplicate: false,
    primary: true,
    groupId: undefined,
  }));

  // Queue the harness-sourced copies per (class, text), in order, so an absorbed
  // OTel copy can be paired with a specific harness entry for its groupId. A
  // queue (not a count) is how we keep the pairing stable while still matching
  // bridge-ui's counting semantics: we pop one harness copy per OTel copy.
  const harnessQueues = new Map<string, Entry[]>();
  for (const e of result) {
    const cls = dedupClass(e);
    if (cls === null) continue;
    const text = e.text;
    if (!text) continue;
    if (isOTelSourced(e)) continue;
    const key = contentKey(cls, text);
    let q = harnessQueues.get(key);
    if (!q) {
      q = [];
      harnessQueues.set(key, q);
    }
    q.push(e);
  }

  for (const e of result) {
    const cls = dedupClass(e);
    if (cls === null) continue;
    const text = e.text;
    if (!text) continue;
    if (!isOTelSourced(e)) continue;
    const key = contentKey(cls, text);
    const q = harnessQueues.get(key);
    if (q && q.length > 0) {
      // A harness copy stands in for this OTel copy — group them and hide the
      // OTel one from the collapsed view.
      const harnessPrimary = q.shift() as Entry;
      const gid = 'otelgrp_' + harnessPrimary.id;
      harnessPrimary.groupId = gid;
      harnessPrimary.primary = true;
      harnessPrimary.duplicate = false;
      e.groupId = gid;
      e.primary = false;
      e.duplicate = true;
    }
    // else: unmatched OTel copy — leave it primary + visible (recovery / PTY).
  }

  return result;
}

/** All copies sharing an entry's group, in the input order. For the "sources"
 *  badge: an entry with no group returns just itself. */
export function groupMembers(entries: Entry[], entryId: string): Entry[] {
  const target = entries.find((e) => e.id === entryId);
  if (!target) return [];
  if (!target.groupId) return [target];
  return entries.filter((e) => e.groupId === target.groupId);
}
