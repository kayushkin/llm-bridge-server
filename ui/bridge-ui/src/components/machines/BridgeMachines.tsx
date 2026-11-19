import { useEffect, useMemo, useState, type MouseEvent, type ReactNode } from 'react'
import type {
  MachineBattery,
  MachineDrive,
  MachineGPU,
  MachineStats,
  MachineStatsHistory,
  MachineStatsOverview,
} from '@kayushkin/llm-bridge-types'
import { useBridgeConfig, type BridgeConfig } from '../../context'
import {
  MACHINE_STATS_HISTORY_RANGES,
  MACHINE_STATS_NOT_REPORTED_TEXT,
  batteryChargeSeries,
  batteryLevel,
  batteryStateText,
  batteryTimeText,
  commitCharge,
  cpuAverageSeries,
  cpuMaximumSeries,
  driveUsedPercentSeries,
  formatBytes,
  formatBytesOfTotal,
  formatChartTime,
  formatDurationSeconds,
  formatPercent,
  gpuMemoryPercent,
  gpuMemoryUsedSeries,
  gpuUsageSeries,
  historyBatteryNames,
  historyDriveMountPoints,
  historyGPUAt,
  historyGPUs,
  historyHasWSL,
  isUnsetTime,
  largestGPUMemoryTotal,
  largestTotal,
  machineKind,
  machineReadingParts,
  machineStatsFreshness,
  machineStatsSourceNote,
  machineUptimeSeconds,
  memoryBreakdown,
  memoryLevel,
  memoryNotes,
  memoryPressurePercent,
  memoryUsedSeries,
  percentOf,
  runnerConnectionText,
  swapUsedSeries,
  usageLevel,
  wslCPUSeries,
  wslMemoryUsedSeries,
  type MachineReadingPart,
  type MachineStatsFreshness,
  type MachineStatsHistoryRange,
  type MachineStatsServer,
} from '../../machineStats'
import {
  machineStatsServerConfig,
  useMachineStatsHistory,
  useMachineStatsOverview,
  useMachineStatsServers,
} from '../../useMachineStats'
import { timeAgo } from '../../utils'
import { MachineStatsChart } from './MachineStatsChart'
import {
  Bar,
  BatteryGlyph,
  Dial,
  LevelWord,
  MachinePicture,
  MemoryBar,
  MemoryKey,
  ThinBarRow,
  type MachineHue,
  type MachinePictureName,
} from './MachineGauges'
import styles from './BridgeMachines.module.css'

/**
 * The Machines page: the health of every machine this server knows and every
 * machine each connected server knows — CPU, GPU, memory, drives, batteries,
 * uptime and OS — from llm-bridge-server's `GET /machines/stats`, read every
 * 30 seconds, with each machine's history from
 * `GET /machines/{id}/stats/history` when its strip is opened.
 *
 * A spec sheet: one strip per machine in shared columns — Machine, CPU, GPU,
 * Memory, Drives, Battery — so machines compare down a column. Every server
 * group sits in one grid (strips are subgrids of it), so columns line up
 * across groups. A runner in WSL gets an indented sub-strip for the WSL
 * machine under its Windows strip. Opening a strip shows its details and
 * history under it. On a narrow page each strip becomes a card of labeled
 * rows.
 *
 * A section per server, this one first. A connected server is read through
 * this server's proxy for the connection. One that cannot be read now says
 * why; one built before machine stats says that, not an error.
 *
 * Each measure has its own mark (`MachineGauges.tsx`). The zine skin also
 * paints the machine and column pictures from the empty `.bc-machine-picture`
 * hooks; the other themes hide them.
 */
export function BridgeMachines() {
  const config = useBridgeConfig()
  const { servers, error } = useMachineStatsServers(config)
  // Re-render once a minute so "last reported" and uptime keep moving between polls.
  const now = useMinuteClock()

  return (
    <div className={`${styles.page} bc-machines`}>
      <header className={styles.header}>
        <h2 className={`${styles.title} page-title`}>Machines</h2>
        <p className={styles.subtitle}>
          Each machine sends a reading every minute; the server keeps 30 days. Read down a column to compare machines;
          open one for its details and history. Times are in this browser's time zone.
        </p>
      </header>
      {error ? <p className={styles.error}>{error}</p> : null}
      <div className={styles.sheet}>
        {servers.map(server => <ServerSection key={server.key} server={server} config={config} now={now} />)}
      </div>
    </div>
  )
}

function useMinuteClock(): Date {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), 60_000)
    return () => clearInterval(timer)
  }, [])
  return now
}

function ServerSection({ server, config, now }: { server: MachineStatsServer; config: BridgeConfig; now: Date }) {
  return (
    <section className={`${styles.server} bc-machine-server`} aria-label={server.label}>
      <h3 className={`${styles.serverHeading} bc-machine-server-heading`}>
        <span className={styles.serverName}>{server.label}</span>
        <span className={styles.chip}>{server.connectionId === null ? 'this server' : 'connected server'}</span>
      </h3>
      {server.blockedReason
        ? <p className={`${styles.notice} ${styles.span}`}>{server.blockedReason}</p>
        : <ServerMachines server={server} config={config} now={now} />}
    </section>
  )
}

function ServerMachines({ server, config, now }: { server: MachineStatsServer; config: BridgeConfig; now: Date }) {
  const serverConfig = useMemo(() => machineStatsServerConfig(config, server), [config, server.connectionId])
  const overview = useMachineStatsOverview(serverConfig)
  const [openMachineID, setOpenMachineID] = useState<string | null>(null)

  if (overview.loading && !overview.data) return <p className={`${styles.muted} ${styles.span}`}>Loading…</p>
  const answer = overview.data
  return (
    <>
      {overview.error ? <p className={`${styles.error} ${styles.span}`}>{overview.error}</p> : null}
      {answer?.state === 'not reported' ? (
        <div className={`${styles.notReported} ${styles.span} bc-machine-not-reported`}>
          <MachinePicture name="crab-waiting" />
          <p className={styles.notice}>{MACHINE_STATS_NOT_REPORTED_TEXT}</p>
        </div>
      ) : null}
      {answer?.state === 'reported' && answer.value.length === 0 ? <p className={`${styles.muted} ${styles.span}`}>This server has no machines.</p> : null}
      {answer?.state === 'reported' && answer.value.length > 0 ? (
        <>
          <ColumnHeadings />
          {answer.value.map(entry => (
            <MachineStrip
              key={entry.machine.id}
              overview={entry}
              now={now}
              open={openMachineID === entry.machine.id}
              onToggle={() => setOpenMachineID(current => current === entry.machine.id ? null : entry.machine.id)}
              serverConfig={serverConfig}
            />
          ))}
        </>
      ) : null}
    </>
  )
}

/** The columns' names, once per server group. Each cell also carries its own
 *  name, shown when the strip is a card on a narrow page, so this row is for
 *  the eye only. */
function ColumnHeadings() {
  return (
    <div className={`${styles.columns} bc-machine-columns`} aria-hidden="true">
      <span>Machine</span>
      <span><MachinePicture name="cpu" size="small" />CPU</span>
      <span><MachinePicture name="gpu" size="small" />GPU</span>
      <span><MachinePicture name="memory" size="small" />Memory</span>
      <span><MachinePicture name="drive" size="small" />Drives</span>
      <span><MachinePicture name="battery" size="small" />Battery</span>
      <span />
    </div>
  )
}

/** One machine: its strip, the WSL machine's sub-strip under it when it has
 *  one, and — open — its details and history. A click anywhere on the strip
 *  opens it; the chevron at its end is the same control for the keyboard. */
export function MachineStrip({ overview, now, open, onToggle, serverConfig }: {
  overview: MachineStatsOverview
  now: Date
  /** Whether its details and history are shown. */
  open: boolean
  onToggle: () => void
  serverConfig: BridgeConfig
}) {
  const { machine, latest } = overview
  const freshness = machineStatsFreshness(latest, now)
  const fresh = freshness === 'fresh'
  const parts = latest ? machineReadingParts(latest) : []
  const panelID = `machine-details-${machine.id}`
  // Toggling on a click that ends a text selection would close the strip under
  // someone copying a number from it.
  const onRowClick = (event: MouseEvent<HTMLDivElement>) => {
    if ((event.target as HTMLElement).closest('button, a')) return
    if (window.getSelection()?.toString()) return
    onToggle()
  }

  return (
    <article
      className={`${styles.strip} ${open ? styles.stripOpen : ''} bc-machine-strip`}
      data-freshness={freshness}
      data-open={open ? 'true' : undefined}
      aria-label={machine.name}
    >
      <div className={`${styles.row} bc-machine-row`} onClick={latest ? onRowClick : undefined}>
        <MachineCell overview={overview} part={parts[0] ?? null} freshness={freshness} now={now} />
        {latest && parts[0] ? (
          <PartCells part={parts[0]} />
        ) : (
          <div className={`${styles.cell} ${styles.neverCell}`}>This machine has never sent a reading.</div>
        )}
        <div className={`${styles.cell} ${styles.toggleCell}`}>
          {latest ? (
            <button
              type="button"
              className={`${styles.toggle} bc-machine-button`}
              aria-expanded={open}
              aria-controls={panelID}
              title={open ? 'Hide details and history' : 'Details and history'}
              onClick={onToggle}
            >
              <svg className={styles.chevron} viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6l4 4 4-4" /></svg>
              <span className={styles.visuallyHidden}>{open ? 'Hide details and history' : 'Details and history'}</span>
            </button>
          ) : null}
        </div>
      </div>
      {parts.slice(1).map(part => (
        <div key={part.key} className={`${styles.row} ${styles.subRow} bc-machine-subrow`} onClick={onRowClick}>
          <PartNameCell part={part} fresh={fresh} now={now} />
          <PartCells part={part} />
          <div className={`${styles.cell} ${styles.toggleCell}`} />
        </div>
      ))}
      {open && latest ? (
        <div id={panelID} className={`${styles.panel} bc-machine-panel`}>
          <div className={styles.panelParts}>
            {parts.map(part => <PartDetails key={part.key} part={part} showTitle={parts.length > 1} now={now} fresh={fresh} />)}
          </div>
          <MachineHistory serverConfig={serverConfig} machineID={machine.id} />
        </div>
      ) : null}
    </article>
  )
}

/** A cell's name: hidden while the strip sits under its column heading,
 *  shown when the strip is a card. */
function CellLabel({ picture, children }: { picture?: MachinePictureName; children: ReactNode }) {
  return <span className={`${styles.cellLabel} bc-machine-cell-label`}>{picture ? <MachinePicture name={picture} size="small" /> : null}{children}</span>
}

function Absent({ picture, name }: { picture: MachinePictureName; name: string }) {
  return (
    <div className={`${styles.cell} ${styles.absent}`}>
      <CellLabel picture={picture}>{name}</CellLabel>
      <span className={styles.dash} aria-label={`no ${name}`}>—</span>
    </div>
  )
}

function uptimeTitle(stats: MachineStats): string {
  const words: string[] = []
  if (stats.hostname) words.push(`host ${stats.hostname}`)
  if (!isUnsetTime(stats.booted_at)) words.push(`last reboot ${new Date(stats.booted_at!).toLocaleString()}`)
  return words.join(' · ')
}

/** The first column: the machine's picture and name, its OS and kernel on
 *  lines of their own, and whether it is reporting and for how long. */
function MachineCell({ overview, part, freshness, now }: {
  overview: MachineStatsOverview
  part: MachineReadingPart | null
  freshness: MachineStatsFreshness
  now: Date
}) {
  const { machine, latest } = overview
  const kind = machineKind(latest)
  const runnerText = runnerConnectionText(overview)
  const stats = part?.stats
  const uptime = latest && freshness === 'fresh' ? machineUptimeSeconds(latest, now) : null
  const sourceNote = latest && !latest.wsl ? machineStatsSourceNote(latest.source) : ''
  const errors = latest?.collection_errors?.length ?? 0
  return (
    <div className={`${styles.cell} ${styles.machineCell}`}>
      {kind ? <MachinePicture name={kind} /> : null}
      <div className={styles.machineText}>
        <h4 className={`${styles.machineName} bc-machine-name`}>
          {machine.emoji ? <span aria-hidden="true">{machine.emoji} </span> : null}{machine.name}
        </h4>
        {stats?.operating_system ? <span className={`${styles.osLine} bc-machine-os-line`} title={uptimeTitle(stats)}>{stats.operating_system}</span> : null}
        {stats?.kernel_version ? <span className={`${styles.osLine} bc-machine-os-line`}>{stats.kernel_version}</span> : null}
        <span className={`${styles.status} bc-machine-freshness`} data-freshness={freshness} title={runnerText ?? undefined}>
          <span className={`${styles.statusDot} bc-machine-freshness-dot`} aria-hidden="true" />
          {freshness === 'fresh' ? (uptime !== null ? `up ${formatDurationSeconds(uptime)}` : 'reporting')
            : freshness === 'stale' ? `stale · ${timeAgo(latest!.collected_at)}`
              : 'not reporting'}
          {freshness === 'fresh' && uptime !== null ? <span className={styles.visuallyHidden}>, reporting</span> : null}
        </span>
        {runnerText === 'runner not connected' ? <span className={styles.osLine}>{runnerText}</span> : null}
        {sourceNote ? <span className={styles.osLine}>{sourceNote}</span> : null}
        {errors ? <span className={`${styles.errorChip} bc-machine-error-chip`}>could not read {errors === 1 ? 'one part' : `${errors} parts`}</span> : null}
      </div>
    </div>
  )
}

/** The first column of the WSL sub-strip. */
function PartNameCell({ part, fresh, now }: { part: MachineReadingPart; fresh: boolean; now: Date }) {
  const uptime = fresh ? machineUptimeSeconds(part.stats, now) : null
  return (
    <div className={`${styles.cell} ${styles.machineCell}`}>
      <div className={styles.machineText}>
        <h5 className={`${styles.machineName} ${styles.partName} bc-machine-name`}>{part.label}</h5>
        {part.stats.operating_system ? <span className={`${styles.osLine} bc-machine-os-line`}>{part.stats.operating_system}</span> : null}
        {part.stats.kernel_version ? <span className={`${styles.osLine} bc-machine-os-line`} title={part.stats.kernel_version}>{part.stats.kernel_version}</span> : null}
        {uptime !== null ? <span className={styles.status}>up {formatDurationSeconds(uptime)}</span> : null}
      </div>
    </div>
  )
}

/** The measure columns of one part: CPU, GPU, memory, drives, battery. */
function PartCells({ part }: { part: MachineReadingPart }) {
  const { stats } = part
  const gpus = stats.gpus ?? []
  return (
    <>
      {stats.cpu_usage_percent !== undefined && stats.cpu_usage_percent !== null ? (
        <DialCell picture="cpu" name="CPU" percent={stats.cpu_usage_percent} hue="cpu"
          detail={[stats.cpu_model, stats.cpu_logical_cores ? `${stats.cpu_logical_cores} cores` : ''].filter(Boolean).join(' · ')} />
      ) : <Absent picture="cpu" name="CPU" />}
      {gpus.length ? <GPUCell gpus={gpus} /> : <Absent picture="gpu" name="GPU" />}
      <MemoryCell stats={stats} />
      {stats.drives?.length ? <DrivesCell drives={stats.drives} virtualDisk={part.key === 'wsl'} /> : <Absent picture="drive" name="Drives" />}
      {stats.batteries?.length ? <BatteryCell batteries={stats.batteries} /> : <Absent picture="battery" name="Battery" />}
    </>
  )
}

/** A dial with its share large beside it, and a line under them. */
function DialCell({ picture, name, percent, hue, detail }: { picture: MachinePictureName; name: string; percent: number; hue: MachineHue; detail: string }) {
  return (
    <div className={`${styles.cell} bc-machine-cell`} data-cell={picture}>
      <CellLabel picture={picture}>{name}</CellLabel>
      <DialReading name={name} percent={percent} hue={hue} />
      {detail ? <span className={styles.detail}>{detail}</span> : null}
    </div>
  )
}

function DialReading({ name, percent, hue }: { name: string; percent: number; hue: MachineHue }) {
  const level = usageLevel(percent)
  return (
    <div className={styles.dialReading}>
      <Dial percent={percent} label={`${name} in use`} hue={hue} />
      <span className={styles.bigStack}>
        <span className={`${styles.big} bc-machine-big`}>{formatPercent(percent).replace('%', '')}<small>%</small></span>
        <LevelWord level={level} />
      </span>
    </div>
  )
}

function GPUCell({ gpus }: { gpus: readonly MachineGPU[] }) {
  return (
    <div className={`${styles.cell} bc-machine-cell`} data-cell="gpu">
      <CellLabel picture="gpu">{gpus.length > 1 ? 'GPUs' : 'GPU'}</CellLabel>
      {gpus.map((gpu, index) => {
        const words = [gpu.name]
        if (gpu.memory_used_bytes !== undefined && gpu.memory_used_bytes !== null) {
          words.push(gpu.memory_total_bytes ? formatBytesOfTotal(gpu.memory_used_bytes, gpu.memory_total_bytes) : `${formatBytes(gpu.memory_used_bytes)} memory`)
        }
        if (gpu.temperature_celsius !== undefined && gpu.temperature_celsius !== null) words.push(`${Math.round(gpu.temperature_celsius)} °C`)
        return (
          <div key={`${index}-${gpu.name}`} className={styles.gpu}>
            {gpu.usage_percent !== undefined && gpu.usage_percent !== null
              ? <DialReading name={gpus.length > 1 ? `GPU ${index + 1}` : 'GPU'} percent={gpu.usage_percent} hue="gpu" />
              : <span className={styles.dash}>use not reported</span>}
            <span className={styles.detail}>{words.join(' · ')}</span>
          </div>
        )
      })}
    </div>
  )
}

/** Memory: its share large, used of total, the segmented bar, and a note of
 *  compressed, pressure, commit charge and swap. */
function MemoryCell({ stats }: { stats: MachineStats }) {
  const breakdown = memoryBreakdown(stats)
  if (!breakdown) return <Absent picture="memory" name="Memory" />
  const level = memoryLevel(stats)
  const notes = memoryNotes(stats)
  return (
    <div className={`${styles.cell} bc-machine-cell`} data-cell="memory">
      <CellLabel picture="memory">Memory</CellLabel>
      <div className={styles.memoryTop}>
        <span className={`${styles.big} bc-machine-big`}>{formatPercent(breakdown.usedPercent).replace('%', '')}<small>%</small></span>
        <span className={styles.ofTotal}>{formatBytesOfTotal(breakdown.usedBytes, breakdown.totalBytes)}</span>
        <LevelWord level={level} />
      </div>
      <MemoryBar breakdown={breakdown} level={level} />
      {notes.length ? (
        <span className={styles.detail}>
          {notes.map((note, index) => (
            <span key={note.name} className={styles.note}>
              {index ? ' · ' : ''}{note.text}<LevelWord level={note.level} />
            </span>
          ))}
        </span>
      ) : null}
    </div>
  )
}

/** Drives: one line each — mount point, bar, share, free space, level word. */
function DrivesCell({ drives, virtualDisk }: { drives: readonly MachineDrive[]; virtualDisk: boolean }) {
  return (
    <div className={`${styles.cell} ${styles.wideCell} bc-machine-cell`} data-cell="drive">
      <CellLabel picture="drive">{virtualDisk ? 'Virtual disk' : 'Drives'}</CellLabel>
      <div className={styles.drives}>
        {drives.map(drive => {
          const percent = percentOf(drive.used_bytes, drive.total_bytes)
          return (
            <div key={drive.mount_point} className={styles.drive} title={[drive.label, drive.filesystem_type, drive.device].filter(Boolean).join(' · ') || undefined}>
              <span className={`${styles.mountPoint} bc-machine-mount-point`}>{drive.mount_point}</span>
              <Bar percent={percent ?? 0} label={`${drive.mount_point} in use`} hue="drive" />
              <span className={styles.drivePercent}>{formatPercent(percent)}</span>
              <span className={styles.driveFree}>
                {drive.label ? <>{drive.label} · </> : null}{formatBytes(drive.free_bytes)} free
              </span>
              <LevelWord level={usageLevel(percent)} />
            </div>
          )
        })}
      </div>
    </div>
  )
}

function BatteryCell({ batteries }: { batteries: readonly MachineBattery[] }) {
  return (
    <div className={`${styles.cell} bc-machine-cell`} data-cell="battery">
      <CellLabel picture="battery">{batteries.length > 1 ? 'Batteries' : 'Battery'}</CellLabel>
      {batteries.map(battery => {
        const level = batteryLevel(battery)
        const time = batteryTimeText(battery)
        return (
          <div key={battery.name} className={styles.batteryReading} title={battery.name}>
            <div className={styles.dialReading}>
              <BatteryGlyph battery={battery} />
              <span className={styles.bigStack}>
                <span className={`${styles.big} bc-machine-big`}>{formatPercent(battery.charge_percent).replace('%', '')}<small>%</small></span>
                <LevelWord level={level} word={level === 'danger' ? 'nearly empty' : 'low'} />
              </span>
            </div>
            <span className={styles.detail}>{time ?? batteryStateText(battery.state)}</span>
          </div>
        )
      })}
    </div>
  )
}

/** Everything a strip leaves out, for one part: the facts, the memory key,
 *  swap, pressure and commit charge as bars, each GPU and drive in full, the
 *  battery's health, and what could not be read. */
function PartDetails({ part, showTitle, now, fresh }: { part: MachineReadingPart; showTitle: boolean; now: Date; fresh: boolean }) {
  const { stats } = part
  // A machine that stopped reporting may be down, so its uptime is given as
  // of its last reading rather than carried on to now.
  const uptime = machineUptimeSeconds(stats, fresh ? now : new Date(stats.collected_at))
  const breakdown = memoryBreakdown(stats)
  const level = memoryLevel(stats)
  const pressure = memoryPressurePercent(stats)
  const commit = commitCharge(stats)
  const swapPercent = percentOf(stats.swap_used_bytes ?? 0, stats.swap_total_bytes ?? 0)

  return (
    <div className={styles.details}>
      {showTitle ? <h5 className={`${styles.partTitle} bc-machine-part-heading`}>{part.label}</h5> : null}
      <dl className={`${styles.facts} bc-machine-facts`}>
        {stats.operating_system ? <><dt>OS</dt><dd>{stats.operating_system}</dd></> : null}
        {stats.kernel_version ? <><dt>Kernel</dt><dd>{stats.kernel_version}</dd></> : null}
        {stats.hostname ? <><dt>Host</dt><dd>{stats.hostname}</dd></> : null}
        {uptime !== null ? <><dt>Uptime</dt><dd>{fresh ? 'up' : 'was up'} {formatDurationSeconds(uptime)}{fresh ? '' : ' at its last reading'}</dd></> : null}
        {!isUnsetTime(stats.booted_at) ? <><dt>Last reboot</dt><dd>{new Date(stats.booted_at!).toLocaleString()}</dd></> : null}
        {stats.cpu_model ? <><dt>CPU</dt><dd>{stats.cpu_model}</dd></> : null}
        {stats.cpu_logical_cores ? <><dt>Cores</dt><dd>{stats.cpu_logical_cores} logical</dd></> : null}
        {stats.load_average?.length ? <><dt>Load</dt><dd>{stats.load_average.map(load => load.toFixed(2)).join(' · ')}</dd></> : null}
        {(stats.gpus ?? []).map((gpu, index) => (
          <GPUFacts key={`${index}-${gpu.name}`} gpu={gpu} numbered={(stats.gpus ?? []).length > 1 ? index + 1 : null} />
        ))}
        {(stats.drives ?? []).map(drive => <DriveFacts key={drive.mount_point} drive={drive} />)}
        {(stats.batteries ?? []).map(battery => <BatteryFacts key={battery.name} battery={battery} />)}
      </dl>
      {breakdown ? <MemoryKey breakdown={breakdown} level={level} /> : null}
      <div className={styles.thinRows}>
        {pressure !== null ? (
          <ThinBarRow name="Pressure" percent={pressure} valueText={formatPercent(pressure)} hue="memory"
            note={`macOS could free ${formatPercent(stats.memory_free_under_pressure_percent)} of memory`} />
        ) : null}
        {commit ? (
          <ThinBarRow name="Commit charge" percent={commit.percent} valueText={formatBytesOfTotal(commit.committedBytes, commit.limitBytes)} hue="memory"
            note="memory promised to programs, against RAM and the page file" />
        ) : null}
        {stats.swap_total_bytes ? (
          <ThinBarRow name="Swap" percent={swapPercent ?? 0} valueText={formatBytesOfTotal(stats.swap_used_bytes ?? 0, stats.swap_total_bytes)} hue="swap" />
        ) : null}
      </div>
      {stats.collection_errors?.length ? (
        <div className={styles.collectionErrors}>
          <span>Could not read:</span>
          <ul>{stats.collection_errors.map(text => <li key={text}>{text}</li>)}</ul>
        </div>
      ) : null}
    </div>
  )
}

function GPUFacts({ gpu, numbered }: { gpu: MachineGPU; numbered: number | null }) {
  const memoryPercent = gpuMemoryPercent(gpu)
  const words = [gpu.name]
  if (memoryPercent !== null) words.push(`${formatBytesOfTotal(gpu.memory_used_bytes!, gpu.memory_total_bytes!)} memory (${formatPercent(memoryPercent)})`)
  else if (gpu.memory_used_bytes !== undefined && gpu.memory_used_bytes !== null) words.push(`${formatBytes(gpu.memory_used_bytes)} memory in use`)
  if (gpu.temperature_celsius !== undefined && gpu.temperature_celsius !== null) words.push(`${Math.round(gpu.temperature_celsius)} °C`)
  return <><dt>{numbered === null ? 'GPU' : `GPU ${numbered}`}</dt><dd>{words.join(' · ')}</dd></>
}

function DriveFacts({ drive }: { drive: MachineDrive }) {
  const words = [
    drive.label ?? '',
    `${formatBytesOfTotal(drive.used_bytes, drive.total_bytes)} used`,
    `${formatBytes(drive.free_bytes)} free`,
    drive.filesystem_type ?? '',
    drive.device ?? '',
  ].filter(Boolean)
  return <><dt className={styles.mountPoint}>{drive.mount_point}</dt><dd>{words.join(' · ')}</dd></>
}

function BatteryFacts({ battery }: { battery: MachineBattery }) {
  const words = [
    batteryStateText(battery.state),
    batteryTimeText(battery) ?? '',
    battery.health_percent !== undefined ? `health ${formatPercent(battery.health_percent)} of design capacity` : '',
    battery.cycle_count !== undefined ? `${battery.cycle_count} cycles` : '',
  ].filter(Boolean)
  return <><dt>{battery.name}</dt><dd>{words.join(' · ')}</dd></>
}

function MachineHistory({ serverConfig, machineID }: { serverConfig: BridgeConfig; machineID: string }) {
  const [range, setRange] = useState<MachineStatsHistoryRange>(MACHINE_STATS_HISTORY_RANGES[1])
  const { answer, loading, error } = useMachineStatsHistory(serverConfig, machineID, range)

  return (
    <div className={styles.historyBody}>
      <div className={`${styles.rangeRow} bc-machine-range`} role="group" aria-label="History range">
        {MACHINE_STATS_HISTORY_RANGES.map(option => (
          <button
            key={option.key}
            type="button"
            className={`${styles.button} ${option.key === range.key ? styles.buttonSelected : ''} bc-machine-button`}
            aria-pressed={option.key === range.key}
            onClick={() => setRange(option)}
          >
            {option.label}
          </button>
        ))}
        {loading ? <span className={styles.muted}>Loading…</span> : null}
      </div>
      {error ? <p className={styles.error}>{error}</p> : null}
      {answer?.state === 'not reported' ? <p className={styles.notice}>{MACHINE_STATS_NOT_REPORTED_TEXT}</p> : null}
      {answer?.state === 'reported' ? (
        <div className={loading ? styles.historyRefreshing : undefined}>
          <HistoryCharts history={answer.value} />
        </div>
      ) : null}
    </div>
  )
}

const formatChartPercent = (value: number) => `${Math.round(value)}%`

/** Every chart of one machine's history, on one time axis. */
export function HistoryCharts({ history }: { history: MachineStatsHistory }) {
  const points = history.points ?? []
  const since = Date.parse(history.since)
  const until = Date.parse(history.until)
  const step = history.step_seconds
  if (points.length === 0) return <p className={styles.muted}>No readings in this range.</p>

  // Memory and swap are drawn against their total, so a full axis means full.
  const memoryTotal = largestTotal(points, stats => stats.memory_total_bytes)
  const swapTotal = largestTotal(points, stats => stats.swap_total_bytes)
  const common = { since, until, stepSeconds: step }
  // A runner in WSL reads two machines; its own charts say which is which.
  const hasWSL = historyHasWSL(points)
  const title = (measure: string) => hasWSL ? `Windows ${measure[0].toLowerCase()}${measure.slice(1)}` : measure
  const wslMemoryTotal = hasWSL ? largestTotal(points, stats => stats.wsl?.memory_total_bytes) : 0

  return (
    <>
      <div className={styles.charts}>
        <MachineStatsChart
          title={hasWSL ? 'Windows CPU in use' : 'CPU in use'}
          hue="cpu"
          points={cpuAverageSeries(points)}
          bandTops={cpuMaximumSeries(points)}
          lineName="average"
          bandName="peak"
          yMaximum={100}
          formatValue={formatChartPercent}
          {...common}
        />
        {historyGPUs(points).flatMap(gpu => {
          const memoryTotalOfGPU = largestGPUMemoryTotal(points, gpu)
          const memorySeries = gpuMemoryUsedSeries(points, gpu)
          const memoryAxis = memoryTotalOfGPU || Math.max(0, ...memorySeries.map(point => point.value ?? 0))
          const usageSeries = gpuUsageSeries(points, gpu)
          return [
            usageSeries.some(point => point.value !== null)
              ? <MachineStatsChart key={`gpu-${gpu.index}`} title={`${gpu.name} in use`} hue="gpu" points={usageSeries} yMaximum={100} formatValue={formatChartPercent} {...common} />
              : null,
            memoryAxis > 0
              ? <MachineStatsChart key={`gpu-memory-${gpu.index}`} title={`${gpu.name} memory`} hue="gpu" points={memorySeries} yMaximum={memoryAxis} formatValue={formatBytes} {...common} />
              : null,
          ]
        })}
        <MachineStatsChart title={title('Memory in use')} hue="memory" points={memoryUsedSeries(points)} yMaximum={memoryTotal || 1} formatValue={formatBytes} {...common} />
        {swapTotal > 0 ? (
          <MachineStatsChart title={title('Swap in use')} hue="swap" points={swapUsedSeries(points)} yMaximum={swapTotal} formatValue={formatBytes} {...common} />
        ) : null}
        {hasWSL ? <MachineStatsChart title="WSL CPU in use" hue="cpu" points={wslCPUSeries(points)} yMaximum={100} formatValue={formatChartPercent} {...common} /> : null}
        {wslMemoryTotal > 0 ? (
          <MachineStatsChart title="WSL memory in use" hue="memory" points={wslMemoryUsedSeries(points)} yMaximum={wslMemoryTotal} formatValue={formatBytes} {...common} />
        ) : null}
        {historyDriveMountPoints(points).map(mountPoint => (
          <MachineStatsChart key={`drive-${mountPoint}`} title={`${mountPoint} used`} hue="drive" points={driveUsedPercentSeries(points, mountPoint)} yMaximum={100} formatValue={formatChartPercent} {...common} />
        ))}
        {historyBatteryNames(points).map(name => (
          <MachineStatsChart key={`battery-${name}`} title={`Battery ${name} charge`} hue="battery" points={batteryChargeSeries(points, name)} yMaximum={100} formatValue={formatChartPercent} {...common} />
        ))}
      </div>
      <p className={styles.muted}>
        {points.length} buckets of {formatDurationSeconds(step)}; each shows the newest reading in it, and {hasWSL ? 'the Windows CPU' : 'CPU'} also its average and peak.
      </p>
      <HistoryTable history={history} />
    </>
  )
}

/** The same numbers as a table, so no value needs the pointer to be read. */
function HistoryTable({ history }: { history: MachineStatsHistory }) {
  const rangeSeconds = (Date.parse(history.until) - Date.parse(history.since)) / 1000
  const mountPoints = historyDriveMountPoints(history.points)
  const gpus = historyGPUs(history.points)
  const hasWSL = historyHasWSL(history.points)
  return (
    <details className={styles.tableDetails}>
      <summary>Show as a table</summary>
      <div className={styles.tableScroll}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Time</th><th>Readings</th><th>CPU average</th><th>CPU peak</th><th>Memory</th><th>Swap</th>
              {gpus.map(gpu => <th key={`gpu-${gpu.index}`}>{gpu.name}</th>)}
              {hasWSL ? <><th>WSL CPU</th><th>WSL memory</th></> : null}
              {mountPoints.map(mountPoint => <th key={mountPoint}>{mountPoint}</th>)}
            </tr>
          </thead>
          <tbody>
            {[...history.points].reverse().map(point => (
              <tr key={point.at}>
                <td>{formatChartTime(Date.parse(point.at), rangeSeconds)}</td>
                <td>{point.sample_count}</td>
                <td>{formatPercent(point.cpu_usage_percent_average)}</td>
                <td>{formatPercent(point.cpu_usage_percent_maximum)}</td>
                <td>{point.stats.memory_total_bytes ? formatBytes(point.stats.memory_used_bytes ?? 0) : '—'}</td>
                <td>{point.stats.swap_total_bytes ? formatBytes(point.stats.swap_used_bytes ?? 0) : '—'}</td>
                {gpus.map(gpu => <td key={`gpu-${gpu.index}`}>{formatPercent(historyGPUAt(point, gpu)?.usage_percent)}</td>)}
                {hasWSL ? (
                  <>
                    <td>{formatPercent(point.stats.wsl?.cpu_usage_percent)}</td>
                    <td>{point.stats.wsl?.memory_total_bytes ? formatBytes(point.stats.wsl.memory_used_bytes ?? 0) : '—'}</td>
                  </>
                ) : null}
                {mountPoints.map(mountPoint => {
                  const drive = point.stats.drives?.find(candidate => candidate.mount_point === mountPoint)
                  return <td key={mountPoint}>{drive ? formatPercent(percentOf(drive.used_bytes, drive.total_bytes)) : '—'}</td>
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </details>
  )
}
