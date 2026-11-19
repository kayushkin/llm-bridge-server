// The emoji pickers' rules, shared by the reaction picker, the composer's `:`
// suggestions, the quick-react bar and the Emoji page: which emoji a room is
// offered, how a name search ranks them, where a `:query` sits in the text,
// what a `+:name:` command reacts with, and how a favorite is matched to a
// reaction already on a message. The records are multichat's (`GET /emoji`);
// everything here is covered by test/emojiCatalog.test.ts.
import type {
  DiscordBridgeStatus, DiscordCustomEmoji, DiscordServerSweepStatus, EmojiCatalog, MessageReactionGroup,
} from '@kayushkin/multichat-types'
import type { EmojiGroup } from './emojiPicker'
import { discordEmojiURL } from './messageBody'
import { DISCORD_EMOJI_KEY_PREFIX } from './messageReactions'

/** The multichat routes the emoji pickers and the Emoji page call, for the
 *  host's proxy allowlist. */
export const EMOJI_ROUTES_CALLED: readonly string[] = [
  'GET /emoji',
  'POST /emoji/discord/refresh',
  'PUT /emoji/favorites',
  'PUT /emoji/settings',
  'POST /emoji/discord/nitro-check',
  'POST /emoji/discord/server-sweep',
]

/** What the pickers call the emoji the bridge has seen from servers that are
 *  not bridged; multichat gives them no server. */
export const SEEN_EMOJI_SECTION_NAME = 'Seen in messages'

/** Every custom emoji multichat knows: the bridged servers', then the seen ones. */
export function allCustomEmoji(catalog: EmojiCatalog | null): DiscordCustomEmoji[] {
  return catalog ? [...catalog.discord_custom_emoji, ...(catalog.discord_seen_emoji ?? [])] : []
}

/** The section a custom emoji is listed under: its server, or the seen ones' name. */
export function customEmojiSectionName(emoji: DiscordCustomEmoji): string {
  return emoji.discord_server_name || SEEN_EMOJI_SECTION_NAME
}

/** One emoji a picker can offer. `key` is what a reaction or a favorite
 *  carries: the unicode text (with the skin tone applied), or
 *  `discord-emoji:<id>` for a custom emoji. */
export type EmojiChoice =
  | { kind: 'unicode'; key: string; name: string }
  | { kind: 'custom'; key: string; name: string; emoji: DiscordCustomEmoji }

/** The skin tones: "" for none, then the five Fitzpatrick modifiers multichat
 *  accepts in `skin_tone`. */
export const SKIN_TONES: readonly { tone: string; label: string }[] = [
  { tone: '', label: 'Default' },
  { tone: '\u{1F3FB}', label: 'Light' },
  { tone: '\u{1F3FC}', label: 'Medium-light' },
  { tone: '\u{1F3FD}', label: 'Medium' },
  { tone: '\u{1F3FE}', label: 'Medium-dark' },
  { tone: '\u{1F3FF}', label: 'Dark' },
]

const ZERO_WIDTH_JOINER = '‍'
const VARIATION_SELECTOR_16 = '️'
/** Parts of a joined emoji that mean it shows two people; a tone on only the
 *  first would draw them split, so those are left yellow. */
const TWO_PEOPLE_PARTS = ['\u{1F91D}', '\u{2764}', '\u{1F48B}']

/** The emoji with the skin tone put after its first code point, when it takes
 *  one. An emoji of two people is left as it is. */
export function withSkinTone(emoji: string, takesSkinTone: boolean, tone: string): string {
  if (!tone || !takesSkinTone) return emoji
  const parts = emoji.split(ZERO_WIDTH_JOINER)
  if (parts.length > 1 && TWO_PEOPLE_PARTS.some(part => emoji.includes(part))) return emoji
  const [first, ...rest] = Array.from(parts[0])
  const toned = first + tone + rest.filter(point => point !== VARIATION_SELECTOR_16).join('')
  return [toned, ...parts.slice(1)].join(ZERO_WIDTH_JOINER)
}

/** Every unicode emoji of the table as a choice, in Unicode order, toned. */
export function unicodeChoices(groups: readonly EmojiGroup[], tone: string): EmojiChoice[] {
  return groups.flatMap(group => group.emojis.map(([emoji, name, takesSkinTone]) =>
    ({ kind: 'unicode' as const, key: withSkinTone(emoji, takesSkinTone === 1, tone), name })))
}

export function customChoice(emoji: DiscordCustomEmoji): EmojiChoice {
  return { kind: 'custom', key: emoji.reaction_key, name: emoji.name, emoji }
}

/** The Discord server a room bridges a channel of, from the bridge status;
 *  null for a room that is not a bridged Discord channel. */
export function roomDiscordServerID(status: DiscordBridgeStatus | null, roomID: string): string | null {
  for (const server of status?.bridged_servers ?? []) {
    if (server.channels.some(channel => channel.matrix_room_id === roomID)) return server.discord_server_id
  }
  return null
}

/** A Discord room: a channel of a server (`serverID`), or a direct message
 *  or group DM, which belongs to no server (`serverID` null). */
export interface DiscordRoom {
  serverID: string | null
}

/** The custom emoji a room is offered: none outside Discord (`room` null).
 *  In a server's channel, that server's first; then, when the account has
 *  Nitro, every other server's and the seen ones. A DM has no server of its
 *  own, so without Nitro it gets none and with Nitro all of them (Discord
 *  refuses an emoji outside its own server to an account without Nitro). */
export function customEmojiOffered(custom: readonly DiscordCustomEmoji[], seen: readonly DiscordCustomEmoji[],
  room: DiscordRoom | null, hasNitro: boolean): DiscordCustomEmoji[] {
  if (!room) return []
  const own = room.serverID ? custom.filter(emoji => emoji.discord_server_id === room.serverID) : []
  if (!hasNitro) return own
  return [...own, ...custom.filter(emoji => emoji.discord_server_id !== room.serverID), ...seen]
}

/** The query as a name search reads it: any case, with the colons of
 *  `:name:` and surrounding spaces dropped. */
export function normalizedEmojiQuery(query: string): string {
  return query.trim().replace(/^:+/, '').replace(/:+$/, '').toLowerCase()
}

const compact = (text: string) => text.replace(/[\s_-]+/g, '')

/** One choice as the search reads it, worked out once rather than on every
 *  keystroke. `server` is a custom emoji's server name in lower case, "" for
 *  a unicode one. */
export interface EmojiSearchEntry {
  choice: EmojiChoice
  index: number
  lower: string
  lowerCompact: string
  laterWordStarts: string[]
  server: string
}

function searchEntry(choice: EmojiChoice, index: number): EmojiSearchEntry {
  const lower = choice.name.toLowerCase()
  return {
    choice, index, lower, lowerCompact: compact(lower),
    laterWordStarts: choice.name.split(/[\s_-]+|(?<=[a-z0-9])(?=[A-Z])/).slice(1).map(word => word.toLowerCase()),
    server: choice.kind === 'custom' ? choice.emoji.discord_server_name.toLowerCase() : '',
  }
}

/** The search index of a list of choices. Build it once per list. */
export function emojiSearchIndex(choices: readonly EmojiChoice[]): EmojiSearchEntry[] {
  return choices.map(searchEntry)
}

function nameRank(entry: EmojiSearchEntry, query: string): number | null {
  const queryCompact = compact(query)
  if (entry.lower === query || entry.lowerCompact === queryCompact) return 0
  if (entry.lower.startsWith(query) || entry.lowerCompact.startsWith(queryCompact)) return 1
  if (entry.laterWordStarts.some(word => word.startsWith(queryCompact))) return 2
  // Anywhere else only as typed: dropping separators there would let "kerm"
  // match across the words of "speaker medium volume".
  if (entry.lower.includes(query)) return 3
  return null
}

/** The rank of a match on the server's name alone, below every name match. */
export const SERVER_MATCH_RANK = 4

/** How well an emoji matches the query, best first: 0 its whole name, 1 its
 *  start, 2 the start of a later word (after a space, `_`, `-` or a capital,
 *  so "par" finds "partyParrot" at 1 and "Parrot" in "partyParrot" at 2),
 *  3 anywhere in the name, 4 only its server's name; null when it does not
 *  match. Separators are ignored for the whole name and its start, so
 *  "party parrot" and "party_parrot" both find "partyParrot". A query of
 *  several words that is not a name matches when each word is in the name
 *  or the server's name, so "reno kerm" finds Reno's kermit emoji. */
function entryRank(entry: EmojiSearchEntry, query: string): number | null {
  const whole = nameRank(entry, query)
  if (whole !== null) return whole
  let best: number | null = null
  for (const word of query.split(/\s+/)) {
    if (!word) continue
    const rank = nameRank(entry, word)
    if (rank !== null) { best = best === null ? rank : Math.min(best, rank); continue }
    if (!entry.server.includes(word)) return null
  }
  return best ?? SERVER_MATCH_RANK
}

/** How well one name matches the query; see entryRank. */
export function emojiNameMatchRank(name: string, query: string): number | null {
  if (!query) return null
  return nameRank(searchEntry({ kind: 'unicode', key: name, name }, 0), query)
}

/** The indexed choices that match the query, best match first; among equals,
 *  favorites, then shorter names (for a name match), then the order given. */
export function searchEmojiIndex(index: readonly EmojiSearchEntry[], query: string, favoriteKeys: readonly string[],
  limit = Infinity): EmojiChoice[] {
  const normalized = normalizedEmojiQuery(query)
  if (!normalized) return []
  const favorites = new Set(favoriteKeys)
  const matches: { entry: EmojiSearchEntry; rank: number }[] = []
  for (const entry of index) {
    const rank = entryRank(entry, normalized)
    if (rank !== null) matches.push({ entry, rank })
  }
  return matches
    .sort((a, b) => a.rank - b.rank
      || Number(favorites.has(b.entry.choice.key)) - Number(favorites.has(a.entry.choice.key))
      // A server's emoji found by its name stay in catalog order.
      || (a.rank < SERVER_MATCH_RANK ? a.entry.choice.name.length - b.entry.choice.name.length : 0)
      || a.entry.index - b.entry.index)
    .slice(0, limit)
    .map(match => match.entry.choice)
}

/** searchEmojiIndex over a list not yet indexed. */
export function searchEmojiChoices(choices: readonly EmojiChoice[], query: string, favoriteKeys: readonly string[],
  limit = Infinity): EmojiChoice[] {
  return searchEmojiIndex(emojiSearchIndex(choices), query, favoriteKeys, limit)
}

/** The fewest characters after `:` before suggestions open, so ":)" and ":P"
 *  stay what they are. */
export const COLON_QUERY_MIN_LENGTH = 2

/** The `:query` the cursor sits at the end of, if any: a `:` at the start, or
 *  after a space, `(` or `+`, then at least two of letters, digits, `_`, `-`
 *  or `+`, up to the cursor. */
export function colonQueryAt(text: string, cursor: number): { start: number; query: string } | null {
  const before = text.slice(0, cursor)
  const match = /(^|[\s(+]):([\w+-]+)$/u.exec(before)
  if (!match || match[2].length < COLON_QUERY_MIN_LENGTH) return null
  return { start: cursor - match[2].length - 1, query: match[2] }
}

/** Whether the draft is a react command, as on Discord: `+:name:` or `+` and
 *  an emoji reacts to the newest message instead of being sent. */
export function reactionCommandOf(text: string): { name: string } | { emoji: string } | null {
  const trimmed = text.trim()
  const named = /^\+:([\w+-]+):$/u.exec(trimmed)
  if (named) return { name: named[1] }
  const direct = /^\+(\p{Extended_Pictographic}[\p{Extended_Pictographic}\p{Emoji_Modifier}️‍]*)$/u.exec(trimmed)
  if (direct) return { emoji: direct[1] }
  return null
}

/** Whether a `:query` sits in a react command (`+:par`), where custom emoji
 *  can be picked; elsewhere in a message they cannot be sent. */
export function colonQueryIsReactionCommand(text: string, start: number): boolean {
  return /^\s*\+$/.test(text.slice(0, start))
}

/** The emoji a `+:name:` command names: the first choice whose whole name is
 *  that name (the room's own custom emoji come first in `choices`). */
export function choiceNamed(choices: readonly EmojiChoice[], name: string): EmojiChoice | null {
  const normalized = normalizedEmojiQuery(name)
  return choices.find(choice => emojiNameMatchRank(choice.name, normalized) === 0) ?? null
}

/** The favorites that still name an emoji: a custom one deleted from its
 *  server since it was favorited is left out. Unicode names come from the
 *  table, matched without a skin tone. */
export function favoriteChoices(favoriteKeys: readonly string[], custom: readonly DiscordCustomEmoji[],
  unicodeNames: ReadonlyMap<string, string>): EmojiChoice[] {
  const customByKey = new Map(custom.map(emoji => [emoji.reaction_key, emoji]))
  return favoriteKeys.flatMap(key => {
    if (key.startsWith(DISCORD_EMOJI_KEY_PREFIX)) {
      const emoji = customByKey.get(key)
      return emoji ? [customChoice(emoji)] : []
    }
    return [{ kind: 'unicode' as const, key, name: unicodeNames.get(withoutSkinTone(key)) ?? key }]
  })
}

/** Unicode emoji names by emoji, for naming favorites. */
export function unicodeNamesOf(groups: readonly EmojiGroup[]): Map<string, string> {
  return new Map(groups.flatMap(group => group.emojis.map(([emoji, name]) => [withoutSkinTone(emoji), name] as const)))
}

function withoutSkinTone(emoji: string): string {
  return emoji.replace(/[\u{1F3FB}-\u{1F3FF}]/gu, '').replace(/️/g, '')
}

/** The favorites with `key` added at the end, or taken out if it is there. */
export function withFavoriteToggled(favoriteKeys: readonly string[], key: string): string[] {
  return favoriteKeys.includes(key) ? favoriteKeys.filter(k => k !== key) : [...favoriteKeys, key]
}

/** The favorites with `key` moved one place earlier (-1) or later (+1). */
export function withFavoriteMoved(favoriteKeys: readonly string[], key: string, step: -1 | 1): string[] {
  const from = favoriteKeys.indexOf(key)
  const to = from + step
  if (from < 0 || to < 0 || to >= favoriteKeys.length) return [...favoriteKeys]
  const next = [...favoriteKeys]
  ;[next[from], next[to]] = [next[to], next[from]]
  return next
}

/** Whether a reaction group on a message is the emoji `key` names. A custom
 *  emoji's reactions carry the bridge's mxc:// URI (live) or its
 *  `discord-emoji:` key (archive); unicode ones carry the emoji. */
export function reactionGroupIsEmoji(group: MessageReactionGroup, key: string,
  custom: readonly DiscordCustomEmoji[]): boolean {
  if (group.key === key) return true
  if (!key.startsWith(DISCORD_EMOJI_KEY_PREFIX)) return false
  const emoji = custom.find(e => e.reaction_key === key)
  return !!emoji?.bridge_mxc && group.key === emoji.bridge_mxc
}

/** What a custom emoji's image is loaded from. */
export function customEmojiImageURL(emoji: DiscordCustomEmoji): string {
  return discordEmojiURL(emoji.discord_emoji_id, emoji.animated)
}

/** What the Emoji page says about the listing of every server's emoji:
 *  its progress while it runs, else how many servers are listed. */
export function serverSweepSummary(sweep: DiscordServerSweepStatus, serversListed: number): string {
  if (sweep.running) {
    return sweep.servers_to_list > 0
      ? `Listing your servers' emoji: ${sweep.servers_listed} of ${sweep.servers_to_list} done, one server at a time.`
      : 'Asking Discord which servers you are in.'
  }
  const listed = `${serversListed} ${serversListed === 1 ? 'server' : 'servers'} listed.`
  return sweep.finished_at
    ? `${listed} The last listing checked ${sweep.servers_to_list} of your ${sweep.servers_in_account} servers.`
    : `${listed} The next listing starts the first time you open Messages tomorrow, and lists only servers not listed in the past week.`
}
