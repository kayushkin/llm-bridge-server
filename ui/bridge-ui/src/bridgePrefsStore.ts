import { sharedInstance } from './sharedInstance'
import type { BridgePrefs, FetchFn } from './types'

// The bridge-prefs record, held once per endpoint instead of once per caller.
//
// `useBridgePrefs` used to be a plain hook over `useState`, so every component
// that called it loaded its own copy with its own `GET /bridge-prefs` and kept
// it privately. Two consumers on one page therefore disagreed the moment either
// wrote, and this record is not a display cache: it holds `max_budget` and
// `disabled_tools`. `PUT /bridge-prefs` replaces a harness's record whole, so
// every writer merges over the copy it is holding — and a stale copy does not
// merely show an old ceiling, it writes the old ceiling back over a cleared one.
//
// Same shape as `SharedPoll`, for the same reason, minus the timer: prefs are
// read once and then only change because somebody on this page changed them.
//
// What this deliberately does NOT do is check whether the server accepted the
// write. `updatePrefs` has always set local state first and awaited the PUT
// without reading its status, so a refused write leaves every subscriber
// showing a value the server does not have. Sharing the record makes that
// visible on more surfaces at once rather than causing it; changing it is a
// behavior change and is filed separately.

type Listener = () => void

export interface BridgePrefsSnapshot {
  readonly prefs: BridgePrefs
  readonly loaded: boolean
}

/** Where one store reads its record from and writes it back to. */
export interface BridgePrefsBackend {
  /** The saved record. `{}` when nothing is saved or the read failed — a
   *  failed read must not be reported as a record that exists and is empty,
   *  because callers write merges on top of whatever this returns. */
  load(): Promise<BridgePrefs>
  /** Persist the caller's `partial` and the `merged` record it produced.
   *  Resolves when the write has settled. */
  save(partial: BridgePrefs, merged: BridgePrefs): Promise<void>
}

/** `partial` folded onto `prev`. Field-by-field rather than a spread: the three
 *  map-valued fields merge by key, so writing one harness's defaults must not
 *  drop every other harness's. Pure — the store publishes the result, and the
 *  render checks call it directly. */
export function mergePrefs(prev: BridgePrefs, partial: BridgePrefs): BridgePrefs {
  const next: BridgePrefs = { ...prev }
  if (partial.last_harness) next.last_harness = partial.last_harness
  if (partial.last_instance_id) next.last_instance_id = partial.last_instance_id
  if (partial.last_session) next.last_session = { ...next.last_session, ...partial.last_session }
  if (partial.last_instance) next.last_instance = { ...next.last_instance, ...partial.last_instance }
  if (partial.defaults) next.defaults = { ...next.defaults, ...partial.defaults }
  return next
}

/** Deep-equal for the shapes `BridgePrefs` actually holds: strings, numbers,
 *  booleans, string arrays, and one level of record-of-record. Enough for
 *  `reconcilePrefs`, and deliberately not a general deep-equal. */
function sameValue(a: unknown, b: unknown): boolean {
  if (a === b) return true
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((item, i) => sameValue(item, b[i]))
  }
  if (a && b && typeof a === 'object' && typeof b === 'object') {
    const ka = Object.keys(a as object)
    const kb = Object.keys(b as object)
    if (ka.length !== kb.length) return false
    return ka.every(k => sameValue((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k]))
  }
  return false
}

/** `next` published against `prev`, keeping `prev`'s object identity wherever
 *  the value did not change — the whole record when nothing changed, and each
 *  unchanged sub-record otherwise.
 *
 *  Not a micro-optimization. `useBridgePrefs` deliberately re-creates
 *  `getDefaults` whenever `prefs.defaults` changes identity, because the
 *  settings form hangs a seeding effect off it — and that effect calls
 *  `setLocalDefaults`, throwing away edits the user has not saved. A re-read
 *  that rebuilt `defaults` from JSON every time would wipe a half-typed spend
 *  ceiling every time anything else on the page refreshed the record. So a
 *  refresh that changes nothing must be indistinguishable from no refresh. */
export function reconcilePrefs(prev: BridgePrefs, next: BridgePrefs): BridgePrefs {
  if (sameValue(prev, next)) return prev
  const out: BridgePrefs = { ...next }
  for (const key of ['last_session', 'last_instance', 'defaults'] as const) {
    const before = prev[key]
    const after = next[key]
    if (before && after && sameValue(before, after)) {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (out as any)[key] = before
    }
  }
  return out
}

export class BridgePrefsStore {
  private readonly backend: BridgePrefsBackend
  private readonly listeners = new Set<Listener>()
  private snapshot: BridgePrefsSnapshot = { prefs: {}, loaded: false }
  private loading: Promise<void> | null = null
  /** Partials written while the initial load is in flight. The load publishes
   *  the server's record wholesale, so without replaying these on top of it a
   *  write made in the first few hundred milliseconds is silently reverted —
   *  and one of the things a caller writes here is a cleared spend ceiling.
   *  Null whenever no load is in flight. */
  private writesDuringLoad: BridgePrefs[] | null = null

  constructor(backend: BridgePrefsBackend) {
    this.backend = backend
  }

  getSnapshot = (): BridgePrefsSnapshot => this.snapshot

  get subscriberCount(): number {
    return this.listeners.size
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener)
    void this.ensureLoaded()
    return () => {
      this.listeners.delete(listener)
      // The snapshot is kept on purpose, as in SharedPoll: a component that
      // unmounts and comes back renders the record it already had instead of
      // re-reading it, and `loaded` stays true so a first-render decision
      // gated on it is not taken twice against an empty record.
    }
  }

  /** Read the saved record, once per store. Concurrent callers share the one
   *  attempt; a settled store resolves immediately without another request. */
  ensureLoaded = (): Promise<void> => {
    if (this.snapshot.loaded) return Promise.resolve()
    if (this.loading) return this.loading
    return this.startLoad()
  }

  /** Re-read the record from the backend and publish it, even though the store
   *  has already settled.
   *
   *  This exists because not every writer of this record goes through
   *  `update`. `permission_mode` is written through `POST
   *  /bridge/permission-mode` on purpose — the type's own doc-comment says so,
   *  because a partial `PUT /bridge-prefs` must not be able to clobber the
   *  mode. A reader hanging off this store would therefore go stale the moment
   *  that other endpoint was written, so the writer calls this afterwards. The
   *  alternative, letting that writer publish into the store directly, puts a
   *  second writer of the record on a path that does not go through `update`,
   *  which is the thing this store exists to stop.
   *
   *  A refresh that finds nothing changed notifies nobody: see
   *  `reconcilePrefs`. Concurrent refreshes share one request. */
  refresh = (): Promise<void> => {
    if (this.loading) {
      // A load already in flight was started before whatever prompted this
      // refresh, so its answer may predate the change we are here to pick up.
      // Wait for it, then read again.
      const inFlight = this.loading
      return inFlight.then(() => (this.loading ? this.loading : this.startLoad()))
    }
    return this.startLoad()
  }

  /** One read of the backend, publishing the result with any writes made while
   *  it was in flight replayed on top. Shared by the initial load and by
   *  `refresh`, so a write cannot be reverted by either. */
  private startLoad(): Promise<void> {
    const writes: BridgePrefs[] = []
    this.writesDuringLoad = writes
    const attempt = (async () => {
      let loaded: BridgePrefs = {}
      try {
        loaded = await this.backend.load()
      } catch {
        // A backend reports a failed read as `{}`. If one throws anyway that
        // must not leave every consumer gated on `loaded` waiting forever.
      }
      let prefs = loaded
      for (const write of writes) prefs = mergePrefs(prefs, write)
      if (this.writesDuringLoad === writes) this.writesDuringLoad = null
      this.emit({ prefs: reconcilePrefs(this.snapshot.prefs, prefs), loaded: true })
    })().finally(() => {
      if (this.loading === attempt) this.loading = null
    })
    this.loading = attempt
    return attempt
  }

  /** Fold `partial` into the record, publish it to every subscriber, and
   *  persist it. Resolves when the write has settled. */
  update = (partial: BridgePrefs): Promise<void> => {
    const merged = mergePrefs(this.snapshot.prefs, partial)
    this.writesDuringLoad?.push(partial)
    this.emit({ prefs: merged, loaded: this.snapshot.loaded })
    return this.backend.save(partial, merged)
  }

  private emit(next: BridgePrefsSnapshot): void {
    const current = this.snapshot
    if (current.prefs === next.prefs && current.loaded === next.loaded) return
    this.snapshot = next
    for (const listener of [...this.listeners]) listener()
  }
}

function readLocal(storagePrefix: string): BridgePrefs {
  try {
    const stored = localStorage.getItem(storagePrefix)
    if (stored) return JSON.parse(stored) as BridgePrefs
  } catch { /* ignore */ }
  return {}
}

function writeLocal(storagePrefix: string, prefs: BridgePrefs): void {
  try { localStorage.setItem(storagePrefix, JSON.stringify(prefs)) } catch { /* ignore */ }
}

/** Server-backed prefs. The server is the source of truth for the read;
 *  localStorage is still written so the two modes leave the same trace. */
export function serverPrefsBackend(fetchFn: FetchFn, endpoint: string, storagePrefix: string): BridgePrefsBackend {
  return {
    load: async () => {
      try {
        const res = await fetchFn(endpoint)
        if (res.ok) return (await res.json()) as BridgePrefs
      } catch { /* ignore */ }
      return {}
    },
    save: async (partial, merged) => {
      writeLocal(storagePrefix, merged)
      const res = await fetchFn(endpoint, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(partial),
      })
      // A refused write used to resolve like a landed one, so a settings form
      // said "Saved" over a record the server never took.
      if (!res.ok) throw new Error(`PUT ${endpoint} → ${res.status}: ${await res.text()}`)
    },
  }
}

/** Prefs for a surface with no server behind it. */
export function localPrefsBackend(storagePrefix: string): BridgePrefsBackend {
  return {
    load: async () => readLocal(storagePrefix),
    save: async (_partial, merged) => { writeLocal(storagePrefix, merged) },
  }
}

/** Owner for stores with no fetch function to key on. There is one localStorage
 *  per document, so its stores are keyed on the prefix alone. */
const localOnlyOwner = {}

export interface BridgePrefsStoreKey {
  fetch?: FetchFn
  endpoint?: string
  storagePrefix: string
}

/** The one store for this (fetch, endpoint) — or for this storage prefix when
 *  there is no server. Every caller with the same key gets the same object,
 *  which is what makes two consumers on a page one consumer of the record.
 *
 *  Exported for the render checks, which cannot reach a store through the hook.
 *  Not part of the public API: `index.ts` exports only `useBridgePrefs`. */
export function bridgePrefsStoreFor(key: BridgePrefsStoreKey): BridgePrefsStore {
  const { fetch: fetchFn, endpoint, storagePrefix } = key
  if (fetchFn && endpoint) {
    return sharedInstance(fetchFn, `bridge-prefs ${endpoint} ${storagePrefix}`, () =>
      new BridgePrefsStore(serverPrefsBackend(fetchFn, endpoint, storagePrefix)))
  }
  return sharedInstance(localOnlyOwner, `bridge-prefs local ${storagePrefix}`, () =>
    new BridgePrefsStore(localPrefsBackend(storagePrefix)))
}
