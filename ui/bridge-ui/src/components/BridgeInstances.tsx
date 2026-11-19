import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../context'
import { useBridgeHarnesses } from '../useBridgeHarnesses'
import { useBridgeInstances } from '../useBridgeInstances'
import { useBridgeMachines } from '../useBridgeMachines'
import { TRANSPORT_LABEL } from '../constants'
import type { BridgeInstance, InstanceStatus, InstanceCredential, Machine, CreateMachineRequest } from '../types'
import type { Credential, Transport } from '@kayushkin/llm-bridge-types'

type MachineFormState = {
  name: string
  emoji: string
  hostname: string
  transport: Transport
  ssh_user: string
  ssh_key_path: string
  ssh_port: number
  default_working_dir: string
  notes: string
}

const emptyMachineForm = (): MachineFormState => ({
  name: '',
  emoji: '',
  hostname: '',
  transport: 'local',
  ssh_user: '',
  ssh_key_path: '',
  ssh_port: 22,
  default_working_dir: '',
  notes: '',
})

type InstanceFormState = {
  name: string
  harness_type: string
  machine_id: string
  working_dir: string
  max_concurrent_sessions: number
}

const emptyInstanceForm = (defaultMachineID = ''): InstanceFormState => ({
  name: '',
  harness_type: 'claude_code',
  machine_id: defaultMachineID,
  working_dir: '',
  max_concurrent_sessions: 1,
})

export function BridgeInstances() {
  const { fetch: apiFetch, basePath } = useBridgeConfig()
  const inst = useBridgeInstances()
  const mach = useBridgeMachines()
  const { harnesses } = useBridgeHarnesses()

  const [credentials, setCredentials] = useState<Credential[]>([])
  const [statusCache, setStatusCache] = useState<Record<string, InstanceStatus>>({})
  const [credCache, setCredCache] = useState<Record<string, InstanceCredential[]>>({})
  const [expandedCreds, setExpandedCreds] = useState<Record<string, boolean>>({})
  const [bindForm, setBindForm] = useState<{ instanceId: string; credential_id: string; priority: number; max_concurrent: number } | null>(null)

  const [showMachineForm, setShowMachineForm] = useState(false)
  const [editMachineId, setEditMachineId] = useState<string | null>(null)
  const [machineForm, setMachineForm] = useState<MachineFormState>(emptyMachineForm())

  const [showInstanceForm, setShowInstanceForm] = useState(false)
  const [editInstanceId, setEditInstanceId] = useState<string | null>(null)
  const [instanceForm, setInstanceForm] = useState<InstanceFormState>(emptyInstanceForm())

  useEffect(() => {
    apiFetch(`${basePath}/credentials`).then(r => r.ok ? r.json() : []).then(setCredentials).catch(() => {})
  }, [apiFetch, basePath])

  useEffect(() => {
    for (const i of inst.instances) {
      inst.getStatus(i.id).then(s => { if (s) setStatusCache(prev => ({ ...prev, [i.id]: s })) })
      inst.getCredentials(i.id).then(c => setCredCache(prev => ({ ...prev, [i.id]: c })))
    }
  }, [inst.instances, inst.getStatus, inst.getCredentials])

  // ────────────── Machine form ──────────────

  const resetMachineForm = () => {
    setMachineForm(emptyMachineForm())
    setEditMachineId(null)
    setShowMachineForm(false)
  }

  const startEditMachine = (m: Machine) => {
    setMachineForm({
      name: m.name,
      emoji: m.emoji ?? '',
      hostname: m.hostname ?? '',
      transport: m.transport,
      ssh_user: m.ssh_user ?? '',
      ssh_key_path: m.ssh_key_path ?? '',
      ssh_port: m.ssh_port ?? 22,
      default_working_dir: m.default_working_dir ?? '',
      notes: m.notes ?? '',
    })
    setEditMachineId(m.id)
    setShowMachineForm(true)
  }

  const submitMachine = useCallback(async () => {
    if (!machineForm.name.trim()) return
    const body: CreateMachineRequest = {
      name: machineForm.name.trim(),
      emoji: machineForm.emoji,
      hostname: machineForm.hostname,
      transport: machineForm.transport,
      ssh_user: machineForm.ssh_user,
      ssh_key_path: machineForm.ssh_key_path,
      ssh_port: machineForm.ssh_port,
      default_working_dir: machineForm.default_working_dir,
      notes: machineForm.notes,
    }
    if (editMachineId) await mach.updateMachine(editMachineId, body)
    else await mach.createMachine(body)
    resetMachineForm()
  }, [machineForm, editMachineId, mach])

  const handleDeleteMachine = async (id: string) => {
    if (!confirm('Delete this machine? All bound instances will be deleted too.')) return
    await mach.deleteMachine(id)
  }

  // ────────────── Instance form ──────────────

  const defaultMachineID = mach.machines[0]?.id ?? ''
  const resetInstanceForm = () => {
    setInstanceForm(emptyInstanceForm(defaultMachineID))
    setEditInstanceId(null)
    setShowInstanceForm(false)
  }

  const startEditInstance = (i: BridgeInstance) => {
    setInstanceForm({
      name: i.name,
      harness_type: i.harness_type,
      machine_id: i.machine_id ?? '',
      working_dir: i.working_dir ?? '',
      max_concurrent_sessions: i.max_concurrent_sessions,
    })
    setEditInstanceId(i.id)
    setShowInstanceForm(true)
  }

  const submitInstance = useCallback(async () => {
    if (!instanceForm.name.trim() || !instanceForm.machine_id) return
    if (editInstanceId) await inst.updateInstance(editInstanceId, instanceForm)
    else await inst.createInstance(instanceForm)
    resetInstanceForm()
  }, [instanceForm, editInstanceId, inst])

  const handleDeleteInstance = async (id: string) => {
    if (!confirm('Delete this instance?')) return
    await inst.deleteInstance(id)
  }

  // ────────────── Credential bindings (unchanged shape) ──────────────

  const toggleCreds = (id: string) => setExpandedCreds(prev => ({ ...prev, [id]: !prev[id] }))

  const handleBind = async () => {
    if (!bindForm) return
    const ok = await inst.bindCredential(bindForm.instanceId, bindForm.credential_id, bindForm.priority, bindForm.max_concurrent)
    if (ok) {
      const creds = await inst.getCredentials(bindForm.instanceId)
      setCredCache(prev => ({ ...prev, [bindForm.instanceId]: creds }))
      setBindForm(null)
    }
  }

  const handleUnbind = async (instanceId: string, credId: string) => {
    const ok = await inst.unbindCredential(instanceId, credId)
    if (ok) {
      const creds = await inst.getCredentials(instanceId)
      setCredCache(prev => ({ ...prev, [instanceId]: creds }))
    }
  }

  // ────────────── Render helpers ──────────────

  const machineCounts = useMemo(() => {
    const out = new Map<string, number>()
    for (const i of inst.instances) {
      out.set(i.machine_id, (out.get(i.machine_id) ?? 0) + 1)
    }
    return out
  }, [inst.instances])

  const groups = useMemo(() => {
    const m = new Map<string, BridgeInstance[]>()
    for (const i of inst.instances) {
      const arr = m.get(i.harness_type) || []
      arr.push(i)
      m.set(i.harness_type, arr)
    }
    return m
  }, [inst.instances])

  return (
    <div className="bi-container">
      {/* Machines panel */}
      <div className="bi-header">
        <h2>Machines</h2>
        <button className="bi-add-btn" onClick={() => { resetMachineForm(); setShowMachineForm(true) }}>+ Add Machine</button>
      </div>

      {showMachineForm && (
        <div className="bi-form-card">
          <div className="bi-form-grid">
            <label><span>Name</span><input value={machineForm.name} onChange={e => setMachineForm(f => ({ ...f, name: e.target.value }))} placeholder="laptop" /></label>
            <label><span>Emoji</span><input value={machineForm.emoji} onChange={e => setMachineForm(f => ({ ...f, emoji: e.target.value }))} placeholder="🖥" maxLength={4} /></label>
            <label><span>Transport</span>
              <select value={machineForm.transport} onChange={e => setMachineForm(f => ({ ...f, transport: e.target.value as Transport }))}>
                <option value="local">Local</option>
                <option value="ssh">SSH</option>
                <option value="runner">Runner</option>
              </select>
            </label>
            <label><span>Hostname</span><input value={machineForm.hostname} onChange={e => setMachineForm(f => ({ ...f, hostname: e.target.value }))} placeholder={machineForm.transport === 'runner' ? '(runner self-reports)' : 'host or IP'} /></label>
            {machineForm.transport === 'ssh' && (
              <>
                <label><span>SSH User</span><input value={machineForm.ssh_user} onChange={e => setMachineForm(f => ({ ...f, ssh_user: e.target.value }))} /></label>
                <label><span>SSH Key Path</span><input value={machineForm.ssh_key_path} onChange={e => setMachineForm(f => ({ ...f, ssh_key_path: e.target.value }))} /></label>
                <label><span>SSH Port</span><input type="number" value={machineForm.ssh_port} onChange={e => setMachineForm(f => ({ ...f, ssh_port: parseInt(e.target.value) || 22 }))} /></label>
              </>
            )}
            <label><span>Default Workdir</span><input value={machineForm.default_working_dir} onChange={e => setMachineForm(f => ({ ...f, default_working_dir: e.target.value }))} placeholder="/home/user" /></label>
            <label className="bi-form-wide"><span>Notes</span><input value={machineForm.notes} onChange={e => setMachineForm(f => ({ ...f, notes: e.target.value }))} /></label>
          </div>
          <div className="bi-form-actions">
            <button className="bi-save-btn" onClick={submitMachine}>{editMachineId ? 'Update' : 'Create'}</button>
            <button className="bi-cancel-btn" onClick={resetMachineForm}>Cancel</button>
          </div>
        </div>
      )}

      <div className="bi-card-grid">
        {mach.machines.map(m => (
          <div key={m.id} className="bi-card">
            <div className="bi-card-header">
              {m.emoji && <span className="bi-machine-emoji" aria-hidden>{m.emoji}</span>}
              <span className="bi-card-name">{m.name}</span>
              <span className="bi-transport">{TRANSPORT_LABEL[m.transport] ?? m.transport}</span>
            </div>
            <div className="bi-card-meta">
              {m.hostname && <span>{m.hostname}</span>}
              {m.transport === 'ssh' && m.ssh_user && <span>{m.ssh_user}@:{m.ssh_port || 22}</span>}
              {m.transport === 'runner' && m.user && <span>user: {m.user}</span>}
              {m.default_working_dir && <span>cwd: {m.default_working_dir}</span>}
            </div>
            <div className="bi-card-stats">
              <span>Instances: {machineCounts.get(m.id) ?? 0}</span>
              {m.os && <span>{m.os}/{m.arch}</span>}
            </div>
            <div className="bi-card-actions">
              <button onClick={() => startEditMachine(m)}>Edit</button>
              <button className="bi-delete-btn" onClick={() => handleDeleteMachine(m.id)}>Delete</button>
            </div>
          </div>
        ))}
      </div>

      {!mach.loading && mach.machines.length === 0 && (
        <div className="bi-empty">No machines configured. Click "Add Machine" to create one.</div>
      )}

      {/* Instances panel */}
      <div className="bi-header" style={{ marginTop: 24 }}>
        <h2>Harness Instances</h2>
        <button
          className="bi-add-btn"
          onClick={() => { resetInstanceForm(); setShowInstanceForm(true) }}
          disabled={mach.machines.length === 0}
          title={mach.machines.length === 0 ? 'Create a machine first' : ''}
        >+ Add Instance</button>
      </div>

      {showInstanceForm && (
        <div className="bi-form-card">
          <div className="bi-form-grid">
            <label><span>Name</span><input value={instanceForm.name} onChange={e => setInstanceForm(f => ({ ...f, name: e.target.value }))} placeholder="my-instance" /></label>
            <label><span>Harness</span>
              <select value={instanceForm.harness_type} onChange={e => setInstanceForm(f => ({ ...f, harness_type: e.target.value }))}>
                {harnesses.map(h => <option key={h.name} value={h.name}>{h.name}</option>)}
              </select>
            </label>
            <label><span>Machine</span>
              <select value={instanceForm.machine_id} onChange={e => setInstanceForm(f => ({ ...f, machine_id: e.target.value }))}>
                <option value="">— pick a machine —</option>
                {mach.machines.map(m => <option key={m.id} value={m.id}>{m.emoji ? `${m.emoji} ` : ''}{m.name}</option>)}
              </select>
            </label>
            <label><span>Working Dir</span><input value={instanceForm.working_dir} onChange={e => setInstanceForm(f => ({ ...f, working_dir: e.target.value }))} placeholder="(uses machine default)" /></label>
            <label><span>Max Sessions</span><input type="number" value={instanceForm.max_concurrent_sessions} onChange={e => setInstanceForm(f => ({ ...f, max_concurrent_sessions: parseInt(e.target.value) || 1 }))} min={1} /></label>
          </div>
          <div className="bi-form-actions">
            <button className="bi-save-btn" onClick={submitInstance}>{editInstanceId ? 'Update' : 'Create'}</button>
            <button className="bi-cancel-btn" onClick={resetInstanceForm}>Cancel</button>
          </div>
        </div>
      )}

      {inst.loading && <div className="bi-loading">Loading...</div>}
      {inst.error && <div className="bridge-error">{inst.error}</div>}

      {Array.from(groups.entries()).map(([harness, items]) => (
        <div key={harness} className="bi-group">
          <h3 className="bi-group-title">{harness}</h3>
          <div className="bi-card-grid">
            {items.map(item => {
              const status = statusCache[item.id]
              const creds = credCache[item.id] || []
              const expanded = expandedCreds[item.id]
              const m = item.machine
              return (
                <div key={item.id} className="bi-card">
                  <div className="bi-card-header">
                    <span className="bi-card-name">{item.name}</span>
                    {m && (
                      <span className={`bi-transport ${m.transport === 'ssh' ? 'bi-transport-ssh' : ''}`}>
                        {m.emoji ? `${m.emoji} ` : ''}{m.name}
                      </span>
                    )}
                    {status && <span className={`bi-reach ${status.reachable ? 'bi-reach-ok' : 'bi-reach-fail'}`} title={status.reachable ? 'Reachable' : 'Unreachable'} />}
                  </div>
                  <div className="bi-card-meta">
                    {m?.hostname && <span>{m.hostname}</span>}
                    {m?.transport === 'ssh' && <span>{m.ssh_user}@:{m.ssh_port || 22}</span>}
                    {item.working_dir && <span>{item.working_dir}</span>}
                  </div>
                  <div className="bi-card-stats">
                    <span>Sessions: {status?.active_sessions ?? 0} / {item.max_concurrent_sessions}</span>
                    <span>Credentials: {creds.length}</span>
                  </div>

                  <button className="bi-cred-toggle" onClick={() => toggleCreds(item.id)}>
                    {expanded ? '▾' : '▸'} Credentials ({creds.length})
                  </button>
                  {expanded && (
                    <div className="bi-cred-section">
                      {creds.map(c => {
                        const slotInfo = status?.credentials?.find(s => s.credential_id === c.credential_id)
                        return (
                          <div key={c.credential_id} className="bi-cred-row">
                            <span className="bi-cred-id">{c.credential_id}</span>
                            <span className="bi-cred-pri">pri {c.priority}</span>
                            {slotInfo?.enabled && <span className="bi-cred-slots">active</span>}
                            <button className="bi-cred-unbind" onClick={() => handleUnbind(item.id, c.credential_id)}>x</button>
                          </div>
                        )
                      })}
                      {bindForm?.instanceId === item.id ? (
                        <div className="bi-bind-form">
                          <select value={bindForm.credential_id} onChange={e => setBindForm(f => f ? { ...f, credential_id: e.target.value } : f)}>
                            <option value="">Select credential</option>
                            {credentials.filter(c => c.enabled).map(c => (
                              <option key={c.id} value={c.id}>{c.label || c.id} ({c.provider})</option>
                            ))}
                          </select>
                          <input type="number" placeholder="Pri" value={bindForm.priority} onChange={e => setBindForm(f => f ? { ...f, priority: parseInt(e.target.value) || 0 } : f)} style={{ width: 50 }} />
                          <input type="number" placeholder="Max" value={bindForm.max_concurrent} onChange={e => setBindForm(f => f ? { ...f, max_concurrent: parseInt(e.target.value) || 1 } : f)} style={{ width: 50 }} />
                          <button className="bi-save-btn" onClick={handleBind}>Bind</button>
                          <button className="bi-cancel-btn" onClick={() => setBindForm(null)}>x</button>
                        </div>
                      ) : (
                        <button className="bi-bind-btn" onClick={() => setBindForm({ instanceId: item.id, credential_id: '', priority: 0, max_concurrent: 1 })}>+ Bind credential</button>
                      )}
                    </div>
                  )}

                  <div className="bi-card-actions">
                    <button onClick={() => startEditInstance(item)}>Edit</button>
                    <button className="bi-delete-btn" onClick={() => handleDeleteInstance(item.id)}>Delete</button>
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      ))}

      {!inst.loading && inst.instances.length === 0 && (
        <div className="bi-empty">No instances configured. Click "Add Instance" to create one.</div>
      )}
    </div>
  )
}
