import { useCallback, useEffect, useMemo, useSyncExternalStore } from 'react'
import type { DiscordNitroCheck, EmojiCatalog, EmojiSettings } from '@kayushkin/multichat-types'
import type { EmojiGroup } from '../../emojiPicker'
import { useBridgeConfig } from '../../context'
import { errorText, useMultichat } from './useMultichat'

/** The unicode table is ~60 KB of source (17 KB gzipped), so it is its own
 *  chunk, fetched the first time anything asks for it and kept after. */
let emojiGroupsLoad: Promise<readonly EmojiGroup[]> | null = null
export function loadEmojiGroups(): Promise<readonly EmojiGroup[]> {
  emojiGroupsLoad ??= import('../../emojiData').then(module => module.EMOJI_GROUPS)
  return emojiGroupsLoad
}

/** The unicode emoji table, null until loaded; a failed load is thrown to
 *  the console and leaves it null, so pickers show custom emoji alone. */
export function useEmojiGroups(): { groups: readonly EmojiGroup[] | null; error: string | null } {
  const store = groupsStore
  const state = useSyncExternalStore(store.subscribe, store.get)
  useEffect(() => {
    if (state.groups || state.error || store.loading) return
    store.loading = true
    loadEmojiGroups()
      .then(groups => store.set({ groups, error: null }))
      .catch(err => store.set({ groups: null, error: errorText(err) }))
  }, [state, store])
  return state
}

type GroupsState = { groups: readonly EmojiGroup[] | null; error: string | null }
const groupsStore = createStore<GroupsState>({ groups: null, error: null })

type CatalogState = { catalog: EmojiCatalog | null; error: string | null }

/** One catalog per multichat base path, shared by every picker, suggestion
 *  list and quick-react bar on the page, so a favorite saved on one shows on
 *  all. */
const catalogStores = new Map<string, Store<CatalogState>>()

function catalogStoreFor(basePath: string) {
  let store = catalogStores.get(basePath)
  if (!store) {
    store = createStore<CatalogState>({ catalog: null, error: null })
    catalogStores.set(basePath, store)
  }
  return store
}

/**
 * multichat's emoji catalog (`GET /emoji`): every bridged Discord server's
 * custom emoji, the favorites and the emoji settings, loaded once and shared.
 * The writes return multichat's refusal in its own words, or null.
 */
export function useEmojiCatalog() {
  const { multichatBasePath } = useBridgeConfig()
  const { read, write, configured } = useMultichat()
  const store = catalogStoreFor(multichatBasePath)
  const state = useSyncExternalStore(store.subscribe, store.get)

  const reload = useCallback(async () => {
    try {
      store.set({ catalog: await read<EmojiCatalog>('/emoji'), error: null })
    } catch (err) {
      store.set({ ...store.get(), error: errorText(err) })
    }
  }, [read, store])

  useEffect(() => {
    if (!configured || state.catalog || store.loading) return
    store.loading = true
    void reload().finally(() => { store.loading = false })
  }, [configured, reload, state.catalog, store])

  const saveFavorites = useCallback(async (favoriteKeys: string[]): Promise<string | null> => {
    const current = store.get().catalog
    // Shown at once; multichat's answer, or its refusal, settles it.
    if (current) store.set({ catalog: { ...current, favorite_keys: favoriteKeys }, error: null })
    const saved = await write<{ favorite_keys: string[] }>('PUT', '/emoji/favorites', { favorite_keys: favoriteKeys })
    const latest = store.get().catalog
    if (!saved.ok) {
      if (current && latest) store.set({ catalog: { ...latest, favorite_keys: current.favorite_keys }, error: null })
      return saved.error
    }
    if (latest) store.set({ catalog: { ...latest, favorite_keys: saved.value.favorite_keys }, error: null })
    return null
  }, [store, write])

  const saveSettings = useCallback(async (settings: EmojiSettings): Promise<string | null> => {
    const saved = await write<EmojiSettings>('PUT', '/emoji/settings', settings)
    if (!saved.ok) return saved.error
    const latest = store.get().catalog
    if (latest) store.set({ catalog: { ...latest, settings: saved.value }, error: null })
    return null
  }, [store, write])

  /** Asks Discord about Nitro now, even if multichat already did today. */
  const checkNitroNow = useCallback(async (): Promise<string | null> => {
    const answer = await write<NitroCheckAnswer>('POST', '/emoji/discord/nitro-check?force=true')
    if (!answer.ok) return answer.error
    applyNitroCheck(store, answer.value)
    return null
  }, [store, write])

  /** Starts the listing of every server's emoji now; multichat keeps its pace
   *  and lists only servers not listed in the past week. The catalog is read
   *  again so the page shows its progress. */
  const startServerSweep = useCallback(async (): Promise<string | null> => {
    const started = await write<unknown>('POST', '/emoji/discord/server-sweep')
    if (!started.ok) return started.error
    await reload()
    return null
  }, [reload, write])

  const refreshFromDiscord = useCallback(async (): Promise<string | null> => {
    const refreshed = await write<EmojiCatalog>('POST', '/emoji/discord/refresh')
    if (!refreshed.ok) return refreshed.error
    store.set({ catalog: refreshed.value, error: null })
    return null
  }, [store, write])

  return useMemo(() => ({ ...state, reload, saveFavorites, saveSettings, refreshFromDiscord, checkNitroNow, startServerSweep }),
    [state, reload, saveFavorites, saveSettings, refreshFromDiscord, checkNitroNow, startServerSweep])
}

/** `POST /emoji/discord/nitro-check`'s answer. */
interface NitroCheckAnswer {
  discord_nitro: DiscordNitroCheck | null
  discord_nitro_error: string
  checked_now: boolean
}

function applyNitroCheck(store: Store<CatalogState>, answer: NitroCheckAnswer) {
  const current = store.get().catalog
  if (current) {
    store.set({ ...store.get(), catalog: { ...current, discord_nitro: answer.discord_nitro, discord_nitro_error: answer.discord_nitro_error } })
  }
}

/** The local day each multichat was last asked for its Nitro check. */
const nitroCheckAskedOn = new Map<string, string>()

/**
 * Asks multichat to check whether the Discord account has Nitro, which
 * decides whether a Discord room is offered other servers' emoji. Every
 * Messages page calls it when it opens; it asks once a day per browser tab,
 * and multichat itself calls Discord at most once a day, so no check is made
 * on a day nobody opens Messages. A refusal is logged: the Emoji page shows
 * multichat's own record of a failed check.
 */
export function useDailyNitroCheck() {
  const { multichatBasePath } = useBridgeConfig()
  const { write, configured } = useMultichat()
  useEffect(() => {
    const today = new Date().toDateString()
    if (!configured || nitroCheckAskedOn.get(multichatBasePath) === today) return
    nitroCheckAskedOn.set(multichatBasePath, today)
    void write<NitroCheckAnswer>('POST', '/emoji/discord/nitro-check').then(answer => {
      if (!answer.ok) { console.error('The daily Nitro check failed:', answer.error); return }
      applyNitroCheck(catalogStoreFor(multichatBasePath), answer.value)
    })
  }, [configured, multichatBasePath, write])
}

interface Store<T> {
  loading: boolean
  get: () => T
  set: (next: T) => void
  subscribe: (listener: () => void) => () => void
}

function createStore<T>(initial: T): Store<T> {
  let value = initial
  const listeners = new Set<() => void>()
  return {
    loading: false,
    get: () => value,
    set: (next: T) => { value = next; listeners.forEach(listener => listener()) },
    subscribe: (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener) } },
  }
}
