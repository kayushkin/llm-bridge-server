import { useCallback } from 'react'
import { useBridgeConfig } from '../../context'
import { ROOM_SHORT_NAMES_PATH, roomShortNamePath, shortNameRefusal, shortNameResolvePath } from '../../messageShortNames'
import type { ShortName, ShortNameResolution } from '../../types-short-name-store'

export type ShortNameLookup = { kind: 'found'; resolution: ShortNameResolution } | { kind: 'missing' }
export type ShortNameWriteResult = { ok: true; name: ShortName } | { ok: false; error: string }

/** The rooms' typeable names, through multichat (`multichatBasePath`), which
 *  reads and writes them in short-name-store. A read throws the refusal in its
 *  own words; a rename returns it, for the form that caused it. */
export function useRoomShortNames() {
  const { fetch: apiFetch, multichatBasePath } = useBridgeConfig()

  /** Every room's current name. */
  const listRoomNames = useCallback(async (): Promise<ShortName[]> => {
    const res = await apiFetch(`${multichatBasePath}${ROOM_SHORT_NAMES_PATH}`)
    if (!res.ok) throw new Error(`GET ${ROOM_SHORT_NAMES_PATH} → ${shortNameRefusal(res.status, await res.text())}`)
    return await res.json() as ShortName[] | null ?? []
  }, [apiFetch, multichatBasePath])

  /** What a path names. A 404 is an answer, not a failure. */
  const resolve = useCallback(async (shortPath: string): Promise<ShortNameLookup> => {
    const path = shortNameResolvePath(shortPath)
    const res = await apiFetch(`${multichatBasePath}${path}`)
    if (res.status === 404) return { kind: 'missing' }
    if (!res.ok) throw new Error(`GET ${path} → ${shortNameRefusal(res.status, await res.text())}`)
    return { kind: 'found', resolution: await res.json() as ShortNameResolution }
  }, [apiFetch, multichatBasePath])

  /** Gives a room the name typed, as the operator. */
  const rename = useCallback(async (roomID: string, typed: string): Promise<ShortNameWriteResult> => {
    try {
      const res = await apiFetch(`${multichatBasePath}${roomShortNamePath(roomID)}`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: typed }),
      })
      if (!res.ok) return { ok: false, error: shortNameRefusal(res.status, await res.text()) }
      return { ok: true, name: await res.json() as ShortName }
    } catch (err) {
      return { ok: false, error: err instanceof Error ? err.message : String(err) }
    }
  }, [apiFetch, multichatBasePath])

  return { listRoomNames, resolve, rename, configured: !!multichatBasePath }
}
