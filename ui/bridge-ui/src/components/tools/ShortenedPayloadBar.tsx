import { shortenedPayloadLabel } from '../../toolPayloadPreview'
import type { Entry } from '@kayushkin/chat-core'

/** The line under a tool card whose payload the page carries only as a preview: what is
 *  held back, a button that loads it, and the error if loading failed. Renders nothing
 *  for an entry that carries its whole payload. */
export default function ShortenedPayloadBar({
  entry,
  shortened,
  loading,
  error,
  loadFull,
}: {
  entry: Entry
  shortened: boolean
  loading: boolean
  error: string | null
  loadFull: () => void
}) {
  const label = shortened ? shortenedPayloadLabel(entry) : null
  if (!label && !error) return null
  return (
    <div className="bc-tool-shortened" onClick={(e) => e.stopPropagation()}>
      {label && <span>{label}</span>}
      {label && (
        <button type="button" className="bc-tool-show-all" disabled={loading} onClick={loadFull}>
          {loading ? 'Loading…' : 'Load full'}
        </button>
      )}
      {error && <span className="bc-tool-shortened-error">Could not load the full entry: {error}</span>}
    </div>
  )
}
