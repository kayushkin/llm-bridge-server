// Reads a bridge brief: a message a service, not the person, wrote and sent
// into a session as its user message (llm-bridge msg.BridgeBrief). Its whole
// text is one block:
//
//   <bridge-brief kind="peer-patch" from="llm-bridge-server" title="…" fence="a1f5…">
//   markdown the service wrote
//   --- summary a1f5… from its-skayushkin ---
//   text the service quotes from someone else
//   --- end of summary a1f5… ---
//   </bridge-brief>
//
// The fence is a tag the service drew at random, so the quoted text cannot
// close its own quote: the quote starts at the first line
// "--- <noun> <fence><caption> ---" and ends at the first line after it that
// reads "--- end of <noun> <fence> ---". The quoted text may itself be a brief.

import {
  BridgeBriefKindPeerPatch,
  BridgeBriefKindPeerPlacement,
  BridgeBriefKindPeerRequest,
  BridgeBriefKindSweep,
} from '@kayushkin/llm-bridge-types'

export interface BridgeBriefQuote {
  /** The opening line's words, without the fence: "summary from its-skayushkin". */
  caption: string
  text: string
}

export interface BridgeBrief {
  kind: string
  from: string
  title: string
  fence: string
  /** The service's markdown before the quote; the whole body when there is none. */
  lead: string
  quote: BridgeBriefQuote | null
  /** The service's markdown after the quote. */
  trailer: string
}

const openingLine = /^<bridge-brief((?: [a-z]+="[^"]*")*)>$/
const attribute = / ([a-z]+)="([^"]*)"/g
const closingLine = '</bridge-brief>'

function unescapeAttribute(value: string): string {
  return value.replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&')
}

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/** The brief a message is, or null when it is not one. Anchored at both ends,
 *  so a message that only quotes a brief is not taken for one. */
export function parseBridgeBrief(text: string): BridgeBrief | null {
  const lines = text.trim().split('\n')
  if (lines.length < 2 || lines[lines.length - 1] !== closingLine) return null
  const opening = openingLine.exec(lines[0])
  if (!opening) return null
  const attributes = new Map<string, string>()
  for (const match of opening[1].matchAll(attribute)) attributes.set(match[1], unescapeAttribute(match[2]))
  const kind = attributes.get('kind') ?? ''
  if (!kind) return null
  const fence = attributes.get('fence') ?? ''
  const body = lines.slice(1, -1)
  const brief: BridgeBrief = {
    kind,
    from: attributes.get('from') ?? '',
    title: attributes.get('title') ?? '',
    fence,
    lead: body.join('\n'),
    quote: null,
    trailer: '',
  }
  if (!fence) return brief
  const quoteStart = new RegExp(`^--- (\\S+) ${escapeRegExp(fence)}(.*) ---$`)
  const startIndex = body.findIndex((line) => quoteStart.test(line))
  if (startIndex < 0) return brief
  const [, noun, caption] = quoteStart.exec(body[startIndex])!
  const endLine = `--- end of ${noun} ${fence} ---`
  const endIndex = body.indexOf(endLine, startIndex + 1)
  if (endIndex < 0) return brief
  brief.lead = body.slice(0, startIndex).join('\n')
  brief.quote = { caption: `${noun}${caption}`, text: body.slice(startIndex + 1, endIndex).join('\n') }
  brief.trailer = body.slice(endIndex + 1).join('\n')
  return brief
}

const kindLabels: Record<string, string> = {
  [BridgeBriefKindPeerPatch]: 'Patch from a connected server',
  [BridgeBriefKindPeerRequest]: 'Request from a connected server',
  [BridgeBriefKindPeerPlacement]: 'Approved; choose where it runs',
  [BridgeBriefKindSweep]: 'Orchestrator sweep',
}

/** What a card says a brief of this kind is. A kind this library does not
 *  know yet is shown as itself. */
export function bridgeBriefKindLabel(kind: string): string {
  return kindLabels[kind] ?? kind
}
