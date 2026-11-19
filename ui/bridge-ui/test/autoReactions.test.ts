import { describe, expect, it } from 'vitest'
import type { DiscordCustomEmoji } from '@kayushkin/multichat-types'
import type { BridgeConfig } from '../src/context'
import { BRIDGE_PAGES } from '../src/pages'
import { emojiPanelBox } from '../src/components/messages/BridgeAutoReactions'
import { accountAutoReactionPlaceLabel, accountReactionKeyOf, autoReactionEmojiLabel, autoReactionEmojiOf, autoReactionPersonLabel, memberSearchPath, type AccountAutoReaction, type AutoReaction } from '../src/autoReactions'
import { MULTICHAT_ROUTES_CALLED } from '../src/multichatMessages'

describe('autoReactionEmojiOf', () => {
  it('sends a unicode emoji as itself and a custom one as name:id', () => {
    expect(autoReactionEmojiOf({ kind: 'unicode', key: '🥛', name: 'glass_of_milk' })).toBe('🥛')
    const custom = { discord_emoji_id: '123456789012345678', name: 'milkies' } as DiscordCustomEmoji
    expect(autoReactionEmojiOf({ kind: 'custom', key: 'discord-emoji:123456789012345678', name: 'milkies', emoji: custom })).toBe('milkies:123456789012345678')
  })
})

describe('labels', () => {
  it('shows a custom emoji by name and a unicode one as it is', () => {
    expect(autoReactionEmojiLabel('milkies:123456789012345678')).toBe(':milkies:')
    expect(autoReactionEmojiLabel('🥛')).toBe('🥛')
  })
  it('names the person, or says it has only their id', () => {
    const rule = { discord_user_id: '198680080069689344', member_display_name: '' } as AutoReaction
    expect(autoReactionPersonLabel(rule)).toBe('Discord user 198680080069689344')
    expect(autoReactionPersonLabel({ ...rule, member_display_name: 'Loukic' })).toBe('Loukic')
  })
  it('escapes the search path', () => {
    expect(memberSearchPath('1451516687173156926', 'lou k')).toBe('/guilds/1451516687173156926/members/search?q=lou%20k')
  })
})

describe('the Auto-reactions page', () => {
  it('shows when the host proxies multichat', () => {
    const page = BRIDGE_PAGES.find(p => p.route === 'autoReactions')
    expect(page?.group).toBe('messages')
    const available = (c: Partial<BridgeConfig>) => page?.available?.(c as BridgeConfig, {} as never)
    expect(available({ multichatBasePath: '/api/multichat' })).toBe(true)
    expect(available({ multichatBasePath: '' })).toBe(false)
  })

  it("reaches the bot's rules through multichat, on the routes the host must forward", () => {
    expect(MULTICHAT_ROUTES_CALLED).toContain('GET /discord-signup-store/auto-reactions')
    expect(MULTICHAT_ROUTES_CALLED).toContain('GET /discord-signup-store/guilds/{guildID}/members/search')
  })
})

describe('account rules', () => {
  it('react with a standard emoji only', () => {
    expect(accountReactionKeyOf({ kind: 'unicode', key: '🥛', name: 'glass_of_milk' })).toBe('🥛')
    const custom = { discord_emoji_id: '1', name: 'milkies' } as DiscordCustomEmoji
    expect(accountReactionKeyOf({ kind: 'custom', key: 'discord-emoji:1', name: 'milkies', emoji: custom })).toBeNull()
  })
  it('say where they apply', () => {
    const rule = { room_id: '', room_name: '' } as AccountAutoReaction
    expect(accountAutoReactionPlaceLabel(rule)).toBe('in every chat')
    expect(accountAutoReactionPlaceLabel({ ...rule, room_id: '!x:chat', room_name: 'Lillian, Loukic, Maleeha' })).toBe('in Lillian, Loukic, Maleeha')
  })
})

describe('emojiPanelBox', () => {
  const inside = (box: { left: number; top: number; width: number }, w: number, h: number) =>
    box.left >= 0 && box.top >= 0 && box.left + box.width <= w && box.top + 320 <= h
  it('opens below a button with room under it', () => {
    expect(emojiPanelBox({ left: 100, top: 200, bottom: 220 }, 1280, 720)).toEqual({ left: 100, top: 224, width: 360 })
  })
  it('stays inside the window from every corner, as the browser check found it did not', () => {
    for (const [left, top] of [[1200, 300], [20, 472 - 30], [1200, 690], [0, 0], [600, 400]]) {
      const box = emojiPanelBox({ left, top, bottom: top + 20 }, 1280, 720)
      expect(inside(box, 1280, 720), `button at ${left},${top}: ${JSON.stringify(box)}`).toBe(true)
    }
  })
  it('opens above a button near the bottom', () => {
    expect(emojiPanelBox({ left: 100, top: 650, bottom: 670 }, 1280, 720).top).toBe(650 - 4 - 320)
  })
  it('narrows on a phone', () => {
    const box = emojiPanelBox({ left: 200, top: 100, bottom: 120 }, 320, 640)
    expect(box.width).toBe(304)
    expect(box.left).toBe(8)
  })
})
