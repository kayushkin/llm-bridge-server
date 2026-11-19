// Adapting chat-core's `Entry` to bridge-ui's `ToolEvent`, so chat can render
// tool calls with the same five renderers the original chat uses (Bash, Grep,
// Web, File, and Edit/Write/MultiEdit/NotebookEdit) instead of a JSON dump.
//
// This is a presentation-layer conversion and it lives at the edge on purpose:
// chat-core carries the payload as it arrived (`toolInput`/`toolResult` are
// `unknown`, because the store has no business asserting a shape it did not
// define), and bridge-ui's renderers want a flat record with a string output.
// Neither side is wrong; the seam between them is here.

import type { Entry } from '@kayushkin/chat-core'
import { toolIdOf } from '@kayushkin/chat-core'
import type { ToolEvent } from '../../types'

/** The renderers read `input` field-by-field (`tool.input?.command`, `.file_path`,
 *  …), so a payload that is not a plain object cannot be read that way and is
 *  dropped rather than coerced. A coerced one would render as a card with every
 *  field empty, which claims the harness sent nothing when it sent something
 *  this adapter could not describe. */
function asRecord(value: unknown): Record<string, unknown> | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined
  return value as Record<string, unknown>
}

/** A tool's output is shown as text. A string passes through untouched — JSON
 *  round-tripping it would add quotes and escapes to output that is already
 *  meant to be read, which is most of them (Bash stdout, file contents, grep
 *  hits). Anything else is pretty-printed. */
function asOutputText(value: unknown): string | undefined {
  if (value === undefined || value === null) return undefined
  if (typeof value === 'string') return value
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    // A cyclic payload is not a reason to lose the whole tool card.
    return String(value)
  }
}

/** Whether the tool failed. Read rather than inferred: an empty output is not an
 *  error, and a populated one is not a success.
 *
 *  `toolError` is a field on `Entry` now — log-store carries it on the materialized
 *  page and chat-core's reducer folds it off the live event, so neither path needs
 *  the raw payload. The `raw` branch is a temporary fallback for a log-store that
 *  predates the promotion; it goes when the projected page ships, because the
 *  default page carries no `raw` for it to read. */
function readIsError(entry: Entry): boolean | undefined {
  if (entry.toolError !== undefined) return entry.toolError
  const raw = entry.raw as { tool_result?: { is_error?: unknown } } | undefined
  const flag = raw?.tool_result?.is_error
  return typeof flag === 'boolean' ? flag : undefined
}

/**
 * One tool card's worth of data.
 *
 * ⚠️ `tool_id` is load-bearing beyond identity: it is the key the Edit and Bash
 * renderers fetch file snapshots with, and an empty one disables that fetch
 * silently. An entry whose source event carried no tool id (`unpairable`) yields
 * `''` here and correctly gets a card with no diff — there is nothing to look up.
 */
export function toToolEvent(entry: Entry): ToolEvent {
  return {
    tool_id: toolIdOf(entry) ?? '',
    tool: entry.toolName ?? 'tool',
    input: asRecord(entry.toolInput),
    output: asOutputText(entry.toolResult),
    error: readIsError(entry),
    ...(entry.toolInputTruncated ? { input_truncated: true } : {}),
  }
}

/**
 * Whether a tool call is still in flight, given the ids that have received a
 * result somewhere in the loaded model.
 *
 * Three distinct answers collapse into "not running", and only one of them is
 * "it finished":
 *   - the entry already carries its result (the live-tail reducer merges them);
 *   - the id appears in `resulted` (cold-loaded models keep call and result as
 *     two rows, so the call's own fields say nothing);
 *   - the entry is `unpairable` — no tool id was ever emitted, so its result can
 *     never be recognized. Rendering that as a spinner would leave a tool
 *     spinning forever on every OTel-derived stand-in.
 */
export function isToolRunning(entry: Entry, resulted: ReadonlySet<string>): boolean {
  if (entry.kind !== 'tool_call') return false
  if (entry.toolResult !== undefined) return false
  if (entry.unpairable) return false
  const id = toolIdOf(entry)
  if (!id) return false
  return !resulted.has(id)
}
