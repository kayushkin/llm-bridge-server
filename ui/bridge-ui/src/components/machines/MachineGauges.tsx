import type { MachineBattery } from '@kayushkin/llm-bridge-types'
import {
  USAGE_DANGER_PERCENT,
  USAGE_WARNING_PERCENT,
  batteryLevel,
  batteryStateText,
  formatBytes,
  formatPercent,
  usageLevel,
  type MemoryBreakdown,
  type MemorySegmentKind,
  type UsageLevel,
} from '../../machineStats'
import styles from './BridgeMachines.module.css'

/**
 * The marks the Machines page draws a reading with, one shape per measure so
 * a card reads at a glance: a round dial for CPU and GPU, one segmented bar
 * for memory, thin bars for swap, pressure and commit charge, thick bars for
 * drives, and a battery that fills. Each takes its hue from a CSS variable
 * (`--machine-hue-<measure>`) and turns to the warning or danger color, with
 * stripes and a word, at the shared thresholds, so no level is told by color
 * alone.
 */

export type MachineHue = 'cpu' | 'gpu' | 'memory' | 'swap' | 'drive' | 'battery'

/** Said beside a share that reached a threshold. */
const LEVEL_WORD: Record<UsageLevel, string> = {
  normal: '',
  warning: 'high',
  danger: 'critical',
}

export function LevelWord({ level, word }: { level: UsageLevel; word?: string }) {
  const text = word ?? LEVEL_WORD[level]
  if (level === 'normal' || !text) return null
  return <span className={styles.levelWord} data-level={level}>{text}</span>
}

/** A picture the zine skin paints, by name; nothing in the other themes.
 *  The hook is `.bc-machine-picture[data-picture="<name>"]`, drawn in
 *  zine.css from `zine/machine-<name>.png`. */
export type MachinePictureName =
  | 'cpu' | 'gpu' | 'memory' | 'drive' | 'battery'
  | 'server' | 'desktop' | 'laptop'
  | 'crab-waiting'

/** `small` is the compact card's size: 32px for a machine's kind, 20px for
 *  a measure's icon. */
export function MachinePicture({ name, size }: { name: MachinePictureName; size?: 'small' }) {
  return <span className={`${styles.picture} bc-machine-picture`} data-picture={name} data-size={size} aria-hidden="true" />
}

// ── The dial ────────────────────────────────────────────────────────────────

const DIAL_CENTER_X = 60
const DIAL_CENTER_Y = 58
const DIAL_RADIUS = 44
/** The dial's arc opens at the bottom: it starts at the lower left and sweeps
 *  clockwise over the top to the lower right. */
const DIAL_START_DEGREES = 150
const DIAL_SWEEP_DEGREES = 240

function dialPoint(degrees: number, radius: number): [number, number] {
  const radians = (degrees * Math.PI) / 180
  return [DIAL_CENTER_X + radius * Math.cos(radians), DIAL_CENTER_Y + radius * Math.sin(radians)]
}

/** The SVG path of the dial's arc between two shares, or '' when empty. */
function dialArcPath(fromPercent: number, toPercent: number): string {
  const from = Math.min(100, Math.max(0, fromPercent)) / 100
  const to = Math.min(100, Math.max(0, toPercent)) / 100
  if (to <= from) return ''
  const sweep = DIAL_SWEEP_DEGREES * (to - from)
  const [startX, startY] = dialPoint(DIAL_START_DEGREES + DIAL_SWEEP_DEGREES * from, DIAL_RADIUS)
  const [endX, endY] = dialPoint(DIAL_START_DEGREES + DIAL_SWEEP_DEGREES * to, DIAL_RADIUS)
  const largeArc = sweep > 180 ? 1 : 0
  return `M${startX.toFixed(2)},${startY.toFixed(2)} A${DIAL_RADIUS},${DIAL_RADIUS} 0 ${largeArc} 1 ${endX.toFixed(2)},${endY.toFixed(2)}`
}

/** A round gauge about 48px across: a track whose last stretch is tinted
 *  where the warning and danger thresholds begin, and the share in the
 *  measure's hue over it. Its number sits beside it, drawn by the caller. */
export function Dial({ percent, label, hue }: {
  percent: number
  label: string
  hue: MachineHue
}) {
  const level = usageLevel(percent)
  const track = dialArcPath(0, 100)
  const value = dialArcPath(0, percent)
  return (
    <div
      className={`${styles.dial} bc-machine-dial`}
      data-hue={hue}
      data-level={level}
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(percent)}
      aria-valuetext={`${formatPercent(percent)}${LEVEL_WORD[level] ? `, ${LEVEL_WORD[level]}` : ''}`}
    >
      <svg className={styles.dialSvg} viewBox="0 0 120 100" aria-hidden="true">
        {/* the edge sits under the track, a little wider: a theme that wants an
            outlined dial colors it, and the default leaves it clear */}
        <path className={styles.dialEdge} data-arc="edge" d={track} />
        <path className={styles.dialTrack} data-arc="track" d={track} />
        <path className={styles.dialZone} data-arc="warning-zone" d={dialArcPath(USAGE_WARNING_PERCENT, USAGE_DANGER_PERCENT)} />
        <path className={styles.dialZone} data-arc="danger-zone" d={dialArcPath(USAGE_DANGER_PERCENT, 100)} />
        {value ? <path className={styles.dialValue} data-arc="value" d={value} /> : null}
      </svg>
    </div>
  )
}

// ── Bars ────────────────────────────────────────────────────────────────────

function clampPercent(percent: number): number {
  return Math.min(100, Math.max(0, percent))
}

/** A bar with one fill: a drive, swap, pressure or commit charge. */
export function Bar({ percent, label, hue, level }: {
  percent: number
  label: string
  hue: MachineHue
  /** The level when it is not the share's own (a Mac's memory pressure). */
  level?: UsageLevel
}) {
  const shown = level ?? usageLevel(percent)
  return (
    <div
      className={`${styles.bar} bc-machine-bar`}
      data-hue={hue}
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(percent)}
      aria-valuetext={`${formatPercent(percent)}${LEVEL_WORD[shown] ? `, ${LEVEL_WORD[shown]}` : ''}`}
    >
      <div className={styles.barFill} data-level={shown} style={{ width: `${clampPercent(percent)}%` }} />
    </div>
  )
}

/** A labeled thin bar on one line: what it is, the bar, and the numbers. */
export function ThinBarRow({ name, percent, valueText, hue, level, note }: {
  name: string
  percent: number
  valueText: string
  hue: MachineHue
  level?: UsageLevel
  note?: string
}) {
  const shown = level ?? usageLevel(percent)
  return (
    <div className={styles.thinRow}>
      <span className={styles.thinName}>{name}</span>
      <Bar percent={percent} label={name} hue={hue} level={shown} />
      <span className={styles.thinValue}>
        {valueText}
        <LevelWord level={shown} />
      </span>
      {note ? <span className={styles.thinNote}>{note}</span> : null}
    </div>
  )
}

const MEMORY_SEGMENT_WORDS: Record<MemorySegmentKind, string> = {
  used: 'in use',
  compressed: 'compressed',
  cached: 'file cache',
  free: 'free',
}

function segmentLevel(kind: MemorySegmentKind, level: UsageLevel): UsageLevel {
  return kind === 'used' || kind === 'compressed' ? level : 'normal'
}

/** Memory as one bar split into what programs hold, the compressor's share,
 *  file cache and free; `MemoryKey` names each part and its size. */
export function MemoryBar({ breakdown, level }: { breakdown: MemoryBreakdown; level: UsageLevel }) {
  const usedPercent = breakdown.usedPercent
  return (
    <div
      className={`${styles.memoryBar} bc-machine-memory-bar`}
      role="meter"
      aria-label="Memory in use"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(usedPercent)}
      aria-valuetext={`${formatPercent(usedPercent)} in use${LEVEL_WORD[level] ? `, ${LEVEL_WORD[level]}` : ''}`}
    >
      {breakdown.segments.map(segment => (
        <div
          key={segment.kind}
          className={styles.memorySegment}
          data-segment={segment.kind}
          data-level={segmentLevel(segment.kind, level)}
          style={{ flexGrow: segment.percent }}
          title={`${MEMORY_SEGMENT_WORDS[segment.kind]}: ${formatBytes(segment.bytes)}`}
        />
      ))}
    </div>
  )
}

export function MemoryKey({ breakdown, level }: { breakdown: MemoryBreakdown; level: UsageLevel }) {
  return (
    <ul className={styles.memoryKey}>
      {breakdown.segments.map(segment => (
        <li key={segment.kind}>
          <span className={styles.memorySwatch} data-segment={segment.kind} data-level={segmentLevel(segment.kind, level)} aria-hidden="true" />
          {MEMORY_SEGMENT_WORDS[segment.kind]} <strong>{formatBytes(segment.bytes)}</strong>
        </li>
      ))}
    </ul>
  )
}

// ── The battery ─────────────────────────────────────────────────────────────

/** A battery drawn as one: a body that fills to its charge, in the warning or
 *  danger color when it runs low, and a bolt across it while it charges. */
export function BatteryGlyph({ battery }: { battery: Pick<MachineBattery, 'charge_percent' | 'state' | 'name'> }) {
  const level = batteryLevel(battery)
  const share = clampPercent(battery.charge_percent) / 100
  const charging = battery.state === 'charging'
  return (
    <div
      className={`${styles.battery} bc-machine-battery`}
      data-level={level}
      role="meter"
      aria-label={`Battery ${battery.name} charge`}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(battery.charge_percent)}
      aria-valuetext={`${formatPercent(battery.charge_percent)}, ${batteryStateText(battery.state)}`}
    >
      <svg className={styles.batterySvg} viewBox="0 0 68 32" aria-hidden="true">
        <rect className={styles.batteryBody} data-part="body" x={2} y={2} width={57} height={28} rx={6} />
        <rect className={styles.batteryNub} data-part="nub" x={61} y={10} width={5.5} height={12} rx={2} />
        {share > 0 ? <rect className={styles.batteryFill} data-part="fill" x={5} y={5} width={51 * share} height={22} rx={3} /> : null}
        {charging ? <path className={styles.batteryBolt} data-part="bolt" d="M33 3 L21 18 H29 L26 29 L39 13 H31 Z" /> : null}
      </svg>
    </div>
  )
}
