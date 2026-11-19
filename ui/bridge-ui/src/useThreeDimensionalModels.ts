import { useEffect, useState } from 'react'
import { useBridgeConfig } from './context'
import type { ThreeDimensionalModelsAnswer } from './threeDimensionalModels'

const POLL_INTERVAL_MS = 5000

export interface ThreeDimensionalModelsState {
  answer: ThreeDimensionalModelsAnswer | null
  /** The host's refusal, verbatim, or the network failure. The last answer
   *  stays on screen beside it. */
  error: string | null
  loaded: boolean
}

/** The host's 3D models answer, read again every few seconds while the page is
 *  open and the tab is visible. No request is made when the host carries none
 *  (`threeDimensionalModelsBasePath` empty). */
export function useThreeDimensionalModels(): ThreeDimensionalModelsState {
  const { fetch: fetchFn, threeDimensionalModelsBasePath } = useBridgeConfig()
  const [state, setState] = useState<ThreeDimensionalModelsState>({ answer: null, error: null, loaded: false })

  useEffect(() => {
    if (!threeDimensionalModelsBasePath) return
    let cancelled = false
    let inFlight = false
    const poll = async () => {
      if (inFlight) return
      if (typeof document !== 'undefined' && document.hidden) return
      inFlight = true
      try {
        const response = await fetchFn(threeDimensionalModelsBasePath)
        if (cancelled) return
        if (!response.ok) {
          const text = (await response.text()).trim()
          setState(current => ({ ...current, error: `${response.status}: ${text}`, loaded: true }))
          return
        }
        const answer = (await response.json()) as ThreeDimensionalModelsAnswer
        if (!cancelled) setState({ answer, error: null, loaded: true })
      } catch (error) {
        if (!cancelled) setState(current => ({ ...current, error: String(error), loaded: true }))
      } finally {
        inFlight = false
      }
    }
    void poll()
    const timer = setInterval(() => void poll(), POLL_INTERVAL_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [fetchFn, threeDimensionalModelsBasePath])

  return state
}
