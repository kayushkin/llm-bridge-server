import { describe, expect, it } from 'vitest'
import type { Overview, WaitingConversation } from '@kayushkin/multichat-types'
import {
  AWAITING_REPLY_ROUTES_CALLED, DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, alertActions, arrivalText, clearBodyFor, clearedByLabel,
  arrivalsToAnnounce, entryToClearOnOpen, inQuietHours, roomOnScreen, newlyArrived, previewMessageText, readBrowserAlertSettings, sendersLabel, settingsWriteBody, waitingAgeLabel,
} from '../src/awaitingReply'
import { MULTICHAT_ROUTES_CALLED } from '../src/multichatMessages'

const settings = { tag_ids: [6], max_group_member_count: 5, max_message_age_days: 30, clear_when_opened: true, clear_when_replied: true, updated_at: '2026-10-20T21:40:14Z' }

function entry(roomID: string, newest: string, overrides: Partial<WaitingConversation> = {}): WaitingConversation {
  return {
    room_id: roomID, name: roomID, platform: 'discord', member_count: 2, direct_message: true, counted_because_room_is_tagged: false,
    senders: [{ person_id: 'person_000097', display_name: 'DeandreT', user_id: '@discord_1:chat.example' }],
    unanswered_message_count: 1,
    messages: [{ event_id: '$1', sender: { person_id: 'person_000097', display_name: 'DeandreT', user_id: '@discord_1:chat.example' }, message_type: 'm.text', body: 'sounded awful lol', media_url: '', media_mimetype: '', sent_at: newest }],
    waiting_since: newest, newest_message_at: newest,
    ...overrides,
  }
}

function overview(...waiting: WaitingConversation[]): Overview {
  return { settings, waiting, cleared: [], computed_at: '2026-10-20T22:00:00Z' }
}

describe('what counts as newly arrived', () => {
  it('is nothing on the first answer, so opening dash plays no sound for old messages', () => {
    expect(newlyArrived(null, overview(entry('!d', '2026-10-12T15:56:29Z')))).toEqual([])
  })
  it('is a room that was not waiting, or whose newest message moved on', () => {
    const before = overview(entry('!d', '2026-10-12T15:56:29Z'), entry('!g', '2026-10-20T10:00:00Z'))
    const after = overview(entry('!d', '2026-10-12T15:56:29Z'), entry('!g', '2026-10-20T10:05:00Z'), entry('!m', '2026-10-20T10:06:00Z'))
    expect(newlyArrived(before, after).map(e => e.room_id)).toEqual(['!g', '!m'])
  })
  it('is not a room that left the list', () => {
    expect(newlyArrived(overview(entry('!d', '2026-10-12T15:56:29Z')), overview())).toEqual([])
  })
})

describe('what is announced', () => {
  const routes = { chat: '/', messageWaiting: '/messages/waiting' }
  it('knows the conversation on screen from the URL', () => {
    expect(roomOnScreen('/messages/waiting', '?room=!d', routes)).toBe('!d')
    expect(roomOnScreen('/', '?view=conversation&room=!d', routes)).toBe('!d')
    expect(roomOnScreen('/', '?session=br_1&room=!d', routes)).toBeNull()
    expect(roomOnScreen('/messages', '?room=!d', routes)).toBeNull()
    expect(roomOnScreen('/messages/waiting', '', routes)).toBeNull()
  })
  it('leaves out the conversation on screen', () => {
    const arrived = [entry('!d', '2026-10-20T10:00:00Z'), entry('!g', '2026-10-20T10:01:00Z')]
    expect(arrivalsToAnnounce(arrived, '!d').map(e => e.room_id)).toEqual(['!g'])
    expect(arrivalsToAnnounce(arrived, null)).toHaveLength(2)
  })
})

describe('opening and dismissing', () => {
  it('clears through the newest message shown', () => {
    expect(clearBodyFor(entry('!d', '2026-10-12T15:56:29.057Z'), 'opened'))
      .toEqual({ room_id: '!d', reason: 'opened', cleared_through: '2026-10-12T15:56:29.057Z' })
  })
  it('clears on open only a waiting room, and only when the settings say so', () => {
    const waiting = overview(entry('!d', '2026-10-12T15:56:29Z'))
    expect(entryToClearOnOpen(waiting, '!d')?.room_id).toBe('!d')
    expect(entryToClearOnOpen(waiting, '!other')).toBeNull()
    expect(entryToClearOnOpen({ ...waiting, settings: { ...settings, clear_when_opened: false } }, '!d')).toBeNull()
    expect(entryToClearOnOpen(null, '!d')).toBeNull()
  })
  it('says how a room was cleared', () => {
    const cleared = { ...entry('!d', '2026-10-12T15:56:29Z'), last_message: entry('!d', 'x').messages[0], cleared_at: '2026-10-19T00:00:00Z' }
    expect(clearedByLabel({ ...cleared, cleared_by: 'replied' })).toBe('you replied')
    expect(clearedByLabel({ ...cleared, cleared_by: 'opened' })).toBe('opened in dash')
    expect(clearedByLabel({ ...cleared, cleared_by: 'dismissed' })).toBe('dismissed')
  })
})

describe('what a row says', () => {
  it('names a file by its kind and keeps text as it is', () => {
    const base = entry('!d', 'x').messages[0]
    expect(previewMessageText(base)).toBe('sounded awful lol')
    expect(previewMessageText({ ...base, media_url: 'mxc://s/i', media_mimetype: 'image/jpeg', body: 'IMG_1.jpg' })).toBe('📷 photo')
    expect(previewMessageText({ ...base, media_url: 'mxc://s/i', media_mimetype: 'video/mp4', body: 'a.mp4' })).toBe('🎞 video')
    expect(previewMessageText({ ...base, media_url: 'mxc://s/i', media_mimetype: 'application/pdf', body: 'plan.pdf' })).toBe('📎 plan.pdf')
  })
  it('names the senders in a group and nobody in a direct message', () => {
    const group = entry('!g', 'x', { direct_message: false, senders: [
      { person_id: 'person_000025', display_name: 'Loukic', user_id: 'a' }, { person_id: 'person_000012', display_name: 'Maleeha', user_id: 'b' }] })
    expect(sendersLabel(group)).toBe('Loukic, Maleeha')
    expect(sendersLabel(entry('!d', 'x'))).toBe('')
    expect(arrivalText({ ...group, name: 'Lillian, Loukic, Maleeha' }).title).toBe('DeandreT in Lillian, Loukic, Maleeha')
    expect(arrivalText(entry('!d', 'x', { name: 'D' }))).toEqual({ title: 'D', line: 'sounded awful lol' })
  })
  it('says how long ago in the shortest words', () => {
    const now = new Date('2026-10-20T12:00:00Z')
    expect(waitingAgeLabel('2026-10-20T11:59:40Z', now)).toBe('now')
    expect(waitingAgeLabel('2026-10-20T11:15:00Z', now)).toBe('45 min')
    expect(waitingAgeLabel('2026-10-20T07:00:00Z', now)).toBe('5 h')
    expect(waitingAgeLabel('2026-10-12T15:56:29Z', now)).toBe('7 d')
  })
})

describe('the settings multichat keeps', () => {
  it('are written whole, without the time multichat stamps', () => {
    expect(settingsWriteBody(settings)).toEqual({ tag_ids: [6], max_group_member_count: 5, max_message_age_days: 30, clear_when_opened: true, clear_when_replied: true })
  })
  it('are reached through routes the host proxies', () => {
    for (const route of AWAITING_REPLY_ROUTES_CALLED) expect(MULTICHAT_ROUTES_CALLED).toContain(route)
  })
})

describe("this browser's sound settings", () => {
  it('are the defaults when nothing is saved', () => {
    expect(readBrowserAlertSettings(null)).toEqual({ settings: DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, problem: null })
  })
  it('say what is wrong with a saved value rather than quietly using the defaults', () => {
    expect(readBrowserAlertSettings('{').problem).toMatch(/not JSON/)
    expect(readBrowserAlertSettings(JSON.stringify({ ...DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, sound: 'kazoo' })).problem).toMatch(/sound must be one of/)
    expect(readBrowserAlertSettings(JSON.stringify({ ...DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, volume: 3 })).problem).toMatch(/volume/)
    expect(readBrowserAlertSettings(JSON.stringify({ ...DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, quietHours: { enabled: true, start: '25:00', end: '07:00' } })).problem).toMatch(/HH:MM/)
  })
  it('read back what was saved', () => {
    const saved = { ...DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, sound: 'ping', volume: 0.2 }
    expect(readBrowserAlertSettings(JSON.stringify(saved))).toEqual({ settings: saved, problem: null })
  })
  it('keep quiet across midnight and not outside the span', () => {
    const at = (hours: number, minutes = 0) => new Date(2026, 9, 20, hours, minutes)
    const s = DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS
    expect(inQuietHours(s, at(23))).toBe(true)
    expect(inQuietHours(s, at(3))).toBe(true)
    expect(inQuietHours(s, at(7))).toBe(false)
    expect(inQuietHours(s, at(21, 59))).toBe(false)
    expect(inQuietHours({ ...s, quietHours: { enabled: true, start: '13:00', end: '14:00' } }, at(13, 30))).toBe(true)
    expect(inQuietHours({ ...s, quietHours: { ...s.quietHours, enabled: false } }, at(23))).toBe(false)
  })
  it('turn sound and pop-ups off in quiet hours, and send a desktop notification only to a hidden page', () => {
    const s = { ...DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, desktopNotification: true }
    expect(alertActions(s, new Date(2026, 9, 20, 12), false)).toEqual({ playSound: true, popUp: true, desktopNotification: false })
    expect(alertActions(s, new Date(2026, 9, 20, 12), true).desktopNotification).toBe(true)
    expect(alertActions(s, new Date(2026, 9, 20, 23), true)).toEqual({ playSound: false, popUp: false, desktopNotification: false })
  })
})
