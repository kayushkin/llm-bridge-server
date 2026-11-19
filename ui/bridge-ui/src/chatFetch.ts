import type { FetchFn } from './types'

// Adapt the host's fetch (bridge-ui's `FetchFn`, string-only) to the full
// `typeof fetch` signature ChatProvider expects. ApiClient only ever passes string
// URLs, but URL/Request are covered too so the types are honest.
export function chatFetchOver(fetchFn: FetchFn): typeof fetch {
  return (input, init) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    return fetchFn(url, init)
  }
}
