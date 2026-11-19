import { describe, expect, it } from 'vitest'
import {
  groupReactions, reactionChipAction, reactionFace, reactionMapKeyOf, reactionTooltip, reactionsOfLoggedMessage,
  withReactionPosted, withReactionTakenBack,
} from '../src/messageReactions'
import type { MessageReactionGroup } from '@kayushkin/multichat-types'

/** Go sends a nil slice as null, whatever the rendered type says. */
const NO_NAMES = null as unknown as string[]

const reaction = (overrides: Partial<MessageReactionGroup> = {}): MessageReactionGroup => ({
  key: '\u{1F44D}', shortcode: '', count: 1, sender_display_names: ['Ann'], reacted_by_me: false, my_reaction_event_id: '', ...overrides,
})

describe('finding a row’s reactions', () => {
  it('files a live row under its event id and an archive row under its Discord message id', () => {
    expect(reactionMapKeyOf({ event_id: '$e', discord_message_id: '' })).toBe('$e')
    expect(reactionMapKeyOf({ event_id: '', discord_message_id: '42' })).toBe('discord:42')
    expect(reactionMapKeyOf({ event_id: '', discord_message_id: '' })).toBe('')
  })

  it('reads the row’s list from the page map, empty when it has none', () => {
    const map = { 'discord:42': [reaction({ key: 'discord-emoji:9', shortcode: ':cat:', count: 3, sender_display_names: [] })] }
    expect(reactionsOfLoggedMessage({ event_id: '', discord_message_id: '42' }, map)).toHaveLength(1)
    expect(reactionsOfLoggedMessage({ event_id: '$other' }, map)).toEqual([])
    expect(reactionsOfLoggedMessage({ event_id: '$e' }, undefined)).toEqual([])
  })
})

describe('grouping', () => {
  it('adds counts and joins names without repeats, keeping first-seen order', () => {
    const grouped = groupReactions([
      reaction(), reaction({ key: '❤️', sender_display_names: ['Bo'] }), reaction({ count: 2, sender_display_names: ['Ann', 'Cy'] }),
    ])
    expect(grouped).toEqual([
      reaction({ count: 3, sender_display_names: ['Ann', 'Cy'] }),
      reaction({ key: '❤️', sender_display_names: ['Bo'] }),
    ])
  })

  it('does not change what it was given, and copes with null names', () => {
    const input = [reaction({ sender_display_names: NO_NAMES }), reaction({ sender_display_names: ['Bo'] })]
    expect(groupReactions(input)[0].sender_display_names).toEqual(['Bo'])
    expect(input[0].sender_display_names).toBe(NO_NAMES)
  })
})

describe('what a chip shows', () => {
  it('draws unicode as text and custom emoji as images', () => {
    expect(reactionFace(reaction(), '/api/multichat')).toEqual({ kind: 'text', text: '\u{1F44D}' })
    expect(reactionFace(reaction({ key: 'mxc://s.example/abc', shortcode: ':blob:' }), '/api/multichat'))
      .toEqual({ kind: 'image', src: '/api/multichat/media/s.example/abc', alt: ':blob:' })
    expect(reactionFace(reaction({ key: 'discord-emoji:1234', shortcode: ':cat:' }), '/api/multichat'))
      .toEqual({ kind: 'image', src: 'https://cdn.discordapp.com/emojis/1234.webp?size=48', alt: ':cat:' })
  })

  it('falls back to the shortcode for a custom key it cannot load', () => {
    expect(reactionFace(reaction({ key: 'mxc://bad/a/b', shortcode: ':x:' }), '/m')).toEqual({ kind: 'text', text: ':x:' })
    expect(reactionFace(reaction({ key: 'discord-emoji:abc', shortcode: '' }), '/m')).toEqual({ kind: 'text', text: 'discord-emoji:abc' })
  })

  it('names who reacted when known, else how many', () => {
    expect(reactionTooltip(reaction({ sender_display_names: ['Ann', 'Bo'], count: 2 }))).toBe('Ann, Bo reacted with \u{1F44D}')
    expect(reactionTooltip(reaction({ key: 'discord-emoji:1', shortcode: ':cat:', count: 3, sender_display_names: [] })))
      .toBe('3 reactions with :cat:')
    expect(reactionTooltip(reaction({ key: 'mxc://s/i', shortcode: '', count: 1, sender_display_names: NO_NAMES })))
      .toBe('1 reaction with a custom emoji')
  })
})

describe('reacting from the Conversations page', () => {
  const group = (extra: Partial<MessageReactionGroup>): MessageReactionGroup => ({
    key: '🔥', shortcode: '', count: 1, sender_display_names: ['Kai'], reacted_by_me: false, my_reaction_event_id: '', ...extra,
  })
  it('adds ours to a key we have not used, takes back ours, and leaves one made in the app alone', () => {
    expect(reactionChipAction(group({}))).toEqual({ kind: 'post', key: '🔥' })
    expect(reactionChipAction(group({ reacted_by_me: true, my_reaction_event_id: '$mine' }))).toEqual({ kind: 'take-back', reactionEventID: '$mine' })
    expect(reactionChipAction(group({ reacted_by_me: true }))).toEqual({ kind: 'made-in-the-app' })
  })
  it('shows a posted reaction at once, on a new key or an existing one', () => {
    expect(withReactionPosted([], '❤️', '$r1', 'You')).toEqual([
      { key: '❤️', shortcode: '', count: 1, sender_display_names: ['You'], reacted_by_me: true, my_reaction_event_id: '$r1' },
    ])
    expect(withReactionPosted([group({})], '🔥', '$r2', 'You')).toEqual([
      group({ count: 2, sender_display_names: ['Kai', 'You'], reacted_by_me: true, my_reaction_event_id: '$r2' }),
    ])
  })
  it('takes ours back, dropping the chip when it was the only one', () => {
    const shared = group({ count: 2, sender_display_names: ['Kai', 'You'], reacted_by_me: true, my_reaction_event_id: '$r2' })
    expect(withReactionTakenBack([shared], '$r2', 'You')).toEqual([group({})])
    expect(withReactionTakenBack([group({ sender_display_names: ['You'], reacted_by_me: true, my_reaction_event_id: '$r1' })], '$r1', 'You')).toEqual([])
    expect(withReactionTakenBack([group({})], '$other', 'You')).toEqual([group({})])
  })
  it('keeps our reaction when two groups with one key are merged', () => {
    const merged = groupReactions([group({}), group({ sender_display_names: ['You'], reacted_by_me: true, my_reaction_event_id: '$r' })])
    expect(merged).toEqual([group({ count: 2, sender_display_names: ['Kai', 'You'], reacted_by_me: true, my_reaction_event_id: '$r' })])
  })
})
