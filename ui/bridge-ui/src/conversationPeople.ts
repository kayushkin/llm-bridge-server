// The Conversations list by person, and the merged thread of one person's
// chats, as pure rules so they are tested (test/conversationPeople.test.ts).
// The page is components/messages/BridgeMessageConversations.tsx.
//
// A person is a row of multichat's unified contacts: people-store's person and
// every chat account linked to them. A person's chats are the one-to-one rooms
// with any of their accounts — the rooms multichat gives a
// `direct_message_partner_user_id` — and every room linked with one of those
// (multichat's `/conversations/links`), which is how a chat the bridge does not
// mark one-to-one (Google Messages for web) joins them.

import type { ConversationLink } from '@kayushkin/multichat-types'
import { conversationEntries, conversationShownName, foldEdits, type ConversationEntry, type ShownMessage } from './multichatMessages'
import type { Account as StoreAccount, Conversation as StoreConversation, Message as StoreMessage } from '@kayushkin/message-store-types'
import type { MultichatConversation, MultichatMessage, MultichatUnifiedContact } from './types-multichat'

/** One row of the list: a person and their chats, or a room (or the rooms of
 *  one link) that belongs to no person. `chats` is newest first. */
export type ConversationListRow =
  | { kind: 'person'; key: string; person: MultichatUnifiedContact; chats: MultichatConversation[] }
  | { kind: 'room'; key: string; entry: ConversationEntry; chats: MultichatConversation[] }

const newestFirst = (a: MultichatConversation, b: MultichatConversation) => b.last_activity - a.last_activity

/** The key a person's row goes by: people-store's id, or the one account of
 *  someone linked to no one. */
export function personKey(person: MultichatUnifiedContact): string {
  return person.person_id ?? person.identities[0]?.user_id ?? person.display_name
}

/** Which person each chat account belongs to, by Matrix user id. */
function personByUserID(contacts: readonly MultichatUnifiedContact[]): Map<string, MultichatUnifiedContact> {
  const byUserID = new Map<string, MultichatUnifiedContact>()
  for (const contact of contacts) for (const identity of contact.identities) byUserID.set(identity.user_id, contact)
  return byUserID
}

/** The person each room belongs to, by room id: a one-to-one room goes to the
 *  other person in it, and every room of a link that holds one goes with it.
 *  A link holding two people's one-to-one rooms goes to the first listed. */
export function roomOwners(
  all: readonly MultichatConversation[], links: readonly ConversationLink[], contacts: readonly MultichatUnifiedContact[],
): Map<string, MultichatUnifiedContact> {
  const byUserID = personByUserID(contacts)
  const owners = new Map<string, MultichatUnifiedContact>()
  for (const conversation of all) {
    const partner = conversation.direct_message_partner_user_id
    const person = partner ? byUserID.get(partner) : undefined
    if (person) owners.set(conversation.room_id, person)
  }
  for (const link of links) {
    const owner = link.room_ids.map(roomID => owners.get(roomID)).find(Boolean)
    if (!owner) continue
    for (const roomID of link.room_ids) if (!owners.has(roomID)) owners.set(roomID, owner)
  }
  return owners
}

/** Whether the person's name or any account's name contains the typed text. */
export function personMatchesText(person: MultichatUnifiedContact, text: string): boolean {
  const lower = text.trim().toLowerCase()
  if (!lower) return false
  return person.display_name.toLowerCase().includes(lower)
    || person.identities.some(identity => (identity.display_name ?? '').toLowerCase().includes(lower))
}

/** The list's rows for the rooms that passed the filter, newest first: one per
 *  person with a matching chat, holding all their chats, and one per other
 *  room or link, as `conversationEntries` groups them. A person whose name
 *  matches `byName.text` is listed too when one of their chats is in
 *  `byName.matchingAllButText` — the rooms that pass every filter but the
 *  text — since their chats can be named anything ("D" on Discord). */
export function conversationListRows(
  all: readonly MultichatConversation[], matching: readonly MultichatConversation[],
  links: readonly ConversationLink[], contacts: readonly MultichatUnifiedContact[],
  byName?: { text: string; matchingAllButText: readonly MultichatConversation[] },
): ConversationListRow[] {
  const owners = roomOwners(all, links, contacts)
  const chatsOf = new Map<string, MultichatConversation[]>()
  for (const conversation of all) {
    const owner = owners.get(conversation.room_id)
    if (!owner) continue
    const key = personKey(owner)
    chatsOf.set(key, [...(chatsOf.get(key) ?? []), conversation])
  }
  const rows: ConversationListRow[] = []
  const peopleTaken = new Set<string>()
  const unowned: MultichatConversation[] = []
  const matchingIDs = new Set(matching.map(conversation => conversation.room_id))
  const named = byName && byName.text.trim()
    ? byName.matchingAllButText.filter(conversation => {
      const owner = owners.get(conversation.room_id)
      return !matchingIDs.has(conversation.room_id) && !!owner && personMatchesText(owner, byName.text)
    })
    : []
  for (const conversation of [...matching, ...named]) {
    const owner = owners.get(conversation.room_id)
    if (!owner) { unowned.push(conversation); continue }
    const key = personKey(owner)
    if (peopleTaken.has(key)) continue
    peopleTaken.add(key)
    rows.push({ kind: 'person', key: `person:${key}`, person: owner, chats: [...(chatsOf.get(key) ?? [])].sort(newestFirst) })
  }
  for (const entry of conversationEntries(all, unowned, links)) {
    rows.push({ kind: 'room', key: `room:${entry.conversations[0].room_id}`, entry, chats: entry.conversations })
  }
  return rows.sort((a, b) => newestFirst(a.chats[0], b.chats[0]))
}

/** The row a room is in, among rows built from every room. */
export function conversationListRowOf(rows: readonly ConversationListRow[], roomID: string): ConversationListRow | null {
  return rows.find(row => row.chats.some(chat => chat.room_id === roomID)) ?? null
}

/** The `?person=` value that keeps a person's page open while one of their
 *  chats, or a group chat they are in, is shown: the key after `person:`. */
export function personParamOf(row: ConversationListRow): string | null {
  return row.kind === 'person' ? personKey(row.person) : null
}

/** The row the page shows for an open room. With `?person=` naming someone
 *  the room is one of — their own chats, or any other room one of their
 *  accounts is in — it is that person's row, so moving between their chats
 *  stays on their page. A room that belongs to no one but holds exactly one
 *  person with a row opens on that person's page too: the Google Messages
 *  for web bridge marks no room one-to-one, so a chat with one person there
 *  would otherwise open with none of their other chats in reach. Otherwise
 *  it is the row the room is listed in. */
export function shownRowOf(
  rows: readonly ConversationListRow[], roomID: string, personParam: string | null,
  all: readonly MultichatConversation[], contacts: readonly MultichatUnifiedContact[],
): ConversationListRow | null {
  if (personParam) {
    const personRow = rows.find(row => row.key === `person:${personParam}`)
    if (personRow && personRow.kind === 'person'
      && (personRow.chats.some(chat => chat.room_id === roomID)
        || personOtherChats(personRow.person, personRow.chats, all).some(chat => chat.room_id === roomID))) {
      return personRow
    }
  }
  const listed = conversationListRowOf(rows, roomID)
  if (listed?.kind !== 'room') return listed
  const room = all.find(conversation => conversation.room_id === roomID)
  const personRows = room ? peopleInRoom(room, contacts).flatMap(person => rows.filter(row => row.key === `person:${personKey(person)}`)) : []
  return personRows.length === 1 ? personRows[0] : listed
}

/** The name a row shows: the person's, or the room's. */
export function conversationListRowName(row: ConversationListRow): string {
  return row.kind === 'person' ? row.person.display_name : conversationShownName(row.chats[0])
}

/** Every other room any of the person's accounts is in: their group chats
 *  and server channels, and one-to-one rooms not yet theirs. Shown rooms come
 *  first and hidden ones (big server channels) after, each newest first, so a
 *  busy channel does not push a small group chat out of sight. */
export function personOtherChats(
  person: MultichatUnifiedContact, chats: readonly MultichatConversation[], all: readonly MultichatConversation[],
): MultichatConversation[] {
  const accounts = new Set(person.identities.map(identity => identity.user_id))
  const own = new Set(chats.map(chat => chat.room_id))
  return all.filter(conversation => !own.has(conversation.room_id)
    && (conversation.members ?? []).some(member => accounts.has(member.user_id)))
    .sort((a, b) => Number(a.hidden) - Number(b.hidden) || newestFirst(a, b))
}

/** The people in a room who have a row of their own in the list, so the room
 *  can point to them and be made one of their chats. */
export function peopleInRoom(
  conversation: MultichatConversation, contacts: readonly MultichatUnifiedContact[],
): MultichatUnifiedContact[] {
  const byUserID = personByUserID(contacts)
  const people = new Map<string, MultichatUnifiedContact>()
  for (const member of conversation.members ?? []) {
    const person = byUserID.get(member.user_id)
    if (person) people.set(personKey(person), person)
  }
  return [...people.values()]
}

/** The link body that makes a room one of the person's chats: the room with
 *  one of their chats already in a link (multichat adds a room to the link of
 *  the other), else with their newest chat. Null when they have none. */
export function addToPersonChatsBody(
  roomID: string, chats: readonly MultichatConversation[], links: readonly ConversationLink[],
): { room_ids: string[] } | null {
  const linked = new Set(links.flatMap(link => link.room_ids))
  const anchor = chats.find(chat => linked.has(chat.room_id)) ?? [...chats].sort(newestFirst)[0]
  return anchor ? { room_ids: [anchor.room_id, roomID] } : null
}

/** What a chat's tab and a send button call it among the person's chats: the
 *  app, and the room's name too when two of the chats are on one app. */
export function chatLabel(chats: readonly MultichatConversation[], chat: MultichatConversation): string {
  const platform = chat.platform || 'unknown app'
  const sharesApp = chats.filter(other => other.platform === chat.platform).length > 1
  return sharesApp ? `${platform} · ${conversationShownName(chat)}` : platform
}

/** One room's loaded messages in a merged thread. `hasOlder` says multichat
 *  holds older pages than the oldest loaded. */
export interface LoadedRoom {
  roomID: string
  messages: readonly MultichatMessage[]
  hasOlder: boolean
}

/** A message in a merged thread, with the room it came from. */
export type MergedMessage = ShownMessage & { roomID: string }

/** The time a merged thread starts at: the newest of the oldest loaded
 *  message of every room with older pages still to load. Before it, a room
 *  could hold messages not loaded yet, so the thread shows nothing older and
 *  never shows one room's past with a gap where another's should be. 0 when
 *  every room is loaded to its start. */
export function mergedHorizon(rooms: readonly LoadedRoom[]): number {
  let horizon = 0
  for (const room of rooms) {
    if (!room.hasOlder) continue
    const oldest = room.messages.length > 0 ? room.messages[0].timestamp : Number.POSITIVE_INFINITY
    horizon = Math.max(horizon, oldest)
  }
  return horizon
}

/** Every room's messages, edits folded, in time order, from the horizon on. */
export function mergedMessages(rooms: readonly LoadedRoom[]): MergedMessage[] {
  const horizon = mergedHorizon(rooms)
  return rooms
    .flatMap(room => foldEdits(room.messages).map(message => ({ ...message, roomID: room.roomID })))
    .filter(message => message.timestamp >= horizon)
    .sort((a, b) => a.timestamp - b.timestamp)
}

/** The rooms to page back to move the horizon: those whose oldest loaded
 *  message sets it. */
export function roomsToPageBack(rooms: readonly LoadedRoom[]): string[] {
  const horizon = mergedHorizon(rooms)
  if (horizon === 0) return []
  return rooms.filter(room => room.hasOlder
    && (room.messages.length === 0 ? Number.POSITIVE_INFINITY : room.messages[0].timestamp) >= horizon)
    .map(room => room.roomID)
}

/** Where a merged thread sends until the reader picks: the room of the newest
 *  message the other person sent, else the most recently active chat. */
export function defaultSendRoom(chats: readonly MultichatConversation[], messages: readonly MergedMessage[]): string {
  for (let index = messages.length - 1; index >= 0; index--) {
    if (!messages[index].is_me) return messages[index].roomID
  }
  return [...chats].sort(newestFirst)[0]?.room_id ?? ''
}

/** How many stored messages a history tab loads at a time. */
export const HISTORY_PAGE_SIZE = 50

/** message-store's conversations of one person: `GET /conversations?person=`. */
export function personHistoryConversationsPath(personID: string): string {
  return `/conversations?person=${encodeURIComponent(personID)}&limit=200`
}

/** A page of a stored conversation, newest first: `cursor` is the `next` of
 *  the page before, to go further back. */
export function historyMessagesPath(conversationID: string, cursor?: string | null): string {
  const query = new URLSearchParams({ conversation: conversationID, order: 'newest', limit: String(HISTORY_PAGE_SIZE) })
  if (cursor) query.set('cursor', cursor)
  return `/messages?${query.toString()}`
}

/** The live room that carries a stored conversation, or null: one
 *  message-store imported from a Matrix room carries that room's id, and one
 *  from Discord carries the Discord channel id multichat reports on the room. */
export function storedChatRoom(conversation: StoreConversation, rooms: readonly MultichatConversation[]): MultichatConversation | null {
  if (conversation.source === 'matrix') return rooms.find(room => room.room_id === conversation.source_ref) ?? null
  if (conversation.source === 'discord') return rooms.find(room => room.discord_channel_id === conversation.source_ref) ?? null
  return null
}

/** The stored conversations behind the open rooms, each with its room: the
 *  room's past from before it was bridged, shown under its own messages. */
export function storedChatsBehindRooms(
  stored: readonly StoreConversation[], rooms: readonly MultichatConversation[],
): { roomID: string; conversation: StoreConversation }[] {
  return stored.flatMap(conversation => {
    const room = storedChatRoom(conversation, rooms)
    return room && conversation.message_count > 0 ? [{ roomID: room.room_id, conversation }] : []
  })
}

/** The person's account a stored one-to-one Discord chat with no room would
 *  be answered from: their Discord puppet whose Discord id is the other
 *  participant's handle. Null for any other app, where no route opens a room
 *  for a stored chat. */
export function storedChatSendAccount(conversation: StoreConversation, person: MultichatUnifiedContact): string | null {
  if (conversation.source !== 'discord' || conversation.is_group) return null
  const handles = new Set((conversation.participants ?? []).filter(participant => !participant.is_self).map(participant => participant.handle))
  const account = person.identities.find(identity => {
    const localpart = identity.user_id.split(':')[0]
    return localpart.startsWith('@discord_') && handles.has(localpart.slice('@discord_'.length))
  })
  return account?.user_id ?? null
}

/** Whether one of the operator's bots takes part in a stored conversation:
 *  a participant has a bot account's handle on the conversation's platform. */
export function storedChatHasBot(conversation: StoreConversation, botAccounts: readonly StoreAccount[]): boolean {
  return (conversation.participants ?? []).some(participant =>
    botAccounts.some(bot => bot.platform === conversation.platform && bot.handle === participant.handle))
}

/** The person's one-to-one chats that message-store keeps and no live room
 *  carries — a Discord DM the bridge never made a room for, an old phone's
 *  texts — newest first. Each is a read-only tab on their page. A chat
 *  between the person and one of the operator's bots (`botAccounts`, the
 *  store's accounts of kind bot) is the bot's, not the operator's, so it is
 *  left out. */
export function historyOnlyChats(
  stored: readonly StoreConversation[], rooms: readonly MultichatConversation[], botAccounts: readonly StoreAccount[],
): StoreConversation[] {
  return stored.filter(conversation => !conversation.is_group && conversation.message_count > 0
    && !storedChatRoom(conversation, rooms) && !storedChatHasBot(conversation, botAccounts))
    .sort((a, b) => b.last_message_at - a.last_message_at)
}

/** A stored chat's tab label, as a room's is: the app, with the chat's title
 *  too when another of the person's chats is on the same app. */
export function storedChatLabel(
  chats: readonly MultichatConversation[], stored: readonly StoreConversation[], conversation: StoreConversation,
): string {
  const sharesApp = chats.some(chat => chat.platform === conversation.platform)
    || stored.filter(other => other.platform === conversation.platform).length > 1
  return sharesApp ? `${conversation.platform} · ${conversation.title}` : conversation.platform
}

/** A stored message drawn as a thread draws a room's: its id, sender, text
 *  and time. A Discord message's text is Discord markdown. Attachments are
 *  named in the text, since message-store keeps no file to show. */
export function historyMessageShown(message: StoreMessage): MultichatMessage {
  const attachmentNames = (message.attachments ?? []).map(attachment => attachment.name || attachment.mime_type || 'a file')
  const attachmentText = attachmentNames.length > 0 ? `📎 ${attachmentNames.join(', ')}` : ''
  return {
    event_id: message.id,
    sender: message.sender_handle,
    sender_name: message.sender_name,
    // A message kept with no text and no file (a sticker, an embed) says so
    // rather than drawing an empty bubble.
    body: [message.body, attachmentText].filter(Boolean).join('\n') || '(no text kept)',
    msg_type: 'm.text',
    timestamp: message.sent_at,
    is_me: message.from_self,
    format: message.source === 'discord' ? 'discord-markdown' : '',
  }
}
