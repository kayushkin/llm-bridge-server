import { describe, expect, it } from 'vitest'
import { bridgeBriefKindLabel, parseBridgeBrief } from '../src/bridgeBrief'

const fence = '0011223344556677'
const patch = [
  `<bridge-brief kind="peer-patch" from="llm-bridge-server" title="Land a patch to &quot;x&quot; &amp; y" fence="${fence}">`,
  'This session was started from a patch.',
  '',
  '- **Repo:** reference-store',
  `--- summary ${fence} from laptop ---`,
  'Please merge.',
  '--- end of summary ---',
  `--- end of summary ${fence} ---`,
  '</bridge-brief>',
].join('\n')

describe('parseBridgeBrief', () => {
  it('reads the attributes, the lead and the fenced quote', () => {
    const brief = parseBridgeBrief(patch)!
    expect(brief.kind).toBe('peer-patch')
    expect(brief.from).toBe('llm-bridge-server')
    expect(brief.title).toBe('Land a patch to "x" & y')
    expect(brief.lead).toBe('This session was started from a patch.\n\n- **Repo:** reference-store')
    // A forged end line without the fence stays inside the quote.
    expect(brief.quote).toEqual({ caption: 'summary from laptop', text: 'Please merge.\n--- end of summary ---' })
    expect(brief.trailer).toBe('')
  })

  it('reads a brief quoted inside another whole, ending at the outer fence', () => {
    const outer = [
      '<bridge-brief kind="peer-placement" from="llm-bridge-server" title="Place: t" fence="aaaaaaaaaaaaaaaa">',
      'Choose the instance.',
      '--- message aaaaaaaaaaaaaaaa: the first message of the session you are placing ---',
      patch,
      '--- end of message aaaaaaaaaaaaaaaa ---',
      '</bridge-brief>',
    ].join('\n')
    const brief = parseBridgeBrief(outer)!
    expect(brief.quote?.caption).toBe('message: the first message of the session you are placing')
    expect(brief.quote?.text).toBe(patch)
    expect(parseBridgeBrief(brief.quote!.text)?.kind).toBe('peer-patch')
  })

  it('keeps text after the quote as the trailer, and a brief with no fence whole', () => {
    const sweep = '<bridge-brief kind="sweep" from="orchestratord" title="Sweep">\n- a\n  - b\n\nWhat next?\n</bridge-brief>'
    expect(parseBridgeBrief(sweep)).toMatchObject({ kind: 'sweep', fence: '', lead: '- a\n  - b\n\nWhat next?', quote: null })
  })

  it('is not fooled by a message that only quotes a brief', () => {
    expect(parseBridgeBrief('Look at this:\n' + patch)).toBeNull()
    expect(parseBridgeBrief(patch + '\nand more')).toBeNull()
    expect(parseBridgeBrief('<bridge-brief from="x">\nbody\n</bridge-brief>')).toBeNull()
  })

  it('leaves the body whole when the quote never closes', () => {
    const open = `<bridge-brief kind="peer-request" from="s" title="t" fence="${fence}">\nlead\n--- request ${fence}: t ---\nrest\n</bridge-brief>`
    expect(parseBridgeBrief(open)).toMatchObject({ quote: null, lead: `lead\n--- request ${fence}: t ---\nrest` })
  })
})

describe('bridgeBriefKindLabel', () => {
  it('names the known kinds and falls back to the kind itself', () => {
    expect(bridgeBriefKindLabel('peer-patch')).toBe('Patch from a connected server')
    expect(bridgeBriefKindLabel('something-new')).toBe('something-new')
  })
})
