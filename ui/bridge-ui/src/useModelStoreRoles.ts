import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from './context'
import { fetchModelStoreRoles } from './modelRoles'
import type { ModelStoreRoles } from './types-model-store'

/** model-store's roles as the host proxies them, read once and again on
 *  `reload`. `error` says why they could not be read — including a host that
 *  does not proxy model-store — so a page shows it instead of an empty list. */
export type ModelStoreRolesState =
  | { phase: 'loading' }
  | { phase: 'loaded'; roles: ModelStoreRoles }
  | { phase: 'failed'; error: string }

export function useModelStoreRoles(): { state: ModelStoreRolesState; reload: () => void } {
  const { fetch: fetchFn, modelStoreBasePath } = useBridgeConfig()
  const [state, setState] = useState<ModelStoreRolesState>({ phase: 'loading' })
  const [generation, setGeneration] = useState(0)

  useEffect(() => {
    if (!modelStoreBasePath) {
      setState({ phase: 'failed', error: 'This host does not proxy model-store (modelStoreBasePath is empty), so the roles cannot be listed.' })
      return
    }
    let canceled = false
    fetchModelStoreRoles(fetchFn, modelStoreBasePath)
      .then(roles => { if (!canceled) setState({ phase: 'loaded', roles }) })
      .catch(err => { if (!canceled) setState({ phase: 'failed', error: err instanceof Error ? err.message : String(err) }) })
    return () => { canceled = true }
  }, [fetchFn, modelStoreBasePath, generation])

  const reload = useCallback(() => setGeneration(g => g + 1), [])
  return { state, reload }
}
