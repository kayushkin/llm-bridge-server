import { describe, expect, it } from 'vitest'
import type { ConversationLink } from '@kayushkin/multichat-types'
import type { Account as StoreAccount, Conversation as StoreConversation, Message as StoreMessage } from '@kayushkin/message-store-types'
import {
  addToPersonChatsBody, chatLabel, conversationListRowOf, conversationListRows, defaultSendRoom, mergedHorizon, mergedMessages,
  peopleInRoom, personOtherChats, roomOwners, roomsToPageBack, shownRowOf, personParamOf,
  storedChatLabel, storedChatSendAccount, storedChatsBehindRooms, historyMessageShown, historyMessagesPath, historyOnlyChats, storedChatHasBot, type LoadedRoom, type MergedMessage,
} from '../src/conversationPeople'
import type { MultichatConversation, MultichatMessage, MultichatUnifiedContact } from '../src/types-multichat'

const room = (room_id: string, last_activity: number, extra: Partial<MultichatConversation> = {}): MultichatConversation =>
  ({ room_id, name: room_id, member_count: 2, last_activity, visibility: 'auto', hidden: false, ...extra }) as MultichatConversation
const identity = (user_id: string, platform: string) => ({ user_id, platform, display_name: user_id })
const sam: MultichatUnifiedContact = {
  person_id: 'person_000001', display_name: 'Sam',
  identities: [identity('@discord_1:x', 'discord'), identity('@gmessages_1:x', 'gmessages'), identity('@web_1:x', 'gmessagesweb')],
} as MultichatUnifiedContact
const kim: MultichatUnifiedContact = { display_name: 'Kim', identities: [identity('@whatsapp_2:x', 'whatsapp')] } as MultichatUnifiedContact
const member = (user_id: string) => ({ user_id, display_name: user_id })

const samDiscord = room('!dm-discord', 300, { platform: 'discord', direct_message_partner_user_id: '@discord_1:x', members: [member('@discord_1:x')] })
const samSMS = room('!dm-sms', 100, { platform: 'gmessages', direct_message_partner_user_id: '@gmessages_1:x', members: [member('@gmessages_1:x')] })
// The web bridge does not mark one-to-one rooms; this one is joined by a link.
const samWeb = room('!web', 250, { platform: 'gmessagesweb', members: [member('@web_1:x')] })
const group = room('!group', 400, { platform: 'gmessages', member_count: 4, members: [member('@gmessages_1:x'), member('@whatsapp_2:x')] })
const kimDM = room('!kim', 200, { platform: 'whatsapp', direct_message_partner_user_id: '@whatsapp_2:x', members: [member('@whatsapp_2:x')] })
const stranger = room('!stranger', 50, { platform: 'whatsapp', direct_message_partner_user_id: '@whatsapp_9:x' })
const all = [samDiscord, samSMS, samWeb, group, kimDM, stranger]
const links: ConversationLink[] = [{ id: 1, room_ids: ['!dm-discord', '!web'] }] as ConversationLink[]
const contacts = [sam, kim]

describe('who a room belongs to', () => {
  it('gives a one-to-one room to its partner, and a linked room to the same person', () => {
    const owners = roomOwners(all, links, contacts)
    expect(owners.get('!dm-discord')).toBe(sam)
    expect(owners.get('!dm-sms')).toBe(sam)
    expect(owners.get('!web')).toBe(sam)
    expect(owners.get('!kim')).toBe(kim)
    expect(owners.has('!group')).toBe(false)
    expect(owners.has('!stranger')).toBe(false)
  })
})

describe('the list', () => {
  it('has one row per person holding all their chats, rooms apart, newest first', () => {
    const rows = conversationListRows(all, all, links, contacts)
    expect(rows.map(row => row.key)).toEqual(['room:!group', 'person:person_000001', 'person:@whatsapp_2:x', 'room:!stranger'])
    expect(rows[1].chats.map(chat => chat.room_id)).toEqual(['!dm-discord', '!web', '!dm-sms'])
  })

  it('shows a person when any one of their chats matches, with every chat', () => {
    const rows = conversationListRows(all, [samSMS], links, contacts)
    expect(rows).toHaveLength(1)
    expect(rows[0].chats).toHaveLength(3)
  })

  it('finds a person by name when no chat of theirs is named for them', () => {
    const rows = conversationListRows(all, [], links, contacts, { text: 'sam', matchingAllButText: all })
    expect(rows.map(row => row.key)).toEqual(['person:person_000001'])
    expect(conversationListRows(all, [], links, contacts, { text: 'sam', matchingAllButText: [group] })).toEqual([])
  })

  it('keeps a person\'s page open on a group chat they are in', () => {
    const rows = conversationListRows(all, all, links, contacts)
    expect(shownRowOf(rows, '!group', 'person_000001', all, contacts)?.key).toBe('person:person_000001')
    expect(shownRowOf(rows, '!group', '@whatsapp_2:x', all, contacts)?.key).toBe('person:@whatsapp_2:x')
    expect(shownRowOf(rows, '!group', null, all, contacts)?.key).toBe('room:!group')
    // A room the named person is not in opens its own row.
    expect(shownRowOf(rows, '!stranger', 'person_000001', all, contacts)?.key).toBe('room:!stranger')
    expect(personParamOf(rows[1])).toBe('person_000001')
  })

  it('opens a room holding one known person on that person\'s page', () => {
    // An unmarked chat with Sam alone, as the web bridge makes them.
    const webOnly = room('!web-only', 500, { platform: 'gmessagesweb', members: [member('@web_1:x')] })
    const rooms = [...all, webOnly]
    const rows = conversationListRows(rooms, rooms, links, contacts)
    expect(shownRowOf(rows, '!web-only', null, rooms, contacts)?.key).toBe('person:person_000001')
    // Two known people: the room keeps its own row.
    expect(shownRowOf(rows, '!group', null, rooms, contacts)?.key).toBe('room:!group')
  })

  it('finds the row a room is in', () => {
    const rows = conversationListRows(all, all, links, contacts)
    expect(conversationListRowOf(rows, '!web')?.key).toBe('person:person_000001')
    expect(conversationListRowOf(rows, '!nowhere')).toBeNull()
  })
})

describe('a person\'s other chats', () => {
  it('lists every other room any of their accounts is in', () => {
    expect(personOtherChats(sam, [samDiscord, samWeb, samSMS], all).map(chat => chat.room_id)).toEqual(['!group'])
  })

  it('lists hidden rooms after shown ones', () => {
    const channel = room('!channel', 900, { platform: 'discord', member_count: 100, hidden: true, members: [member('@discord_1:x')] })
    expect(personOtherChats(sam, [samDiscord], [channel, group]).map(chat => chat.room_id)).toEqual(['!group', '!channel'])
  })

  it('names the people in a room', () => {
    expect(peopleInRoom(group, contacts)).toEqual([sam, kim])
  })

  it('links a room to the chat already in a link, else to the newest', () => {
    expect(addToPersonChatsBody('!group', [samSMS, samDiscord], links)).toEqual({ room_ids: ['!dm-discord', '!group'] })
    expect(addToPersonChatsBody('!group', [samSMS, kimDM], [])).toEqual({ room_ids: ['!kim', '!group'] })
    expect(addToPersonChatsBody('!group', [], [])).toBeNull()
  })

  it('labels a chat by app, with its name when two share an app', () => {
    expect(chatLabel([samDiscord, samSMS], samDiscord)).toBe('discord')
    expect(chatLabel([samSMS, group], samSMS)).toBe('gmessages · !dm-sms')
  })
})

const message = (event_id: string, timestamp: number, extra: Partial<MultichatMessage> = {}): MultichatMessage =>
  ({ event_id, sender: '@x', body: event_id, msg_type: 'm.text', timestamp, ...extra })

describe('the merged thread', () => {
  const busy: LoadedRoom = { roomID: '!a', messages: [message('a1', 500), message('a2', 600)], hasOlder: true }
  const quiet: LoadedRoom = { roomID: '!b', messages: [message('b1', 100), message('b2', 550)], hasOlder: false }

  it('starts where a room with older pages stops, so no room shows a gap', () => {
    expect(mergedHorizon([busy, quiet])).toBe(500)
    expect(mergedMessages([busy, quiet]).map(m => `${m.roomID}:${m.event_id}`)).toEqual(['!a:a1', '!b:b2', '!a:a2'])
  })

  it('shows everything once every room is loaded to its start', () => {
    expect(mergedHorizon([{ ...busy, hasOlder: false }, quiet])).toBe(0)
    expect(mergedMessages([{ ...busy, hasOlder: false }, quiet])).toHaveLength(4)
  })

  it('pages back the rooms that set the horizon', () => {
    expect(roomsToPageBack([busy, quiet])).toEqual(['!a'])
    expect(roomsToPageBack([{ ...busy, hasOlder: false }, quiet])).toEqual([])
  })

  it('folds edits per room', () => {
    const edited: LoadedRoom = { roomID: '!c', hasOlder: false, messages: [message('c1', 10), message('c2', 20, { replaces_event_id: 'c1', body: 'fixed' })] }
    expect(mergedMessages([edited]).map(m => [m.body, m.edited])).toEqual([['fixed', true]])
  })

  it('sends to the room the other person last wrote in, else the newest chat', () => {
    const merged = (roomID: string, is_me: boolean, timestamp: number) => ({ ...message('m', timestamp, { is_me }), edited: false, roomID }) as MergedMessage
    expect(defaultSendRoom([samDiscord, samSMS], [merged('!dm-sms', false, 1), merged('!dm-discord', true, 2)])).toBe('!dm-sms')
    expect(defaultSendRoom([samSMS, samDiscord], [])).toBe('!dm-discord')
  })
})

describe('history tabs', () => {
  const stored = (id: string, source: string, source_ref: string, last: number, extra: Partial<StoreConversation> = {}) =>
    ({ id, platform: source === 'matrix' ? 'sms' : source, source, source_ref, title: id, is_group: false, participants: [],
      message_count: 3, first_message_at: 1, last_message_at: last, created_at: 0, updated_at: 0, ...extra }) as StoreConversation
  const discordRoom = room('!dc', 10, { platform: 'discord', discord_channel_id: '555' })
  const smsRoom = room('!sms', 10, { platform: 'gmessages' })

  it('keeps the one-to-one chats no live room carries, newest first', () => {
    const kept = historyOnlyChats([
      stored('conversation_000001', 'discord', '555', 5), // carried by the Discord room
      stored('conversation_000002', 'matrix', '!sms', 6), // carried by the SMS room
      stored('conversation_000003', 'discord', '777', 7), // a DM with no room
      stored('conversation_000004', 'android_sms', '42', 9), // an old phone's texts
      stored('conversation_000005', 'discord', '888', 8, { is_group: true }),
      stored('conversation_000006', 'discord', '999', 10, { message_count: 0 }),
    ], [discordRoom, smsRoom], [])
    expect(kept.map(conversation => conversation.id)).toEqual(['conversation_000004', 'conversation_000003'])
  })

  it('leaves out a chat between the person and one of the operator\'s bots', () => {
    const bot: StoreAccount = { id: 'message_account_000002', platform: 'discord', handle: '200', display_name: 'Helper', kind: 'bot', created_at: 0, updated_at: 0 }
    const person = { handle: '300', display_name: 'Pat', person_id: 'person_000001', is_self: false }
    const withBot = stored('conversation_000008', 'discord', '901', 9, { participants: [person, { handle: '200', display_name: 'Helper', person_id: '', is_self: false }] })
    const withOperator = stored('conversation_000009', 'discord', '902', 8, { participants: [person, { handle: '100', display_name: 'Me', person_id: '', is_self: true }] })
    // The same handle on another app is someone else.
    const sameHandleElsewhere = stored('conversation_000010', 'telegram', '903', 7, { participants: [person, { handle: '200', display_name: 'X', person_id: '', is_self: true }] })
    expect(historyOnlyChats([withBot, withOperator, sameHandleElsewhere], [], [bot]).map(conversation => conversation.id))
      .toEqual(['conversation_000009', 'conversation_000010'])
    expect(storedChatHasBot(withBot, [bot])).toBe(true)
    expect(storedChatHasBot(withOperator, [bot])).toBe(false)
  })

  it('labels a stored chat by app, as a room is', () => {
    const a = stored('conversation_000003', 'discord', '777', 7, { title: 'midnachew' })
    const b = stored('conversation_000007', 'discord', '778', 7, { title: 'old' })
    expect(storedChatLabel([smsRoom], [a], a)).toBe('discord')
    expect(storedChatLabel([smsRoom], [a, b], a)).toBe('discord · midnachew')
    expect(storedChatLabel([discordRoom], [a], a)).toBe('discord · midnachew')
  })

  it('puts a stored chat behind the room that carries it', () => {
    expect(storedChatsBehindRooms([
      stored('conversation_000001', 'discord', '555', 5),
      stored('conversation_000003', 'discord', '777', 7),
    ], [discordRoom, smsRoom]).map(behind => [behind.roomID, behind.conversation.id])).toEqual([['!dc', 'conversation_000001']])
  })

  it('answers a stored Discord DM from the person\'s account with that Discord id', () => {
    const dm = stored('conversation_000003', 'discord', '777', 7, {
      participants: [{ handle: '110', display_name: 'me', person_id: '', is_self: true }, { handle: '1', display_name: 'Sam', person_id: 'person_000001', is_self: false }],
    })
    expect(storedChatSendAccount(dm, sam)).toBe('@discord_1:x')
    expect(storedChatSendAccount({ ...dm, is_group: true }, sam)).toBeNull()
    expect(storedChatSendAccount(stored('conversation_000004', 'android_sms', '42', 9), sam)).toBeNull()
  })

  it('pages a stored conversation newest first', () => {
    expect(historyMessagesPath('conversation_000454')).toBe('/messages?conversation=conversation_000454&order=newest&limit=50')
    expect(historyMessagesPath('conversation_000454', '1790304717000:58352'))
      .toBe('/messages?conversation=conversation_000454&order=newest&limit=50&cursor=1790304717000%3A58352')
  })

  it('draws a stored message as a room message', () => {
    const stub = {
      id: 'chat_message_000002', conversation_id: 'conversation_000003', sent_at: 2000, sender_handle: '110', sender_name: 'Kuxir',
      sender_person_id: '', from_self: true, kind: 'text', body: '', attachments: [], source: 'discord', source_ref: 'm2', imported_by: 'message_import_000001',
    } as StoreMessage
    const shown = historyMessageShown({
      id: 'chat_message_000001', conversation_id: 'conversation_000003', sent_at: 1000, sender_handle: '110', sender_name: 'Kuxir',
      sender_person_id: '', from_self: true, kind: 'text', body: '**hi**', attachments: [{ name: 'cat.png' }],
      source: 'discord', source_ref: 'm1', imported_by: 'message_import_000001',
    } as StoreMessage)
    expect(shown).toMatchObject({ event_id: 'chat_message_000001', timestamp: 1000, is_me: true, format: 'discord-markdown', body: '**hi**\n📎 cat.png' })
    expect(historyMessageShown({ ...stub, body: '', attachments: [] }).body).toBe('(no text kept)')
  })
})
