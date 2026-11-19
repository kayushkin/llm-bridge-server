// What a message reference chip shows, as pure rules so they are tested
// (test/messageRefs.test.ts). The chip is MessageRefChip in
// components/chat/RefChip.tsx; its data is multichat's answer to
// GET /message-log/{ref}, passed through by the host's resolver.

import type { LoggedMessage, MessageLogPage } from '@kayushkin/multichat-types'

/** The one message in multichat's answer, or null when the answer is not the
 *  one-message page that route sends. */
export function messageOfAnswer(answer: unknown): LoggedMessage | null {
  const messages = (answer as Partial<MessageLogPage> | null)?.messages
  if (!Array.isArray(messages) || messages.length !== 1) return null
  return messages[0] as LoggedMessage
}

/** The day a message was sent, in the reader's time zone, as YYYY-MM-DD. */
export function messageDayOf(message: Pick<LoggedMessage, 'sent_at'>): string {
  const date = new Date(message.sent_at)
  if (Number.isNaN(date.getTime())) return ''
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${date.getFullYear()}-${month}-${day}`
}

/** A message's name on a chip: who sent it and the day. */
export function messageChipLabel(message: Pick<LoggedMessage, 'sender_display_name' | 'sender_user_id' | 'sent_at'>): string {
  const sender = message.sender_display_name.trim() || message.sender_user_id
  const day = messageDayOf(message)
  return day ? `${sender} · ${day}` : sender
}

/** Where the Messages page opens the message's conversation, or '' when the
 *  host has no Messages page. */
export function messageConversationHref(conversationsPath: string, roomID: string): string {
  return conversationsPath && roomID ? `${conversationsPath}?room=${encodeURIComponent(roomID)}` : ''
}
