import { describe, expect, it } from 'vitest'
import type { Account, Conversation, Message } from '@kayushkin/message-store-types'
import {
  accountConversationMessagesPath, accountConversationName, accountConversationParameters, accountConversationsPath, accountPickParameters,
  attachmentNamesOf, buttonChipOf, embedHasContent, messageSentByAccount, pickableBotAccounts, pickedBotAccount, safeWebAddress,
  storedMessageFormat, storedMessageIsEmpty, accountConversationKind, accountDiscordNames, accountDraftProblem, accountSendPath, countLabel,
  DISCORD_MESSAGE_LIMIT, MESSAGE_STORE_ROUTES_CALLED,
} from '../src/messageAccounts'
import { MULTICHAT_ROUTES_CALLED } from '../src/multichatMessages'

const account = (id: string, handle: string, kind: string, display_name = handle): Account =>
  ({ id, platform: 'discord', handle, display_name, kind, credential_id: '', created_at: 1, updated_at: 1 })
const bot = account('message_account_000002', '200', 'bot', 'Helper')
const otherBot = account('message_account_000003', '300', 'bot', 'Planner')
const user = account('message_account_000001', '100', 'user', 'Me')

const participant = (handle: string, display_name: string, is_self = false) => ({ handle, display_name, person_id: '', is_self })
const conversation = (extra: Partial<Conversation>): Conversation => ({
  id: 'conversation_000001', platform: 'discord', source: 'discord', source_ref: 'channel', title: '', is_group: false, parent_source_ref: '',
  participants: [], message_count: 3, first_message_at: 1, last_message_at: 2, created_at: 1, updated_at: 1, ...extra,
})
const message = (extra: Partial<Message>): Message => ({
  id: 'chat_message_000001', conversation_id: 'conversation_000001', sent_at: 1, sender_handle: '200', sender_name: 'Helper',
  sender_person_id: '', from_self: false, kind: 'text', body: '', attachments: [], embeds: [], buttons: [],
  source: 'discord', source_ref: 'm1', imported_by: 'test', ...extra,
})

describe('the account picker', () => {
  it('offers only bots, in the store order', () => {
    expect(pickableBotAccounts([user, bot, otherBot]).map(a => a.id)).toEqual([bot.id, otherBot.id])
  })
  it('finds the picked bot, and nothing for "Me", a user account or an unknown id', () => {
    expect(pickedBotAccount([user, bot], bot.id)).toBe(bot)
    expect(pickedBotAccount([user, bot], '')).toBeNull()
    expect(pickedBotAccount([user, bot], user.id)).toBeNull()
    expect(pickedBotAccount([user, bot], 'message_account_000099')).toBeNull()
  })
  it('keeps only the account and its open conversation in the URL', () => {
    expect(accountPickParameters('').toString()).toBe('')
    expect(accountPickParameters(bot.id).toString()).toBe('account=message_account_000002')
    expect(accountConversationParameters(bot.id, 'conversation_000004').toString()).toBe('account=message_account_000002&conversation=conversation_000004')
    expect(accountConversationParameters(bot.id, '').toString()).toBe('account=message_account_000002')
  })
})

describe('message-store paths', () => {
  it('asks for an account’s conversations and a conversation’s pages newest first', () => {
    expect(accountConversationsPath(bot.id)).toBe('/conversations?account=message_account_000002&limit=500')
    expect(accountConversationMessagesPath('conversation_000001')).toBe('/messages?conversation=conversation_000001&order=newest&limit=50')
    expect(accountConversationMessagesPath('conversation_000001', 'abc')).toBe('/messages?conversation=conversation_000001&order=newest&limit=50&cursor=abc')
  })
})

describe('accountConversationName', () => {
  it('names a direct message by whoever is not the bot', () => {
    const direct = conversation({ title: 'Helper', participants: [participant('100', 'Pat', true), participant('200', 'Helper')] })
    expect(accountConversationName(direct, bot)).toBe('Pat')
  })
  it('names a group by its title', () => {
    expect(accountConversationName(conversation({ is_group: true, title: 'general', participants: [participant('200', 'Helper')] }), bot)).toBe('general')
  })
  it('falls to the title, then the id, when nobody else is known', () => {
    expect(accountConversationName(conversation({ title: 'kept', participants: [participant('200', 'Helper')] }), bot)).toBe('kept')
    expect(accountConversationName(conversation({ participants: [] }), bot)).toBe('conversation_000001')
  })
})

describe('a stored message in a bot’s view', () => {
  it('is the bot’s when the bot’s handle sent it, whoever the operator is', () => {
    expect(messageSentByAccount(message({ sender_handle: '200', from_self: false }), bot)).toBe(true)
    expect(messageSentByAccount(message({ sender_handle: '100', from_self: true }), bot)).toBe(false)
  })
  it('renders Discord text as Discord markdown', () => {
    expect(storedMessageFormat({ source: 'discord' })).toBe('discord-markdown')
    expect(storedMessageFormat({ source: 'whatsapp' })).toBe('')
  })
  it('names its attachments', () => {
    expect(attachmentNamesOf(message({ attachments: [{ name: 'a.png' }, { mime_type: 'image/gif' }, {}] }))).toEqual(['a.png', 'image/gif', 'a file'])
  })
  it('is not empty when a card or a button is all it has', () => {
    expect(storedMessageIsEmpty(message({}))).toBe(true)
    expect(storedMessageIsEmpty(message({ embeds: [{}] }))).toBe(true)
    expect(storedMessageIsEmpty(message({ embeds: [{ fields: [{ name: 'When', value: 'Friday' }] }] }))).toBe(false)
    expect(storedMessageIsEmpty(message({ buttons: [{ label: 'Join' }] }))).toBe(false)
  })
  it('counts an embed with only fields as content', () => {
    expect(embedHasContent({ fields: [{ name: 'Where', value: 'Here', inline: true }] })).toBe(true)
    expect(embedHasContent({ url: 'https://example.com' })).toBe(false)
  })
})

describe('buttons and links', () => {
  it('links only http and https', () => {
    expect(safeWebAddress('https://example.com/a')).toBe('https://example.com/a')
    expect(safeWebAddress('javascript:alert(1)')).toBeNull()
    expect(safeWebAddress('not a url')).toBeNull()
    expect(safeWebAddress(undefined)).toBeNull()
  })
  it('makes a link button a link and an action button a chip', () => {
    expect(buttonChipOf({ label: 'Open', emoji: '🔗', url: 'https://example.com' })).toEqual({ text: '🔗 Open', link: 'https://example.com/', disabled: false })
    expect(buttonChipOf({ label: 'Join' })).toEqual({ text: 'Join', link: null, disabled: false })
    expect(buttonChipOf({ label: 'Closed', url: 'https://example.com', disabled: true })).toEqual({ text: 'Closed', link: null, disabled: true })
  })
})

describe('threads, mentions, counts and sending', () => {
  const channel = conversation({ id: 'conversation_000010', source_ref: '700', title: 'events', is_group: true })
  const thread = conversation({ id: 'conversation_000011', source_ref: '701', title: 'picnic', is_group: true, parent_source_ref: '700' })
  const orphanThread = conversation({ id: 'conversation_000012', source_ref: '702', title: 'hike', is_group: true, parent_source_ref: '799' })
  const direct = conversation({ participants: [participant('200', 'Helper'), participant('42', 'Carol')] })

  it('badges threads, channels and direct messages, and names a thread by its channel when the list holds it', () => {
    expect([thread, channel, direct].map(accountConversationKind)).toEqual(['thread', 'channel', 'direct message'])
    expect(accountConversationName(thread, bot, [channel, thread])).toBe('events › picnic')
    expect(accountConversationName(orphanThread, bot, [channel])).toBe('hike')
    expect(accountConversationName(direct, bot)).toBe('Carol')
  })

  it('names mentions by the conversation\'s people and channels by the account\'s list', () => {
    const names = accountDiscordNames(direct, [channel, thread, direct])
    expect(names.userNames.get('42')).toBe('Carol')
    expect(names.channelNames.get('700')).toBe('events')
    expect(names.channelNames.has('channel')).toBe(false)
  })

  it('names someone mentioned in a channel they never wrote in from the account\'s other conversations, the open one first', () => {
    const quiet = conversation({ id: 'conversation_000020', source_ref: '710', title: 'reminders', is_group: true, participants: [participant('200', 'Helper')] })
    const elsewhere = conversation({ id: 'conversation_000021', source_ref: '711', title: 'chat', is_group: true, participants: [participant('42', 'Carol R')] })
    expect(accountDiscordNames(quiet, [quiet, elsewhere]).userNames.get('42')).toBe('Carol R')
    const here = conversation({ id: 'conversation_000022', participants: [participant('42', 'Carol')] })
    expect(accountDiscordNames(here, [elsewhere, here]).userNames.get('42')).toBe('Carol')
  })

  it('says one message and one person, and plurals otherwise', () => {
    expect(countLabel(1, 'message', 'messages')).toBe('1 message')
    expect(countLabel(0, 'message', 'messages')).toBe('0 messages')
    expect(countLabel(2, 'person', 'people')).toBe('2 people')
  })

  it('sends to the route under the account and conversation, and refuses an empty or too long draft', () => {
    expect(accountSendPath('message_account_000002', 'conversation_000010')).toBe('/message-store/accounts/message_account_000002/conversations/conversation_000010/messages')
    expect(accountDraftProblem('   ')).not.toBeNull()
    expect(accountDraftProblem('a'.repeat(DISCORD_MESSAGE_LIMIT))).toBeNull()
    expect(accountDraftProblem('a'.repeat(DISCORD_MESSAGE_LIMIT + 1))).toContain(String(DISCORD_MESSAGE_LIMIT + 1))
  })
})

describe('the routes the bot views call through multichat', () => {
  it('are on the list the host proxy must forward', () => {
    for (const route of MESSAGE_STORE_ROUTES_CALLED) expect(MULTICHAT_ROUTES_CALLED).toContain(route)
  })
})
