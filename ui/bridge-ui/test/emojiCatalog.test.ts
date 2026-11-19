import { describe, expect, it } from 'vitest'
import type { DiscordBridgeStatus, DiscordCustomEmoji, MessageReactionGroup } from '@kayushkin/multichat-types'
import { EMOJI_GROUPS } from '../src/emojiData'
import {
  choiceNamed, colonQueryAt, colonQueryIsReactionCommand, customChoice, customEmojiOffered, emojiNameMatchRank,
  favoriteChoices, reactionCommandOf, reactionGroupIsEmoji, roomDiscordServerID, searchEmojiChoices, unicodeChoices,
  serverSweepSummary, unicodeNamesOf, withFavoriteMoved, withFavoriteToggled, withSkinTone, type EmojiChoice,
} from '../src/emojiCatalog'

const emoji = (id: string, name: string, server = 'reno', bridgeMXC = ''): DiscordCustomEmoji => ({
  discord_emoji_id: id, name, animated: false, discord_server_id: server, discord_server_name: server,
  fetched_at: '2026-09-29T00:00:00Z', reaction_key: `discord-emoji:${id}`, bridge_mxc: bridgeMXC,
})

describe('name search', () => {
  const choices: EmojiChoice[] = [
    customChoice(emoji('1', 'partyParrot')),
    customChoice(emoji('2', 'parrot')),
    customChoice(emoji('3', 'sparkle_dog')),
    { kind: 'unicode', key: '🦜', name: 'parrot' },
    { kind: 'unicode', key: '🎉', name: 'party popper' },
  ]

  it('finds a name from any part of it, as ":par" finds parrot and partyParrot', () => {
    const found = searchEmojiChoices(choices, ':par', []).map(c => c.name)
    expect(found).toEqual(['parrot', 'parrot', 'partyParrot', 'party popper', 'sparkle_dog'])
  })

  it('ranks a later word of a name above a match inside a word', () => {
    expect(emojiNameMatchRank('partyParrot', 'parrot')).toBe(2)
    expect(emojiNameMatchRank('sparkle_dog', 'park')).toBe(3)
    expect(emojiNameMatchRank('thumbs up', 'up')).toBe(2)
    expect(emojiNameMatchRank('partyParrot', 'party parrot')).toBe(0)
    expect(emojiNameMatchRank('partyParrot', 'zzz')).toBeNull()
    // Separators are not dropped for a match inside the name.
    expect(emojiNameMatchRank('speaker medium volume', 'kerm')).toBeNull()
    expect(emojiNameMatchRank('up arrow', 'par')).toBeNull()
  })

  it('finds a server’s emoji by the server’s name, below every name match', () => {
    const servers: EmojiChoice[] = [
      customChoice(emoji('1', 'kermit_blush', 'Reno 20s and 30s')),
      customChoice(emoji('2', 'kermit_coke', 'Pretend')),
      customChoice(emoji('3', 'renoCity', 'Pretend')),
      { kind: 'unicode', key: '🐸', name: 'frog' },
    ]
    expect(searchEmojiChoices(servers, 'pretend', []).map(c => c.name)).toEqual(['kermit_coke', 'renoCity'])
    expect(searchEmojiChoices(servers, 'reno', []).map(c => c.name)).toEqual(['renoCity', 'kermit_blush'])
    expect(searchEmojiChoices(servers, 'reno kerm', []).map(c => c.name)).toEqual(['kermit_blush'])
    expect(searchEmojiChoices(servers, 'pretend frog', [])).toEqual([])
  })

  it('says how far the server listing has got', () => {
    const idle = { running: false, started_at: null, finished_at: null, servers_in_account: 0, servers_to_list: 0, servers_listed: 0, error: '' }
    expect(serverSweepSummary({ ...idle, running: true, servers_to_list: 132, servers_listed: 40 }, 42)).toContain('40 of 132')
    expect(serverSweepSummary({ ...idle, finished_at: '2026-09-30T12:00:00Z', servers_to_list: 132, servers_in_account: 134 }, 134))
      .toBe('134 servers listed. The last listing checked 132 of your 134 servers.')
  })

  it('puts favorites first among equal matches', () => {
    expect(searchEmojiChoices(choices, 'parrot', ['🦜']).map(c => c.key)[0]).toBe('🦜')
  })

  it('matches nothing for an empty query', () => {
    expect(searchEmojiChoices(choices, '::', [])).toEqual([])
  })

  it('finds unicode emoji by their Unicode names', () => {
    const found = searchEmojiChoices(unicodeChoices(EMOJI_GROUPS, ''), 'thumbs', []).map(c => c.key)
    expect(found).toContain('\u{1F44D}')
  })
})

describe('the :query at the cursor', () => {
  it('opens after two characters, at the start or after a space', () => {
    expect(colonQueryAt(':pa', 3)).toEqual({ start: 0, query: 'pa' })
    expect(colonQueryAt('hi :party', 9)).toEqual({ start: 3, query: 'party' })
    expect(colonQueryAt('hi :p', 5)).toBeNull()
  })

  it('stays shut inside a word, a time or a finished :name:', () => {
    expect(colonQueryAt('http://x', 8)).toBeNull()
    expect(colonQueryAt('at 10:30', 8)).toBeNull()
    expect(colonQueryAt(':fire: ', 7)).toBeNull()
    expect(colonQueryAt(':fire', 3)).toEqual({ start: 0, query: 'fi' })
  })

  it('knows a react command', () => {
    expect(colonQueryAt('+:par', 5)).toEqual({ start: 1, query: 'par' })
    expect(colonQueryIsReactionCommand('+:par', 1)).toBe(true)
    expect(colonQueryIsReactionCommand('so +:par', 4)).toBe(false)
  })
})

describe('react commands', () => {
  it('reads +:name: and + with an emoji', () => {
    expect(reactionCommandOf('+:partyParrot:')).toEqual({ name: 'partyParrot' })
    expect(reactionCommandOf(' +🔥 ')).toEqual({ emoji: '🔥' })
    expect(reactionCommandOf('+👍🏽')).toEqual({ emoji: '👍🏽' })
    expect(reactionCommandOf('+1')).toBeNull()
    expect(reactionCommandOf('ok +:fire:')).toBeNull()
  })

  it('names the room’s own emoji before another server’s', () => {
    const offered = customEmojiOffered([emoji('9', 'wave', 'pretend'), emoji('1', 'wave', 'reno')], [], { serverID: 'reno' }, true)
    const found = choiceNamed(offered.map(customChoice), 'wave')
    expect(found?.kind === 'custom' && found.emoji.discord_emoji_id).toBe('1')
  })
})

describe('what a room is offered', () => {
  const all = [emoji('1', 'a', 'reno'), emoji('2', 'b', 'pretend')]
  const seen = [emoji('7', 'seen', '')]
  it('offers only the room’s own server without Nitro, and every server’s and the seen ones with it', () => {
    expect(customEmojiOffered(all, seen, { serverID: 'reno' }, false).map(e => e.name)).toEqual(['a'])
    expect(customEmojiOffered(all, seen, { serverID: 'pretend' }, true).map(e => e.name)).toEqual(['b', 'a', 'seen'])
    expect(customEmojiOffered(all, seen, null, true)).toEqual([])
  })

  it('offers a Discord DM every server’s emoji with Nitro, and none without', () => {
    expect(customEmojiOffered(all, seen, { serverID: null }, true).map(e => e.name)).toEqual(['a', 'b', 'seen'])
    expect(customEmojiOffered(all, seen, { serverID: null }, false)).toEqual([])
  })

  it('finds a room’s server from the bridge status', () => {
    const status = { bridged_servers: [{ discord_server_id: 'reno', channels: [{ matrix_room_id: '!general:x' }] }] } as unknown as DiscordBridgeStatus
    expect(roomDiscordServerID(status, '!general:x')).toBe('reno')
    expect(roomDiscordServerID(status, '!dm:x')).toBeNull()
  })
})

describe('skin tones', () => {
  it('tones an emoji that takes one, after its first code point', () => {
    expect(withSkinTone('👍', true, '🏽')).toBe('👍🏽')
    expect(withSkinTone('✌️', true, '🏾')).toBe('✌🏾')
    expect(withSkinTone('👩‍💻', true, '🏻')).toBe('👩🏻‍💻')
  })

  it('leaves the rest alone', () => {
    expect(withSkinTone('🔥', false, '🏽')).toBe('🔥')
    expect(withSkinTone('👍', true, '')).toBe('👍')
    expect(withSkinTone('🧑‍🤝‍🧑', true, '🏽')).toBe('🧑‍🤝‍🧑')
  })

  it('reads which emoji take one from the table', () => {
    const toned = unicodeChoices(EMOJI_GROUPS, '🏿')
    expect(toned.find(c => c.name === 'thumbs up')?.key).toBe('👍🏿')
    expect(toned.find(c => c.name === 'fire')?.key).toBe('🔥')
  })
})

describe('favorites', () => {
  it('toggles and moves', () => {
    expect(withFavoriteToggled(['🔥'], '👍')).toEqual(['🔥', '👍'])
    expect(withFavoriteToggled(['🔥', '👍'], '🔥')).toEqual(['👍'])
    expect(withFavoriteMoved(['a', 'b', 'c'], 'c', -1)).toEqual(['a', 'c', 'b'])
    expect(withFavoriteMoved(['a', 'b'], 'a', -1)).toEqual(['a', 'b'])
  })

  it('names unicode favorites from the table and drops deleted custom ones', () => {
    const choices = favoriteChoices(['👍🏽', 'discord-emoji:1', 'discord-emoji:gone'], [emoji('1', 'parrot')], unicodeNamesOf(EMOJI_GROUPS))
    expect(choices.map(c => c.name)).toEqual(['thumbs up', 'parrot'])
  })

  it('matches a custom favorite to its live reactions by the bridge’s mxc', () => {
    const custom = [emoji('1', 'parrot', 'reno', 'mxc://chat/abc')]
    const group = (key: string) => ({ key } as MessageReactionGroup)
    expect(reactionGroupIsEmoji(group('mxc://chat/abc'), 'discord-emoji:1', custom)).toBe(true)
    expect(reactionGroupIsEmoji(group('discord-emoji:1'), 'discord-emoji:1', custom)).toBe(true)
    expect(reactionGroupIsEmoji(group('mxc://chat/other'), 'discord-emoji:1', custom)).toBe(false)
    expect(reactionGroupIsEmoji(group('🔥'), '🔥', custom)).toBe(true)
  })
})
