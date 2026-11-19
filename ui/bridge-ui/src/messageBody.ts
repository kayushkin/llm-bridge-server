// How a chat message's body is shown: which renderer a message gets, Matrix
// HTML cut down to a safe tree, `mxc://` media turned into multichat's media
// route, and Discord's inline tokens (custom emoji, channel and user
// mentions) turned into what a reader expects to see.
//
// The Matrix HTML sanitizer is an allowlist walker over a parsed document,
// not DOMPurify: it returns a plain tree (`SafeNode`) that the page turns into
// React elements, so no string of message HTML ever reaches innerHTML, and the
// rules that are ours rather than generic HTML hygiene — mxc rewriting,
// dropping reply fallbacks, custom emoji, Discord channel tokens left in the
// text — live in the same one pass. It adds nothing to the bundle. The parse
// itself is the browser's (`DOMParser`, which builds an inert document: no
// script runs and no image loads), handed in so a test can pass its own.

/** Matrix's rich-text format, `format` on a message whose `formatted_body` is HTML. */
export const MATRIX_HTML_FORMAT = 'org.matrix.custom.html'
/** multichat's format for a Discord archive row: `body` is Discord's own markdown. */
export const DISCORD_MARKDOWN_FORMAT = 'discord-markdown'

export type MessageBodyKind = 'matrix-html' | 'discord-markdown' | 'plain'

/** Which renderer a message's text gets. HTML with nothing in it falls back
 *  to nothing: the plain `body` is shown, which Matrix requires every
 *  formatted message to carry. */
export function messageBodyKind(message: { format?: string; formatted_body?: string }): MessageBodyKind {
  if (message.format === MATRIX_HTML_FORMAT && message.formatted_body) return 'matrix-html'
  if (message.format === DISCORD_MARKDOWN_FORMAT) return 'discord-markdown'
  return 'plain'
}

// ---- mxc:// media ----------------------------------------------------------

/** A server name (host, IPv4, bracketed IPv6, optional port) and a media id
 *  (Matrix spec: `[A-Za-z0-9_-]+`). Anything else is not a URI we fetch. */
const MXC_PATTERN = /^mxc:\/\/([A-Za-z0-9.\-:[\]]+)\/([A-Za-z0-9_-]+)$/

export interface MxcParts {
  serverName: string
  mediaID: string
}

export function parseMxc(uri: string): MxcParts | null {
  const match = MXC_PATTERN.exec(uri.trim())
  return match ? { serverName: match[1], mediaID: match[2] } : null
}

/** multichat's route for an `mxc://S/I` URI: `{base}/media/S/I`, with
 *  `?width=&height=` for a thumbnail. null for anything that is not a valid
 *  mxc URI, so a caller drops the image rather than loading some other URL. */
export function mediaPathOfMxc(multichatBasePath: string, uri: string, thumbnail?: { width: number; height: number }): string | null {
  const parts = parseMxc(uri)
  if (!parts) return null
  const path = `${multichatBasePath}/media/${encodeURIComponent(parts.serverName)}/${encodeURIComponent(parts.mediaID)}`
  if (!thumbnail) return path
  return `${path}?${new URLSearchParams({ width: String(thumbnail.width), height: String(thumbnail.height) }).toString()}`
}

/** A message whose `media_url` is an image the page can show inline. */
export function isImageMedia(message: { media_url?: string; media_mimetype?: string }): boolean {
  return !!message.media_url && !!message.media_mimetype?.startsWith('image/')
}

/** The size asked of the thumbnail route; the page scales it down to fit. */
export const MESSAGE_IMAGE_THUMBNAIL = { width: 480, height: 360 } as const

// ---- Discord custom emoji and inline tokens ---------------------------------

/** Discord's public CDN address for a custom emoji: `.gif` when animated. */
export function discordEmojiURL(emojiID: string, animated: boolean): string {
  return `https://cdn.discordapp.com/emojis/${encodeURIComponent(emojiID)}.${animated ? 'gif' : 'webp'}?size=48`
}

/** What the renderers know about names on Discord, joined by id. Channel names
 *  come from `GET /discord/status`; user names from the display names the log
 *  holds for its senders. An id with no name here is shown as the id. */
export interface DiscordNames {
  channelNames: ReadonlyMap<string, string>
  userNames: ReadonlyMap<string, string>
}

export const NO_DISCORD_NAMES: DiscordNames = { channelNames: new Map(), userNames: new Map() }

export type InlineSegment =
  | { kind: 'text'; text: string }
  | { kind: 'emoji'; src: string; alt: string }
  | { kind: 'mention'; label: string; title: string }

// <:name:id> <a:name:id> <#id> <@id> <@!id> <@&id> <t:unix> <t:unix:style>
const DISCORD_TOKEN = /<(a?):(\w{1,64}):(\d{1,25})>|<#(\d{1,25})>|<@(!?|&)(\d{1,25})>|<t:(-?\d{1,13})(?::([tTdDfFR]))?>/g

/** Splits text on Discord's inline tokens: custom emoji become images, channel
 *  and user mentions `#name` / `@name`, a timestamp the reader's local time.
 *  Text around them is returned as it was. */
export function discordInlineSegments(text: string, names: DiscordNames): InlineSegment[] {
  const segments: InlineSegment[] = []
  let last = 0
  for (const match of text.matchAll(DISCORD_TOKEN)) {
    const at = match.index ?? 0
    if (at > last) segments.push({ kind: 'text', text: text.slice(last, at) })
    last = at + match[0].length
    const [, animated, emojiName, emojiID, channelID, userKind, userID, unixSeconds, timeStyle] = match
    if (emojiID) {
      segments.push({ kind: 'emoji', src: discordEmojiURL(emojiID, animated === 'a'), alt: `:${emojiName}:` })
    } else if (channelID) {
      const name = names.channelNames.get(channelID)
      segments.push({ kind: 'mention', label: `#${name ?? channelID}`, title: `Discord channel ${channelID}` })
    } else if (userID) {
      if (userKind === '&') {
        segments.push({ kind: 'mention', label: '@role', title: `Discord role ${userID}` })
      } else {
        const name = names.userNames.get(userID)
        segments.push({ kind: 'mention', label: `@${name ?? userID}`, title: `Discord user ${userID}` })
      }
    } else if (unixSeconds) {
      const when = new Date(Number(unixSeconds) * 1000)
      segments.push({ kind: 'mention', label: discordTimestampLabel(when, timeStyle ?? 'f'), title: when.toISOString() })
    }
  }
  if (last < text.length) segments.push({ kind: 'text', text: text.slice(last) })
  return segments
}

function discordTimestampLabel(when: Date, style: string): string {
  switch (style) {
    case 't': return when.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
    case 'T': return when.toLocaleTimeString()
    case 'd': return when.toLocaleDateString()
    case 'D': return when.toLocaleDateString(undefined, { dateStyle: 'long' })
    case 'F': return when.toLocaleString(undefined, { dateStyle: 'full', timeStyle: 'short' })
    case 'R': return relativeTimeLabel(when)
    default: return when.toLocaleString(undefined, { dateStyle: 'long', timeStyle: 'short' })
  }
}

function relativeTimeLabel(when: Date): string {
  const seconds = Math.round((when.getTime() - Date.now()) / 1000)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [['year', 31536000], ['month', 2592000], ['day', 86400], ['hour', 3600], ['minute', 60]]
  const format = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })
  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size) return format.format(Math.round(seconds / size), unit)
  }
  return format.format(seconds, 'second')
}

/** The Discord user id in a mautrix-discord puppet's Matrix id,
 *  `@discord_<id>:server`; null for any other user. */
export function discordUserIDOfMatrixUser(matrixUserID: string): string | null {
  const match = /^@discord_(\d+):/.exec(matrixUserID)
  return match ? match[1] : null
}

/** Names for the Discord tokens in a set of messages: channels by their
 *  Discord id from the bridge status, users from the senders the messages
 *  themselves name. */
export function discordNamesOf(
  channels: readonly { discord_channel_id: string; name: string }[],
  senders: readonly { sender_user_id: string; sender_display_name: string }[],
): DiscordNames {
  const channelNames = new Map(channels.map(c => [c.discord_channel_id, c.name]))
  const userNames = new Map<string, string>()
  for (const sender of senders) {
    const id = discordUserIDOfMatrixUser(sender.sender_user_id)
    if (id && sender.sender_display_name && !userNames.has(id)) userNames.set(id, sender.sender_display_name)
  }
  return { channelNames, userNames }
}

// ---- Matrix HTML -----------------------------------------------------------

/** What survives sanitizing: text, an allowed element with allowed attributes,
 *  an inline image (custom emoji, or an `mxc://` image in the HTML), or a
 *  mention. Attributes are plain strings, already checked; the renderer passes
 *  them through. */
export type SafeNode =
  | { kind: 'text'; text: string }
  | { kind: 'element'; tag: string; attributes: Record<string, string>; children: SafeNode[] }
  | { kind: 'image'; src: string; alt: string; title: string; emoticon: boolean; width?: number; height?: number }
  | { kind: 'mention'; label: string; title: string }

/** The tags the Matrix spec lets a client render (m.room.message, "HTML"),
 *  minus `font`, which only carries colors we do not apply, and `img`, which
 *  is handled on its own. */
const ALLOWED_TAGS = new Set([
  'del', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'blockquote', 'p', 'a', 'ul', 'ol', 'sup', 'sub', 'li',
  'b', 'i', 'u', 'strong', 'em', 's', 'code', 'hr', 'br', 'div', 'table', 'thead', 'tbody', 'tr', 'th',
  'td', 'caption', 'pre', 'span', 'details', 'summary',
])

/** Tags dropped together with everything inside them. `mx-reply` is the
 *  quoted fallback of a reply, which the spec says to strip; the rest could
 *  carry script, style or a form whose text means nothing as a message. */
const DROPPED_WITH_CONTENTS = new Set([
  'mx-reply', 'script', 'style', 'noscript', 'template', 'iframe', 'object', 'embed', 'svg', 'math',
  'head', 'title', 'textarea', 'select', 'button', 'form', 'input', 'frame', 'frameset', 'audio', 'video',
])

/** Link schemes a message may point at. */
const ALLOWED_LINK_SCHEMES = new Set(['http:', 'https:', 'mailto:'])

/** The spec's suggested cap on nesting; deeper content is dropped. */
const MAX_DEPTH = 100

/** matrix.to links to a user, the way bridges write mentions. */
const MATRIX_TO_USER = /^https:\/\/matrix\.to\/#\/(@[^/?#]+)/

const ELEMENT_NODE = 1
const TEXT_NODE = 3

export interface SanitizeOptions {
  multichatBasePath: string
  discordNames: DiscordNames
}

/** Walks a parsed document's `body` and keeps only what the allowlist names.
 *  A tag not on it is unwrapped (its text kept), except the ones in
 *  DROPPED_WITH_CONTENTS; every attribute not named below goes, including
 *  every `on*` handler and `style`. */
export function sanitizeMatrixHtml(root: Node, options: SanitizeOptions): SafeNode[] {
  return sanitizeChildren(root, options, 0)
}

function sanitizeChildren(parent: Node, options: SanitizeOptions, depth: number): SafeNode[] {
  if (depth > MAX_DEPTH) return []
  const out: SafeNode[] = []
  // Adjacent text nodes are joined before tokenizing, so a Discord token split
  // across them (a parser may split text at an entity) is still found.
  let text = ''
  for (const child of Array.from(parent.childNodes)) {
    if (child.nodeType === TEXT_NODE) { text += child.textContent ?? ''; continue }
    out.push(...textNodes(text, options.discordNames))
    text = ''
    out.push(...sanitizeNode(child, options, depth))
  }
  out.push(...textNodes(text, options.discordNames))
  return out
}

function sanitizeNode(node: Node, options: SanitizeOptions, depth: number): SafeNode[] {
  if (node.nodeType !== ELEMENT_NODE) return []
  const element = node as Element
  const tag = element.tagName.toLowerCase()
  if (DROPPED_WITH_CONTENTS.has(tag)) return []
  if (tag === 'img') return imageNode(element, options)
  const children = sanitizeChildren(element, options, depth + 1)
  if (!ALLOWED_TAGS.has(tag)) return children
  if (tag === 'a') return linkNodes(element, children)
  const attributes: Record<string, string> = {}
  if (tag === 'ol') {
    const start = element.getAttribute('start')
    if (start && /^\d{1,9}$/.test(start)) attributes.start = start
  }
  if (tag === 'code') {
    const language = element.getAttribute('class')
    if (language && /^language-[\w+-]{1,40}$/.test(language)) attributes.class = language
  }
  if (tag === 'span' && element.hasAttribute('data-mx-spoiler')) attributes['data-mx-spoiler'] = ''
  return [{ kind: 'element', tag, attributes, children }]
}

function textNodes(text: string, names: DiscordNames): SafeNode[] {
  if (!text) return []
  return discordInlineSegments(text, names).map((segment): SafeNode => {
    switch (segment.kind) {
      case 'text': return { kind: 'text', text: segment.text }
      case 'emoji': return { kind: 'image', src: segment.src, alt: segment.alt, title: segment.alt, emoticon: true }
      case 'mention': return segment
    }
  })
}

function imageNode(element: Element, options: SanitizeOptions): SafeNode[] {
  const src = mediaPathOfMxc(options.multichatBasePath, element.getAttribute('src') ?? '')
  if (!src) return []
  const alt = element.getAttribute('alt') ?? ''
  const title = element.getAttribute('title') || alt
  const emoticon = element.hasAttribute('data-mx-emoticon')
  const image: SafeNode = { kind: 'image', src, alt, title, emoticon }
  if (!emoticon) {
    const width = pixelSize(element.getAttribute('width'))
    const height = pixelSize(element.getAttribute('height'))
    if (width) image.width = width
    if (height) image.height = height
  }
  return [image]
}

function pixelSize(value: string | null): number | undefined {
  if (!value || !/^\d{1,5}$/.test(value)) return undefined
  const size = Number(value)
  return size > 0 ? size : undefined
}

function linkNodes(element: Element, children: SafeNode[]): SafeNode[] {
  const href = (element.getAttribute('href') ?? '').trim()
  const user = MATRIX_TO_USER.exec(href)
  if (user) {
    const label = plainTextOf(children)
    return [{ kind: 'mention', label: label.startsWith('@') ? label : `@${label}`, title: decodeURIComponentSafely(user[1]) }]
  }
  if (!linkIsAllowed(href)) return children
  return [{ kind: 'element', tag: 'a', attributes: { href, target: '_blank', rel: 'noopener noreferrer' }, children }]
}

/** Whether an href is an absolute URL on an allowed scheme. A relative or
 *  unparseable one is refused rather than resolved against this page. */
export function linkIsAllowed(href: string): boolean {
  try {
    return ALLOWED_LINK_SCHEMES.has(new URL(href).protocol)
  } catch {
    return false
  }
}

function decodeURIComponentSafely(value: string): string {
  try { return decodeURIComponent(value) } catch { return value }
}

/** The text a sanitized tree reads as, for a label. */
export function plainTextOf(nodes: readonly SafeNode[]): string {
  return nodes.map(node => {
    switch (node.kind) {
      case 'text': return node.text
      case 'element': return plainTextOf(node.children)
      case 'image': return node.alt
      case 'mention': return node.label
    }
  }).join('')
}
