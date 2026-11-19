// The agent prompt carried by a card.
//
// A card is a noteboard item, and noteboard items have a fixed set of columns —
// there is no field to put this in, and kanban-store has no cards table at all.
// Adding one would mean a schema migration on a live database whose migrate()
// is CREATE TABLE IF NOT EXISTS with no version table and no ALTER path. So the
// prompt lives inside the body, fenced by markers, the same way email-classifier
// fences its evidence section.
//
// Placement matters and is not cosmetic. email-classifier rewrites card bodies
// on its 15-minute tick, replacing everything BELOW its evidence marker and
// preserving everything above it. A prompt written below that line would be
// deleted by the next tick. writeAgentPrompt therefore always puts the block at
// the very top of the body.

export const AGENT_PROMPT_OPEN = '<!-- agent-prompt -->'
export const AGENT_PROMPT_CLOSE = '<!-- /agent-prompt -->'

/**
 * readAgentPrompt returns the stored prompt, or null when the card carries none.
 *
 * Returns null rather than '' for an empty block so callers can tell "nobody has
 * written a prompt" from "somebody deliberately cleared it" — the first should
 * offer a suggestion, the second should not overwrite the decision.
 */
export function readAgentPrompt(body: string | null | undefined): string | null {
  if (!body) return null
  const start = body.indexOf(AGENT_PROMPT_OPEN)
  if (start === -1) return null
  const from = start + AGENT_PROMPT_OPEN.length
  const end = body.indexOf(AGENT_PROMPT_CLOSE, from)
  // An unterminated block means the body was hand-edited and the close marker
  // lost. Reading to the end would swallow the whole card into the prompt, so
  // treat it as absent and leave the text alone for a human to sort out.
  if (end === -1) return null
  const inner = body.slice(from, end).trim()
  return inner === '' ? null : inner
}

/**
 * stripAgentPrompt removes the block, returning the rest of the body unchanged.
 */
export function stripAgentPrompt(body: string | null | undefined): string {
  if (!body) return ''
  const start = body.indexOf(AGENT_PROMPT_OPEN)
  if (start === -1) return body
  const end = body.indexOf(AGENT_PROMPT_CLOSE, start)
  if (end === -1) return body
  const rest = body.slice(0, start) + body.slice(end + AGENT_PROMPT_CLOSE.length)
  return rest.replace(/^\n+/, '')
}

/**
 * writeAgentPrompt returns a new body carrying this prompt, replacing any block
 * already there. An empty prompt removes the block rather than storing a blank
 * one, so clearing the box and saving actually clears it.
 */
export function writeAgentPrompt(body: string | null | undefined, prompt: string): string {
  const rest = stripAgentPrompt(body)
  const trimmed = prompt.trim()
  if (trimmed === '') return rest
  const block = `${AGENT_PROMPT_OPEN}\n${trimmed}\n${AGENT_PROMPT_CLOSE}`
  return rest ? `${block}\n\n${rest}` : block
}

/**
 * suggestAgentPrompt drafts a starting prompt from what the card already says.
 *
 * This is the fallback shown when a card has no stored prompt. It is a template,
 * not a model call: it renders instantly, costs nothing, and is the same every
 * time, which matters for something a human is about to read and edit. A drafted
 * prompt is only worth paying for once someone chooses to spend on it.
 *
 * It mirrors the instructions autoworker and kanban-dispatcher already give
 * their workers, so a hand-started agent closes its card the same way a
 * cron-started one does — otherwise the curator never sees it finish.
 */
export function suggestAgentPrompt(args: {
  cardID: string
  title: string
  body?: string | null
  linkedEmailCount?: number
}): string {
  const { cardID, title, linkedEmailCount = 0 } = args
  const body = stripAgentPrompt(args.body).trim()

  const lines: string[] = [
    `You are picking up a task from the kanban board.`,
    ``,
    `## Task`,
    `**Card:** ${cardID}`,
    `**Title:** ${title}`,
    ``,
    body || '(The card has no description — work from the title.)',
  ]

  if (linkedEmailCount > 0) {
    lines.push(
      ``,
      `## Context`,
      `${linkedEmailCount} email${linkedEmailCount === 1 ? '' : 's'} ${linkedEmailCount === 1 ? 'is' : 'are'} attached to this card. Read them before acting — mailstack serves them at /api/messages/{id}?account={account}, and each linked reference is account-qualified as account:message_id.`,
    )
  }

  lines.push(
    ``,
    `## When you are done`,
    `1. Do the work described above.`,
    `2. PATCH http://localhost:8191/api/items/${cardID} with {"status":"done"} and a body update saying what you did.`,
    `3. Exit cleanly. The kanban-curator moves the card to Done once this session ends.`,
    `4. If it is blocked or unclear, leave a body update explaining why and exit — the card stays where it is and a human will pick it up.`,
  )

  return lines.join('\n')
}
