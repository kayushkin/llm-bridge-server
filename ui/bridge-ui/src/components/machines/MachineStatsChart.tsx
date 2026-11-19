import { useState, type KeyboardEvent, type PointerEvent } from 'react'
import { chartRuns, formatChartTime, nearestPointIndex, type ChartPoint } from '../../machineStats'
import type { MachineHue } from './MachineGauges'
import styles from './BridgeMachines.module.css'

/** The width of the drawing space. The SVG stretches it to the chart's width,
 *  with strokes that keep their pixel width (`vector-effect`), so no text is
 *  drawn inside it: the axis labels and readout are HTML around it. */
const VIEW_WIDTH = 1000
const VIEW_HEIGHT = 100

export interface MachineStatsChartProps {
  title: string
  /** Which measure it is, for its color: the same hue as the measure's mark
   *  on the card. */
  hue: MachineHue
  /** The line, one point per history bucket. */
  points: readonly ChartPoint[]
  /** A faint band from the line up to these values, at the same times: the
   *  bucket's peak above its average. */
  bandTops?: readonly ChartPoint[]
  lineName?: string
  bandName?: string
  /** The time range the x axis covers, in ms since the epoch. */
  since: number
  until: number
  stepSeconds: number
  yMaximum: number
  formatValue: (value: number) => string
}

/** One measure over time: a 2px line, an optional faint band, three hairline
 *  gridlines, and a crosshair that snaps to the nearest bucket under the
 *  pointer or the arrow keys and reads out its time and values. */
export function MachineStatsChart({
  title, hue, points, bandTops, lineName, bandName, since, until, stepSeconds, yMaximum, formatValue,
}: MachineStatsChartProps) {
  const [hoverIndex, setHoverIndex] = useState<number | null>(null)
  const span = Math.max(1, until - since)
  const rangeSeconds = span / 1000
  const x = (at: number) => ((at - since) / span) * VIEW_WIDTH
  const y = (value: number) => VIEW_HEIGHT - (Math.min(Math.max(value, 0), yMaximum) / yMaximum) * VIEW_HEIGHT

  const linePaths = chartRuns(points, stepSeconds).map(run =>
    run.map((point, i) => `${i === 0 ? 'M' : 'L'}${x(point.at).toFixed(1)},${y(point.value).toFixed(1)}`).join(''))

  const bandPaths: string[] = []
  if (bandTops) {
    const lineAt = new Map(points.map(point => [point.at, point.value]))
    const paired = bandTops.map(top => {
      const bottom = lineAt.get(top.at)
      return { at: top.at, value: top.value !== null && bottom !== null && bottom !== undefined ? top.value : null }
    })
    for (const run of chartRuns(paired, stepSeconds)) {
      const upper = run.map(point => `${x(point.at).toFixed(1)},${y(point.value).toFixed(1)}`)
      const lower = [...run].reverse().map(point => `${x(point.at).toFixed(1)},${y(lineAt.get(point.at) ?? 0).toFixed(1)}`)
      bandPaths.push(`M${upper.join('L')}L${lower.join('L')}Z`)
    }
  }

  const latestIndex = (() => {
    for (let i = points.length - 1; i >= 0; i--) if (points[i].value !== null) return i
    return -1
  })()
  const shownIndex = hoverIndex ?? latestIndex
  const shown = shownIndex >= 0 ? points[shownIndex] : null
  const shownBand = shown && bandTops ? bandTops.find(top => top.at === shown.at)?.value ?? null : null

  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const box = event.currentTarget.getBoundingClientRect()
    if (box.width <= 0) return
    const at = since + ((event.clientX - box.left) / box.width) * span
    const index = nearestPointIndex(points, at)
    setHoverIndex(index >= 0 ? index : null)
  }
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!points.length) return
    const from = hoverIndex ?? latestIndex
    if (event.key === 'ArrowLeft') { setHoverIndex(Math.max(0, from - 1)); event.preventDefault() }
    if (event.key === 'ArrowRight') { setHoverIndex(Math.min(points.length - 1, from + 1)); event.preventDefault() }
    if (event.key === 'Escape') setHoverIndex(null)
  }

  const crosshairLeft = hoverIndex !== null && points[hoverIndex] ? (x(points[hoverIndex].at) / VIEW_WIDTH) * 100 : null
  const dotTop = hoverIndex !== null && points[hoverIndex]?.value !== null && points[hoverIndex]
    ? (y(points[hoverIndex].value as number) / VIEW_HEIGHT) * 100
    : null

  return (
    <figure className={`${styles.chart} bc-machine-chart`} data-hue={hue}>
      <figcaption className={styles.chartCaption}>
        <span className={`${styles.chartTitle} bc-machine-chart-title`}>{title}</span>
        <span className={styles.chartReadout} aria-live="polite">
          {shown && shown.value !== null ? (
            <>
              <strong>{formatValue(shown.value)}</strong>
              {lineName ? <span className={styles.muted}> {lineName}</span> : null}
              {shownBand !== null ? <> · <strong>{formatValue(shownBand)}</strong><span className={styles.muted}> {bandName}</span></> : null}
              <span className={styles.muted}> · {formatChartTime(shown.at, rangeSeconds)}{hoverIndex === null ? ' (latest)' : ''}</span>
            </>
          ) : <span className={styles.muted}>no readings in this range</span>}
        </span>
      </figcaption>
      {bandTops && lineName && bandName ? (
        <div className={styles.chartLegend}>
          <span><span className={styles.legendLine} aria-hidden="true" /> {lineName}</span>
          <span><span className={styles.legendBand} aria-hidden="true" /> {bandName}</span>
        </div>
      ) : null}
      <div className={styles.chartFrame}>
        <div className={styles.chartAxisY} aria-hidden="true">
          <span>{formatValue(yMaximum)}</span>
          <span>{formatValue(yMaximum / 2)}</span>
          <span>{formatValue(0)}</span>
        </div>
        <div
          className={styles.chartPlot}
          tabIndex={0}
          role="img"
          aria-label={`${title}, ${formatChartTime(since, rangeSeconds)} to ${formatChartTime(until, rangeSeconds)}. Arrow keys step through the readings.`}
          onPointerMove={onPointerMove}
          onPointerLeave={() => setHoverIndex(null)}
          onKeyDown={onKeyDown}
          onBlur={() => setHoverIndex(null)}
        >
          <svg className={styles.chartSvg} viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`} preserveAspectRatio="none" aria-hidden="true">
            {[0, VIEW_HEIGHT / 2, VIEW_HEIGHT].map(gridY => (
              <line key={gridY} className={styles.chartGrid} x1={0} x2={VIEW_WIDTH} y1={gridY} y2={gridY} vectorEffect="non-scaling-stroke" />
            ))}
            {bandPaths.map((d, i) => <path key={`band-${i}`} className={styles.chartBand} d={d} />)}
            {linePaths.map((d, i) => <path key={`line-${i}`} className={styles.chartLine} d={d} vectorEffect="non-scaling-stroke" />)}
          </svg>
          {crosshairLeft !== null ? <div className={styles.chartCrosshair} style={{ left: `${crosshairLeft}%` }} /> : null}
          {crosshairLeft !== null && dotTop !== null ? <div className={styles.chartDot} style={{ left: `${crosshairLeft}%`, top: `${dotTop}%` }} /> : null}
        </div>
      </div>
      <div className={styles.chartAxisX} aria-hidden="true">
        <span>{formatChartTime(since, rangeSeconds)}</span>
        <span>{formatChartTime(since + span / 2, rangeSeconds)}</span>
        <span>{formatChartTime(until, rangeSeconds)}</span>
      </div>
    </figure>
  )
}
