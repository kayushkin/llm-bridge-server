import { describe, expect, it } from 'vitest'
import { DOMParser } from 'linkedom'
import {
  discordEmojiURL, discordInlineSegments, discordNamesOf, isImageMedia, linkIsAllowed, mediaPathOfMxc, messageBodyKind,
  sanitizeMatrixHtml, type DiscordNames, type SafeNode,
} from '../src/messageBody'

const names: DiscordNames = {
  channelNames: new Map([['1554134165673353317', 'events']]),
  userNames: new Map([['532249601122762782', 'Jess']]),
}

/** The sanitizer over linkedom's parse, as the page runs it over the browser's. */
function sanitize(html: string): SafeNode[] {
  const document = new DOMParser().parseFromString(`<!doctype html><html><body>${html}</body></html>`, 'text/html')
  return sanitizeMatrixHtml(document.body as unknown as Node, { multichatBasePath: '/api/multichat', discordNames: names })
}

/** Every tag and attribute name in a sanitized tree, to assert on what survived. */
function tagsAndAttributes(nodes: readonly SafeNode[]): string[] {
  return nodes.flatMap(node => node.kind === 'element'
    ? [node.tag, ...Object.keys(node.attributes).map(a => `${node.tag}@${a}`), ...tagsAndAttributes(node.children)]
    : node.kind === 'image' ? ['img'] : [])
}

describe('which renderer a message gets', () => {
  it('uses HTML only when there is HTML to render', () => {
    expect(messageBodyKind({ format: 'org.matrix.custom.html', formatted_body: '<b>hi</b>' })).toBe('matrix-html')
    expect(messageBodyKind({ format: 'org.matrix.custom.html', formatted_body: '' })).toBe('plain')
    expect(messageBodyKind({ format: 'discord-markdown' })).toBe('discord-markdown')
    expect(messageBodyKind({ format: '' })).toBe('plain')
    expect(messageBodyKind({})).toBe('plain')
  })
})

describe('mxc:// media', () => {
  it('maps an mxc URI to multichat’s media route, with a thumbnail size when asked', () => {
    expect(mediaPathOfMxc('/api/multichat', 'mxc://chat.kayushkin.com/QHmyVBnIJWrPQgCPYOFJKmSn'))
      .toBe('/api/multichat/media/chat.kayushkin.com/QHmyVBnIJWrPQgCPYOFJKmSn')
    expect(mediaPathOfMxc('/api/multichat', 'mxc://s.example:8448/abc_-9', { width: 480, height: 360 }))
      .toBe('/api/multichat/media/s.example%3A8448/abc_-9?width=480&height=360')
  })

  it('refuses anything that is not a single-segment mxc URI', () => {
    for (const bad of ['https://evil.example/x.png', 'mxc://s/a/b', 'mxc://s/', 'mxc:///id', 'mxc://s/../x', 'mxc://s/id?x=1', 'javascript:alert(1)', '']) {
      expect(mediaPathOfMxc('/api/multichat', bad)).toBeNull()
    }
  })

  it('shows only image mimetypes inline', () => {
    expect(isImageMedia({ media_url: 'mxc://s/i', media_mimetype: 'image/gif' })).toBe(true)
    expect(isImageMedia({ media_url: 'mxc://s/i', media_mimetype: 'video/mp4' })).toBe(false)
    expect(isImageMedia({ media_url: '', media_mimetype: 'image/png' })).toBe(false)
  })
})

describe('the Matrix HTML sanitizer', () => {
  it('keeps the spec’s formatting tags', () => {
    const nodes = sanitize('<h3>Jazz Night</h3><p>✅ <strong>Going</strong> (5): <u>Joel</u><br>🤷 <em>Maybe</em></p><ol start="3"><li>x</li></ol>')
    expect(tagsAndAttributes(nodes)).toEqual(['h3', 'p', 'strong', 'u', 'br', 'em', 'ol', 'ol@start', 'li'])
  })

  it('drops scripts, handlers, style and javascript: links, keeping only harmless text', () => {
    const nodes = sanitize([
      '<script>alert(1)</script>',
      '<img src="x" onerror="alert(2)">',
      '<img src="mxc://s/i" onerror="alert(3)" onload="alert(4)">',
      '<a href="javascript:alert(5)">click</a>',
      '<a href="JaVaScRiPt:alert(6)">again</a>',
      '<a href="data:text/html,<script>alert(7)</script>">data</a>',
      '<p style="background:url(javascript:alert(8))" onclick="alert(9)" class="evil">text</p>',
      '<iframe src="https://evil.example"></iframe>',
      '<svg><script>alert(10)</script></svg>',
      '<style>body{display:none}</style>',
      '<form action="https://evil.example"><input name="x"></form>',
    ].join(''))
    expect(tagsAndAttributes(nodes)).toEqual(['img', 'p'])
    const serialized = JSON.stringify(nodes)
    expect(serialized).not.toMatch(/alert|javascript|onerror|onload|onclick|style|evil/i)
    const image = nodes.find(n => n.kind === 'image')
    expect(image).toMatchObject({ kind: 'image', src: '/api/multichat/media/s/i' })
  })

  it('drops an image whose src is not mxc://', () => {
    expect(sanitize('<img src="https://tracker.example/pixel.gif" alt="x">')).toEqual([])
  })

  it('strips a reply fallback', () => {
    const nodes = sanitize('<mx-reply><blockquote><a href="https://matrix.to/#/!r/$e">In reply to</a> quoted</blockquote></mx-reply>the answer')
    expect(nodes).toEqual([{ kind: 'text', text: 'the answer' }])
  })

  it('draws a custom emoji inline from the media route, titled with its shortcode', () => {
    const nodes = sanitize('wakes me up <img data-mx-emoticon src="mxc://chat.kayushkin.com/aPtIEylewqwfCRkNjUchwoCC" alt=":happy:" title=":happy:" height="32"/>')
    expect(nodes[1]).toEqual({
      kind: 'image', src: '/api/multichat/media/chat.kayushkin.com/aPtIEylewqwfCRkNjUchwoCC',
      alt: ':happy:', title: ':happy:', emoticon: true,
    })
  })

  it('opens links in a new tab without an opener', () => {
    expect(sanitize('<a href="https://example.com/x" target="_self">x</a>')[0]).toEqual({
      kind: 'element', tag: 'a', attributes: { href: 'https://example.com/x', target: '_blank', rel: 'noopener noreferrer' },
      children: [{ kind: 'text', text: 'x' }],
    })
  })

  it('turns a matrix.to user link into a mention', () => {
    expect(sanitize('<a href="https://matrix.to/#/@discord_4939:chat.kayushkin.com">Twili Midna</a>')[0])
      .toEqual({ kind: 'mention', label: '@Twili Midna', title: '@discord_4939:chat.kayushkin.com' })
  })

  it('names a Discord channel token the bridge left in the text', () => {
    const nodes = sanitize('<p>📍 Lo-Bar<br>\n&lt;#1554134165673353317&gt;</p>')
    const paragraph = nodes[0] as Extract<SafeNode, { kind: 'element' }>
    expect(paragraph.children).toContainEqual({ kind: 'mention', label: '#events', title: 'Discord channel 1554134165673353317' })
  })

  it('unwraps an unknown tag and keeps its text', () => {
    expect(sanitize('<marquee>hello</marquee>')).toEqual([{ kind: 'text', text: 'hello' }])
  })

  it('keeps only a language class on code and nothing else on a span but the spoiler mark', () => {
    expect(tagsAndAttributes(sanitize('<code class="language-go x">a</code><code class="evil">b</code><span data-mx-spoiler="why" data-mx-color="#f00">c</span>')))
      .toEqual(['code', 'code', 'span', 'span@data-mx-spoiler'])
  })

  it('stops at the spec’s depth limit', () => {
    const deep = '<div>'.repeat(150) + 'bottom' + '</div>'.repeat(150)
    expect(JSON.stringify(sanitize(deep))).not.toContain('bottom')
  })
})

describe('link schemes', () => {
  it('allows web and mail links only', () => {
    expect(linkIsAllowed('https://a.example')).toBe(true)
    expect(linkIsAllowed('mailto:a@b.example')).toBe(true)
    expect(linkIsAllowed('javascript:alert(1)')).toBe(false)
    expect(linkIsAllowed('/relative')).toBe(false)
    expect(linkIsAllowed('vbscript:x')).toBe(false)
  })
})

describe('Discord inline tokens', () => {
  it('turns custom emoji into CDN images, gif when animated', () => {
    expect(discordInlineSegments('hi <:bearlaugh:1234> and <a:dance:5678>!', names)).toEqual([
      { kind: 'text', text: 'hi ' },
      { kind: 'emoji', src: 'https://cdn.discordapp.com/emojis/1234.webp?size=48', alt: ':bearlaugh:' },
      { kind: 'text', text: ' and ' },
      { kind: 'emoji', src: 'https://cdn.discordapp.com/emojis/5678.gif?size=48', alt: ':dance:' },
      { kind: 'text', text: '!' },
    ])
    expect(discordEmojiURL('1', false)).toBe('https://cdn.discordapp.com/emojis/1.webp?size=48')
  })

  it('names channels and users it knows, and shows the id of those it does not', () => {
    expect(discordInlineSegments('<#1554134165673353317> <@532249601122762782> <@!999> <#42> <@&7>', names)
      .filter(s => s.kind === 'mention').map(s => s.kind === 'mention' && s.label))
      .toEqual(['#events', '@Jess', '@999', '#42', '@role'])
  })

  it('leaves text with no token alone', () => {
    expect(discordInlineSegments('a < b and <c>', names)).toEqual([{ kind: 'text', text: 'a < b and <c>' }])
  })

  it('takes user names from Discord puppet senders only', () => {
    const found = discordNamesOf([{ discord_channel_id: '1', name: 'general' }], [
      { sender_user_id: '@discord_532249601122762782:chat.kayushkin.com', sender_display_name: 'Jess' },
      { sender_user_id: '@admin:chat.kayushkin.com', sender_display_name: 'Admin' },
      { sender_user_id: '@discord_1:chat.kayushkin.com', sender_display_name: '' },
    ])
    expect([...found.userNames]).toEqual([['532249601122762782', 'Jess']])
    expect(found.channelNames.get('1')).toBe('general')
  })
})
