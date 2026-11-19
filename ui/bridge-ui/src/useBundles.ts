import { useEffect, useState } from 'react'
import { useBridgeConfig } from './context'
import { listBundles } from './bundleStoreClient'
import type { Bundle } from '@kayushkin/bundle-store-types'

export interface BundlesList {
  /** False when the host passed no `bundleStoreBasePath`: nothing was fetched,
   *  and a bundle id can only be shown as itself. */
  enabled: boolean
  /** Every bundle, enabled or not, or null until the store has answered. */
  bundles: Bundle[] | null
  error: string | null
}

/**
 * bundle-store's whole list, read once per mount — for turning a board's
 * `default_bundle_id` into a name on a card, and for the settings page's
 * picker. Small (six rows on this host), so no search and no shared poll.
 */
export function useBundles(): BundlesList {
  const { fetch: fetchFn, bundleStoreBasePath } = useBridgeConfig()
  const [bundles, setBundles] = useState<Bundle[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!bundleStoreBasePath) return
    let canceled = false
    listBundles(fetchFn, bundleStoreBasePath).then(result => {
      if (canceled) return
      // Go encodes an empty slice it never allocated as null.
      if (result.ok) { setBundles(result.value ?? []); setError(null) } else setError(result.error)
    })
    return () => { canceled = true }
  }, [fetchFn, bundleStoreBasePath])
  return { enabled: !!bundleStoreBasePath, bundles, error }
}
