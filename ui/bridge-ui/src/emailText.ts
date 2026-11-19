/**
 * Makes a raw email body readable in a preview pane.
 *
 * Bulk mail is mostly not words. Measured on a live Meetup message from this
 * host: `body_text` is 24,924 characters, opening with ~90 blank lines and then
 * a single ~400-character click-tracking URL. Rendered raw, the preview shows
 * whitespace and then a wall of tracking parameters — which is worse than
 * showing nothing, because it looks like the email had no content.
 *
 * This is deliberately NOT the same transform as the classifier's Go cleaner.
 * That one collapses everything to a single line, because a model reads a flat
 * string. A person reads paragraphs, so line structure is preserved here and
 * only runs of blank lines are collapsed.
 *
 * It is also not a sanitiser and must never be treated as one. The preview
 * renders plain text through React, which escapes it; HTML bodies go to the
 * mail page, which sandboxes them in an iframe.
 */

/** Zero-width and padding characters senders use to pad the preview line. */
const INVISIBLE = /[\u00AD\u034F\u200B-\u200F\u2060-\u2064\uFEFF]/g

const URL_PATTERN = /https?:\/\/[^\s<>"')\]]+/g

/** Reduces a URL to its host: a tracking path encodes the campaign, not meaning. */
export function shortenUrlForPreview(raw: string): string {
  const trimmed = raw.replace(/^https?:\/\//, '')
  const host = trimmed.split(/[/?#]/)[0].replace(/\.$/, '')
  return host ? `[${host}]` : ''
}

export function cleanEmailBodyForPreview(raw: string | undefined | null): string {
  if (!raw) return ''
  let s = raw.replace(INVISIBLE, '')
  s = s.replace(URL_PATTERN, shortenUrlForPreview)
  // Blank out lines that hold only whitespace. [^\S\n] is "whitespace that is
  // not a newline", which matters because these lines are padded with U+00A0
  // non-breaking spaces, not plain spaces — a /[ \t]+$/ pass leaves all ~90 of
  // them standing and the collapse below then finds nothing to collapse.
  s = s.replace(/[^\S\n]+$/gm, '')
  // Three or more newlines collapse to a paragraph break. Keeps structure while
  // removing the ~90 blank lines that open a Meetup message.
  s = s.replace(/\n{3,}/g, '\n\n')
  return s.trim()
}
