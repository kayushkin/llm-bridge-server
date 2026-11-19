// The rows of the chat's Turns transcript: which of a session's items stand as rows
// of their own, which join one agent row, and the React key each row is drawn under.
//
// The keys are the reason this is a module with a test. The transcript is a virtua
// list, whose items are absolutely positioned, and React matches a list's old and new
// children by key. When two rows share a key and the list's rows are next replaced —
// a session switch does that — React keeps the first of the two in the page for good:
// it stays at its old offset, drawn over the rows of whatever session is opened next,
// until the page is reloaded. A production build gives no warning. So every key
// `transcriptRows` returns is different from every other, whatever order the entries
// arrive in.

import type { Entry, SessionAction, SessionFile } from '@kayushkin/chat-core'
import type { BridgeBrief } from './bridgeBrief'

/** One rendered row derived from a turn's CONTENT (not from turn.role). A chat-core turn
 *  is opened by the user prompt (turn.role === 'user') but carries the assistant's whole
 *  reply, so a single turn yields a user item, a separate assistant item, and any compact
 *  markers — exactly like bridge-ui's rowsToTurns emits a `user` item for the prompt and a
 *  distinct `assistant` item for the turn's assistant content. */
export type RenderItem =
  | {
      kind: 'user'
      key: string
      ts: string
      text: string
      entryIds: string[]
      /** Set on a message the server held while it waited to restart, drawn from
       *  its `message_held_for_restart` event until the message is sent. Absent
       *  `failedBecause` means it is still waiting. */
      held?: { failedBecause?: string }
      /** Set when a service, not the person, wrote the message: a bridge brief
       *  (llm-bridge msg.BridgeBrief), drawn as a card. */
      brief?: BridgeBrief
      /** Who wrote it, as the row's label (`userActorLabel`). */
      actor: string
    }
  | { kind: 'marker'; key: string; ts: string }
  /** A HIDDEN harness task-notification that opened this turn. Not a user row —
   *  the reader never wrote it — but the span renderer surfaces it as a
   *  formatted, auto-collapsed chip at the seam it explains. */
  | { kind: 'notification'; key: string; ts: string; text: string }
  /** A file shared into the session. One the user shared stands as its own row, on
   *  the user's side; one the agent shared sits in the agent's reply. */
  | { kind: 'file'; key: string; ts: string; file: SessionFile; sharedByAgent: boolean; actor: string }
  /** A session action: the button where the agent offered it, and a line for each
   *  later step of its run. */
  | { kind: 'action'; key: string; ts: string; action: SessionAction }
  | {
      kind: 'assistant'
      key: string
      ts: string
      prose: string
      narration: string
      thinking: string
      errors: Entry[]
      usage?: Entry['usage']
      /** How many narration MESSAGES fed the narration dropdown — its "N steps"
       *  label. Presentation metadata only; the classification stays per message. */
      narrationCount?: number
      /** Every entry that fed this item, so the sources badge can state which
       *  sources reported the content actually on screen. */
      entryIds: string[]
    }

/** One TURN's worth of a span: the hidden notification that opened it (when one
 *  did), and its assistant/marker items in order. */
export interface SpanSegment {
  turnId: string
  notification: string | null
  ts: string
  items: RenderItem[]
}

/** One turn's items, in the order its entries arrived. */
export interface TurnItems {
  turnId: string
  items: readonly RenderItem[]
}

/** One row of the Turns transcript, with the React key it is drawn under. */
export type TranscriptRow =
  /** A message a person or a service wrote. */
  | { kind: 'user'; key: string; item: Extract<RenderItem, { kind: 'user' }> }
  /** A file a person shared. */
  | { kind: 'file'; key: string; item: Extract<RenderItem, { kind: 'file' }> }
  /** The agent's reply, across every turn it ran before a person next wrote. */
  | { kind: 'span'; key: string; segments: SpanSegment[]; streaming: boolean }

/** The transcript's rows, in order, from each turn's items.
 *
 *  SPAN assembly. A turn opened by a HIDDEN task-notification (or by no user
 *  message at all — a wire fragment) CONTINUES the previous span: the model
 *  yielded, a background task finished, the harness re-prompted it. The whole
 *  span is ONE row (owner: "they are still 5 separate messages" — they must not
 *  be): one header, then each segment in order — its notification as a
 *  formatted auto-collapsed chip at the seam it explains, narration collapsed,
 *  its answer at full prose weight. A bridge report (a button's outcome, a
 *  deploy's) opens a segment the same way. A person's message or file always
 *  ends the span.
 *
 *  `streaming` says the session is producing output now; the span that holds
 *  the last turn's last assistant item is the one marked streaming.
 *
 *  A span's key is `span:<turn id>`, from the turn its first segment is in, so the
 *  key never changes while the span grows. One turn can begin more than one span: a
 *  person's file or message that lands in the middle of a turn ends the span before
 *  it, and what the agent writes next in that turn begins another. An event with no
 *  turn id joins the turn that is running, so pasting a picture while the agent
 *  works does exactly that. The second span a turn begins is keyed
 *  `span:<turn id>:2`, the third `:3`.
 *
 *  A person's message or file is keyed by its entry's id. Every key goes through
 *  `unusedKey`, so an entry listed twice is two rows with two keys, never two rows
 *  with one. */
export function transcriptRows(turns: readonly TurnItems[], streaming: boolean): TranscriptRow[] {
  const rows: TranscriptRow[] = []
  const timesKeyWasAskedFor = new Map<string, number>()
  /** `key` the first time it is asked for, then `key:2`, `key:3`. */
  const unusedKey = (key: string): string => {
    const times = (timesKeyWasAskedFor.get(key) ?? 0) + 1
    timesKeyWasAskedFor.set(key, times)
    return times === 1 ? key : `${key}:${times}`
  }
  let span: SpanSegment[] = []
  let spanStreaming = false
  const flushSpan = () => {
    if (span.length === 0) return
    rows.push({ kind: 'span', key: unusedKey(`span:${span[0]!.turnId}`), segments: span, streaming: spanStreaming })
    span = []
    spanStreaming = false
  }

  const lastTurnIndex = turns.length - 1
  turns.forEach(({ turnId, items }, turnIndex) => {
    let lastAssistantItem: RenderItem | null = null
    for (let i = items.length - 1; i >= 0; i--) {
      if (items[i]!.kind === 'assistant') { lastAssistantItem = items[i]!; break }
    }

    // Does this turn CONTINUE the span? Opened by a notification, or by
    // nothing at all. A real user opener breaks it below, item by item.
    const opener = items.find((item) => item.kind === 'user' || item.kind === 'notification')
    if (turnIndex === 0 || (opener && opener.kind === 'user')) flushSpan()

    let segment: SpanSegment | null = null
    for (const item of items) {
      if (item.kind === 'user') {
        flushSpan()
        segment = null
        rows.push({ kind: 'user', key: unusedKey(item.key), item })
        continue
      }
      if (item.kind === 'file' && !item.sharedByAgent) {
        flushSpan()
        segment = null
        rows.push({ kind: 'file', key: unusedKey(item.key), item })
        continue
      }
      if (item.kind === 'notification') {
        segment = { turnId, notification: item.text, ts: item.ts, items: [] }
        span.push(segment)
        continue
      }
      if (!segment) {
        segment = { turnId, notification: null, ts: item.ts, items: [] }
        span.push(segment)
      }
      segment.items.push(item)
      if (item.kind === 'assistant' && streaming && turnIndex === lastTurnIndex && item === lastAssistantItem) {
        spanStreaming = true
      }
    }
  })
  flushSpan()
  return rows
}
