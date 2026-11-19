// The Conversations page's account picker: the operator's own chats ("Me",
// read from multichat) or what one of their bots sent and received, read
// from message-store. These are the pure rules of the bot view
// (test/messageAccounts.test.ts); BridgeMessageAccountView draws them.
import type { Account, Button, Conversation, Embed, Message } from '@kayushkin/message-store-types'
import type { DiscordNames } from './messageBody'

/** The URL parameter that holds the picked account's id; absent means "Me". */
export const ACCOUNT_PARAMETER = 'account'
/** The URL parameter that holds the open conversation in an account's view. */
export const ACCOUNT_CONVERSATION_PARAMETER = 'conversation'
/** The `kind` message-store gives a bot the user runs (one of its `account_kinds`). */
export const BOT_ACCOUNT_KIND: Account['kind'] = 'bot'
/** How many conversations an account's list asks for. */
export const ACCOUNT_CONVERSATIONS_LIMIT = 500
/** How many messages one page of an account's conversation holds. */
export const ACCOUNT_MESSAGES_PAGE_SIZE = 50

export const ACCOUNTS_PATH = '/accounts'

/** Where multichat carries message-store's records, under the host's
 *  multichat base path (multichat's internal/api/message_store.go): the paths
 *  below are message-store's own, read there unchanged. */
export const MESSAGE_STORE_THROUGH_MULTICHAT = '/message-store'

/** The multichat routes the bot views and a person's stored chats call, as
 *  `METHOD path`, for the host's proxy allowlist. */
export const MESSAGE_STORE_ROUTES_CALLED: readonly string[] = [
  'GET /message-store/accounts',
  'GET /message-store/conversations',
  'GET /message-store/messages',
  'POST /message-store/accounts/{account_id}/conversations/{conversation_id}/messages',
]

/** The conversations an account takes part in, newest activity first. */
export function accountConversationsPath(accountID: string): string {
  const query = new URLSearchParams({ account: accountID, limit: String(ACCOUNT_CONVERSATIONS_LIMIT) })
  return `/conversations?${query.toString()}`
}

/** One page of a conversation, newest first; `cursor` is the previous page's `next`. */
export function accountConversationMessagesPath(conversationID: string, cursor?: string | null): string {
  const query = new URLSearchParams({ conversation: conversationID, order: 'newest', limit: String(ACCOUNT_MESSAGES_PAGE_SIZE) })
  if (cursor) query.set('cursor', cursor)
  return `/messages?${query.toString()}`
}

/** The accounts the picker offers beside "Me": the bots, in the store's order. */
export function pickableBotAccounts(accounts: readonly Account[]): Account[] {
  return accounts.filter(account => account.kind === BOT_ACCOUNT_KIND)
}

/** The account the URL names, or null for "Me" — and for an id the store
 *  does not list as a bot, which the page reports rather than hides. */
export function pickedBotAccount(accounts: readonly Account[], accountID: string): Account | null {
  if (!accountID) return null
  return pickableBotAccounts(accounts).find(account => account.id === accountID) ?? null
}

/** Whether a conversation is a thread: message-store names the channel a
 *  thread hangs off in `parent_source_ref`. */
export function conversationIsThread(conversation: Pick<Conversation, 'parent_source_ref'>): boolean {
  return !!conversation.parent_source_ref
}

/** What a conversation is, for its badge in an account's list. */
export function accountConversationKind(conversation: Conversation): 'thread' | 'channel' | 'direct message' {
  if (!conversation.is_group) return 'direct message'
  return conversationIsThread(conversation) ? 'thread' : 'channel'
}

/** A conversation's name in an account's list: a group's title — a thread's
 *  as `channel › thread` when the account's list holds its channel — and a
 *  direct message named by whoever in it is not the account. */
export function accountConversationName(conversation: Conversation, account: Account, conversations: readonly Conversation[] = []): string {
  if (conversation.is_group) {
    const title = conversation.title || conversation.id
    if (!conversationIsThread(conversation)) return title
    const parent = conversations.find(other => other.source === conversation.source && other.source_ref === conversation.parent_source_ref)
    return parent?.title ? `${parent.title} › ${title}` : title
  }
  const others = (conversation.participants ?? []).filter(participant => participant.handle !== account.handle)
  const names = others.map(participant => participant.display_name || participant.handle)
  return names.length > 0 ? names.join(', ') : conversation.title || conversation.id
}

/** Whether the account sent the message, rather than received or read it. */
export function messageSentByAccount(message: Message, account: Account): boolean {
  return message.sender_handle === account.handle
}

/** The renderer format of a stored message's text: Discord's markdown for a
 *  Discord message, plain text otherwise. */
export function storedMessageFormat(message: Pick<Message, 'source'>): string {
  return message.source === 'discord' ? 'discord-markdown' : ''
}

/** What a message's attachments are called, for a line under its text. */
export function attachmentNamesOf(message: Message): string[] {
  return (message.attachments ?? []).map(attachment => attachment.name || attachment.mime_type || 'a file')
}

/** A web address an embed or button may link to or load: http and https only. */
export function safeWebAddress(address: string | undefined): string | null {
  if (!address) return null
  let parsed: URL
  try {
    parsed = new URL(address)
  } catch {
    return null
  }
  return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? parsed.href : null
}

/** A button's chip: its words, and where a link button leads. A button that
 *  runs something on the bot has no link and is drawn but not pressable. */
export function buttonChipOf(button: Button): { text: string; link: string | null; disabled: boolean } {
  const text = [button.emoji, button.label].filter(Boolean).join(' ')
  return { text: text || '(button)', link: button.disabled ? null : safeWebAddress(button.url), disabled: !!button.disabled }
}

/** Whether an embed has anything a reader would see. */
export function embedHasContent(embed: Embed): boolean {
  return !!(embed.title || embed.description || embed.author_name || embed.footer_text || embed.image_url || embed.thumbnail_url
    || (embed.fields ?? []).length > 0)
}

/** Whether a message has nothing to draw at all: no text, file, card or button. */
export function storedMessageIsEmpty(message: Message): boolean {
  return !message.body && attachmentNamesOf(message).length === 0
    && !(message.embeds ?? []).some(embedHasContent) && (message.buttons ?? []).length === 0
}

/** The page's parameters for a picked account: `account` set (or removed for
 *  "Me"), and everything the other view kept dropped, since its rooms and
 *  people mean nothing in the other. */
export function accountPickParameters(accountID: string): URLSearchParams {
  const next = new URLSearchParams()
  if (accountID) next.set(ACCOUNT_PARAMETER, accountID)
  return next
}

/** An account's conversation list with the open one marked by the URL. */
export function accountConversationParameters(accountID: string, conversationID: string): URLSearchParams {
  const next = accountPickParameters(accountID)
  if (conversationID) next.set(ACCOUNT_CONVERSATION_PARAMETER, conversationID)
  return next
}

/** A count with its noun, singular for one: "1 message", "3 messages". */
export function countLabel(count: number, singular: string, plural: string): string {
  return `${count} ${count === 1 ? singular : plural}`
}

/** Names for the Discord tokens in an account's conversation: people by
 *  their handle (their Discord id), from the open conversation's participants
 *  first and then every other conversation of the account, since a message
 *  can name someone who never wrote where it was posted; channels by the
 *  account's conversations (a channel's source_ref is its Discord id). */
export function accountDiscordNames(conversation: Conversation, conversations: readonly Conversation[]): DiscordNames {
  const userNames = new Map<string, string>()
  for (const holder of [conversation, ...conversations]) {
    for (const participant of holder.participants ?? []) {
      if (participant.display_name && !userNames.has(participant.handle)) userNames.set(participant.handle, participant.display_name)
    }
  }
  const channelNames = new Map<string, string>()
  for (const other of conversations) {
    if (other.source === 'discord' && other.is_group && other.title) channelNames.set(other.source_ref, other.title)
  }
  return { channelNames, userNames }
}

/** The most characters Discord takes in one message; the send route refuses more. */
export const DISCORD_MESSAGE_LIMIT = 2000
/** How close to the limit the composer starts showing the count. */
export const MESSAGE_LIMIT_WARNING_AT = 1800

/** multichat's route that sends as a bot account into one of its
 *  conversations, under the host's multichat base path. */
export function accountSendPath(accountID: string, conversationID: string): string {
  return `${MESSAGE_STORE_THROUGH_MULTICHAT}${ACCOUNTS_PATH}/${encodeURIComponent(accountID)}/conversations/${encodeURIComponent(conversationID)}/messages`
}

/** Why a draft cannot be sent as it is, or null when it can. */
export function accountDraftProblem(draft: string): string | null {
  if (!draft.trim()) return 'Write something to send.'
  const length = [...draft].length
  if (length > DISCORD_MESSAGE_LIMIT) return `Discord takes at most ${DISCORD_MESSAGE_LIMIT} characters; this is ${length}.`
  return null
}
