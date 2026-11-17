import { describe, expect, it, vi } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import {
  FilterStore,
  boundFilterValues,
  FILTER_STORAGE_KEY,
  FILTER_RECORD_VERSION,
  MAX_PERSISTED_FILTER_VALUES,
  PERSISTED_FILTER_AXES,
} from '../src/store/filterStorage.js';
import type { PersistedFilterAxis } from '../src/store/filterStorage.js';
import type { WebStorageLike } from '../src/store/webStorage.js';

// The sidebar's filter selection used to live in memory only: `ChatState.filter` was
// initialized to `EMPTY_FILTER` and nothing ever rehydrated it, so every reload threw
// away whatever the user had narrowed the list down to.
//
// Four things have to hold, and they are why these tests exist rather than a
// screenshot of a chip row:
//
//  1. The filter is read back SYNCHRONOUSLY, at store construction. Arriving one
//     paint later means the list is drawn unfiltered and rows are then pulled out
//     from under a user already reading it.
//  2. `search` and `folder` are NEVER persisted. A restored query re-runs a
//     transcript search on load and hands back a short list with no visible cause; a
//     restored folder can point at one that has since been deleted.
//  3. The persisted shape is chat-core's own INCLUSION arrays, not bridge-ui's
//     exclusion sets. The two surfaces share an origin, so this is checked by key as
//     well as by shape.
//  4. Nothing grows without a bound, and a record this store did not write is
//     discarded rather than guessed at.

/** A `Storage`-shaped object over a plain Map, so a "reload" is just a second
 *  `createChatStore` reading the same backing object. */
function fakeStorage(seed: Record<string, string> = {}): WebStorageLike & {
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

function storedAxes(storage: WebStorageLike): Record<string, string[]> {
  const raw = storage.getItem(FILTER_STORAGE_KEY);
  if (raw === null) return {};
  return (JSON.parse(raw) as { axes: Record<string, string[]> }).axes;
}

/** The seven axes this suite pins, written out rather than read from `filterStorage`.
 *
 *  ⚠️ Do not replace this with `PERSISTED_FILTER_AXES`. Comparing the shipped table
 *  against itself agrees by construction: measured 2026-08-10, dropping `machine` from
 *  BOTH authorings in `filterStorage.ts` — the array and the literal inside
 *  `emptyPersistedFilterAxes` — compiles clean and leaves all 15 tests here green,
 *  including the one named "restores all six axes and only those six". The word "six"
 *  was asserted by nothing. Changing an axis should have to change this line too, and
 *  the diff is where that decision becomes visible. */
const EXPECTED_PERSISTED_AXES = ['harness', 'status', 'type', 'purpose', 'mode', 'machine', 'principal'];

describe('the sidebar filter survives a reload', () => {
  it('reads every persisted axis back synchronously at construction', () => {
    const storage = fakeStorage();
    const first = createChatStore({ filterStorage: storage, draftStorage: null });
    first.getState().actions.setFilter({ harness: ['claude-code'], status: ['running'] });

    const second = createChatStore({ filterStorage: storage, draftStorage: null });
    // Read off the state the store was CONSTRUCTED with — no awaits, no ticks.
    expect(second.getState().filter.harness).toEqual(['claude-code']);
    expect(second.getState().filter.status).toEqual(['running']);
  });

  it('restores all seven axes and only those seven', () => {
    const storage = fakeStorage();
    const first = createChatStore({ filterStorage: storage, draftStorage: null });
    first.getState().actions.setFilter({
      harness: ['claude-code'],
      status: ['running'],
      type: ['chat'],
      purpose: ['coding'],
      mode: ['events'],
      machine: ['inst-1'],
      principal: ['principal_000002'],
    });

    const restored = createChatStore({ filterStorage: storage, draftStorage: null }).getState().filter;

    // The count first, against the literal above — otherwise every line below is a claim
    // about however many axes happen to ship, and "seven" is decoration.
    expect([...PERSISTED_FILTER_AXES].sort()).toEqual([...EXPECTED_PERSISTED_AXES].sort());
    for (const axis of EXPECTED_PERSISTED_AXES) {
      expect(restored[axis as PersistedFilterAxis]).toHaveLength(1);
    }
    expect(Object.keys(storedAxes(storage)).sort()).toEqual([...EXPECTED_PERSISTED_AXES].sort());
  });

  it('never persists the search query', () => {
    const storage = fakeStorage();
    const store = createChatStore({ filterStorage: storage, draftStorage: null });
    store.getState().actions.setFilter({ harness: ['claude-code'] });
    store.getState().actions.setFilter({ search: 'a phrase from a transcript' });

    expect(JSON.stringify(storedAxes(storage))).not.toContain('a phrase from a transcript');
    expect(createChatStore({ filterStorage: storage, draftStorage: null }).getState().filter.search).toBe('');
  });

  it('never persists the open folder', () => {
    const storage = fakeStorage();
    const store = createChatStore({ filterStorage: storage, draftStorage: null });
    store.getState().actions.setFilter({ harness: ['claude-code'] });
    store.getState().actions.openFolder('a folder that may be deleted tomorrow');

    expect(JSON.stringify(storedAxes(storage))).not.toContain('a folder that may be deleted tomorrow');
    expect(createChatStore({ filterStorage: storage, draftStorage: null }).getState().filter.folder).toBeNull();
  });

  it('does not write on a search keystroke, only on an axis change', () => {
    const storage = fakeStorage();
    const store = createChatStore({ filterStorage: storage, draftStorage: null });
    store.getState().actions.setFilter({ harness: ['claude-code'] });

    const setItem = vi.spyOn(storage, 'setItem');
    for (const q of ['a', 'ab', 'abc']) store.getState().actions.setFilter({ search: q });
    expect(setItem).not.toHaveBeenCalled();

    store.getState().actions.setFilter({ status: ['running'] });
    expect(setItem).toHaveBeenCalledTimes(1);
  });

  it('clearing every chip removes the record rather than storing an empty one', () => {
    const storage = fakeStorage();
    const store = createChatStore({ filterStorage: storage, draftStorage: null });
    store.getState().actions.setFilter({ harness: ['claude-code'] });
    expect(storage.getItem(FILTER_STORAGE_KEY)).not.toBeNull();

    store.getState().actions.setFilter({
      harness: [],
      status: [],
      type: [],
      purpose: [],
      mode: [],
      machine: [],
    });
    expect(storage.getItem(FILTER_STORAGE_KEY)).toBeNull();
  });

  it('writes under chat-core’s own key, not one of bridge-ui’s', () => {
    // dash served bridge-ui's chat beside its own — one origin, so a
    // shared key would have the two pages overwriting each other, and bridge-ui's
    // values mean the opposite of these (exclusion, not inclusion).
    const storage = fakeStorage();
    createChatStore({ filterStorage: storage, draftStorage: null })
      .getState()
      .actions.setFilter({ harness: ['claude-code'] });

    expect([...storage.data.keys()]).toEqual([FILTER_STORAGE_KEY]);
    for (const bridgeUiKey of [
      'bridge-ui-harness-filter',
      'bridge-ui-session-type-filter',
      'bridge-ui-session-purpose-filter',
      'bridge-ui-session-mode-filter',
      'bridge-ui-session-status-filter',
      'bridge.machineFilter.excluded',
    ]) {
      expect(storage.data.has(bridgeUiKey)).toBe(false);
    }
  });

  it('restores an inclusion array as an inclusion array', () => {
    // The trap this pins: one selected harness must come back as "show ONLY this
    // one". Read as bridge-ui's exclusion shape it would mean "hide this one", which
    // is the exact opposite and would silently hide the sessions the user was reading.
    const storage = fakeStorage();
    createChatStore({ filterStorage: storage, draftStorage: null })
      .getState()
      .actions.setFilter({ harness: ['claude-code'] });

    const state = createChatStore({ filterStorage: storage, draftStorage: null }).getState();
    const matching = { sessionId: 's1', harness: 'claude-code' };
    const other = { sessionId: 's2', harness: 'codex' };
    expect(state.filter.harness.includes(matching.harness)).toBe(true);
    expect(state.filter.harness.includes(other.harness)).toBe(false);
  });
});

describe('a filter record this store did not write', () => {
  it('is discarded when the schema version differs', () => {
    const storage = fakeStorage({
      [FILTER_STORAGE_KEY]: JSON.stringify({
        v: FILTER_RECORD_VERSION + 1,
        axes: { harness: ['claude-code'] },
      }),
    });
    expect(createChatStore({ filterStorage: storage, draftStorage: null }).getState().filter.harness).toEqual([]);
  });

  it('is discarded when it is not valid JSON, without throwing', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const storage = fakeStorage({ [FILTER_STORAGE_KEY]: '{not json' });
    expect(createChatStore({ filterStorage: storage, draftStorage: null }).getState().filter.harness).toEqual([]);
    expect(warn).toHaveBeenCalledOnce();
    warn.mockRestore();
  });

  it('keeps only the string values of an axis', () => {
    const storage = fakeStorage({
      [FILTER_STORAGE_KEY]: JSON.stringify({
        v: FILTER_RECORD_VERSION,
        axes: { harness: ['claude-code', 7, null, '', { nested: true }, 'codex'], status: 'running' },
      }),
    });
    const filter = createChatStore({ filterStorage: storage, draftStorage: null }).getState().filter;
    expect(filter.harness).toEqual(['claude-code', 'codex']);
    // A whole axis of the wrong TYPE is not a filter of one character — a bare
    // string would otherwise be spread into ['r','u','n',…] by anything iterating it.
    expect(filter.status).toEqual([]);
  });
});

describe('bounds', () => {
  it('de-duplicates and caps an axis', () => {
    const many = Array.from({ length: MAX_PERSISTED_FILTER_VALUES + 50 }, (_, i) => `h${i}`);
    expect(boundFilterValues([...many, ...many])).toHaveLength(MAX_PERSISTED_FILTER_VALUES);
    expect(boundFilterValues(['a', 'a', 'b'])).toEqual(['a', 'b']);
  });

  it('bounds what is READ as well as what is written', () => {
    // The write path is not the only way bytes get into localStorage — a record
    // written by a future version, or by hand, arrives through `load` alone.
    const storage = fakeStorage({
      [FILTER_STORAGE_KEY]: JSON.stringify({
        v: FILTER_RECORD_VERSION,
        axes: { harness: Array.from({ length: 5000 }, (_, i) => `h${i}`) },
      }),
    });
    expect(new FilterStore(storage).load().harness).toHaveLength(MAX_PERSISTED_FILTER_VALUES);
  });
});

describe('storage that refuses to store', () => {
  it('is not an error the caller ever sees', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const refusing: WebStorageLike = {
      getItem: () => {
        throw new Error('blocked by policy');
      },
      setItem: () => {
        throw new Error('quota exceeded');
      },
      removeItem: () => {
        throw new Error('blocked by policy');
      },
    };
    const store = createChatStore({ filterStorage: refusing, draftStorage: null });
    expect(() => store.getState().actions.setFilter({ harness: ['claude-code'] })).not.toThrow();
    // The filter still applies in memory — persistence failing must not cost the user
    // the click they just made.
    expect(store.getState().filter.harness).toEqual(['claude-code']);
    warn.mockRestore();
  });

  it('persists nothing at all when the caller passes null', () => {
    const store = createChatStore({ filterStorage: null, draftStorage: null });
    expect(new FilterStore(null).isEnabled).toBe(false);
    expect(() => store.getState().actions.setFilter({ harness: ['claude-code'] })).not.toThrow();
  });
});
