/** The chat page's localStorage key prefix, and the one-time carry-over from the
 *  prefix it had under the page's old name.
 *
 *  A storage key is a contract with the user's browser, not an internal name. Renaming
 *  one does not move a stored value, it abandons it: every preference under the old
 *  prefix — sidebar collapsed, pane sizes, markdown on or off, and six more — would
 *  silently reset to its default on the next load. So the rename of the page did not
 *  rename these, and this file is what finally lets it: each persistence module hands
 *  its keys to `migrateLegacyKeys` from its own `storage()` accessor, so the first read
 *  after the rename copies the legacy row across and deletes it.
 *
 *  Idempotent and cheap on purpose — nine `getItem`s per accessor call — because it
 *  runs on every read rather than once at boot. A once-at-boot migration would miss a
 *  legacy row written by a tab that is still open on the old build, and a preference
 *  saved from that tab would then be lost to this one. */

export const CHAT_KEY_PREFIX = 'chat:'
const LEGACY_KEY_PREFIX = 'dashv2:'

/** Build a key under the chat prefix. The suffix is the part that survives the rename. */
export function chatKey(suffix: string): string {
  return CHAT_KEY_PREFIX + suffix
}

/** Carry each key's legacy-prefix row over to its chat (`chat:`) row.
 *
 *  A key that already has a chat row keeps it — a preference set since the rename
 *  outranks one set before it — but the legacy row is removed either way, so the
 *  carry-over happens at most once per key. A key given without the chat prefix is a
 *  programming error and throws rather than silently migrating nothing. */
export function migrateLegacyKeys(store: Storage, keys: readonly string[]): void {
  for (const key of keys) {
    if (!key.startsWith(CHAT_KEY_PREFIX)) {
      throw new Error(`migrateLegacyKeys: "${key}" is not under "${CHAT_KEY_PREFIX}"`)
    }
    const legacyKey = LEGACY_KEY_PREFIX + key.slice(CHAT_KEY_PREFIX.length)
    let legacyValue: string | null
    try {
      legacyValue = store.getItem(legacyKey)
    } catch {
      return
    }
    if (legacyValue === null) continue
    try {
      if (store.getItem(key) === null) store.setItem(key, legacyValue)
      store.removeItem(legacyKey)
    } catch {
      // A refused write leaves the legacy row in place, so the next read tries again.
    }
  }
}
