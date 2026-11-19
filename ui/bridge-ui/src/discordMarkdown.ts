// A remark plugin for Discord's markdown, as multichat's archive rows carry it
// (`format: "discord-markdown"`): custom emoji, channel and user mentions and
// timestamps become images and labels (discordInlineSegments), and a single
// newline is a line break, as Discord draws it, rather than CommonMark's
// space. Code spans and blocks are other node types, so tokens inside them
// stay as typed, as on Discord.
import type { Break, Emphasis, Image, Paragraph, Parent, PhrasingContent, Root, Text } from 'mdast'
import { SKIP, visit } from 'unist-util-visit'
import { discordInlineSegments, type DiscordNames } from './messageBody'

export interface DiscordInlineOptions {
  names: DiscordNames
  /** Class names for the elements the plugin makes, so a CSS module can style them. */
  emojiClassName: string
  mentionClassName: string
}

/** The mdast nodes one text node becomes. */
export function discordPhrasing(text: string, options: DiscordInlineOptions): PhrasingContent[] {
  const out: PhrasingContent[] = []
  for (const segment of discordInlineSegments(text, options.names)) {
    if (segment.kind === 'text') {
      segment.text.split('\n').forEach((line, index) => {
        if (index > 0) out.push({ type: 'break' } satisfies Break)
        if (line) out.push({ type: 'text', value: line } satisfies Text)
      })
    } else if (segment.kind === 'emoji') {
      out.push({
        type: 'image', url: segment.src, alt: segment.alt, title: segment.alt,
        data: { hProperties: { className: [options.emojiClassName] } },
      } satisfies Image)
    } else {
      out.push({
        type: 'emphasis', children: [{ type: 'text', value: segment.label }],
        data: { hName: 'span', hProperties: { className: [options.mentionClassName], title: segment.title } },
      } satisfies Emphasis)
    }
  }
  return out
}

export function remarkDiscordInline(options: DiscordInlineOptions) {
  return (tree: Root) => {
    visit(tree, 'text', (node: Text, index, parent: Parent | undefined) => {
      if (!parent || index === undefined) return
      const replacement = discordPhrasing(node.value, options)
      if (replacement.length === 1 && replacement[0].type === 'text') return
      parent.children.splice(index, 1, ...(replacement as Parent['children']))
      return [SKIP, index + replacement.length]
    })
  }
}

/** Discord's small-text marker: a line that starts with it is drawn small and grey. */
export const DISCORD_SUBTEXT_MARKER = '-# '

/**
 * Discord's `-# ` lines. A line of a paragraph — its start, or after a line
 * break — that begins with the marker loses the marker and its rest, up to the
 * next line break, goes in a span with `subtextClassName`. Run after
 * remarkDiscordInline, which turns each newline into a break.
 */
export function remarkDiscordSubtext(options: { subtextClassName: string }) {
  return (tree: Root) => {
    visit(tree, 'paragraph', (paragraph: Paragraph) => {
      const children = paragraph.children
      const out: PhrasingContent[] = []
      let index = 0
      while (index < children.length) {
        const child = children[index]
        const atLineStart = index === 0 || children[index - 1].type === 'break'
        if (atLineStart && child.type === 'text' && child.value.startsWith(DISCORD_SUBTEXT_MARKER)) {
          const line: PhrasingContent[] = []
          const rest = child.value.slice(DISCORD_SUBTEXT_MARKER.length)
          if (rest) line.push({ type: 'text', value: rest } satisfies Text)
          index++
          while (index < children.length && children[index].type !== 'break') {
            line.push(children[index])
            index++
          }
          out.push({
            type: 'emphasis', children: line,
            data: { hName: 'span', hProperties: { className: [options.subtextClassName] } },
          } satisfies Emphasis)
          continue
        }
        out.push(child)
        index++
      }
      paragraph.children = out
    })
  }
}
