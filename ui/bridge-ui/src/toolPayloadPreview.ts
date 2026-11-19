// How much of a tool's text reaches the page, and how a shortened payload is described.
//
// Two different shortenings meet here, and they must not be confused:
//
//   - the SERVER's: a reading page carries each tool string cut to 2 KB
//     (`toolResultTruncated` / `toolInputTruncated` on the entry). The rest is not in the
//     browser at all and has to be fetched (`useFullEntry`).
//   - the PAGE's: text that IS in the browser but is too long to hand the DOM in one go.
//     A single 586 KB grep result written into one element is what the Raw view and the
//     fallback tool card used to do. That text is capped here and shown in full on request.

/** The most characters of one tool text handed to the DOM before the reader asks for more. */
export const DOM_TEXT_LIMIT = 20_000

/** The shown part of a text and how much was held back. */
export interface CappedText {
  shown: string
  hiddenCharacters: number
}

export function capText(text: string, limit = DOM_TEXT_LIMIT): CappedText {
  if (text.length <= limit) return { shown: text, hiddenCharacters: 0 }
  return { shown: text.slice(0, limit), hiddenCharacters: text.length - limit }
}

/** A byte count as a person reads it: 900 B, 2.0 KB, 586 KB, 1.3 MB. */
export function formatBytes(bytes: number): string {
  if (bytes < 1000) return `${bytes} B`
  if (bytes < 10_000) return `${(bytes / 1000).toFixed(1)} KB`
  if (bytes < 1_000_000) return `${Math.round(bytes / 1000)} KB`
  return `${(bytes / 1_000_000).toFixed(1)} MB`
}

/** What a shortened entry still holds back, for the "load full" line. Null when the
 *  entry carries its whole payload. */
export function shortenedPayloadLabel(entry: {
  toolInputTruncated?: boolean
  toolInputBytes?: number
  toolResultTruncated?: boolean
  toolResultBytes?: number
}): string | null {
  const parts: string[] = []
  if (entry.toolInputTruncated) {
    parts.push(`input ${entry.toolInputBytes !== undefined ? formatBytes(entry.toolInputBytes) : '(size unknown)'}`)
  }
  if (entry.toolResultTruncated) {
    parts.push(`output ${entry.toolResultBytes !== undefined ? formatBytes(entry.toolResultBytes) : '(size unknown)'}`)
  }
  if (parts.length === 0) return null
  return `Preview only — full ${parts.join(', ')}`
}
