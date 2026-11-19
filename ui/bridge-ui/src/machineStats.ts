// Machine health: CPU, memory, drives, batteries and uptime of every machine a
// bridge server knows, read from llm-bridge-server's `GET /machines/stats`
// (the newest reading of each machine) and `GET /machines/{id}/stats/history`
// (bucketed readings over a range). A runner sends a reading every minute and
// the server reads its own machine the same way; the server keeps 30 days.
//
// The Machines page shows this server's machines and, through this server's
// proxy for each connection, every connected server's. A server built before
// these routes answers 404, which the page says in plain words rather than as
// an error.
//
// The pure rules the page draws from; `components/BridgeMachines.tsx` fetches.

import type {
  MachineBattery,
  MachineGPU,
  MachineStats,
  MachineStatsHistoryPoint,
  MachineStatsOverview,
  PeerConnectionView,
  ServerDescription,
} from '@kayushkin/llm-bridge-types'
import { MachineStatsSourceWindowsFromWSL } from '@kayushkin/llm-bridge-types'
import { connectedServerListings, thisServerLabel } from './serverListings'

export const MACHINE_STATS_PATH = '/machines/stats'

/** How often the page reads the newest readings again. */
export const MACHINE_STATS_POLL_MS = 30_000

/** How often an open machine's history is read again. A sample arrives every
 *  minute, so reading faster shows nothing new. */
export const MACHINE_STATS_HISTORY_REFRESH_MS = 60_000

/** A reading older than this means the machine has stopped reporting: three
 *  missed samples at one a minute, so one slow sample is not called stale. */
export const MACHINE_STATS_STALE_AFTER_MS = 3 * 60_000

/** Disk or memory use at or above this is drawn in the warning color. */
export const USAGE_WARNING_PERCENT = 85
/** And at or above this, in the danger color. */
export const USAGE_DANGER_PERCENT = 95

export function machineStatsHistoryPath(machineID: string, query: MachineStatsHistoryQuery): string {
  const params = new URLSearchParams({
    since: query.since,
    until: query.until,
    step_seconds: String(query.stepSeconds),
  })
  return `/machines/${encodeURIComponent(machineID)}/stats/history?${params.toString()}`
}

// ── History ranges ──────────────────────────────────────────────────────────

export interface MachineStatsHistoryRange {
  key: string
  label: string
  seconds: number
}

export const MACHINE_STATS_HISTORY_RANGES: readonly MachineStatsHistoryRange[] = [
  { key: '1h', label: '1 hour', seconds: 3600 },
  { key: '24h', label: '24 hours', seconds: 24 * 3600 },
  { key: '7d', label: '7 days', seconds: 7 * 24 * 3600 },
  { key: '30d', label: '30 days', seconds: 30 * 24 * 3600 },
]

/** The bucket widths a history is asked for in, smallest first. Each divides
 *  an hour or a day evenly, so bucket edges fall on round clock times. The
 *  smallest is the sample interval: a bucket narrower than that is empty. */
export const MACHINE_STATS_HISTORY_STEPS_SECONDS: readonly number[] = [
  60, 120, 180, 300, 600, 900, 1200, 1800, 3600, 5400, 7200, 10800, 21600, 43200, 86400,
]

/** The most points a chart is asked to draw. Past this a line is denser than
 *  the pixels it is drawn on. */
export const MACHINE_STATS_HISTORY_MAX_POINTS = 600

/** The bucket width for a range: the smallest step that keeps the range
 *  within `MACHINE_STATS_HISTORY_MAX_POINTS` buckets. One hour is drawn from
 *  raw minutes (60 points); a day in 3-minute buckets (480); a week in
 *  20-minute buckets (504); 30 days in 90-minute buckets (480). */
export function machineStatsHistoryStepSeconds(rangeSeconds: number): number {
  for (const step of MACHINE_STATS_HISTORY_STEPS_SECONDS) {
    if (rangeSeconds / step <= MACHINE_STATS_HISTORY_MAX_POINTS) return step
  }
  return MACHINE_STATS_HISTORY_STEPS_SECONDS[MACHINE_STATS_HISTORY_STEPS_SECONDS.length - 1]
}

export interface MachineStatsHistoryQuery {
  since: string
  until: string
  stepSeconds: number
}

/** The history request for a range ending now. */
export function machineStatsHistoryQuery(range: MachineStatsHistoryRange, now: Date): MachineStatsHistoryQuery {
  return {
    since: new Date(now.getTime() - range.seconds * 1000).toISOString(),
    until: now.toISOString(),
    stepSeconds: machineStatsHistoryStepSeconds(range.seconds),
  }
}

// ── Times ───────────────────────────────────────────────────────────────────

/** A Go `time.Time` the server left at its zero value, which `omitempty`
 *  does not drop: `0001-01-01T00:00:00Z`. It means "not known". */
export function isUnsetTime(value: string | undefined | null): boolean {
  if (!value) return true
  const ms = Date.parse(value)
  return Number.isNaN(ms) || new Date(ms).getUTCFullYear() <= 1
}

export type MachineStatsFreshness = 'never reported' | 'fresh' | 'stale'

/** Whether a machine's newest reading is recent enough to show as current. */
export function machineStatsFreshness(latest: Pick<MachineStats, 'collected_at'> | null | undefined, now: Date): MachineStatsFreshness {
  if (!latest || isUnsetTime(latest.collected_at)) return 'never reported'
  return now.getTime() - Date.parse(latest.collected_at) > MACHINE_STATS_STALE_AFTER_MS ? 'stale' : 'fresh'
}

/** A length of time in the two largest units: "3 d 4 h", "5 h 12 min",
 *  "12 min", "40 s". */
export function formatDurationSeconds(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds))
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return hours > 0 ? `${days} d ${hours} h` : `${days} d`
  if (hours > 0) return minutes > 0 ? `${hours} h ${minutes} min` : `${hours} h`
  if (minutes > 0) return `${minutes} min`
  return `${seconds} s`
}

/** How long the machine has been up now. From `booted_at` when the machine
 *  gave one; else the uptime it reported, carried forward to now. Null when
 *  it gave neither. */
export function machineUptimeSeconds(stats: Pick<MachineStats, 'booted_at' | 'uptime_seconds' | 'collected_at'>, now: Date): number | null {
  if (!isUnsetTime(stats.booted_at)) return (now.getTime() - Date.parse(stats.booted_at!)) / 1000
  if (stats.uptime_seconds) return stats.uptime_seconds + (now.getTime() - Date.parse(stats.collected_at)) / 1000
  return null
}

// ── Sizes and levels ────────────────────────────────────────────────────────

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']

/** Bytes in binary units, three significant digits: "512 B", "1.50 GiB",
 *  "31.4 GiB", "932 GiB". Binary because that is what every OS's own disk and
 *  memory tools mean, whatever letters they print. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024
    unit++
  }
  if (unit === 0) return `${value} B`
  const digits = value >= 100 ? 0 : value >= 10 ? 1 : 2
  return `${value.toFixed(digits)} ${BYTE_UNITS[unit]}`
}

/** Part of a whole as a percent, or null when the whole is unknown. */
export function percentOf(used: number, total: number): number | null {
  if (!total) return null
  return (used / total) * 100
}

export type UsageLevel = 'normal' | 'warning' | 'danger'

export function usageLevel(percent: number | null): UsageLevel {
  if (percent === null) return 'normal'
  if (percent >= USAGE_DANGER_PERCENT) return 'danger'
  if (percent >= USAGE_WARNING_PERCENT) return 'warning'
  return 'normal'
}

/** A share: one decimal below 10 so a small one is not rounded to nothing,
 *  except a whole number ("7%", "0.2%", "34%"). */
export function formatPercent(percent: number | null | undefined): string {
  if (percent === null || percent === undefined || !Number.isFinite(percent)) return '—'
  if (percent < 10 && !Number.isInteger(percent)) return `${percent.toFixed(1)}%`
  return `${Math.round(percent)}%`
}

/** A battery's level: running low is the danger, so it reads the share left
 *  against the same thresholds — warning at 15% left, danger at 5% — and only
 *  while it is draining. One on mains is never a warning. */
export function batteryLevel(battery: Pick<MachineBattery, 'state' | 'charge_percent'>): UsageLevel {
  if (battery.state !== 'discharging') return 'normal'
  return usageLevel(100 - battery.charge_percent)
}

// ── Machine kind ────────────────────────────────────────────────────────────

/** What sort of machine a reading comes from, for the picture on its card:
 *  one with a battery is a laptop; a Windows machine read from WSL, or one
 *  with a graphics processor and no battery, is a desktop; anything else is a
 *  server. Null for a machine that has never sent a reading. */
export type MachineKind = 'laptop' | 'desktop' | 'server'

export function machineKind(stats: Pick<MachineStats, 'batteries' | 'gpus' | 'source'> | null | undefined): MachineKind | null {
  if (!stats) return null
  if (stats.batteries?.length) return 'laptop'
  if (stats.source === MachineStatsSourceWindowsFromWSL || stats.gpus?.length) return 'desktop'
  return 'server'
}

// ── Parts of a reading ──────────────────────────────────────────────────────

/** One part of a machine's card: the machine itself, or — for a runner in
 *  WSL — the Windows host and the WSL Linux virtual machine inside it, each
 *  with its own OS, kernel, uptime, CPU, memory and drives. */
export interface MachineReadingPart {
  key: 'machine' | 'windows' | 'wsl'
  /** The part's heading; null for a machine that is one part only. */
  label: string | null
  stats: MachineStats
}

export function machineReadingParts(stats: MachineStats): MachineReadingPart[] {
  if (stats.source !== MachineStatsSourceWindowsFromWSL) return [{ key: 'machine', label: null, stats }]
  const parts: MachineReadingPart[] = [{ key: 'windows', label: 'Windows', stats }]
  if (stats.wsl) parts.push({ key: 'wsl', label: 'WSL', stats: stats.wsl })
  return parts
}

// ── Memory ──────────────────────────────────────────────────────────────────

/** One part of the memory bar, in the order it is drawn: what programs hold
 *  (`used`, less any compressed part), the compressor's share of it
 *  (`compressed`, macOS), file cache the OS hands back on demand (`cached`),
 *  and the rest (`free`). */
export type MemorySegmentKind = 'used' | 'compressed' | 'cached' | 'free'

export interface MemorySegment {
  kind: MemorySegmentKind
  bytes: number
  percent: number
}

export interface MemoryBreakdown {
  totalBytes: number
  /** Everything the machine reports as used, compressed part included. */
  usedBytes: number
  usedPercent: number
  /** Present only when the machine reports them. */
  compressedBytes: number | null
  cachedBytes: number | null
  /** Total less used less cache. */
  freeBytes: number
  /** The bar's parts, empty ones left out; they add up to the total. */
  segments: MemorySegment[]
}

/** How a machine's memory splits into used, compressed, cached and free, or
 *  null when it gave no total. Compressed is part of used and cache is part
 *  of available, as llm-bridge reports them; a part bigger than the room left
 *  for it is cut to fit, so the bar never adds up to more than the total. */
export function memoryBreakdown(stats: Pick<MachineStats,
  'memory_total_bytes' | 'memory_used_bytes' | 'memory_cached_bytes' | 'memory_compressed_bytes'>): MemoryBreakdown | null {
  const total = stats.memory_total_bytes ?? 0
  if (!total) return null
  const used = Math.min(stats.memory_used_bytes ?? 0, total)
  const compressed = stats.memory_compressed_bytes ? Math.min(stats.memory_compressed_bytes, used) : null
  const cached = stats.memory_cached_bytes ? Math.min(stats.memory_cached_bytes, total - used) : null
  const free = total - used - (cached ?? 0)
  const parts: [MemorySegmentKind, number][] = [
    ['used', used - (compressed ?? 0)],
    ['compressed', compressed ?? 0],
    ['cached', cached ?? 0],
    ['free', free],
  ]
  return {
    totalBytes: total,
    usedBytes: used,
    usedPercent: (used / total) * 100,
    compressedBytes: compressed,
    cachedBytes: cached,
    freeBytes: free,
    segments: parts.filter(([, bytes]) => bytes > 0).map(([kind, bytes]) => ({ kind, bytes, percent: (bytes / total) * 100 })),
  }
}

/** macOS' memory pressure as a share in use: 100 less the share it says it
 *  could free. Null where the machine does not report it. */
export function memoryPressurePercent(stats: Pick<MachineStats, 'memory_free_under_pressure_percent'>): number | null {
  const free = stats.memory_free_under_pressure_percent
  if (free === undefined || free === null) return null
  return 100 - free
}

/** The memory bar's level. Where the machine reports pressure (macOS), that
 *  decides it: a Mac keeps nearly all its memory in use by design, so the used
 *  share alone would cry wolf. Elsewhere, the used share. */
export function memoryLevel(stats: Pick<MachineStats,
  'memory_total_bytes' | 'memory_used_bytes' | 'memory_free_under_pressure_percent'>): UsageLevel {
  const pressure = memoryPressurePercent(stats)
  if (pressure !== null) return usageLevel(pressure)
  return usageLevel(percentOf(stats.memory_used_bytes ?? 0, stats.memory_total_bytes ?? 0))
}

export interface CommitCharge {
  committedBytes: number
  limitBytes: number
  percent: number
}

/** Windows' commit charge against its limit, or null where it is not reported. */
export function commitCharge(stats: Pick<MachineStats, 'memory_committed_bytes' | 'memory_commit_limit_bytes'>): CommitCharge | null {
  if (!stats.memory_committed_bytes || !stats.memory_commit_limit_bytes) return null
  return {
    committedBytes: stats.memory_committed_bytes,
    limitBytes: stats.memory_commit_limit_bytes,
    percent: (stats.memory_committed_bytes / stats.memory_commit_limit_bytes) * 100,
  }
}

/** The line under a strip's memory bar: what the bar leaves out — the
 *  compressor's share (macOS), memory pressure (macOS), commit charge
 *  (Windows) and swap — each with its level, so one past a threshold can say
 *  so in words. Only what the machine reports. */
export interface MemoryNote {
  name: 'compressed' | 'pressure' | 'commit' | 'swap'
  text: string
  level: UsageLevel
}

export function memoryNotes(stats: Pick<MachineStats,
  'memory_compressed_bytes' | 'memory_free_under_pressure_percent' | 'memory_committed_bytes' | 'memory_commit_limit_bytes' |
  'swap_total_bytes' | 'swap_used_bytes'>): MemoryNote[] {
  const notes: MemoryNote[] = []
  if (stats.memory_compressed_bytes) {
    notes.push({ name: 'compressed', text: `${formatBytes(stats.memory_compressed_bytes)} compressed`, level: 'normal' })
  }
  const pressure = memoryPressurePercent(stats)
  if (pressure !== null) notes.push({ name: 'pressure', text: `pressure ${formatPercent(pressure)}`, level: usageLevel(pressure) })
  const commit = commitCharge(stats)
  if (commit) {
    notes.push({ name: 'commit', text: `commit ${formatBytesOfTotal(commit.committedBytes, commit.limitBytes)}`, level: usageLevel(commit.percent) })
  }
  if (stats.swap_total_bytes) {
    const used = stats.swap_used_bytes ?? 0
    notes.push({ name: 'swap', text: `swap ${formatBytesOfTotal(used, stats.swap_total_bytes)}`, level: usageLevel(percentOf(used, stats.swap_total_bytes)) })
  }
  return notes
}

/** A used size against its total, the unit said once when both share it:
 *  "28.2/32.0 GiB", "614 MiB/4.00 GiB". */
export function formatBytesOfTotal(used: number, total: number): string {
  const usedText = formatBytes(used)
  const totalText = formatBytes(total)
  const usedUnit = usedText.split(' ')[1]
  const totalUnit = totalText.split(' ')[1]
  if (usedUnit && usedUnit === totalUnit) return `${usedText.split(' ')[0]}/${totalText}`
  return `${usedText}/${totalText}`
}

// ── Drives and graphics ─────────────────────────────────────────────────────

/** A graphics processor's memory in use as a share, or null when the driver
 *  gives no total. */
export function gpuMemoryPercent(gpu: Pick<MachineGPU, 'memory_used_bytes' | 'memory_total_bytes'>): number | null {
  if (gpu.memory_used_bytes === undefined || gpu.memory_used_bytes === null || !gpu.memory_total_bytes) return null
  return percentOf(gpu.memory_used_bytes, gpu.memory_total_bytes)
}

// ── Words ───────────────────────────────────────────────────────────────────

/** Where the numbers come from, said when it is not the obvious place: a
 *  runner in WSL reads the Windows machine around it, not its Linux. Empty
 *  for a machine read directly. */
export function machineStatsSourceNote(source: MachineStats['source']): string {
  if (source === MachineStatsSourceWindowsFromWSL) return 'Windows host, read from WSL'
  return ''
}

const BATTERY_STATE_WORDS: Readonly<Record<string, string>> = {
  charging: 'charging',
  discharging: 'on battery',
  full: 'full',
  not_charging: 'plugged in, not charging',
  unknown: 'state unknown',
}

export function batteryStateText(state: MachineBattery['state']): string {
  return BATTERY_STATE_WORDS[state] ?? state
}

/** What the OS's time estimate means for this state, or null when it gave
 *  none: time left on battery, or time until full. */
export function batteryTimeText(battery: Pick<MachineBattery, 'state' | 'seconds_remaining'>): string | null {
  if (battery.seconds_remaining === undefined || battery.seconds_remaining === null) return null
  const duration = formatDurationSeconds(battery.seconds_remaining)
  if (battery.state === 'charging') return `${duration} to full`
  if (battery.state === 'discharging') return `${duration} left`
  return null
}

/** What the page says about a runner machine's connection, or null for a
 *  machine the server reads without a runner. */
export function runnerConnectionText(overview: Pick<MachineStatsOverview, 'runner_connected'>): string | null {
  if (overview.runner_connected === undefined || overview.runner_connected === null) return null
  return overview.runner_connected ? 'runner connected' : 'runner not connected'
}

// ── Answers from a server ───────────────────────────────────────────────────

/** What a server said to `GET /machines/stats`, or to a history read.
 *  `not reported` is a server built before machine stats: it answers 404 with
 *  a plain-text body (its `GET /machines/{id}` handler, or the router's own
 *  "404 page not found"). A 404 that carries the JSON error envelope is a
 *  refusal from a server that knows the route — the proxy's unknown
 *  connection, an unknown machine — and is an error like any other. */
export type MachineStatsAnswer<T> =
  | { state: 'reported'; value: T }
  | { state: 'not reported' }

export function isJSONErrorEnvelope(contentType: string, body: string): boolean {
  if (!contentType.includes('json')) return false
  try {
    const parsed = JSON.parse(body) as { error?: unknown }
    return parsed !== null && typeof parsed === 'object' && 'error' in parsed
  } catch {
    return false
  }
}

/** Whether a failed response means "this server does not have machine stats". */
export function responseMeansNotReported(status: number, contentType: string, body: string): boolean {
  return status === 404 && !isJSONErrorEnvelope(contentType, body)
}

export const MACHINE_STATS_NOT_REPORTED_TEXT = 'This server does not report machine stats yet: it runs a build from before they were added.'

// ── Servers ─────────────────────────────────────────────────────────────────

export interface MachineStatsServer {
  /** Stable React key and poll key: the connection id, or `this-server`. */
  key: string
  /** The connection this server is reached by; null for this server. */
  connectionId: string | null
  label: string
  /** Why it cannot be read now, or null when it can. */
  blockedReason: string | null
}

/** This server first, then every connected server that is not removed, by
 *  name. One that cannot be read now stays listed, saying why, so a server
 *  that went offline does not silently vanish from the page. */
export function machineStatsServers(
  thisServer: Pick<ServerDescription, 'name'> | null,
  views: readonly PeerConnectionView[],
): MachineStatsServer[] {
  const removed = new Set(views.filter(view => view.connection.removed_at).map(view => view.connection.id))
  const connected = connectedServerListings(views)
    .filter(listing => !removed.has(listing.connectionId))
    .sort((a, b) => a.label.localeCompare(b.label))
    .map(listing => ({
      key: listing.connectionId,
      connectionId: listing.connectionId,
      label: listing.label,
      blockedReason: listing.blockedReason,
    }))
  return [
    { key: 'this-server', connectionId: null, label: thisServerLabel(thisServer?.name), blockedReason: null },
    ...connected,
  ]
}

// ── Charts ──────────────────────────────────────────────────────────────────

/** One point of a chart: when (ms since the epoch) and the value, null where
 *  the reading has none. */
export interface ChartPoint {
  at: number
  value: number | null
}

/** The runs of a series to draw as unbroken lines. A run breaks at a null
 *  value and where two neighboring points are more than two buckets apart,
 *  because the machine sent nothing in between and a line across the gap
 *  would claim it did. */
export function chartRuns(points: readonly ChartPoint[], stepSeconds: number): { at: number; value: number }[][] {
  const runs: { at: number; value: number }[][] = []
  let current: { at: number; value: number }[] = []
  let previousAt: number | null = null
  for (const point of points) {
    const gap = previousAt !== null && point.at - previousAt > 2 * stepSeconds * 1000
    if (point.value === null || gap) {
      if (current.length) runs.push(current)
      current = []
    }
    if (point.value !== null) current.push({ at: point.at, value: point.value })
    previousAt = point.at
  }
  if (current.length) runs.push(current)
  return runs
}

/** The index of the point nearest a time, or -1 for no points. */
export function nearestPointIndex(points: readonly { at: number }[], at: number): number {
  let best = -1
  let bestDistance = Infinity
  for (let i = 0; i < points.length; i++) {
    const distance = Math.abs(points[i].at - at)
    if (distance < bestDistance) {
      best = i
      bestDistance = distance
    }
  }
  return best
}

/** A time on a chart's axis or readout, in the viewer's own zone: the clock
 *  time alone for a range of a day or less, the date as well for longer. */
export function formatChartTime(at: number, rangeSeconds: number): string {
  const date = new Date(at)
  if (rangeSeconds <= 24 * 3600) return date.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
  return date.toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
}

/** The drives a machine's history covers, by mount point, in the order of the
 *  newest reading, then any that only older readings had. */
export function historyDriveMountPoints(points: readonly MachineStatsHistoryPoint[]): string[] {
  const order: string[] = []
  const seen = new Set<string>()
  for (let i = points.length - 1; i >= 0; i--) {
    for (const drive of points[i].stats.drives ?? []) {
      if (!seen.has(drive.mount_point)) {
        seen.add(drive.mount_point)
        order.push(drive.mount_point)
      }
    }
  }
  return order
}

/** The batteries a machine's history covers, by name, newest reading first. */
export function historyBatteryNames(points: readonly MachineStatsHistoryPoint[]): string[] {
  const order: string[] = []
  const seen = new Set<string>()
  for (let i = points.length - 1; i >= 0; i--) {
    for (const battery of points[i].stats.batteries ?? []) {
      if (!seen.has(battery.name)) {
        seen.add(battery.name)
        order.push(battery.name)
      }
    }
  }
  return order
}

/** One graphics processor followed through a history. A GPU has no id of its
 *  own, only a name, and two cards of one model share it; so it is followed by
 *  its place in each reading's list and its name together, and a reading
 *  whose GPU at that place has another name counts as having none. */
export interface HistoryGPU {
  index: number
  name: string
}

/** The GPUs a history covers, those of the newest reading first. */
export function historyGPUs(points: readonly MachineStatsHistoryPoint[]): HistoryGPU[] {
  const order: HistoryGPU[] = []
  const seen = new Set<string>()
  for (let i = points.length - 1; i >= 0; i--) {
    ;(points[i].stats.gpus ?? []).forEach((gpu, index) => {
      const key = `${index}\u0000${gpu.name}`
      if (!seen.has(key)) {
        seen.add(key)
        order.push({ index, name: gpu.name })
      }
    })
  }
  return order
}

/** One GPU's reading at one point, or null where that place holds another GPU or none. */
export function historyGPUAt(point: MachineStatsHistoryPoint, gpu: HistoryGPU): MachineGPU | null {
  const candidate = point.stats.gpus?.[gpu.index]
  return candidate && candidate.name === gpu.name ? candidate : null
}

export function gpuUsageSeries(points: readonly MachineStatsHistoryPoint[], gpu: HistoryGPU): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: historyGPUAt(point, gpu)?.usage_percent ?? null }))
}

export function gpuMemoryUsedSeries(points: readonly MachineStatsHistoryPoint[], gpu: HistoryGPU): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: historyGPUAt(point, gpu)?.memory_used_bytes ?? null }))
}

/** The largest memory total one GPU reported over a history, 0 for none. */
export function largestGPUMemoryTotal(points: readonly MachineStatsHistoryPoint[], gpu: HistoryGPU): number {
  let largest = 0
  for (const point of points) largest = Math.max(largest, historyGPUAt(point, gpu)?.memory_total_bytes ?? 0)
  return largest
}

export function pointTime(point: MachineStatsHistoryPoint): number {
  return Date.parse(point.at)
}

/** One drive's used share at each point, null where that reading lacks it. */
export function driveUsedPercentSeries(points: readonly MachineStatsHistoryPoint[], mountPoint: string): ChartPoint[] {
  return points.map(point => {
    const drive = point.stats.drives?.find(candidate => candidate.mount_point === mountPoint)
    return { at: pointTime(point), value: drive ? percentOf(drive.used_bytes, drive.total_bytes) : null }
  })
}

export function batteryChargeSeries(points: readonly MachineStatsHistoryPoint[], name: string): ChartPoint[] {
  return points.map(point => {
    const battery = point.stats.batteries?.find(candidate => candidate.name === name)
    return { at: pointTime(point), value: battery ? battery.charge_percent : null }
  })
}

export function memoryUsedSeries(points: readonly MachineStatsHistoryPoint[]): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: point.stats.memory_total_bytes ? point.stats.memory_used_bytes ?? 0 : null }))
}

export function swapUsedSeries(points: readonly MachineStatsHistoryPoint[]): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: point.stats.swap_total_bytes ? point.stats.swap_used_bytes ?? 0 : null }))
}

export function cpuAverageSeries(points: readonly MachineStatsHistoryPoint[]): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: point.cpu_usage_percent_average ?? null }))
}

export function cpuMaximumSeries(points: readonly MachineStatsHistoryPoint[]): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: point.cpu_usage_percent_maximum ?? null }))
}

/** Whether any reading in a history carries a WSL reading inside it. */
export function historyHasWSL(points: readonly MachineStatsHistoryPoint[]): boolean {
  return points.some(point => point.stats.wsl)
}

/** The WSL virtual machine's CPU at each point: the newest reading in the
 *  bucket, since the bucket's average and peak are the Windows host's. */
export function wslCPUSeries(points: readonly MachineStatsHistoryPoint[]): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: point.stats.wsl?.cpu_usage_percent ?? null }))
}

export function wslMemoryUsedSeries(points: readonly MachineStatsHistoryPoint[]): ChartPoint[] {
  return points.map(point => ({ at: pointTime(point), value: point.stats.wsl?.memory_total_bytes ? point.stats.wsl.memory_used_bytes ?? 0 : null }))
}

/** The largest total a series of readings reported, for the top of a byte
 *  axis: memory or swap can grow while the range is drawn. */
export function largestTotal(points: readonly MachineStatsHistoryPoint[], total: (stats: MachineStats) => number | undefined): number {
  let largest = 0
  for (const point of points) largest = Math.max(largest, total(point.stats) ?? 0)
  return largest
}
