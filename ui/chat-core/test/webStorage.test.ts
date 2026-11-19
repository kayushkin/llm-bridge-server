import { afterEach, describe, expect, it, vi } from 'vitest';
import { defaultWebStorage, type WebStorageLike } from '../src/store/webStorage.js';

// `defaultWebStorage` is the one place chat-core decides whether a synchronous
// key/value store exists, and both persisted stores (draftStorage, filterStorage)
// take their answer from it. Until this file existed it had NO test at all: the
// whole body could be replaced with `return null` and the suite stayed green, so
// every one of its four `null` producers was free to disappear unnoticed.
//
// It answers null for four different reasons, and the reasons are not
// interchangeable — each guards a different way for storage to be absent-or-broken.
// So the tests below are written per PRODUCER, not per outcome: each pins one
// reason by arranging a world where that reason is the ONLY one that fires, and
// where deleting it hands back a storage the caller would then trust.
//
// Two of the four cannot be pinned alone, and the tests say which and why rather
// than covering it over — see `the two producers no assertion can tell apart`.

/** A storage that satisfies the whole `WebStorageLike` contract, so the only thing
 *  that can make `defaultWebStorage` refuse it is a guard firing. */
function workingStorage(): WebStorageLike {
  const held = new Map<string, string>();
  return {
    getItem: (key) => held.get(key) ?? null,
    setItem: (key, value) => void held.set(key, value),
    removeItem: (key) => void held.delete(key),
  };
}

/** Install a `localStorage` whose GETTER throws, the way a browser refuses storage
 *  by policy. `vi.stubGlobal` assigns a value and cannot express this, so the
 *  property is defined by hand and restored by the returned callable. */
function installThrowingLocalStorage(): () => void {
  const original = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    get() {
      throw new DOMException('The operation is insecure.', 'SecurityError');
    },
  });
  return () => {
    if (original) Object.defineProperty(globalThis, 'localStorage', original);
    else delete (globalThis as { localStorage?: unknown }).localStorage;
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('defaultWebStorage — the four separate reasons there is no storage', () => {
  it('hands back the storage itself when there is a window and it works', () => {
    // The positive case, and it is what makes every `toBeNull` below mean
    // something: without it, a function that only ever returned null would pass
    // this entire file.
    const storage = workingStorage();
    vi.stubGlobal('window', {});
    vi.stubGlobal('localStorage', storage);

    // The SAME object, not a copy or a wrapper: both persisted stores keep the
    // reference and write through it.
    expect(defaultWebStorage()).toBe(storage);
  });

  it('refuses on a server even when a perfectly good localStorage is in scope', () => {
    // Producer 1, pinned alone. The pairing is deliberate and it is the only one
    // that isolates this guard: a WORKING localStorage with NO window. Drop the
    // window check and this returns that storage, so a server-rendered pass would
    // read and write a store that does not survive the request.
    //
    // Under plain Node the guard is invisible: Node binds a lazy `localStorage`
    // stub carrying no methods at all, so the method probe below would refuse it
    // anyway and deleting this guard changes nothing observable. That masking is
    // why the working storage here has to be supplied by hand.
    vi.stubGlobal('localStorage', workingStorage());
    expect('window' in globalThis).toBe(false);

    expect(defaultWebStorage()).toBeNull();
  });

  it('refuses an object that merely EXISTS but stores nothing', () => {
    // Producer 3, pinned alone: a window, and a localStorage that is truthy and
    // therefore passes producer 2's guard, but has none of the methods. This is
    // Node's own stub, and it is the case the file's comment is about — "the name
    // is defined" is not the same question as "this can store anything".
    vi.stubGlobal('window', {});
    vi.stubGlobal('localStorage', {});

    expect(defaultWebStorage()).toBeNull();
  });

  it('refuses a PARTIAL storage, one missing method at a time', () => {
    // Producer 3 again, one probe per method. Without this, the three-way check
    // could be narrowed to `getItem` alone and nothing would notice — and a
    // read-only stub accepted here is a draft that reads back fine and silently
    // never saves.
    vi.stubGlobal('window', {});
    const complete = workingStorage();

    for (const missing of ['getItem', 'setItem', 'removeItem'] as const) {
      const partial: Record<string, unknown> = { ...complete };
      delete partial[missing];
      vi.stubGlobal('localStorage', partial);

      expect(defaultWebStorage(), `a storage with no ${missing} must be refused`).toBeNull();
    }

    // ...and the same object with nothing removed is still accepted, so the loop
    // above is failing on the missing method rather than on the spread.
    vi.stubGlobal('localStorage', { ...complete });
    expect(defaultWebStorage()).not.toBeNull();
  });

  it('refuses instead of throwing when the browser refuses storage by policy', () => {
    // Producer 4, pinned alone. Reading `localStorage` can throw outright rather
    // than answer, and callers construct their stores at module scope — so a
    // rethrow here does not degrade to "no persistence", it takes down the app.
    vi.stubGlobal('window', {});
    const restore = installThrowingLocalStorage();
    try {
      expect(() => defaultWebStorage()).not.toThrow();
      expect(defaultWebStorage()).toBeNull();
    } finally {
      restore();
    }
  });
});

describe('defaultWebStorage — the two producers no assertion can tell apart', () => {
  it('refuses when localStorage is absent, which pins producers 2 and 4 only JOINTLY', () => {
    // Producer 2 is `if (!candidate) return null`. It cannot be pinned on its own
    // by any assertion over the return value, and this test does not pretend
    // otherwise. Delete it and the very next line reads `.getItem` off `undefined`,
    // which throws, which producer 4's catch swallows into the same null. Two
    // different paths, one indistinguishable answer.
    //
    // What is pinned is the PAIR: delete producer 2 and make the catch rethrow and
    // this test reddens. Recorded rather than papered over, because a reader who
    // sees producer 2 covered would otherwise believe a single-guard mutation is
    // caught here. It is not, and the fix is not a cleverer assertion — the two
    // are behaviorally identical from outside and the guard earns its place by
    // being the readable one, not the observable one.
    vi.stubGlobal('window', {});
    vi.stubGlobal('localStorage', undefined);

    expect(defaultWebStorage()).toBeNull();
  });
});
