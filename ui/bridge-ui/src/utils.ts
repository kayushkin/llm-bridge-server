export function formatTokens(n: number): string {
  // Cache reads run to billions on a long session: 2,055,182,536 must not
  // read as "2055.2M".
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return `${n}`
}

export function formatCost(n: number): string {
  // Sub-cent amounts need the extra digits to say anything at all. Exactly
  // zero does not: it is not a small quantity, it is none, and "$0.0000"
  // reads as a measurement precise to four places rather than as nothing
  // spent.
  if (n > 0 && n < 0.01) return `$${n.toFixed(4)}`
  return `$${n.toFixed(2)}`
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  const rem = Math.round(s % 60)
  return `${m}m${rem}s`
}

// formatAgeCompact renders an elapsed time short enough to sit in a badge:
// "12m", "5h", "23d". timeAgo below says the same thing in prose, which is
// right in a sentence and too wide in a corner.
//
// Returns null rather than a placeholder when the timestamp cannot be read, so
// the caller renders nothing at all. A badge reading "NaN" or "0m" would be a
// measurement the data never supported.
export function formatAgeCompact(iso: string): string | null {
  const then = new Date(iso).getTime()
  if (!Number.isFinite(then)) return null

  const ms = Date.now() - then
  // A future timestamp means clock skew between this box and whatever wrote the
  // row. Counting up from it would print a growing negative age.
  if (ms < 0) return 'now'

  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

export function timeAgo(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const s = Math.floor(diff / 1000)
  if (s < 60) return 'just now'
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  return `${d}d ago`
}

// formatDurationCompact renders a length of time short enough for a badge:
// "45s", "12m", "1.5h", "3d". Hours carry one decimal below ten because the
// difference between one hour and ninety minutes is the difference between
// inside a two-hour limit and nearly out of it, and "1h" hides that.
//
// Returns null for a duration that cannot be read, so the caller renders nothing
// rather than a badge reading NaN.
export function formatDurationCompact(seconds: number | null | undefined): string | null {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return null
  const s = Math.max(0, seconds)
  if (s < 60) return `${Math.round(s)}s`
  const m = s / 60
  if (m < 60) return `${Math.round(m)}m`
  const h = m / 60
  if (h < 10) return `${(Math.round(h * 10) / 10).toString().replace(/\.0$/, '')}h`
  if (h < 24) return `${Math.round(h)}h`
  const d = h / 24
  if (d < 10) return `${(Math.round(d * 10) / 10).toString().replace(/\.0$/, '')}d`
  return `${Math.round(d)}d`
}

// formatDurationProse says the same thing in words, for a tooltip or a sentence
// where a badge's shorthand would read as jargon.
export function formatDurationProse(seconds: number | null | undefined): string | null {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return null
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s} second${s === 1 ? '' : 's'}`
  const m = Math.round(s / 60)
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'}`
  const h = Math.round((s / 3600) * 10) / 10
  if (h < 48) return `${h} hour${h === 1 ? '' : 's'}`
  const d = Math.round((s / 86400) * 10) / 10
  return `${d} day${d === 1 ? '' : 's'}`
}
