import { describe, expect, it } from 'vitest'
import type { LoggedMessage } from '@kayushkin/multichat-types'
import { messageChipLabel, messageConversationHref, messageOfAnswer } from '../src/messageRefs'

const message = {
  ref: 'message_026563', sender_display_name: 'Kat', sender_user_id: '@gmessagesweb_x:chat.kayushkin.com',
  sent_at: '2026-10-20T19:33:00Z', room_id: '!room:chat.kayushkin.com',
} as LoggedMessage

describe('message chips', () => {
  it('reads the one message out of multichat\'s page', () => {
    expect(messageOfAnswer({ messages: [message], edits: {}, reactions: {} })).toBe(message)
    expect(messageOfAnswer({ messages: [] })).toBeNull()
    expect(messageOfAnswer({ messages: [message, message] })).toBeNull()
    expect(messageOfAnswer(null)).toBeNull()
  })
  it('names a message by its sender and day, falling back to the account', () => {
    expect(messageChipLabel(message)).toMatch(/^Kat · 2026-10-2[01]$/)
    expect(messageChipLabel({ ...message, sender_display_name: ' ' })).toMatch(/^@gmessagesweb_x/)
    expect(messageChipLabel({ ...message, sent_at: 'not a time' })).toBe('Kat')
  })
  it('links the conversation only when the host has a Messages page', () => {
    expect(messageConversationHref('/messages', '!room:chat.kayushkin.com')).toBe('/messages?room=!room%3Achat.kayushkin.com')
    expect(messageConversationHref('', '!room:chat.kayushkin.com')).toBe('')
  })
})
