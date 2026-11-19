import { describe, expect, it } from 'vitest'
import type { DiscordBridgeStatus, DiscordBridgedChannel, LoggedMessage, MessageLogPage } from '@kayushkin/multichat-types'
import {
  EMPTY_MESSAGE_LOG_FILTERS, MESSAGE_LOG_PAGE_SIZE, appendMessageLogPage, archivedDiscordExtras, bridgeProblems, bridgedChannelsOf, channelLabel,
  channelOptions, channelProblems, currentVersionOf, deletedBeforeRead, discordTabOf, messageLogQuery, nextPageBefore,
  serverTotals, versionsOf, whereLabel, withServer,
} from '../src/discordLog'

let nextLogID = 1
const message = (overrides: Partial<LoggedMessage> = {}): LoggedMessage => ({
  id: nextLogID++, source: 'matrix', discord_message_id: '', format: '', formatted_body: '', media_url: '',
  media_mimetype: '', reply_to_event_id: '',
  event_id: '$one', room_id: '!general:x', room_name: '#general', platform: 'discord',
  network_id: '900', network_name: 'Reno', channel_id: '1001', channel_name: '#general',
  channel_external_url: 'https://discord.com/channels/900/1001',
  sender_user_id: '@discord_7:x', sender_display_name: 'Ann', message_type: 'm.text',
  body: 'hello', content_json: '{}', replaces_event_id: '', sent_at: '2026-09-28T07:00:00Z',
  logged_at: '2026-09-28T07:00:01Z', redacted_at: null, redaction_event_id: '', redacted_by_user_id: '',
  ...overrides,
})

const channel = (overrides: Partial<DiscordBridgedChannel> = {}): DiscordBridgedChannel => ({
  discord_channel_id: '1001', name: '#general', channel_type: 0, category_name: 'Text', matrix_room_id: '!general:x',
  admin_joined: true, last_bridged_message_at: null, logged_message_count: 3, deleted_message_count: 1,
  last_logged_message_at: null, ...overrides,
})

const status = (overrides: Partial<DiscordBridgeStatus> = {}): DiscordBridgeStatus => ({
  accounts: [{ matrix_user_id: '@admin:x', discord_user_id: '110', logged_in: true, management_room_id: '!m:x' }],
  bridged_servers: [
    { discord_server_id: '900', name: 'Reno', bridging_mode: 3, bridging_mode_name: 'everything', matrix_space_id: '!s:x',
      channels: [channel(), channel({ discord_channel_id: '1002', name: '#quiet', matrix_room_id: '!quiet:x', admin_joined: false, logged_message_count: 0, deleted_message_count: 0 }),
        channel({ discord_channel_id: '1003', name: '#no-room', matrix_room_id: '', admin_joined: false, logged_message_count: 0, deleted_message_count: 0 })] },
    { discord_server_id: '901', name: 'Pretend', bridging_mode: 3, bridging_mode_name: 'everything', matrix_space_id: '',
      channels: [channel({ discord_channel_id: '2001', name: '#general', matrix_room_id: '!pretend:x', channel_type: 5 })] },
  ],
  unbridged_server_count: 132,
  ...overrides,
})

describe('the tab in the URL', () => {
  it('reads a known tab and falls back to Messages', () => {
    expect(discordTabOf('deleted')).toBe('deleted')
    expect(discordTabOf('status')).toBe('status')
    expect(discordTabOf(null)).toBe('messages')
    expect(discordTabOf('nonsense')).toBe('messages')
  })
})

describe('messageLogQuery', () => {
  it('always asks for Discord and sends only the filters that are set', () => {
    const query = new URLSearchParams(messageLogQuery(EMPTY_MESSAGE_LOG_FILTERS, { deletedOnly: false }).split('?')[1])
    expect(Object.fromEntries(query)).toEqual({ platform: 'discord', limit: String(MESSAGE_LOG_PAGE_SIZE) })
  })

  it('carries server, channel room, trimmed text, deleted and before', () => {
    const path = messageLogQuery({ serverID: '900', roomID: '!general:x', text: '  party  ' }, { deletedOnly: true, before: '2026-09-28T07:00:00Z', limit: 50 })
    expect(path.startsWith('/message-log?')).toBe(true)
    expect(Object.fromEntries(new URLSearchParams(path.split('?')[1]))).toEqual({
      platform: 'discord', network_id: '900', room_id: '!general:x', q: 'party', deleted: 'true',
      before: '2026-09-28T07:00:00Z', limit: '50',
    })
  })
})

describe('paging', () => {
  it('offers an older page only after a full one, starting at the last row', () => {
    const full = Array.from({ length: 3 }, (_, i) => message({ event_id: `$${i}`, sent_at: `2026-09-28T0${7 - i}:00:00Z` }))
    expect(nextPageBefore(full, 3)).toBe('2026-09-28T05:00:00Z')
    expect(nextPageBefore(full.slice(0, 2), 3)).toBeNull()
    expect(nextPageBefore([], 3)).toBeNull()
  })

  it('appends an older page without repeating a message and keeps both pages\' edits and reactions', () => {
    const a = message({ event_id: '$a' })
    const thumbs = { key: '\u{1F44D}', shortcode: '', count: 1, sender_display_names: [] }
    const shown: MessageLogPage = { messages: [a], edits: { $a: [message({ event_id: '$a-edit' })] }, reactions: { $a: [thumbs] } }
    const older: MessageLogPage = {
      messages: [a, message({ event_id: '$b' })], edits: { $b: [message({ event_id: '$b-edit' })] }, reactions: { $b: [thumbs] },
    }
    const joined = appendMessageLogPage(shown, older)
    expect(joined.messages.map(m => m.event_id)).toEqual(['$a', '$b'])
    expect(Object.keys(joined.edits).sort()).toEqual(['$a', '$b'])
    expect(Object.keys(joined.reactions ?? {}).sort()).toEqual(['$a', '$b'])
  })

  it('tells archive rows apart by the log id, since they carry no event id', () => {
    const first = message({ event_id: '', source: 'discord-archive', discord_message_id: '11' })
    const second = message({ event_id: '', source: 'discord-archive', discord_message_id: '12' })
    const joined = appendMessageLogPage({ messages: [first], edits: {}, reactions: {} }, { messages: [first, second], edits: {}, reactions: {} })
    expect(joined.messages.map(m => m.discord_message_id)).toEqual(['11', '12'])
  })
})

describe('filter options from the bridge status', () => {
  it('lists only channels with a room, named with their server when every server is shown', () => {
    expect(channelOptions(status(), '').map(c => c.label)).toEqual(['Reno · #general', 'Reno · #quiet', 'Pretend · #general'])
    expect(channelOptions(status(), '900')).toEqual([{ roomID: '!general:x', label: '#general' }, { roomID: '!quiet:x', label: '#quiet' }])
    expect(channelOptions(null, '')).toEqual([])
  })

  it('clears a channel that is not in the server picked', () => {
    const picked = { serverID: '', roomID: '!pretend:x', text: 'x' }
    expect(withServer(picked, status(), '900')).toEqual({ serverID: '900', roomID: '', text: 'x' })
    expect(withServer(picked, status(), '901')).toEqual({ serverID: '901', roomID: '!pretend:x', text: 'x' })
  })
})

describe('a deleted message', () => {
  it('shows its text as first sent, then each edit in order', () => {
    const original = message({ event_id: '$m', body: 'first', redacted_at: '2026-09-28T09:00:00Z' })
    const edits = { $m: [message({ event_id: '$e1', body: 'second', sent_at: '2026-09-28T08:00:00Z', replaces_event_id: '$m' })] }
    expect(versionsOf(original, edits).map(v => [v.message.body, v.isOriginal])).toEqual([['first', true], ['second', false]])
    expect(versionsOf(original, {})).toHaveLength(1)
  })

  it('reads as its newest edit, and an archive row with no event id has no edits', () => {
    const original = message({ event_id: '$m', body: 'first' })
    const edit = message({ event_id: '$e1', body: 'second', replaces_event_id: '$m' })
    expect(currentVersionOf(original, { $m: [edit] }).body).toBe('second')
    expect(currentVersionOf(original, {}).body).toBe('first')
    const archived = message({ event_id: '', source: 'discord-archive', discord_message_id: '5' })
    expect(versionsOf(archived, { '': [edit] })).toHaveLength(1)
  })

  it('lists every bridged channel for the renderers to name', () => {
    expect(bridgedChannelsOf(status()).map(c => c.discord_channel_id)).toEqual(['1001', '1002', '1003', '2001'])
    expect(bridgedChannelsOf(null)).toEqual([])
  })

  it('knows when the log caught it only after it was gone', () => {
    expect(deletedBeforeRead(message({ body: '', redacted_at: '2026-09-28T09:00:00Z' }))).toBe(true)
    expect(deletedBeforeRead(message({ body: 'kept', redacted_at: '2026-09-28T09:00:00Z' }))).toBe(false)
    expect(deletedBeforeRead(message({ body: '' }))).toBe(false)
    expect(deletedBeforeRead(message({ body: '', media_url: 'mxc://s/i', redacted_at: '2026-09-28T09:00:00Z' }))).toBe(false)
  })

  it('names where it was from the bridge state logged with it', () => {
    expect(whereLabel(message())).toBe('Reno · #general')
    expect(whereLabel(message({ network_name: '', channel_name: '', room_name: '' }))).toBe('!general:x')
  })
})

describe('bridge status', () => {
  it('flags a channel with no room or not joined', () => {
    expect(channelProblems(channel())).toEqual([])
    expect(channelProblems(channel({ admin_joined: false }))[0]).toMatch(/not joined/)
    expect(channelProblems(channel({ matrix_room_id: '', admin_joined: false }))[0]).toMatch(/no Matrix room/)
  })

  it('says nothing is wrong only when nothing is', () => {
    const healthy = status({ bridged_servers: [{ ...status().bridged_servers[1] }] })
    expect(bridgeProblems(healthy)).toEqual([])
  })

  it('puts a logged-out account and unreadable channels at the top', () => {
    const problems = bridgeProblems(status({ accounts: [{ matrix_user_id: '@admin:x', discord_user_id: '110', logged_in: false, management_room_id: '' }] }))
    expect(problems).toEqual(['@admin:x is logged out of Discord.', 'Multichat cannot read these 2 channels: Reno #quiet, Reno #no-room.'])
    expect(bridgeProblems(status({ accounts: [] }))[0]).toMatch(/No account/)
  })

  it('totals a server and marks announcement channels', () => {
    expect(serverTotals(status().bridged_servers[0])).toEqual({ channels: 3, unreadable: 2, logged: 3, deleted: 1 })
    expect(channelLabel(status().bridged_servers[1].channels[0])).toBe('#general (announcements)')
  })
})

describe('an archived Discord message', () => {
  const archived = (object: unknown, overrides: Partial<LoggedMessage> = {}) =>
    message({ source: 'discord-archive', event_id: '', discord_message_id: '9', body: '', format: 'discord-markdown', content_json: JSON.stringify(object), ...overrides })

  it('reads a join, a pin or a boost as Discord’s own line, and any other system type by number', () => {
    expect(archivedDiscordExtras(archived({ type: 7 })).systemLabel).toBe('joined the server')
    expect(archivedDiscordExtras(archived({ type: 6 })).systemLabel).toBe('pinned a message')
    expect(archivedDiscordExtras(archived({ type: 8 })).systemLabel).toBe('boosted the server')
    expect(archivedDiscordExtras(archived({ type: 99 })).systemLabel).toBe('Discord system message (type 99)')
  })

  it('treats a plain message and a reply as a person’s, with their stickers and attachments', () => {
    const extras = archivedDiscordExtras(archived({
      type: 19, sticker_items: [{ id: '1', name: 'Wave' }], attachments: [{ filename: 'a.png', url: 'https://cdn.discordapp.com/a.png' }],
    }))
    expect(extras).toEqual({
      systemLabel: '', stickers: [{ id: '1', name: 'Wave' }],
      attachments: [{ filename: 'a.png', url: 'https://cdn.discordapp.com/a.png' }], unreadable: '',
    })
    expect(archivedDiscordExtras(archived({ type: 0, sticker_items: null, attachments: null })).systemLabel).toBe('')
  })

  it('says when the archived object cannot be read, and reads nothing from a live row', () => {
    expect(archivedDiscordExtras(archived({}, { content_json: '{not json' })).unreadable).toMatch(/cannot be read/)
    expect(archivedDiscordExtras(message({ content_json: '{"type":7}' })).systemLabel).toBe('')
  })
})
