// Composer drafts, persisted across a reload.
//
// WHY localStorage AND NOT the IndexedDB SessionCache (the decision this file records):
// a draft has to be on screen in the first paint, and IndexedDB cannot promise that.
// `SessionCache.hydrate()` resolves after the provider mounts, so an IndexedDB-backed
// draft would land in the box some frames AFTER the user could already be typing into
// it — either clobbering the new keystrokes or being clobbered by them, depending on
// which write won. `localStorage.getItem` is synchronous, so `createChatStore()` can
// return with the drafts ALREADY in its initial state and that race cannot happen.
// Drafts are also a few kilobytes of plain text, which is what localStorage is for;
// the reasons SessionCache exists (megabytes of materialized turns, structured
// queries, an LRU over them) do not apply.
//
// The whole record is rewritten on each keystroke rather than debounced. A debounce
// buys back microseconds of a synchronous stringify over a few kilobytes and costs the
// last words typed before a reload — which is the one thing this file exists to keep.

import { defaultWebStorage, type WebStorageLike } from './webStorage.js';

/** The storage a draft record is kept in. The same narrow `Storage` slice every
 *  persisted chat-core store takes — named for this caller because it is what the
 *  option on `createChatStore` is called. */
export type DraftStorageLike = WebStorageLike;

/** The localStorage key the whole draft record lives under. */
export const DRAFT_STORAGE_KEY = 'chat-core:drafts';

/** Schema version of the persisted record. A record written by an older or newer
 *  chat-core is discarded rather than guessed at — drafts are cheap to lose once,
 *  and mis-reading them is not. */
export const DRAFT_RECORD_VERSION = 1;

/** How many sessions' drafts are kept. The sidebar is paged, so "this session no
 *  longer exists" is NOT answerable from the loaded window — a draft for a session
 *  scrolled out of the first page is still live. Recency is the bound that does not
 *  need that answer. */
export const MAX_PERSISTED_DRAFTS = 50;

/** How long an untouched draft is kept. A sentence abandoned a month ago is not a
 *  sentence anyone is coming back to finish. */
export const MAX_DRAFT_AGE_MS = 30 * 24 * 60 * 60 * 1000;

interface PersistedDraft {
  text: string;
  /** Epoch milliseconds of the last edit. Drives both bounds. */
  updatedAt: number;
}

interface PersistedDraftRecord {
  v: number;
  drafts: Record<string, PersistedDraft>;
}

function isPersistedDraft(value: unknown): value is PersistedDraft {
  if (typeof value !== 'object' || value === null) return false;
  const draft = value as { text?: unknown; updatedAt?: unknown };
  return typeof draft.text === 'string' && typeof draft.updatedAt === 'number';
}

/** Apply both bounds: drop empties and stale entries, then keep the newest
 *  `MAX_PERSISTED_DRAFTS`. Pure, so the bounds are testable without a Storage. */
export function boundDrafts(
  drafts: Record<string, PersistedDraft>,
  now: number,
  maxCount = MAX_PERSISTED_DRAFTS,
  maxAgeMs = MAX_DRAFT_AGE_MS,
): Record<string, PersistedDraft> {
  const live = Object.entries(drafts)
    .filter(([, d]) => d.text !== '' && now - d.updatedAt <= maxAgeMs)
    .sort((a, b) => b[1].updatedAt - a[1].updatedAt)
    .slice(0, maxCount);
  return Object.fromEntries(live);
}

/** Where drafts are kept when the caller names no storage: the browser's
 *  localStorage, or null anywhere it does not work. The probe itself is shared with
 *  every other persisted store — see `webStorage.ts` for what it checks and why. */
export function defaultDraftStorage(): DraftStorageLike | null {
  return defaultWebStorage();
}

/**
 * Reads and writes the persisted draft record.
 *
 * Owns the per-draft `updatedAt` stamps, which the store's `Map<string, string>`
 * has no room for and does not need: the timestamps exist only to bound what is
 * kept on disk, so they belong to the thing doing the bounding.
 */
export class DraftStore {
  private readonly storage: DraftStorageLike | null;
  /** Last text persisted per key, so an unchanged draft keeps its original stamp
   *  instead of looking freshly edited on every unrelated save. */
  private lastWritten = new Map<string, string>();
  private stamps = new Map<string, number>();
  private warned = false;

  constructor(storage: DraftStorageLike | null = defaultDraftStorage()) {
    this.storage = storage;
  }

  get isEnabled(): boolean {
    return this.storage !== null;
  }

  /** Read the persisted drafts, bounded. A record that is missing, unparseable, or
   *  written by a different schema version yields an empty map — never a throw, and
   *  never a partially-guessed record. */
  load(now = Date.now()): Map<string, string> {
    this.lastWritten = new Map();
    this.stamps = new Map();
    if (!this.storage) return new Map();

    let raw: string | null;
    try {
      raw = this.storage.getItem(DRAFT_STORAGE_KEY);
    } catch (err) {
      this.warnOnce('read failed', err);
      return new Map();
    }
    if (!raw) return new Map();

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch (err) {
      this.warnOnce('stored drafts are not valid JSON and were discarded', err);
      return new Map();
    }

    const record = parsed as Partial<PersistedDraftRecord> | null;
    if (
      typeof record !== 'object' ||
      record === null ||
      record.v !== DRAFT_RECORD_VERSION ||
      typeof record.drafts !== 'object' ||
      record.drafts === null
    ) {
      return new Map();
    }

    const wellFormed: Record<string, PersistedDraft> = {};
    for (const [key, value] of Object.entries(record.drafts)) {
      if (isPersistedDraft(value)) wellFormed[key] = value;
    }

    const bounded = boundDrafts(wellFormed, now);
    const drafts = new Map<string, string>();
    for (const [key, draft] of Object.entries(bounded)) {
      drafts.set(key, draft.text);
      this.lastWritten.set(key, draft.text);
      this.stamps.set(key, draft.updatedAt);
    }
    return drafts;
  }

  /** Persist the store's whole draft map. What is fit to keep is decided in exactly
   *  one place — `boundDrafts` — which drops empties (an empty box is the absence of
   *  a draft, and keeping one would let a sent pending draft reappear in the next new
   *  chat) as well as stale and surplus entries. */
  save(drafts: ReadonlyMap<string, string>, now = Date.now()): void {
    if (!this.storage) return;

    const next: Record<string, PersistedDraft> = {};
    for (const [key, text] of drafts) {
      const unchanged = this.lastWritten.get(key) === text;
      const previousStamp = this.stamps.get(key);
      next[key] = { text, updatedAt: unchanged && previousStamp !== undefined ? previousStamp : now };
    }

    const bounded = boundDrafts(next, now);
    this.lastWritten = new Map();
    this.stamps = new Map();
    for (const [key, draft] of Object.entries(bounded)) {
      this.lastWritten.set(key, draft.text);
      this.stamps.set(key, draft.updatedAt);
    }

    try {
      if (Object.keys(bounded).length === 0) {
        this.storage.removeItem(DRAFT_STORAGE_KEY);
        return;
      }
      const record: PersistedDraftRecord = { v: DRAFT_RECORD_VERSION, drafts: bounded };
      this.storage.setItem(DRAFT_STORAGE_KEY, JSON.stringify(record));
      this.warned = false;
    } catch (err) {
      this.warnOnce('write failed — drafts will not survive a reload', err);
    }
  }

  /** Say it once per run of failures. A full quota fails on every keystroke, and a
   *  warning per keystroke buries the one that matters. The flag clears on the next
   *  successful write, so a later failure is reported again. */
  private warnOnce(what: string, err: unknown): void {
    if (this.warned) return;
    this.warned = true;
    console.warn(`[chat-core] composer drafts: ${what}`, err);
  }
}
