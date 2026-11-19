import type { TokenUsage } from '../../types'
import { formatTokens } from '../../utils'

export interface UsageLineProps {
  usage: TokenUsage
}

export function UsageLine({ usage }: UsageLineProps) {
  const parts: string[] = []
  if (usage.input_tokens) parts.push(`in ${formatTokens(usage.input_tokens)}`)
  if (usage.output_tokens) parts.push(`out ${formatTokens(usage.output_tokens)}`)
  if (usage.cache_read_tokens) parts.push(`cache-read ${formatTokens(usage.cache_read_tokens)}`)
  if (usage.cache_write_tokens) parts.push(`cache-write ${formatTokens(usage.cache_write_tokens)}`)
  if (parts.length === 0) return null
  return <div className="bc-row-usage">{parts.join(' · ')}</div>
}
