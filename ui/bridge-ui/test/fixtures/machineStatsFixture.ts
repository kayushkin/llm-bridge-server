// Made-up machine readings, modeled on real ones, for the Machines page's
// render check and for looking at the page without a server: two Linux
// servers (one with a 7.3 TiB drive), a Mac laptop low on battery with
// compressed memory, pressure and a GPU, a Windows desktop read from WSL with
// commit charge, a graphics card, a nearly full drive and the WSL machine
// inside it, a machine that stopped reporting with a full drive, and one that
// never reported. A fetch that answers the page's routes from them, with one
// connected server built before machine stats (it answers 404) and one that
// holds no access.

import type {
  Machine,
  MachineStats,
  MachineStatsHistory,
  MachineStatsHistoryPoint,
  MachineStatsOverview,
  PeerConnectionView,
} from '@kayushkin/llm-bridge-types'

const KIB = 1024
const MIB = 1024 ** 2
const GIB = 1024 ** 3

function machine(id: string, name: string, emoji: string, transport: string): Machine {
  return {
    id, name, emoji, transport,
    computer_use_allowed: false,
    created_at: '2026-09-01T00:00:00Z',
    updated_at: '2026-09-01T00:00:00Z',
  } as Machine
}

export function machineStatsFixtureOverview(now: Date): MachineStatsOverview[] {
  const minutesAgo = (minutes: number) => new Date(now.getTime() - minutes * 60_000).toISOString()
  const server: MachineStats = {
    collected_at: minutesAgo(0.5), source: 'linux', hostname: 'server-1',
    operating_system: 'Ubuntu 24.04.1 LTS', kernel_version: '6.8.0-139-generic',
    booted_at: minutesAgo(57 * 24 * 60 + 200), uptime_seconds: (57 * 24 * 60 + 200) * 60,
    cpu_model: 'AMD EPYC 7713 64-Core Processor', cpu_logical_cores: 6, cpu_usage_percent: 34, load_average: [1.52, 1.1, 0.94],
    memory_total_bytes: 15.6 * GIB, memory_used_bytes: 4.0 * GIB, memory_available_bytes: 11.6 * GIB, memory_cached_bytes: 9.6 * GIB,
    swap_total_bytes: 4.5 * GIB, swap_used_bytes: 3.1 * GIB,
    drives: [
      { mount_point: '/', device: '/dev/sda', filesystem_type: 'ext4', total_bytes: 257 * GIB, used_bytes: 185 * GIB, free_bytes: 72 * GIB },
    ],
  }
  const storage: MachineStats = {
    collected_at: minutesAgo(0.7), source: 'linux', hostname: 'storage-1',
    operating_system: 'Ubuntu 24.04.1 LTS', kernel_version: '6.8.0-88-generic',
    booted_at: minutesAgo(20 * 24 * 60 + 90), cpu_model: 'Intel(R) Core(TM) i5-8500T', cpu_logical_cores: 6, cpu_usage_percent: 0.2,
    load_average: [0.04, 0.06, 0.02],
    memory_total_bytes: 15.5 * GIB, memory_used_bytes: 1.4 * GIB, memory_available_bytes: 14.1 * GIB, memory_cached_bytes: 12.2 * GIB,
    swap_total_bytes: 2 * GIB, swap_used_bytes: 0,
    drives: [
      { mount_point: '/', device: '/dev/md0', filesystem_type: 'ext4', total_bytes: 7.3 * 1024 * GIB, used_bytes: 1.53 * 1024 * GIB, free_bytes: 5.5 * 1024 * GIB },
    ],
  }
  const laptop: MachineStats = {
    collected_at: minutesAgo(1), source: 'darwin', hostname: 'laptop.local',
    operating_system: 'macOS 15.7.9', kernel_version: '24.6.0',
    booted_at: minutesAgo(3 * 60 + 12), cpu_model: 'Apple M2', cpu_logical_cores: 8, cpu_usage_percent: 7, load_average: [2.1, 2.4, 2.2],
    memory_total_bytes: 16 * GIB, memory_used_bytes: 15 * GIB, memory_available_bytes: 1 * GIB, memory_cached_bytes: 0.7 * GIB,
    memory_compressed_bytes: 4.8 * GIB, memory_free_under_pressure_percent: 60,
    swap_total_bytes: 3 * GIB, swap_used_bytes: 1.9 * GIB,
    drives: [{ mount_point: '/', device: 'disk3s1', label: 'Macintosh HD', filesystem_type: 'apfs', total_bytes: 228 * GIB, used_bytes: 36 * GIB, free_bytes: 190 * GIB }],
    batteries: [{ name: 'InternalBattery-0', charge_percent: 9, state: 'discharging', seconds_remaining: 7 * 3600 + 15 * 60, health_percent: 97.7, cycle_count: 30 }],
    gpus: [{ name: 'Apple M2', usage_percent: 18, memory_used_bytes: 1 * GIB }],
  }
  const windows: MachineStats = {
    collected_at: minutesAgo(0.2), source: 'windows_from_wsl', hostname: 'DESKTOP-1',
    operating_system: 'Microsoft Windows 11 Pro', kernel_version: '10.0.26200', booted_at: minutesAgo(26 * 60),
    cpu_model: 'AMD Ryzen 7 5800X3D 8-Core Processor', cpu_logical_cores: 16, cpu_usage_percent: 14,
    memory_total_bytes: 32 * GIB, memory_used_bytes: 28.2 * GIB, memory_available_bytes: 3.8 * GIB,
    memory_committed_bytes: 47.4 * GIB, memory_commit_limit_bytes: 51.8 * GIB,
    swap_total_bytes: 19.8 * GIB, swap_used_bytes: 2.1 * GIB,
    drives: [
      { mount_point: 'C:', filesystem_type: 'NTFS', total_bytes: 3470 * GIB, used_bytes: 3123 * GIB, free_bytes: 347 * GIB },
      { mount_point: 'D:', label: 'Media', filesystem_type: 'NTFS', total_bytes: 3726 * GIB, used_bytes: 1118 * GIB, free_bytes: 2608 * GIB },
      { mount_point: 'G:', label: 'Google Drive', filesystem_type: 'FAT32', total_bytes: 2048 * GIB, used_bytes: 1700 * GIB, free_bytes: 348 * GIB },
    ],
    gpus: [{ name: 'NVIDIA GeForce RTX 3080 Ti', usage_percent: 0, memory_used_bytes: 3.5 * GIB, memory_total_bytes: 12 * GIB, temperature_celsius: 31 }],
    collection_errors: ['batteries: Get-CimInstance Win32_Battery timed out after 5s'],
    wsl: {
      collected_at: minutesAgo(0.2), source: 'linux', hostname: 'DESKTOP-1',
      operating_system: 'Ubuntu 22.04.5 LTS', kernel_version: '5.15.167.4-microsoft-standard-WSL2',
      booted_at: minutesAgo(5 * 60 + 40), cpu_logical_cores: 16, cpu_usage_percent: 3.1, load_average: [0.08, 0.12, 0.09],
      memory_total_bytes: 15952 * MIB, memory_used_bytes: 1315 * MIB, memory_cached_bytes: 2175 * MIB, memory_available_bytes: 14297 * MIB,
      swap_total_bytes: 4096 * MIB, swap_used_bytes: 9 * MIB,
      drives: [{ mount_point: '/', device: '/dev/sdd', filesystem_type: 'ext4', total_bytes: 1_055_762_868 * KIB, used_bytes: 140_362_036 * KIB, free_bytes: (1_055_762_868 - 140_362_036) * KIB }],
    },
  }
  const stale: MachineStats = {
    collected_at: minutesAgo(47), source: 'linux', operating_system: 'Debian GNU/Linux 12', booted_at: minutesAgo(200 * 60),
    cpu_usage_percent: 2, cpu_logical_cores: 4, memory_total_bytes: 8 * GIB, memory_used_bytes: 2 * GIB,
    drives: [{ mount_point: '/', total_bytes: 64 * GIB, used_bytes: 62 * GIB, free_bytes: 2 * GIB }],
  }
  return [
    { machine: machine('m_000001', 'home-server', '', 'local'), latest: server },
    { machine: machine('m_000002', 'laptop', '', 'runner'), runner_connected: true, latest: laptop },
    { machine: machine('m_000005', 'workstation', '', 'runner'), runner_connected: true, latest: windows },
    { machine: machine('m_000006', 'storage', '', 'ssh'), latest: storage },
    { machine: machine('m_000003', 'old-box', '', 'runner'), runner_connected: false, latest: stale },
    { machine: machine('m_000004', 'spare', '', 'ssh') },
  ]
}

/** A day of three-minute buckets for one machine, with a gap where it was off
 *  for an hour and a CPU spike that only the peak shows. */
export function machineStatsFixtureHistory(machineID: string, latest: MachineStats, since: Date, until: Date, stepSeconds: number): MachineStatsHistory {
  const points: MachineStatsHistoryPoint[] = []
  const count = Math.floor((until.getTime() - since.getTime()) / (stepSeconds * 1000))
  for (let i = 0; i < count; i++) {
    if (i > count * 0.4 && i < count * 0.4 + 3600 / stepSeconds) continue
    const at = new Date(since.getTime() + i * stepSeconds * 1000)
    const phase = i / count
    const average = 15 + 12 * Math.sin(phase * Math.PI * 6) + (i % 7)
    const peak = average + 10 + (i % 50 === 0 ? 55 : (i * 13) % 17)
    const drives = (latest.drives ?? []).map(drive => ({ ...drive, used_bytes: drive.used_bytes * (0.94 + 0.06 * phase) }))
    const batteries = (latest.batteries ?? []).map(battery => ({ ...battery, charge_percent: 100 - 60 * ((phase * 3) % 1) }))
    const gpus = (latest.gpus ?? []).map(gpu => ({
      ...gpu,
      usage_percent: gpu.usage_percent === undefined ? undefined : Math.max(0, 30 + 28 * Math.sin(phase * Math.PI * 4) + (i % 11)),
      memory_used_bytes: gpu.memory_used_bytes === undefined ? undefined : gpu.memory_used_bytes * (0.6 + 0.5 * phase),
    }))
    points.push({
      at: at.toISOString(),
      sample_count: Math.max(1, Math.round(stepSeconds / 60)),
      cpu_usage_percent_average: average,
      cpu_usage_percent_maximum: Math.min(100, peak),
      stats: {
        ...latest,
        collected_at: at.toISOString(),
        cpu_usage_percent: average,
        memory_used_bytes: (latest.memory_total_bytes ?? 0) * (0.45 + 0.2 * Math.sin(phase * Math.PI * 2)),
        drives,
        batteries,
        gpus,
        wsl: latest.wsl ? {
          ...latest.wsl,
          collected_at: at.toISOString(),
          cpu_usage_percent: Math.max(0, 4 + 6 * Math.sin(phase * Math.PI * 10) + (i % 5)),
          memory_used_bytes: (latest.wsl.memory_used_bytes ?? 0) * (0.7 + 0.8 * phase),
        } : undefined,
      },
    })
  }
  return { machine_id: machineID, since: since.toISOString(), until: until.toISOString(), step_seconds: stepSeconds, points }
}

export const MACHINE_STATS_FIXTURE_CONNECTIONS: PeerConnectionView[] = [
  {
    connection: { id: 'peer_connection_000001', display_name: 'older-server', transport: 'direct', role: 'accepting', created_by: 'principal_000001', created_at: '2026-10-27T10:00:00Z' },
    socket_connected: false,
    answering: true,
    access_held: { id: 'peer_access_000001', connection_id: 'peer_connection_000001', remote_grant_id: 'peer_grant_000001', remote_peer_principal_id: 'principal_000040', level_key: 'administrator', created_at: '2026-10-27T10:00:00Z' },
    other_server_name: 'older-server',
    other_server_id: 'server_01M64X1V01QDBDE16TEQP2QHKS',
  } as PeerConnectionView,
  {
    connection: { id: 'peer_connection_000002', display_name: 'no-access-server', transport: 'dial_out', role: 'connecting', created_by: 'principal_000001', created_at: '2026-10-28T10:00:00Z' },
    socket_connected: true,
    other_server_name: 'no-access-server',
    other_server_id: 'server_01M64X1V01QDBDE16TEQP2QHKT',
  } as PeerConnectionView,
]

/** A fetch that answers the Machines page from the fixtures above. */
export function machineStatsFixtureFetch(basePath: string, now: () => Date): (input: string) => Promise<Response> {
  return async (input: string) => {
    const url = new URL(input, 'http://fixture.invalid')
    const path = url.pathname.slice(basePath.length)
    const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200, headers: { 'Content-Type': 'application/json' } })
    if (path === '/server') return json({ id: 'server_01M64KCNK3EBR2XV06QZNY6729', name: 'home', keep_awake: {} })
    if (path === '/peer-connections') return json(MACHINE_STATS_FIXTURE_CONNECTIONS)
    if (path.startsWith('/peer-connections/peer_connection_000001/proxy/')) {
      return new Response('machine not found\n', { status: 404, headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
    }
    const overview = machineStatsFixtureOverview(now())
    if (path === '/machines/stats') return json(overview)
    const history = /^\/machines\/([^/]+)\/stats\/history$/.exec(path)
    if (history) {
      const entry = overview.find(candidate => candidate.machine.id === decodeURIComponent(history[1]))
      if (!entry?.latest) return json({ machine_id: history[1], since: url.searchParams.get('since'), until: url.searchParams.get('until'), step_seconds: 60, points: [] })
      return json(machineStatsFixtureHistory(entry.machine.id, entry.latest, new Date(url.searchParams.get('since')!), new Date(url.searchParams.get('until')!), Number(url.searchParams.get('step_seconds'))))
    }
    return new Response(JSON.stringify({ error: { code: 'not_found', message: `no fixture for ${path}` } }), { status: 404, headers: { 'Content-Type': 'application/json' } })
  }
}
