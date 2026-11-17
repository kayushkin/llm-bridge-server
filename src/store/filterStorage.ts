// The sidebar's filter selection, persisted across a reload.
//
// WHAT IS PERSISTED, AND WHAT DELIBERATELY IS NOT. Only the six faceted axes
// (`harness`, `status`, `type`, `purpose`, `mode`, `machine`) survive a reload.
//
//   - `search` is NOT persisted. A restored query would re-run a transcript search
//     against the gateway on every page load and hand the user a short list whose
//     cause is a sentence they typed days ago and cannot see. bridge-ui does not
//     persist its search box either.
//   - `folder` is NOT persisted. It is a navigation position, not a chip: restore a
//     folder that has since been deleted and the sidebar is empty with nothing on
//     screen saying why.
//
// ⚠️ THE SHAPE IS chat-core's OWN, AND THAT IS THE DECISION THIS FILE RECORDS.
// bridge-ui persists the same idea as EXCLUSION sets — "hide these" — under eight
// keys of its own (`bridge-ui-harness-filter`, `bridge-ui-session-type-filter`, …).
// chat-core's `FilterState` is INCLUSION arrays: OR within an axis, AND across axes,
// empty meaning no filter at all. Reading bridge-ui's keys here would invert the
// meaning of every filter — an empty exclusion set ("show everything") would arrive
// as an empty inclusion array, which happens to agree, and a one-element exclusion
// set ("hide claude-code") would arrive as "show ONLY claude-code", which is the
// exact opposite. So this store writes chat-core's own inclusion arrays under its own
// key and the two surfaces diverge on purpose. They also shared an origin while dash
// served bridge-ui's chat beside its own — so a shared key would not merely have
// been ambiguous, it would have had the two pages overwriting each other.
//
// Like `draftStorage.ts`, this is read back SYNCHRONOUSLY inside `createChatStore()`
// so the restored filter is in the very first painted state. An async rehydrate would
// paint the unfiltered list first and then yank rows out from under a user who has
// already started reading.

import { defaultWebStorage, type WebStorageLike } from './webStorage.js';

/** The axes that survive a reload. Not every key of `FilterState` — see the header
 *  for why `search` and `folder` are left out. */
export const PERSISTED_FILTER_AXES = [
  'harness',
  'status',
  'type',
  'purpose',
  'mode',
  'machine',
  'principal',
] as const;

export type PersistedFilterAxis = (typeof PERSISTED_FILTER_AXES)[number];

/** The selected values per persisted axis. An empty array means "no filter on this
 *  axis", exactly as in `FilterState`.
 *
 *  ⚠️ Empty on the `type` axis is still not "show literally everything": the list path
 *  drops `DEFAULT_HIDDEN_SESSION_TYPES` (see `selectors.ts`) while nothing is selected
 *  there. That default is deliberately NOT written here — seeding a stored selection
 *  would mean naming every type the user DOES want, and hiding whatever type the server
 *  adds next. Nothing a user has stored is touched by it. */
export type PersistedFilterAxes = Record<PersistedFilterAxis, string[]>;

/** The localStorage key the whole filter record lives under. */
export const FILTER_STORAGE_KEY = 'chat-core:filters';

/** Schema version of the persisted record. A record written by an older or newer
 *  chat-core is discarded rather than guessed at — a mis-read filter silently hides
 *  sessions, which is the one failure a user cannot diagnose from the screen. */
export const FILTER_RECORD_VERSION = 1;

/** How many values are kept per axis. The facets a real session list offers are far
 *  below this; the bound is here so a corrupt or hand-edited record cannot make every
 *  `includes` check on the list path walk an unbounded array. */
export const MAX_PERSISTED_FILTER_VALUES = 100;

interface PersistedFilterRecord {
  v: number;
  axes: Partial<Record<PersistedFilterAxis, unknown>>;
}

/** No axis filtered — what an absent, unreadable or wrong-version record yields. */
export function emptyPersistedFilterAxes(): PersistedFilterAxes {
  return { harness: [], status: [], type: [], purpose: [], mode: [], machine: [], principal: [] };
}

/** Keep the strings, drop everything else, de-duplicate, and bound the length. Pure,
 *  so the bounds are testable without a Storage. */
export function boundFilterValues(
  value: unknown,
  maxCount = MAX_PERSISTED_FILTER_VALUES,
): string[] {
  if (!Array.isArray(value)) return [];
  const seen = new Set<string>();
  for (const v of value) {
    if (typeof v !== 'string' || v === '') continue;
    seen.add(v);
    if (seen.size >= maxCount) break;
  }
  return [...seen];
}

/**
 * Reads and writes the persisted sidebar filter.
 *
 * Holds no state of its own: the filter is small, wholly owned by the store, and
 * written on the rare event of a chip being clicked, so every save rewrites the whole
 * record and there is nothing to diff.
 */
export class FilterStore {
  private readonly storage: WebStorageLike | null;
  private warned = false;

  constructor(storage: WebStorageLike | null = defaultWebStorage()) {
    this.storage = storage;
  }

  get isEnabled(): boolean {
    return this.storage !== null;
  }

  /** Read the persisted axes. A record that is missing, unparseable, or written by a
   *  different schema version yields no filter at all — never a throw, and never a
   *  partially-guessed selection. */
  load(): PersistedFilterAxes {
    const axes = emptyPersistedFilterAxes();
    if (!this.storage) return axes;

    let raw: string | null;
    try {
      raw = this.storage.getItem(FILTER_STORAGE_KEY);
    } catch (err) {
      this.warnOnce('read failed', err);
      return axes;
    }
    if (!raw) return axes;

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch (err) {
      this.warnOnce('the stored filter is not valid JSON and was discarded', err);
      return axes;
    }

    const record = parsed as Partial<PersistedFilterRecord> | null;
    if (
      typeof record !== 'object' ||
      record === null ||
      record.v !== FILTER_RECORD_VERSION ||
      typeof record.axes !== 'object' ||
      record.axes === null
    ) {
      return axes;
    }

    for (const axis of PERSISTED_FILTER_AXES) {
      axes[axis] = boundFilterValues(record.axes[axis]);
    }
    return axes;
  }

  /** Persist the selection. A filter with nothing selected on any axis removes the
   *  record rather than writing an empty one: "no filter" is the default the store
   *  starts in, so keeping a row that says so only leaves something to mis-read. */
  save(axes: PersistedFilterAxes): void {
    if (!this.storage) return;

    const bounded = emptyPersistedFilterAxes();
    let anySelected = false;
    for (const axis of PERSISTED_FILTER_AXES) {
      bounded[axis] = boundFilterValues(axes[axis]);
      if (bounded[axis].length > 0) anySelected = true;
    }

    try {
      if (!anySelected) {
        this.storage.removeItem(FILTER_STORAGE_KEY);
        return;
      }
      const record: PersistedFilterRecord = { v: FILTER_RECORD_VERSION, axes: bounded };
      this.storage.setItem(FILTER_STORAGE_KEY, JSON.stringify(record));
      this.warned = false;
    } catch (err) {
      this.warnOnce('write failed — the filter will not survive a reload', err);
    }
  }

  /** Say it once per run of failures, and clear the flag on the next successful write
   *  so a later failure is still reported. Same discipline as the draft store. */
  private warnOnce(what: string, err: unknown): void {
    if (this.warned) return;
    this.warned = true;
    console.warn(`[chat-core] sidebar filter: ${what}`, err);
  }
}
