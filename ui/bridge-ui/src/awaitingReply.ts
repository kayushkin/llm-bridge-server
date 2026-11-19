// Who is waiting on a reply: multichat's GET /awaiting-reply, drawn on the
// Messages → Waiting on you page and in the chat sidebar, with a sound and a
// pop-up when a new message arrives. multichat decides who and what counts
// (internal/awaitingreply); this module only reads its answer, works out what
// arrived since the last poll, and keeps this browser's sound settings.

import type { AwaitingReplySettings, ClearedConversation, Overview, PreviewMessage, WaitingConversation } from '@kayushkin/multichat-types'
import { AwaitingReplyClearDismissed, AwaitingReplyClearOpened, ClearedByReply } from '@kayushkin/multichat-types'

export const AWAITING_REPLY_PATH = '/awaiting-reply'
export const AWAITING_REPLY_SETTINGS_PATH = '/awaiting-reply/settings'
export const AWAITING_REPLY_CLEARS_PATH = '/awaiting-reply/clears'

/** Every multichat route this feature calls, for the host's proxy allowlist. */
export const AWAITING_REPLY_ROUTES_CALLED: readonly string[] = [
  `GET ${AWAITING_REPLY_PATH}`,
  `GET ${AWAITING_REPLY_SETTINGS_PATH}`,
  `PUT ${AWAITING_REPLY_SETTINGS_PATH}`,
  `POST ${AWAITING_REPLY_CLEARS_PATH}`,
]

/** How often the list is read. multichat answers in about 170 ms. */
export const AWAITING_REPLY_POLL_MS = 15_000

export type AwaitingReplyClearReason = typeof AwaitingReplyClearOpened | typeof AwaitingReplyClearDismissed

/** The body of POST /awaiting-reply/clears: the room is seen up to the newest
 *  message the operator was shown, so a later one brings it back. */
export function clearBodyFor(entry: WaitingConversation, reason: AwaitingReplyClearReason) {
  return { room_id: entry.room_id, reason, cleared_through: entry.newest_message_at }
}

/** The waiting entry for a room, or null when it is not waiting. */
export function waitingEntryFor(overview: Overview | null, roomID: string): WaitingConversation | null {
  return overview?.waiting.find(entry => entry.room_id === roomID) ?? null
}

/** The entry to clear when the operator opens a room, or null: the room is
 *  waiting and the settings say opening clears. */
export function entryToClearOnOpen(overview: Overview | null, roomID: string): WaitingConversation | null {
  if (!overview?.settings.clear_when_opened) return null
  return waitingEntryFor(overview, roomID)
}

/** The rooms whose newest unanswered message is newer than it was in the
 *  previous answer, including rooms that were not waiting before. Nothing is
 *  new on the first answer, so opening dash plays no sound for old messages. */
export function newlyArrived(previous: Overview | null, next: Overview): WaitingConversation[] {
  if (!previous) return []
  const before = new Map(previous.waiting.map(entry => [entry.room_id, Date.parse(entry.newest_message_at)]))
  return next.waiting.filter(entry => {
    const was = before.get(entry.room_id)
    return was === undefined || Date.parse(entry.newest_message_at) > was
  })
}

/** The arrivals worth a sound and a pop-up: all but the conversation already
 *  on screen, which the operator is reading. */
export function arrivalsToAnnounce(arrived: readonly WaitingConversation[], roomOnScreen: string | null): WaitingConversation[] {
  return arrived.filter(entry => entry.room_id !== roomOnScreen)
}

/** The conversation on screen, from the URL: a room open on the Waiting on you
 *  page, or in the chat's conversation view. Null anywhere else. */
export function roomOnScreen(pathname: string, search: string, routes: { chat: string; messageWaiting: string }): string | null {
  const params = new URLSearchParams(search)
  const room = params.get('room')
  if (!room) return null
  if (pathname === routes.messageWaiting) return room
  if (pathname === routes.chat && params.get('view') === 'conversation') return room
  return null
}

/** One line for a message: its text, or what kind of file it is. */
export function previewMessageText(message: PreviewMessage): string {
  if (message.media_url) {
    const kind = message.media_mimetype.split('/')[0]
    if (kind === 'image') return message.message_type === 'm.sticker' ? `sticker: ${message.body}` : '📷 photo'
    if (kind === 'video') return '🎞 video'
    if (kind === 'audio') return '🎤 audio'
    return `📎 ${message.body}`
  }
  return message.body
}

/** The newest unanswered message of an entry, as one line. */
export function newestPreviewText(entry: WaitingConversation): string {
  const newest = entry.messages[entry.messages.length - 1]
  return newest ? previewMessageText(newest) : ''
}

/** Who wrote, in a group: the senders' names. A direct message needs none. */
export function sendersLabel(entry: WaitingConversation): string {
  return entry.direct_message ? '' : entry.senders.map(sender => sender.display_name).join(', ')
}

/** How a cleared room stopped waiting, in words. */
export function clearedByLabel(cleared: ClearedConversation): string {
  switch (cleared.cleared_by) {
    case ClearedByReply: return 'you replied'
    case AwaitingReplyClearOpened: return 'opened in dash'
    case AwaitingReplyClearDismissed: return 'dismissed'
    default: return cleared.cleared_by
  }
}

/** How long ago, in the shortest words: now, 5 min, 3 h, 2 d. */
export function waitingAgeLabel(sentAt: string, now: Date): string {
  const minutes = Math.floor((now.getTime() - Date.parse(sentAt)) / 60_000)
  if (minutes < 1) return 'now'
  if (minutes < 60) return `${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} h`
  return `${Math.floor(hours / 24)} d`
}

/** What PUT /awaiting-reply/settings takes: the whole object, without the
 *  `updated_at` multichat sets. */
export function settingsWriteBody(settings: AwaitingReplySettings) {
  const { tag_ids, count_every_known_person, max_group_member_count, any_size_person_tags, never_count_person_tags, max_message_age_days, clear_when_opened, clear_when_replied } = settings
  return { tag_ids, count_every_known_person, max_group_member_count, any_size_person_tags, never_count_person_tags, max_message_age_days, clear_when_opened, clear_when_replied }
}

// --------------------------------------------------------------- this browser

/** A sound, as notes played one after another: frequency in hertz, start and
 *  length in seconds. Synthesised, so no audio file ships. */
export interface AlertSound {
  name: string
  label: string
  notes: readonly { frequency: number; start: number; length: number }[]
}

export const ALERT_SOUNDS: readonly AlertSound[] = [
  { name: 'soft-chime', label: 'Soft chime', notes: [{ frequency: 880, start: 0, length: 0.35 }, { frequency: 1318.5, start: 0.12, length: 0.5 }] },
  { name: 'two-tone', label: 'Two tone', notes: [{ frequency: 659.3, start: 0, length: 0.18 }, { frequency: 987.8, start: 0.2, length: 0.25 }] },
  { name: 'ping', label: 'Ping', notes: [{ frequency: 1567.98, start: 0, length: 0.3 }] },
  { name: 'marimba', label: 'Marimba', notes: [{ frequency: 523.25, start: 0, length: 0.2 }, { frequency: 659.3, start: 0.1, length: 0.2 }, { frequency: 783.99, start: 0.2, length: 0.35 }] },
]

/** Sound and pop-ups, kept in this browser only, so a phone can stay quiet
 *  while a desktop chimes. */
export interface MessageAlertBrowserSettings {
  playSound: boolean
  /** An `ALERT_SOUNDS` name. */
  sound: string
  /** 0 to 1. */
  volume: number
  popUp: boolean
  desktopNotification: boolean
  quietHours: { enabled: boolean; start: string; end: string }
}

export const MESSAGE_ALERT_BROWSER_SETTINGS_KEY = 'messages:awaiting-reply-alerts'

export const DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS: MessageAlertBrowserSettings = {
  playSound: true,
  sound: ALERT_SOUNDS[0].name,
  volume: 0.6,
  popUp: true,
  desktopNotification: false,
  quietHours: { enabled: true, start: '22:00', end: '07:00' },
}

const CLOCK = /^([01]\d|2[0-3]):[0-5]\d$/

/** What a saved value is wrong about, or null when it is a valid setting. */
export function browserAlertSettingsProblem(value: unknown): string | null {
  if (typeof value !== 'object' || value === null) return 'not an object'
  const settings = value as Partial<MessageAlertBrowserSettings>
  if (typeof settings.playSound !== 'boolean') return 'playSound must be true or false'
  if (!ALERT_SOUNDS.some(sound => sound.name === settings.sound)) return `sound must be one of ${ALERT_SOUNDS.map(sound => sound.name).join(', ')}`
  if (typeof settings.volume !== 'number' || settings.volume < 0 || settings.volume > 1) return 'volume must be a number from 0 to 1'
  if (typeof settings.popUp !== 'boolean') return 'popUp must be true or false'
  if (typeof settings.desktopNotification !== 'boolean') return 'desktopNotification must be true or false'
  const quiet = settings.quietHours
  if (typeof quiet !== 'object' || quiet === null || typeof quiet.enabled !== 'boolean') return 'quietHours.enabled must be true or false'
  if (!CLOCK.test(quiet.start) || !CLOCK.test(quiet.end)) return 'quiet hours must be HH:MM, 24-hour'
  return null
}

/** This browser's saved settings: the defaults when nothing is saved, and
 *  the reason when what is saved cannot be read, so the settings section can
 *  say so rather than quietly showing defaults. */
export function readBrowserAlertSettings(saved: string | null): { settings: MessageAlertBrowserSettings; problem: string | null } {
  if (saved === null) return { settings: DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, problem: null }
  let parsed: unknown
  try {
    parsed = JSON.parse(saved)
  } catch (err) {
    return { settings: DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, problem: `the saved settings are not JSON (${err instanceof Error ? err.message : String(err)}); showing the defaults` }
  }
  const problem = browserAlertSettingsProblem(parsed)
  if (problem) return { settings: DEFAULT_MESSAGE_ALERT_BROWSER_SETTINGS, problem: `the saved settings are wrong: ${problem}; showing the defaults` }
  return { settings: parsed as MessageAlertBrowserSettings, problem: null }
}

function minutesOfClock(clock: string): number {
  const [hours, minutes] = clock.split(':').map(Number)
  return hours * 60 + minutes
}

/** Whether `now`, in the browser's own time zone, falls in the quiet hours.
 *  A span that ends before it starts runs past midnight. */
export function inQuietHours(settings: MessageAlertBrowserSettings, now: Date): boolean {
  const { enabled, start, end } = settings.quietHours
  if (!enabled || start === end) return false
  const minute = now.getHours() * 60 + now.getMinutes()
  const from = minutesOfClock(start)
  const to = minutesOfClock(end)
  return from < to ? minute >= from && minute < to : minute >= from || minute < to
}

/** What a new arrival does in this browser now. */
export function alertActions(settings: MessageAlertBrowserSettings, now: Date, pageHidden: boolean) {
  const quiet = inQuietHours(settings, now)
  return {
    playSound: settings.playSound && !quiet,
    popUp: settings.popUp && !quiet,
    desktopNotification: settings.desktopNotification && !quiet && pageHidden,
  }
}

/** The pop-up's title and line for an arrival. */
export function arrivalText(entry: WaitingConversation): { title: string; line: string } {
  const newest = entry.messages[entry.messages.length - 1]
  const who = newest?.sender.display_name ?? ''
  const title = entry.direct_message || !who ? entry.name : `${who} in ${entry.name}`
  return { title, line: newestPreviewText(entry) }
}
