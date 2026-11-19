import { describe, expect, it } from 'vitest'
import { unified } from 'unified'
import remarkParse from 'remark-parse'
import remarkGfm from 'remark-gfm'
import type { Root } from 'mdast'
import { remarkDiscordInline, remarkDiscordSubtext } from '../src/discordMarkdown'
import type { DiscordNames } from '../src/messageBody'

const names: DiscordNames = { channelNames: new Map([['77', 'events']]), userNames: new Map([['88', 'Jess']]) }
const options = { names, emojiClassName: 'emoji', mentionClassName: 'mention' }

function tree(markdown: string): Root {
  const processor = unified().use(remarkParse).use(remarkGfm).use(remarkDiscordInline, options)
  return processor.runSync(processor.parse(markdown)) as Root
}

describe('Discord markdown', () => {
  it('keeps headings and bold as markdown and makes emoji images', () => {
    const root = tree('### Jazz Night\n**Going** <:bearlaugh:123> <a:dance:456>')
    expect(root.children[0].type).toBe('heading')
    const paragraph = root.children[1]
    if (paragraph.type !== 'paragraph') throw new Error(paragraph.type)
    expect(paragraph.children[0].type).toBe('strong')
    const images = paragraph.children.filter(c => c.type === 'image')
    expect(images.map(i => i.type === 'image' && i.url)).toEqual([
      'https://cdn.discordapp.com/emojis/123.webp?size=48', 'https://cdn.discordapp.com/emojis/456.gif?size=48',
    ])
    expect((images[0].data as { hProperties?: unknown } | undefined)?.hProperties).toEqual({ className: ['emoji'] })
  })

  it('names channel and user mentions as spans', () => {
    const paragraph = tree('see <#77> with <@88>').children[0]
    if (paragraph.type !== 'paragraph') throw new Error(paragraph.type)
    const mentions = paragraph.children.filter(c => c.type === 'emphasis')
    expect(mentions.map(m => m.type === 'emphasis' && m.children[0].type === 'text' && m.children[0].value)).toEqual(['#events', '@Jess'])
    expect(mentions[0].data).toEqual({ hName: 'span', hProperties: { className: ['mention'], title: 'Discord channel 77' } })
  })

  it('makes a single newline a line break, as Discord does', () => {
    const paragraph = tree('one\ntwo').children[0]
    if (paragraph.type !== 'paragraph') throw new Error(paragraph.type)
    expect(paragraph.children.map(c => c.type)).toEqual(['text', 'break', 'text'])
  })

  it('leaves tokens inside code as typed', () => {
    const paragraph = tree('`<:x:1>`').children[0]
    if (paragraph.type !== 'paragraph') throw new Error(paragraph.type)
    expect(paragraph.children).toMatchObject([{ type: 'inlineCode', value: '<:x:1>' }])
  })
})

describe('Discord small text', () => {
  function subtextTree(markdown: string): Root {
    const processor = unified().use(remarkParse).use(remarkGfm).use(remarkDiscordInline, options).use(remarkDiscordSubtext, { subtextClassName: 'subtext' })
    return processor.runSync(processor.parse(markdown)) as Root
  }

  it('draws a -# line small without its marker, and leaves the line before it alone', () => {
    const paragraph = subtextTree('Saturday at noon\n-# Replies here are passed to <@88>.').children[0]
    if (paragraph.type !== 'paragraph') throw new Error(paragraph.type)
    const small = paragraph.children.find(child => child.type === 'emphasis' && (child.data as { hProperties?: { className?: string[] } })?.hProperties?.className?.[0] === 'subtext')
    if (!small || small.type !== 'emphasis') throw new Error('no small text')
    expect(small.children[0]).toEqual({ type: 'text', value: 'Replies here are passed to ' })
    expect(small.children.some(child => child.type === 'emphasis')).toBe(true)
    expect(paragraph.children[0]).toEqual({ type: 'text', value: 'Saturday at noon' })
  })

  it('leaves -# in the middle of a line as typed', () => {
    const paragraph = subtextTree('a **b** -# c').children[0]
    if (paragraph.type !== 'paragraph') throw new Error(paragraph.type)
    expect(JSON.stringify(paragraph)).toContain('-# c')
  })
})
