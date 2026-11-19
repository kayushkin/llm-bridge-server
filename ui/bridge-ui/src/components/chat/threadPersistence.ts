// Thread-pane state that belongs to the COMPONENT rather than to chat-core: whether
// assistant prose is rendered as markdown or as plain text (the header's MD/TXT toggle).
//
// ⚠️ This key is chat's own, for the same reason `sidebarPersistence.ts` gives for
// its two: dash served bridge-ui's chat beside its own while both existed, so they
// shared one origin and one localStorage. There is a second reason here, and it is the
// stronger one — bridge-ui's `MD_PREF_KEY` is a module-private constant in
// `TurnsView.tsx`, exported from nothing. Copying its literal would join the two pages
// on a name that bridge-ui is free to change without telling anyone, and the failure
// would be silent: chat would simply start each session on the default again.
//
// The two pages therefore keep separate preferences. That is a real divergence from
// the original chat and it is on purpose; the toggles are in different places (chat's
// is in `SessionHeader`, bridge-ui's is inside the Turns pane itself) and chat already
// diverges on folder collapse and the filter bar.

import { chatKey, migrateLegacyKeys } from './storageKeys'

// Keys live under the chat prefix. They were written under the page's old prefix while
// it had its old name, and `storageKeys.ts` carries those rows over on first read — see
// the `migrateLegacyKeys` call in `storage()` below. Add a key here AND to MIGRATED_KEYS.
const MARKDOWN_KEY = chatKey('turns-markdown')
const MIGRATED_KEYS: readonly string[] = [MARKDOWN_KEY]

/** Whether persistence works here at all. Checks the METHODS rather than the name —
 *  see chat-core's `webStorage.ts` for the Node/SSR/blocked-by-policy cases this
 *  covers. Duplicated from `sidebarPersistence.ts` rather than shared, because the two
 *  files are otherwise independent and a shared helper would be the only thing tying
 *  the sidebar's storage to the thread's. */
function storage(): Storage | null {
  try {
    if (typeof window === 'undefined') return null
    const candidate = window.localStorage
    if (
      !candidate ||
      typeof candidate.getItem !== 'function' ||
      typeof candidate.setItem !== 'function' ||
      typeof candidate.removeItem !== 'function'
    ) {
      return null
    }
    // First read after the rename carries the legacy rows across; see storageKeys.ts.
    migrateLegacyKeys(candidate, MIGRATED_KEYS)
    return candidate
  } catch {
    return null
  }
}

/** Whether assistant prose renders as markdown. Defaults to ON, which is what chat
 *  has always done and what bridge-ui does.
 *
 *  Only the literal `'off'` turns it off. Absent means "never chosen" and anything else
 *  means a value this code did not write — both render markdown, so a corrupted or
 *  half-written record degrades to the default rather than to plain text. Plain text is
 *  the state a user has to ask for; it should never be the state they arrive in. */
export function loadMarkdownPref(): boolean {
  const store = storage()
  if (!store) return true
  try {
    return store.getItem(MARKDOWN_KEY) !== 'off'
  } catch {
    return true
  }
}

export function saveMarkdownPref(markdown: boolean): void {
  const store = storage()
  if (!store) return
  try {
    // Written on both edges rather than removed on the default: the toggle is global
    // and long-lived, and a user who turns markdown back on has made a choice worth
    // recording as one. Removing the row instead would be indistinguishable from
    // never having chosen, which matters the day the default changes.
    store.setItem(MARKDOWN_KEY, markdown ? 'on' : 'off')
  } catch {
    // A full or refused quota must not cost the user the click they just made.
  }
}
