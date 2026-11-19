import { useCallback, useMemo, useSyncExternalStore } from 'react'
import { bridgePrefsStoreFor } from './bridgePrefsStore'
import type { FetchFn, BridgePrefs, HarnessDefaults } from './types'

interface BridgePrefsOptions {
  /** If provided, prefs are synced to this server endpoint. Otherwise localStorage-only. */
  fetch?: FetchFn
  /** Server endpoint for prefs (e.g. "/api/session-meta/bridge"). Required if fetch is provided. */
  endpoint?: string
  /** localStorage key prefix (default: "bridge-prefs") */
  storagePrefix?: string
}

// The saved bridge preferences: last harness, last instance, last session per
// harness, and the per-harness defaults record (model, effort, spend ceiling,
// disabled tools).
//
// The record itself lives in `BridgePrefsStore`, one per endpoint and shared by
// every caller — see that file for why. This hook is the React face of it: it
// subscribes to the store's snapshot and binds the setters.
export function useBridgePrefs(options: BridgePrefsOptions = {}) {
  const { fetch: fetchFn, endpoint, storagePrefix = 'bridge-prefs' } = options

  const store = useMemo(
    () => bridgePrefsStoreFor({ fetch: fetchFn, endpoint, storagePrefix }),
    [fetchFn, endpoint, storagePrefix],
  )
  // `loaded` flips true once the initial load resolves (from server or
  // localStorage). Consumers that key a first-render decision off a pref — e.g.
  // the chat's pending-new-chat bootstrap, which needs last_instance_id — gate
  // on this so they don't act on an empty prefs snapshot and pick the wrong
  // default. A consumer that mounts after the store has settled sees it true
  // straight away and costs no second request.
  const { prefs, loaded } = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot)

  const updatePrefs = useCallback(
    (partial: BridgePrefs) => store.update(partial),
    [store],
  )

  // For the one field that is written through a different endpoint on purpose:
  // `permission_mode` goes out via `POST /bridge/permission-mode` so a partial
  // `PUT /bridge-prefs` cannot clobber it, which leaves this record stale until
  // it is re-read. A refresh that finds nothing changed notifies nobody.
  const refreshPrefs = useCallback(() => store.refresh(), [store])

  const setLastHarness = useCallback((harness: string) => {
    updatePrefs({ last_harness: harness })
  }, [updatePrefs])

  const setLastInstanceId = useCallback((instanceId: string) => {
    updatePrefs({ last_instance_id: instanceId })
  }, [updatePrefs])

  const setLastSession = useCallback((harness: string, sessionId: string) => {
    updatePrefs({ last_session: { [harness]: sessionId } })
  }, [updatePrefs])

  const setHarnessDefaults = useCallback((harness: string, defaults: HarnessDefaults): Promise<void> => {
    return updatePrefs({ defaults: { [harness]: defaults } })
  }, [updatePrefs])

  const setLastInstance = useCallback((harness: string, instanceId: string) => {
    updatePrefs({ last_instance: { [harness]: instanceId } })
  }, [updatePrefs])

  // The three getters read the store's live snapshot rather than this render's
  // copy, so a caller that reads-modifies-writes — every writer of `defaults`
  // does, because `PUT /bridge-prefs` replaces a harness's record whole — merges
  // over the newest record even if a sibling wrote it after this render began.
  //
  // Their identity still changes whenever the slice they read changes, because
  // callers hang effects off it. The settings form seeds itself in an effect
  // keyed on (harness names, `getDefaults`); when the record arrives AFTER the
  // harness list, that identity is the only thing left to re-run the effect,
  // and without it the form seeds once from an empty record and stays empty.
  // Measured, not assumed: freezing these deps reddens
  // `dash/e2e/prefs-shared-record.spec.ts`, and that ordering happens on its own
  // often enough that the check without the injected delay caught it too.
  // Hence the deps below, which the bodies deliberately do not close over.
  const prefsDefaults = prefs.defaults
  const getDefaults = useCallback((harness: string): HarnessDefaults => {
    return store.getSnapshot().prefs.defaults?.[harness] ?? {}
  }, [store, prefsDefaults])

  const prefsLastInstance = prefs.last_instance
  const getLastInstance = useCallback((harness: string): string | null => {
    return store.getSnapshot().prefs.last_instance?.[harness] ?? null
  }, [store, prefsLastInstance])

  const prefsLastSession = prefs.last_session
  const getLastSession = useCallback((harness: string): string | null => {
    return store.getSnapshot().prefs.last_session?.[harness] ?? null
  }, [store, prefsLastSession])

  return useMemo(() => ({
    prefs,
    loaded,
    refreshPrefs,
    setLastHarness,
    setLastInstanceId,
    setLastSession,
    setLastInstance,
    setHarnessDefaults,
    getDefaults,
    getLastInstance,
    getLastSession,
  }), [
    prefs,
    loaded,
    refreshPrefs,
    setLastHarness,
    setLastInstanceId,
    setLastSession,
    setLastInstance,
    setHarnessDefaults,
    getDefaults,
    getLastInstance,
    getLastSession,
  ])
}
