import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from './context'
import type { FolderList } from '@kayushkin/llm-bridge-types'

// ARCHIVE_FOLDER is the canonical folder a session moves into when marked
// done. Mirrors the server-side store.ArchiveFolder constant. Auto-created
// by the server on first use.
export const ARCHIVE_FOLDER = 'Archive'

export interface UseBridgeFoldersReturn {
  folderOrder: string[]
  refresh: () => Promise<void>
  createFolder: (name: string) => Promise<void>
  deleteFolder: (name: string) => Promise<void>
  renameFolder: (oldName: string, newName: string) => Promise<void>
  setSessionFolder: (sessionId: string, folder: string) => Promise<void>
  // markSessionDone toggles between "done" (state=completed, folder=Archive)
  // and "active" (state=idle, folder cleared). Atomic on the server side —
  // both halves succeed or fail together.
  markSessionDone: (sessionId: string, done: boolean) => Promise<void>
}

export function useBridgeFolders(): UseBridgeFoldersReturn {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [folderOrder, setFolderOrder] = useState<string[]>([])

  const refresh = useCallback(async () => {
    const res = await fetchFn(`${basePath}/folders`)
    if (!res.ok) return
    const data: FolderList = await res.json()
    setFolderOrder(data.folder_order ?? [])
  }, [fetchFn, basePath])

  useEffect(() => { refresh() }, [refresh])

  const createFolder = useCallback(async (name: string) => {
    const trimmed = name.trim()
    if (!trimmed) return
    setFolderOrder(prev => prev.includes(trimmed) ? prev : [...prev, trimmed])
    await fetchFn(`${basePath}/folders`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: trimmed }),
    })
  }, [fetchFn, basePath])

  const deleteFolder = useCallback(async (name: string) => {
    setFolderOrder(prev => prev.filter(f => f !== name))
    await fetchFn(`${basePath}/folders/${encodeURIComponent(name)}`, { method: 'DELETE' })
  }, [fetchFn, basePath])

  const renameFolder = useCallback(async (oldName: string, newName: string) => {
    const trimmed = newName.trim()
    if (!trimmed || oldName === trimmed) return
    setFolderOrder(prev => {
      if (prev.includes(trimmed)) return prev.filter(f => f !== oldName)
      return prev.map(f => f === oldName ? trimmed : f)
    })
    await fetchFn(`${basePath}/folders/${encodeURIComponent(oldName)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ new_name: trimmed }),
    })
  }, [fetchFn, basePath])

  const setSessionFolder = useCallback(async (sessionId: string, folder: string) => {
    if (folder && !folderOrder.includes(folder)) {
      setFolderOrder(prev => prev.includes(folder) ? prev : [...prev, folder])
    }
    await fetchFn(`${basePath}/sessions/${sessionId}/folder`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ folder }),
    })
  }, [fetchFn, basePath, folderOrder])

  const markSessionDone = useCallback(async (sessionId: string, done: boolean) => {
    if (done && !folderOrder.includes(ARCHIVE_FOLDER)) {
      setFolderOrder(prev => prev.includes(ARCHIVE_FOLDER) ? prev : [...prev, ARCHIVE_FOLDER])
    }
    await fetchFn(`${basePath}/sessions/${sessionId}/mark-done`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ done }),
    })
  }, [fetchFn, basePath, folderOrder])

  return useMemo(() => ({
    folderOrder,
    refresh,
    createFolder,
    deleteFolder,
    renameFolder,
    setSessionFolder,
    markSessionDone,
  }), [folderOrder, refresh, createFolder, deleteFolder, renameFolder, setSessionFolder, markSessionDone])
}
