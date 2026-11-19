import { useCallback } from 'react'
import { useBridgeConfig } from '../../context'

export type MultichatWriteResult<T> = { ok: true; value: T } | { ok: false; error: string }

/** Requests to multichat through the host's proxy (`multichatBasePath`), with
 *  the host's credentials. `read` throws multichat's refusal in its own words;
 *  `write` returns it, so a form can show it beside the button that caused it. */
export function useMultichat() {
  const { fetch: apiFetch, multichatBasePath } = useBridgeConfig()

  const read = useCallback(async <T,>(path: string): Promise<T> => {
    const res = await apiFetch(`${multichatBasePath}${path}`)
    if (!res.ok) throw new Error(`GET ${path} → ${res.status}: ${(await res.text()).trim()}`)
    return await res.json() as T
  }, [apiFetch, multichatBasePath])

  const write = useCallback(async <T,>(method: string, path: string, body?: unknown): Promise<MultichatWriteResult<T>> => {
    try {
      const res = await apiFetch(`${multichatBasePath}${path}`, {
        method,
        headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      })
      if (!res.ok) return { ok: false, error: `${method} ${path} → ${res.status}: ${(await res.text()).trim()}` }
      return { ok: true, value: await res.json() as T }
    } catch (err) {
      return { ok: false, error: err instanceof Error ? err.message : String(err) }
    }
  }, [apiFetch, multichatBasePath])

  /** A POST of a multipart form (a file), whose Content-Type the browser
   *  sets with its boundary. */
  const writeForm = useCallback(async <T,>(path: string, form: FormData): Promise<MultichatWriteResult<T>> => {
    try {
      const res = await apiFetch(`${multichatBasePath}${path}`, { method: 'POST', body: form })
      if (!res.ok) return { ok: false, error: `POST ${path} → ${res.status}: ${(await res.text()).trim()}` }
      return { ok: true, value: await res.json() as T }
    } catch (err) {
      return { ok: false, error: err instanceof Error ? err.message : String(err) }
    }
  }, [apiFetch, multichatBasePath])

  return { read, write, writeForm, configured: !!multichatBasePath }
}

export const errorText = (err: unknown) => err instanceof Error ? err.message : String(err)
