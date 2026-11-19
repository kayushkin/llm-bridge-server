import { useCallback, useEffect, useState } from 'react'
import type { SourceFolderMapping, SourceFolderApplyResult } from '@kayushkin/llm-bridge-types'
import { useBridgeConfig } from '../context'
import { useBridgeFolders } from '../useBridgeFolders'
import { SettingsSection } from './settings/SettingsSection'

// Editor for the runtime purpose→folder map. Each row maps a session.purpose
// tag to a folder. Rows with default=true come from LLMBRIDGE_SOURCE_FOLDERS;
// default=false rows are runtime overrides stored in source_folders. PUT
// replaces; DELETE reverts to the env default. The env var and table keep
// the legacy "source" naming for storage internals; the user-facing axis is
// "purpose".
//
// On Save, the editor offers to rebucket existing sessions whose folder was
// empty or matched the prior effective folder for that purpose. Manual moves
// (folder differs from the prior effective) are preserved.
export function SourceFoldersEditor() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const folders = useBridgeFolders()
  const [rows, setRows] = useState<SourceFolderMapping[]>([])
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [applyToExisting, setApplyToExisting] = useState(true)
  const [busy, setBusy] = useState<string | null>(null)
  const [status, setStatus] = useState<string | null>(null)
  const [newPurpose, setNewPurpose] = useState('')
  const [newFolder, setNewFolder] = useState('')

  const refresh = useCallback(async () => {
    const res = await apiFetch(`${basePath}/source-folders`)
    if (!res.ok) return
    const data: SourceFolderMapping[] = await res.json()
    setRows(data)
    setDraft(Object.fromEntries(data.map(r => [r.purpose, r.folder_name])))
  }, [apiFetch, basePath])

  useEffect(() => { refresh() }, [refresh])

  const save = useCallback(async (purpose: string) => {
    const folder = draft[purpose]?.trim()
    if (!folder) return
    setBusy(purpose); setStatus(null)
    const res = await apiFetch(`${basePath}/source-folders/${encodeURIComponent(purpose)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ folder_name: folder, apply_to_existing: applyToExisting }),
    })
    setBusy(null)
    if (!res.ok) {
      setStatus(`save failed: ${await res.text()}`)
      return
    }
    const result: SourceFolderApplyResult = await res.json()
    setStatus(applyToExisting
      ? `${purpose} → ${folder} (${result.updated} session${result.updated === 1 ? '' : 's'} rebucketed)`
      : `${purpose} → ${folder}`)
    await refresh()
  }, [apiFetch, basePath, draft, applyToExisting, refresh])

  const revert = useCallback(async (purpose: string) => {
    setBusy(purpose); setStatus(null)
    const res = await apiFetch(`${basePath}/source-folders/${encodeURIComponent(purpose)}`, { method: 'DELETE' })
    setBusy(null)
    if (!res.ok) {
      setStatus(`revert failed: ${await res.text()}`)
      return
    }
    setStatus(`${purpose}: reverted to env default`)
    await refresh()
  }, [apiFetch, basePath, refresh])

  const addNew = useCallback(async () => {
    const purpose = newPurpose.trim()
    const fld = newFolder.trim()
    if (!purpose || !fld) return
    setBusy(purpose); setStatus(null)
    const res = await apiFetch(`${basePath}/source-folders/${encodeURIComponent(purpose)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ folder_name: fld, apply_to_existing: applyToExisting }),
    })
    setBusy(null)
    if (!res.ok) {
      setStatus(`add failed: ${await res.text()}`)
      return
    }
    const result: SourceFolderApplyResult = await res.json()
    setStatus(applyToExisting
      ? `+ ${purpose} → ${fld} (${result.updated} rebucketed)`
      : `+ ${purpose} → ${fld}`)
    setNewPurpose(''); setNewFolder('')
    await refresh()
  }, [apiFetch, basePath, newPurpose, newFolder, applyToExisting, refresh])

  return (
    <SettingsSection
      id="source-folders"
      title="Session purpose folders"
      scope="global"
      storedBy="llm-bridge-server · source_folders"
      help={<>Auto-file new sessions into a sidebar folder based on their <code>purpose</code> tag
        (set by the caller when creating the session). Rows marked <em>default</em>{' '}
        come from LLMBRIDGE_SOURCE_FOLDERS; saving over them creates a runtime override.</>}
      precedence="A session's folder can still be moved by hand; a manual move is kept when a mapping is applied to existing sessions."
      status={{ busy: busy !== null, error: status && /failed:/.test(status) ? status : null, note: status && !/failed:/.test(status) ? <span className="bss-help">{status}</span> : null }}
    >

      <label className="bks-check">
        <input type="checkbox" checked={applyToExisting} onChange={e => setApplyToExisting(e.target.checked)} />
        Apply to existing sessions (preserves manual moves)
      </label>

      <table className="bset-sf-table">
        <thead>
          <tr><th>Purpose</th><th>Folder</th><th>Origin</th><th></th></tr>
        </thead>
        <tbody>
          {rows.map(r => (
            <tr key={r.purpose}>
              <td><code>{r.purpose}</code></td>
              <td>
                <select
                  value={draft[r.purpose] ?? r.folder_name}
                  onChange={e => setDraft(prev => ({ ...prev, [r.purpose]: e.target.value }))}
                  disabled={busy === r.purpose}
                >
                  {folders.folderOrder.map(f => <option key={f} value={f}>{f}</option>)}
                  {!folders.folderOrder.includes(r.folder_name) && (
                    <option value={r.folder_name}>{r.folder_name} (missing)</option>
                  )}
                </select>
              </td>
              <td>{r.default ? <em>default</em> : 'override'}</td>
              <td>
                <button
                  className="bset-save-btn"
                  onClick={() => save(r.purpose)}
                  disabled={busy === r.purpose || (draft[r.purpose] ?? r.folder_name) === r.folder_name}
                >Save</button>
                {!r.default && (
                  <button
                    className="bset-save-btn"
                    onClick={() => revert(r.purpose)}
                    disabled={busy === r.purpose}
                    style={{ marginLeft: '0.5rem' }}
                  >Revert</button>
                )}
              </td>
            </tr>
          ))}
          <tr>
            <td><input placeholder="new purpose tag" value={newPurpose} onChange={e => setNewPurpose(e.target.value)} /></td>
            <td>
              <select value={newFolder} onChange={e => setNewFolder(e.target.value)}>
                <option value="">— pick folder —</option>
                {folders.folderOrder.map(f => <option key={f} value={f}>{f}</option>)}
              </select>
            </td>
            <td></td>
            <td>
              <button className="bset-save-btn" onClick={addNew} disabled={!newPurpose.trim() || !newFolder || busy !== null}>
                Add
              </button>
            </td>
          </tr>
        </tbody>
      </table>

    </SettingsSection>
  )
}
