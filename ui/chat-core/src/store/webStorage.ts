// The one place chat-core decides whether a synchronous key/value store exists.
//
// Two things persist across a reload — composer drafts (`draftStorage.ts`) and the
// sidebar's filter selection (`filterStorage.ts`) — and both need the SAME answer to
// the same awkward question: is there a `localStorage` here that actually stores
// anything? The probe below is subtle enough that a second copy of it would drift, so
// it lives here and both stores take it.

/** The slice of the DOM `Storage` interface chat-core needs. Narrow on purpose: it
 *  makes every persisted store testable off a plain object, and `localStorage`
 *  satisfies it. */
export interface WebStorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** Resolve the browser's localStorage, or null anywhere it does not work (SSR, a
 *  test, a browser that blocks it by policy).
 *
 *  The probe checks for the METHODS, not for the name. Node 25 binds a
 *  `globalThis.localStorage` that is an empty object unless the process was started
 *  with `--localstorage-file`, so "the name is defined" is not the same question as
 *  "this can store anything". Reading the property can also throw outright when a
 *  browser refuses storage, hence the try. */
export function defaultWebStorage(): WebStorageLike | null {
  try {
    // Ask whether there is a document first. Node's stub is created lazily on first
    // read and prints a `--localstorage-file` warning when there is no backing file,
    // so on a server the cheapest correct answer is not to look at all.
    if (typeof (globalThis as { window?: unknown }).window === 'undefined') return null;
    const candidate = (globalThis as { localStorage?: Partial<WebStorageLike> }).localStorage;
    if (!candidate) return null;
    if (
      typeof candidate.getItem !== 'function' ||
      typeof candidate.setItem !== 'function' ||
      typeof candidate.removeItem !== 'function'
    ) {
      return null;
    }
    return candidate as WebStorageLike;
  } catch {
    return null;
  }
}
