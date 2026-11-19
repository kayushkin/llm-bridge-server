import { describe, expect, it, vi } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import {
  DraftStore,
  boundDrafts,
  DRAFT_STORAGE_KEY,
  DRAFT_RECORD_VERSION,
  MAX_PERSISTED_DRAFTS,
  MAX_DRAFT_AGE_MS,
  type DraftStorageLike,
} from '../src/store/draftStorage.js';

// Composer drafts used to live in memory only: `ChatState.drafts` was a bare Map and
// there was not one `localStorage` or `sessionStorage` call anywhere in the package.
// Reload the tab mid-sentence and the sentence was gone.
//
// Three things have to hold, and they are the reason these tests exist rather than a
// screenshot of a composer:
//
//  1. A draft is read back SYNCHRONOUSLY, at store construction. If it arrived later
//     it would race the keystrokes of a user already typing into the box.
//  2. Sending a pending chat's draft must not resurrect it in the next pending chat.
//     The pending pane keys its draft under `PENDING_DRAFT_KEY`, not a session id, and
//     that key outlives the chat it belonged to.
//  3. Nothing grows without a bound — and the bound cannot be "is this session still
//     in the sidebar", because the sidebar is PAGED. A draft for a session scrolled
//     out of the loaded window is still live.

/** A `Storage`-shaped object over a plain Map, so a "reload" is just a second
 *  `createChatStore` reading the same backing object. */
function fakeStorage(seed: Record<string, string> = {}): DraftStorageLike & {
  readonly data: Map<string, string>;
} {
  const data = new Map(Object.entries(seed));
  return {
    data,
    getItem: (key) => data.get(key) ?? null,
    setItem: (key, value) => void data.set(key, value),
    removeItem: (key) => void data.delete(key),
  };
}

function storedDrafts(storage: DraftStorageLike): Record<string, { text: string; updatedAt: number }> {
  const raw = storage.getItem(DRAFT_STORAGE_KEY);
  if (raw === null) return {};
  return (JSON.parse(raw) as { drafts: Record<string, { text: string; updatedAt: number }> }).drafts;
}

/** The pending pane's draft key, mirrored from react/hooks.ts. Held here as a literal
 *  on purpose: if that constant ever changes, this test should fail rather than follow
 *  it, because the value is what a previously-persisted record is keyed under. */
const PENDING_DRAFT_KEY = '__pending__';

describe('composer drafts survive a reload', () => {
  it('reads drafts back for every session, synchronously at construction', () => {
    const storage = fakeStorage();
    const first = createChatStore({ draftStorage: storage });
    first.getState().actions.setDraft('sess-a', 'half a sentence about');
    first.getState().actions.setDraft('sess-b', 'a different half sentence');

    // No await anywhere: the reloaded store has the drafts in its FIRST state.
    const reloaded = createChatStore({ draftStorage: storage });
    expect(reloaded.getState().drafts.get('sess-a')).toBe('half a sentence about');
    expect(reloaded.getState().drafts.get('sess-b')).toBe('a different half sentence');
  });

  it('persists the pending pane draft too — an unsent new chat is the same sentence', () => {
    const storage = fakeStorage();
    const first = createChatStore({ draftStorage: storage });
    first.getState().actions.setDraft(PENDING_DRAFT_KEY, 'typed into a chat that has no id yet');

    const reloaded = createChatStore({ draftStorage: storage });
    expect(reloaded.getState().drafts.get(PENDING_DRAFT_KEY)).toBe(
      'typed into a chat that has no id yet',
    );
  });

  it('clearing a draft deletes the persisted copy, so a sent pending chat does not come back', () => {
    const storage = fakeStorage();
    const first = createChatStore({ draftStorage: storage });
    first.getState().actions.setDraft(PENDING_DRAFT_KEY, 'about to be sent');
    // This is exactly what `useComposer.send` does on the way out.
    first.getState().actions.setDraft(PENDING_DRAFT_KEY, '');

    expect(storedDrafts(storage)[PENDING_DRAFT_KEY]).toBeUndefined();
    const reloaded = createChatStore({ draftStorage: storage });
    expect(reloaded.getState().drafts.get(PENDING_DRAFT_KEY) ?? '').toBe('');
  });

  it('an empty draft is never stored as an empty string — that is the absence of a draft', () => {
    const storage = fakeStorage();
    const store = createChatStore({ draftStorage: storage });
    store.getState().actions.setDraft('sess-a', '');
    expect(storage.getItem(DRAFT_STORAGE_KEY)).toBeNull();
  });

  it('removeSession drops that session’s draft from memory AND from storage', () => {
    const storage = fakeStorage();
    const store = createChatStore({ draftStorage: storage });
    store.getState().actions.upsertSession({
      sessionId: 'sess-a',
      displayName: 'a',
      harness: 'claude_code',
      state: 'idle',
      updatedAt: '2026-08-02T00:00:00Z',
    } as never);
    store.getState().actions.setDraft('sess-a', 'orphan-to-be');
    store.getState().actions.setDraft('sess-b', 'keep me');

    store.getState().actions.removeSession('sess-a');

    expect(store.getState().drafts.has('sess-a')).toBe(false);
    expect(storedDrafts(storage)['sess-a']).toBeUndefined();
    // A removal must not take the neighbors with it.
    expect(storedDrafts(storage)['sess-b']?.text).toBe('keep me');
  });

  it('a session the sidebar never loaded keeps its draft — the window is paged, not complete', () => {
    const storage = fakeStorage();
    const first = createChatStore({ draftStorage: storage });
    first.getState().actions.setDraft('sess-off-page', 'written before this session scrolled away');

    // A cold load paints one page of sessions; `sess-off-page` is not in it. That says
    // nothing about whether the session exists, so the draft must survive.
    const reloaded = createChatStore({ draftStorage: storage });
    reloaded.getState().actions.setSessions([]);
    expect(reloaded.getState().drafts.get('sess-off-page')).toBe(
      'written before this session scrolled away',
    );
  });
});

describe('the persisted record is bounded', () => {
  it('keeps only the newest MAX_PERSISTED_DRAFTS', () => {
    const storage = fakeStorage();
    const store = createChatStore({ draftStorage: storage });
    for (let i = 0; i < MAX_PERSISTED_DRAFTS + 10; i++) {
      store.getState().actions.setDraft(`sess-${i}`, `draft ${i}`);
    }
    const stored = storedDrafts(storage);
    expect(Object.keys(stored).length).toBe(MAX_PERSISTED_DRAFTS);
  });

  it('drops a draft nobody has touched in MAX_DRAFT_AGE_MS', () => {
    const now = 1_800_000_000_000;
    const storage = fakeStorage({
      [DRAFT_STORAGE_KEY]: JSON.stringify({
        v: DRAFT_RECORD_VERSION,
        drafts: {
          fresh: { text: 'from this morning', updatedAt: now - 1000 },
          ancient: { text: 'from two months ago', updatedAt: now - MAX_DRAFT_AGE_MS - 1 },
        },
      }),
    });
    const drafts = new DraftStore(storage).load(now);
    expect(drafts.get('fresh')).toBe('from this morning');
    expect(drafts.has('ancient')).toBe(false);
  });

  it('boundDrafts evicts by recency, not by insertion order', () => {
    const now = 1_800_000_000_000;
    const bounded = boundDrafts(
      {
        oldest: { text: 'a', updatedAt: now - 3000 },
        newest: { text: 'b', updatedAt: now - 1000 },
        middle: { text: 'c', updatedAt: now - 2000 },
      },
      now,
      2,
    );
    expect(Object.keys(bounded).sort()).toEqual(['middle', 'newest']);
  });

  it('an untouched draft keeps its original stamp across unrelated saves', () => {
    const storage = fakeStorage();
    const draftStore = new DraftStore(storage);
    draftStore.save(new Map([['sess-a', 'written once']]), 1000);
    draftStore.save(
      new Map([
        ['sess-a', 'written once'],
        ['sess-b', 'written later'],
      ]),
      9000,
    );
    const stored = storedDrafts(storage);
    // If every save re-stamped everything, an abandoned draft would never age out.
    expect(stored['sess-a'].updatedAt).toBe(1000);
    expect(stored['sess-b'].updatedAt).toBe(9000);
  });
});

describe('a broken or absent store never breaks the composer', () => {
  it('unparseable JSON yields no drafts and warns rather than throwing', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const storage = fakeStorage({ [DRAFT_STORAGE_KEY]: '{not json' });
    expect(createChatStore({ draftStorage: storage }).getState().drafts.size).toBe(0);
    expect(warn).toHaveBeenCalledOnce();
    warn.mockRestore();
  });

  it('a record from a different schema version is discarded, not guessed at', () => {
    const storage = fakeStorage({
      [DRAFT_STORAGE_KEY]: JSON.stringify({
        v: DRAFT_RECORD_VERSION + 1,
        drafts: { 'sess-a': { text: 'from the future', updatedAt: Date.now() } },
      }),
    });
    expect(createChatStore({ draftStorage: storage }).getState().drafts.size).toBe(0);
  });

  it('a malformed entry is dropped and its well-formed neighbors are kept', () => {
    const now = 1_800_000_000_000;
    const storage = fakeStorage({
      [DRAFT_STORAGE_KEY]: JSON.stringify({
        v: DRAFT_RECORD_VERSION,
        drafts: {
          good: { text: 'kept', updatedAt: now },
          noStamp: { text: 'no updatedAt' },
          notAnObject: 'just a string',
        },
      }),
    });
    const drafts = new DraftStore(storage).load(now);
    expect([...drafts.keys()]).toEqual(['good']);
  });

  it('no storage at all (node, SSR) is not an error — drafts just stay in memory', () => {
    const store = createChatStore({ draftStorage: null });
    store.getState().actions.setDraft('sess-a', 'in memory only');
    expect(store.getState().drafts.get('sess-a')).toBe('in memory only');
  });

  it('a storage that throws on write warns once, not once per keystroke', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const throwing: DraftStorageLike = {
      getItem: () => null,
      setItem: () => {
        throw new DOMException('quota', 'QuotaExceededError');
      },
      removeItem: () => {},
    };
    const store = createChatStore({ draftStorage: throwing });
    store.getState().actions.setDraft('sess-a', 'a');
    store.getState().actions.setDraft('sess-a', 'ab');
    store.getState().actions.setDraft('sess-a', 'abc');

    expect(warn).toHaveBeenCalledOnce();
    // And the composer still works — the box is not the disk.
    expect(store.getState().drafts.get('sess-a')).toBe('abc');
    warn.mockRestore();
  });

  it('a stored record whose entries are the WRONG SHAPE drops those entries and keeps the rest', () => {
    // `isPersistedDraft` refuses anything that is not an object before it reads
    // a field, and both halves of that guard were unpinned: every other case
    // here stores well-formed drafts, so the type check never met a value that
    // could fail it.
    //
    // The values are deliberately varied rather than three of a kind. `null` is
    // the one that makes the `value === null` half load-bearing (`typeof null`
    // is 'object', so the typeof test alone lets it through to a field read on
    // null); a string and a number are what make the `typeof` half load-bearing
    // (a string has no `.text`, so it is refused one line later and the two
    // halves cannot be told apart by that alone -- the difference is whether it
    // is refused or throws).
    const storage = fakeStorage({
      [DRAFT_STORAGE_KEY]: JSON.stringify({
        v: DRAFT_RECORD_VERSION,
        drafts: {
          nullEntry: null,
          stringEntry: 'not a draft at all',
          numberEntry: 7,
          arrayEntry: ['text', 1],
          missingStamp: { text: 'no updatedAt here' },
          wrongStampType: { text: 'stamp is a string', updatedAt: '2026-08-15' },
          good: { text: 'the only real one', updatedAt: Date.now() },
        },
      }),
    });

    const drafts = new DraftStore(storage).load();

    expect([...drafts.keys()]).toEqual(['good']);
    expect(drafts.get('good')).toBe('the only real one');
  });

  it('a record whose whole drafts field is a primitive yields no drafts and does not throw', () => {
    // The outer shape check, one level up from the per-entry one.
    const storage = fakeStorage({
      [DRAFT_STORAGE_KEY]: JSON.stringify({ v: DRAFT_RECORD_VERSION, drafts: 'nonsense' }),
    });
    expect(new DraftStore(storage).load().size).toBe(0);
  });
});
