import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useBridgeConfig } from './context'
import type { Tool, LocalDescriptor } from '@kayushkin/tool-store-types'

/**
 * useBridgeTools — fetches the global tool list, the in-process registry of
 * available locals, and (lazily, when an instanceID is set) the per-instance
 * opt-in list. Provides toggle actions and a `byInstance` Set for O(1) lookup
 * during render.
 *
 * Polls every 30s like the other bridge hooks. Requires
 * `toolStoreBasePath` on BridgeConfig — the hook returns `loading=false` and
 * empty arrays when unset (the Tools tab is hidden in that case anyway).
 */
export function useBridgeTools(instanceID: string | null) {
  const { fetch: fetchFn, toolStoreBasePath } = useBridgeConfig()

  const [tools, setTools] = useState<Tool[]>([])
  const [locals, setLocals] = useState<LocalDescriptor[]>([])
  const [instanceTools, setInstanceTools] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const lastJsonRef = useRef('')
  const enabled = !!toolStoreBasePath

  const fetchAll = useCallback(async () => {
    if (!enabled) {
      setLoading(false)
      return
    }
    try {
      const [toolsRes, localsRes] = await Promise.all([
        fetchFn(`${toolStoreBasePath}/tools`),
        fetchFn(`${toolStoreBasePath}/locals`),
      ])
      if (!toolsRes.ok) throw new Error(`/tools HTTP ${toolsRes.status}`)
      if (!localsRes.ok) throw new Error(`/locals HTTP ${localsRes.status}`)
      const toolsData: Tool[] = (await toolsRes.json()) ?? []
      const localsData: LocalDescriptor[] = (await localsRes.json()) ?? []

      const json = JSON.stringify([toolsData, localsData])
      if (json !== lastJsonRef.current) {
        lastJsonRef.current = json
        setTools(toolsData)
        setLocals(localsData)
      }
      setError(null)
    } catch (err) {
      setError(`${err}`)
    } finally {
      setLoading(false)
    }
  }, [fetchFn, toolStoreBasePath, enabled])

  const fetchInstanceTools = useCallback(async () => {
    if (!enabled || !instanceID) {
      setInstanceTools([])
      return
    }
    try {
      const res = await fetchFn(`${toolStoreBasePath}/instances/${encodeURIComponent(instanceID)}/tools`)
      if (!res.ok) {
        setInstanceTools([])
        return
      }
      const data: Tool[] = (await res.json()) ?? []
      setInstanceTools(data.map(t => t.name))
    } catch {
      setInstanceTools([])
    }
  }, [fetchFn, toolStoreBasePath, instanceID, enabled])

  useEffect(() => {
    fetchAll()
    const interval = setInterval(fetchAll, 30000)
    return () => clearInterval(interval)
  }, [fetchAll])

  useEffect(() => {
    fetchInstanceTools()
  }, [fetchInstanceTools])

  const setGlobal = useCallback(async (toolID: number, on: boolean): Promise<boolean> => {
    if (!enabled) return false
    const path = `${toolStoreBasePath}/tools/${toolID}/${on ? 'enable' : 'disable'}`
    try {
      const res = await fetchFn(path, { method: 'POST' })
      if (!res.ok) return false
      await fetchAll()
      return true
    } catch {
      return false
    }
  }, [fetchFn, toolStoreBasePath, enabled, fetchAll])

  const setForInstance = useCallback(async (toolName: string, on: boolean): Promise<boolean> => {
    if (!enabled || !instanceID) return false
    const url = `${toolStoreBasePath}/instances/${encodeURIComponent(instanceID)}/tools/by-name/${encodeURIComponent(toolName)}`
    try {
      const res = await fetchFn(url, { method: on ? 'POST' : 'DELETE' })
      if (!res.ok) return false
      await fetchInstanceTools()
      return true
    } catch {
      return false
    }
  }, [fetchFn, toolStoreBasePath, instanceID, enabled, fetchInstanceTools])

  /** Fast lookup: is `toolName` opted-in for the currently-selected instance? */
  const byInstance = useMemo(() => new Set(instanceTools), [instanceTools])

  return useMemo(() => ({
    tools,
    locals,
    byInstance,
    loading,
    error,
    setGlobal,
    setForInstance,
    refresh: fetchAll,
  }), [tools, locals, byInstance, loading, error, setGlobal, setForInstance, fetchAll])
}
