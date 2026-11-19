import type {
  DiscordBridgeStatus, DiscordBridgedChannel, DiscordBridgedServer, LoggedMessage, MessageLogPage,
} from '@kayushkin/multichat-types'

/** The Discord page reads these multichat routes through the host's proxy.
 *  All are GETs; nothing on the page writes. */
export const DISCORD_ROUTES_CALLED = ['GET /message-log', 'GET /discord/status', 'GET /media/{server_name}/{media_id}'] as const

/** multichat's platform key for Discord, one of `GET /inbound-platforms`. */
export const DISCORD_PLATFORM = 'discord'

/** One page of the log. multichat allows up to 1000. */
export const MESSAGE_LOG_PAGE_SIZE = 100

export type DiscordTab = 'messages' | 'deleted' | 'status'

export const DISCORD_TABS: readonly { key: DiscordTab; label: string }[] = [
  { key: 'messages', label: 'Messages' },
  { key: 'deleted', label: 'Deleted' },
  { key: 'status', label: 'Bridge status' },
]

/** The tab named in `?tab=`, Messages when it names none we have. */
export function discordTabOf(param: string | null): DiscordTab {
  return DISCORD_TABS.find(t => t.key === param)?.key ?? 'messages'
}

/** What the filter bar holds. Ids, never names: `serverID` is the Discord
 *  server id (the log's `network_id`), `roomID` the channel's Matrix room id
 *  (the log's `room_id`), both taken from `GET /discord/status`. */
export interface MessageLogFilters {
  serverID: string
  roomID: string
  text: string
}

export const EMPTY_MESSAGE_LOG_FILTERS: MessageLogFilters = { serverID: '', roomID: '', text: '' }

/** The query string for `GET /message-log`: always Discord, the filters that
 *  are set, `deleted=true` on the Deleted tab, and `before` for an older page. */
export function messageLogQuery(filters: MessageLogFilters, options: { deletedOnly: boolean; before?: string; limit?: number }): string {
  const params = new URLSearchParams({ platform: DISCORD_PLATFORM })
  if (filters.serverID) params.set('network_id', filters.serverID)
  if (filters.roomID) params.set('room_id', filters.roomID)
  const text = filters.text.trim()
  if (text) params.set('q', text)
  if (options.deletedOnly) params.set('deleted', 'true')
  if (options.before) params.set('before', options.before)
  params.set('limit', String(options.limit ?? MESSAGE_LOG_PAGE_SIZE))
  return `/message-log?${params.toString()}`
}

/** Where the next older page starts: the last row's `sent_at`, or null when
 *  this page was short and so the last one. multichat answers rows sent
 *  strictly before it, so two messages sent in the same instant across a page
 *  boundary would lose the second; Discord stamps to the millisecond, so that
 *  is rare. */
export function nextPageBefore(messages: readonly LoggedMessage[], limit: number = MESSAGE_LOG_PAGE_SIZE): string | null {
  if (messages.length < limit || messages.length === 0) return null
  return messages[messages.length - 1].sent_at
}

/** Joins an older page onto what is shown, and its edits and reactions onto
 *  the maps. A message already shown is not shown twice: rows are matched by
 *  the log's own `id`, since an archive row has no event id. */
export function appendMessageLogPage(shown: MessageLogPage, older: MessageLogPage): MessageLogPage {
  const seen = new Set(shown.messages.map(m => m.id))
  return {
    messages: [...shown.messages, ...older.messages.filter(m => !seen.has(m.id))],
    edits: { ...shown.edits, ...older.edits },
    reactions: { ...shown.reactions, ...older.reactions },
  }
}

/** A channel the filter bar can pick: only one with a Matrix room, since the
 *  log is keyed by room. */
export interface ChannelOption {
  roomID: string
  label: string
}

export function serverOptions(status: DiscordBridgeStatus | null): { serverID: string; label: string }[] {
  return (status?.bridged_servers ?? []).map(s => ({ serverID: s.discord_server_id, label: s.name }))
}

/** The channels of one server, or of every bridged server when none is
 *  picked, labeled with the server's name when more than one could be shown. */
export function channelOptions(status: DiscordBridgeStatus | null, serverID: string): ChannelOption[] {
  const servers = (status?.bridged_servers ?? []).filter(s => !serverID || s.discord_server_id === serverID)
  const withServerName = servers.length > 1
  return servers.flatMap(server => server.channels
    .filter(c => c.matrix_room_id)
    .map(c => ({ roomID: c.matrix_room_id, label: withServerName ? `${server.name} · ${c.name}` : c.name })))
}

/** Picking a server clears a channel that is not in it. */
export function withServer(filters: MessageLogFilters, status: DiscordBridgeStatus | null, serverID: string): MessageLogFilters {
  const keepRoom = channelOptions(status, serverID).some(c => c.roomID === filters.roomID)
  return { ...filters, serverID, roomID: keepRoom ? filters.roomID : '' }
}

/** One version of a message: the original as sent, then each edit. */
export interface MessageVersion {
  /** The row the version's text is on: the original, or the edit's own row. */
  message: LoggedMessage
  at: string
  isOriginal: boolean
}

/** A message as it stood over time, oldest first. The log keeps the original
 *  row as first sent, and every edit as its own row under `edits`. */
export function versionsOf(message: LoggedMessage, edits: MessageLogPage['edits']): MessageVersion[] {
  const own = message.event_id ? edits[message.event_id] ?? [] : []
  return [
    { message, at: message.sent_at, isOriginal: true },
    ...own.map(e => ({ message: e, at: e.sent_at, isOriginal: false })),
  ]
}

/** The text a message reads as now: its newest edit's, or its own. */
export function currentVersionOf(message: LoggedMessage, edits: MessageLogPage['edits']): LoggedMessage {
  const versions = versionsOf(message, edits)
  return versions[versions.length - 1].message
}

/** The Discord channels the bridge status names, for the renderers' `#name`. */
export function bridgedChannelsOf(status: DiscordBridgeStatus | null): DiscordBridgedChannel[] {
  return (status?.bridged_servers ?? []).flatMap(server => server.channels)
}

/** Who sent it, as the log names them. */
export function senderLabel(message: LoggedMessage): string {
  return message.sender_display_name || message.sender_user_id
}

/** Where it was: server and channel names as the room's bridge state gave them
 *  when the message was logged. */
export function whereLabel(message: LoggedMessage): string {
  const channel = message.channel_name || message.room_name || message.room_id
  return message.network_name ? `${message.network_name} · ${channel}` : channel
}

/** A message the log caught only after it was deleted has no text. */
export function deletedBeforeRead(message: LoggedMessage): boolean {
  return message.redacted_at !== null && message.body === '' && message.media_url === ''
}

/** Discord's announcement channel type (discordgo.ChannelTypeGuildNews);
 *  multichat's status lists text channels (0) and these. */
const ANNOUNCEMENT_CHANNEL_TYPE = 5

/** The channel's name, marked when it is an announcement channel. */
export function channelLabel(channel: DiscordBridgedChannel): string {
  return channel.channel_type === ANNOUNCEMENT_CHANNEL_TYPE ? `${channel.name} (announcements)` : channel.name
}

/** What is wrong with a channel, worst first; empty when nothing is. */
export function channelProblems(channel: DiscordBridgedChannel): string[] {
  if (!channel.matrix_room_id) return ['no Matrix room: the bridge has not made one']
  if (!channel.admin_joined) return ['@admin has not joined: multichat cannot read or log it']
  return []
}

/** How many unreadable channels the headline names before it says "and N more". */
const UNREADABLE_CHANNELS_NAMED = 5

/** The headline problems with the bridge as a whole, for the top of the
 *  Bridge status tab and a flag on the tab itself. */
export function bridgeProblems(status: DiscordBridgeStatus): string[] {
  const problems: string[] = []
  if (status.accounts.length === 0) problems.push('No account has logged in to the Discord bridge.')
  for (const account of status.accounts) {
    if (!account.logged_in) problems.push(`${account.matrix_user_id} is logged out of Discord.`)
  }
  if (status.bridged_servers.length === 0) problems.push('No Discord server is bridged.')
  const unreadable = status.bridged_servers.flatMap(server => server.channels
    .filter(c => channelProblems(c).length > 0)
    .map(c => `${server.name} ${c.name}`))
  if (unreadable.length > 0) {
    const named = unreadable.slice(0, UNREADABLE_CHANNELS_NAMED).join(', ')
    const more = unreadable.length > UNREADABLE_CHANNELS_NAMED ? ` and ${unreadable.length - UNREADABLE_CHANNELS_NAMED} more` : ''
    problems.push(`Multichat cannot read ${unreadable.length === 1 ? 'this channel' : `these ${unreadable.length} channels`}: ${named}${more}.`)
  }
  return problems
}

/** Totals for a server's header row. */
export function serverTotals(server: DiscordBridgedServer): { channels: number; unreadable: number; logged: number; deleted: number } {
  return {
    channels: server.channels.length,
    unreadable: server.channels.filter(c => channelProblems(c).length > 0).length,
    logged: server.channels.reduce((sum, c) => sum + c.logged_message_count, 0),
    deleted: server.channels.reduce((sum, c) => sum + c.deleted_message_count, 0),
  }
}

/** What an archive row carries that its markdown body does not: Discord's
 *  own message type for a system message (a join, a pin, a boost), the
 *  stickers sent, and the files attached. Read from the archived Discord
 *  object multichat keeps in `content_json`. */
export interface ArchivedDiscordExtras {
  /** How the row reads when Discord wrote it rather than a person; "" for a
   *  person's message. */
  systemLabel: string
  stickers: { id: string; name: string }[]
  attachments: { filename: string; url: string }[]
  /** Set when `content_json` could not be read; the row says so. */
  unreadable: string
}

/** Discord message types that are a person's message (discord.com's DEFAULT
 *  and REPLY); every other type is Discord's own. */
const DISCORD_PERSON_MESSAGE_TYPES = new Set([0, 19])

const DISCORD_SYSTEM_MESSAGE_LABELS: Readonly<Record<number, string>> = {
  1: 'added someone to the group',
  2: 'removed someone from the group',
  4: 'renamed the channel',
  6: 'pinned a message',
  7: 'joined the server',
  8: 'boosted the server',
  9: 'boosted the server to level 1',
  10: 'boosted the server to level 2',
  11: 'boosted the server to level 3',
  12: 'followed an announcement channel',
  18: 'started a thread',
  20: 'used a slash command',
  46: 'finished a poll',
}

interface ArchivedDiscordObject {
  type?: number
  sticker_items?: { id?: string; name?: string }[] | null
  attachments?: { filename?: string; url?: string }[] | null
}

export function archivedDiscordExtras(message: LoggedMessage): ArchivedDiscordExtras {
  const none: ArchivedDiscordExtras = { systemLabel: '', stickers: [], attachments: [], unreadable: '' }
  if (message.source !== 'discord-archive') return none
  let archived: ArchivedDiscordObject
  try {
    archived = JSON.parse(message.content_json) as ArchivedDiscordObject
  } catch (err) {
    return { ...none, unreadable: `The archived Discord message cannot be read: ${err instanceof Error ? err.message : String(err)}` }
  }
  const type = archived.type ?? 0
  return {
    systemLabel: DISCORD_PERSON_MESSAGE_TYPES.has(type) ? '' : DISCORD_SYSTEM_MESSAGE_LABELS[type] ?? `Discord system message (type ${type})`,
    stickers: (archived.sticker_items ?? []).map(s => ({ id: s.id ?? '', name: s.name ?? '' })),
    attachments: (archived.attachments ?? []).map(a => ({ filename: a.filename ?? '', url: a.url ?? '' })),
    unreadable: '',
  }
}
