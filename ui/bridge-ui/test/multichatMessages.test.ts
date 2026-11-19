import { describe, expect, it } from 'vitest'
import { DEFAULT_BRIDGE_ROUTES, type BridgeConfig } from '../src/context'
import { activeNavEntry, groupForPath, navEntriesFor } from '../src/pages'
import {
  MESSAGE_PAGE_SIZE, contactListPlatforms, contactTagAssignBody, contactTagRemovePath, contactTags, conversationMessagesPath,
  conversationPlatforms, conversationSendPath, conversationShownName, conversationTagAssignBody, conversationTagRemovePath, conversationTags, filterContacts, filterConversations, highlightSegments,
  mergeNewestMessages, messageTimeLabel, orderedSearchGroups, pageMessages, prependOlderMessages, sameMessages,
  searchPath, foldEdits, conversationPreviewText, reactionPostPath, reactionTakeBackPath, reactionTargetEventID, sendMessageBodyOf, tagCreateBodyOf, tagDeletePath,
  contactKey, contactLinkBody, contactUnlinkBody, linkCandidates, personOfIdentity, OPERATOR_LINK_REASON,
  conversationEntries, conversationEntryOf, conversationLinkBody, conversationLinkCandidates, conversationTabLabel, conversationUnlinkBody,
  conversationVisibilityPath, conversationVisibilityReason,
} from '../src/multichatMessages'
import type {
  MultichatContactTagMap, MultichatConversation, MultichatConversationTagMap, MultichatMessage, MultichatTag, MultichatUnifiedContact,
} from '../src/types-multichat'

const DEFAULT_CONFIG: Partial<BridgeConfig> = { fetch: async () => new Response(), basePath: '/api/bridge', routes: DEFAULT_BRIDGE_ROUTES }
const ROOM = '!yyPKASKymivfZvWbDf:chat.kayushkin.com'
const message = (event_id: string, timestamp: number, extra: Partial<MultichatMessage> = {}): MultichatMessage =>
  ({ event_id, sender: '@whatsapp_1:chat.kayushkin.com', body: event_id, msg_type: 'm.text', timestamp, ...extra })
const ids = (messages: readonly MultichatMessage[]) => messages.map(m => m.event_id)

describe('message paths and bodies', () => {
  it('encodes the room id as one path segment', () => {
    expect(conversationMessagesPath(ROOM)).toBe(`/conversations/!yyPKASKymivfZvWbDf%3Achat.kayushkin.com/messages?limit=${MESSAGE_PAGE_SIZE}`)
    expect(conversationSendPath(ROOM)).toBe('/conversations/!yyPKASKymivfZvWbDf%3Achat.kayushkin.com/send')
  })

  it('passes the end token of the page after as from', () => {
    expect(conversationMessagesPath(ROOM, 't18-9615_0_0')).toMatch(/\?limit=50&from=t18-9615_0_0$/)
    expect(conversationMessagesPath(ROOM, null)).not.toContain('from=')
  })

  it('trims a send and refuses an empty one', () => {
    expect(sendMessageBodyOf('  on my way \n')).toEqual({ ok: true, body: { body: 'on my way' } })
    expect(sendMessageBodyOf(' \n ')).toEqual({ ok: false, error: 'Nothing to send.' })
  })

  it('reads a page with no messages as empty', () => {
    expect(pageMessages({ messages: null, has_more: false })).toEqual([])
  })
})

describe('merging pages', () => {
  it('puts an older page in front and keeps a message on both sides once', () => {
    const shown = [message('c', 3), message('d', 4)]
    const older = [message('a', 1), message('b', 2), message('c', 3)]
    expect(ids(prependOlderMessages(shown, older))).toEqual(['a', 'b', 'c', 'd'])
  })

  it('keeps the pages loaded further back when the newest page is polled', () => {
    const shown = [message('a', 1), message('b', 2), message('c', 3)]
    const newest = [message('b', 2), message('c', 3), message('d', 4)]
    expect(ids(mergeNewestMessages(shown, newest))).toEqual(['a', 'b', 'c', 'd'])
  })

  it('lets the newest page replace what it covers, so a deleted message goes', () => {
    const shown = [message('a', 1), message('b', 2), message('gone', 3), message('c', 4)]
    const newest = [message('b', 2), message('c', 4)]
    expect(ids(mergeNewestMessages(shown, newest))).toEqual(['a', 'b', 'c'])
  })

  it('keeps what is shown when the newest page is empty', () => {
    const shown = [message('a', 1)]
    expect(ids(mergeNewestMessages(shown, []))).toEqual(['a'])
  })

  it('tells an unchanged list from a changed one by event id and order', () => {
    expect(sameMessages([message('a', 1), message('b', 2)], [message('a', 1), message('b', 2)])).toBe(true)
    expect(sameMessages([message('a', 1), message('b', 2)], [message('b', 2), message('a', 1)])).toBe(false)
    expect(sameMessages([message('a', 1)], [message('a', 1), message('b', 2)])).toBe(false)
  })

  it('counts a changed reaction as a change, so the poll redraws it', () => {
    const thumbs = { key: '\u{1F44D}', shortcode: '', count: 1, sender_display_names: ['Ann'] }
    expect(sameMessages([message('a', 1, { reactions: [thumbs] })], [message('a', 1, { reactions: [thumbs] })])).toBe(true)
    expect(sameMessages([message('a', 1)], [message('a', 1, { reactions: [thumbs] })])).toBe(false)
    expect(sameMessages([message('a', 1, { reactions: [thumbs] })], [message('a', 1, { reactions: [{ ...thumbs, count: 2 }] })])).toBe(false)
  })
})

const conversation = (room_id: string, name: string, extra: Partial<MultichatConversation> = {}): MultichatConversation =>
  ({ room_id, name, member_count: 2, last_activity: 0, visibility: 'auto', hidden: false, ...extra })
const tag = (id: number, name: string): MultichatTag => ({ id, name, color: '#f5d40c' })
const NO_TAGS = { contactTagMap: {}, conversationTagMap: {} }

describe('the conversation list', () => {
  const conversations = [
    conversation('!1', 'Alvaro Lopez', { platform: 'whatsapp', last_message: 'Invoice attached', members: [{ user_id: '@whatsapp_1', display_name: 'Alvaro Lopez' }, { user_id: '@meta_1', display_name: 'Ana Ruiz' }] }),
    conversation('!2', '#food', { platform: 'discord', last_message: 'Where is this?' }),
    conversation('!3', 'No platform yet'),
  ]

  it('lists each app once, sorted, and skips a room with none', () => {
    expect(conversationPlatforms(conversations)).toEqual(['discord', 'whatsapp'])
  })

  it('filters by app and by name or last message, in any case', () => {
    expect(filterConversations(conversations, NO_TAGS, { platform: 'discord', text: '', tagID: null, showHidden: false }).map(c => c.room_id)).toEqual(['!2'])
    expect(filterConversations(conversations, NO_TAGS, { platform: '', text: 'INVOICE', tagID: null, showHidden: false }).map(c => c.room_id)).toEqual(['!1'])
    expect(filterConversations(conversations, NO_TAGS, { platform: '', text: ' food ', tagID: null, showHidden: false }).map(c => c.room_id)).toEqual(['!2'])
    expect(filterConversations(conversations, NO_TAGS, { platform: '', text: '', tagID: null, showHidden: false })).toHaveLength(3)
  })

  it('leaves hidden rooms out unless they are asked for', () => {
    const rooms = [...conversations, conversation('!big', 'Stock alerts', { member_count: 342, hidden: true })]
    expect(filterConversations(rooms, NO_TAGS, { platform: '', text: '', tagID: null, showHidden: false }).map(c => c.room_id)).toEqual(['!1', '!2', '!3'])
    expect(filterConversations(rooms, NO_TAGS, { platform: '', text: 'stock', tagID: null, showHidden: false })).toEqual([])
    expect(filterConversations(rooms, NO_TAGS, { platform: '', text: 'stock', tagID: null, showHidden: true }).map(c => c.room_id)).toEqual(['!big'])
  })

  it('says why a room is hidden or shown', () => {
    const rule = { auto_hide_enabled: true, auto_hide_member_count_threshold: 30 }
    expect(conversationVisibilityPath(ROOM)).toBe('/conversations/!yyPKASKymivfZvWbDf%3Achat.kayushkin.com/visibility')
    expect(conversationVisibilityReason(conversation('!a', 'a', { visibility: 'hidden', hidden: true }), rule)).toBe('Hidden by you')
    expect(conversationVisibilityReason(conversation('!a', 'a', { visibility: 'shown', member_count: 99 }), rule)).toBe('Always shown, by you')
    expect(conversationVisibilityReason(conversation('!a', 'a', { member_count: 31, hidden: true }), rule)).toBe('Hidden automatically: more than 30 members')
    expect(conversationVisibilityReason(conversation('!a', 'a', { member_count: 30 }), rule)).toBe('Shown: 30 members or fewer')
    expect(conversationVisibilityReason(conversation('!a', 'a'), { ...rule, auto_hide_enabled: false })).toBe('Shown; auto-hide is off')
  })

  it("matches a member's name even when the room name leaves it out", () => {
    expect(filterConversations(conversations, NO_TAGS, { platform: '', text: 'ruiz', tagID: null, showHidden: false }).map(c => c.room_id)).toEqual(['!1'])
  })

  it('shows a room named after its members by the first three and a count of the rest', () => {
    const member = (n: number) => ({ user_id: `@meta_${n}`, display_name: `Person ${n}` })
    const named = (count: number) => conversation('!g', 'unused', {
      named_after_members: true, members: Array.from({ length: count }, (_, i) => member(i + 1)),
    })
    expect(conversationShownName({ ...named(3), name: 'Person 1, Person 2, Person 3' })).toBe('Person 1, Person 2, Person 3')
    expect(conversationShownName(named(4))).toBe('Person 1, Person 2, Person 3 and 1 other')
    expect(conversationShownName(named(6))).toBe('Person 1, Person 2, Person 3 and 3 others')
    expect(conversationShownName({ ...named(6), named_after_members: false, name: 'Book club' })).toBe('Book club')
  })

  describe('tags', () => {
    const dm = conversation('!dm', 'Alvaro', { platform: 'discord', direct_message_partner_user_id: '@discord_1', members: [{ user_id: '@discord_1' }] })
    const channel = conversation('!channel', '#general', { platform: 'discord', members: [{ user_id: '@discord_1' }, { user_id: '@discord_2' }] })
    const contactTagMap: MultichatContactTagMap = { '@discord_1': [tag(2, 'Reefer'), tag(1, 'LTL')], '@discord_2': [tag(3, 'Zeta')] }
    const conversationTagMap: MultichatConversationTagMap = { '!dm': [tag(1, 'LTL'), tag(4, 'Alpha')], '!channel': [tag(3, 'Zeta')] }
    const tagMaps = { contactTagMap, conversationTagMap }

    it("gives a DM its own tags and its partner's, once each, by name", () => {
      expect(conversationTags(dm, tagMaps).map(t => t.name)).toEqual(['Alpha', 'LTL', 'Reefer'])
    })

    it("gives a group or channel only its own tags, never its members'", () => {
      expect(conversationTags(channel, tagMaps).map(t => t.name)).toEqual(['Zeta'])
      expect(conversationTags(channel, { contactTagMap, conversationTagMap: {} })).toEqual([])
    })

    it('filters by a tag the room has by either route', () => {
      const rooms = [dm, channel]
      const byTag = (tagID: number) => filterConversations(rooms, tagMaps, { platform: '', text: '', tagID, showHidden: false }).map(c => c.room_id)
      expect(byTag(2)).toEqual(['!dm'])
      expect(byTag(3)).toEqual(['!channel'])
      expect(byTag(9)).toEqual([])
    })

    it('writes a room tag by room id, encoded', () => {
      expect(conversationTagAssignBody('!dm:x', 4)).toEqual({ room_id: '!dm:x', tag_id: 4 })
      expect(conversationTagRemovePath('!dm:x', 4)).toBe('/conversations/tags?room=%21dm%3Ax&tag=4')
    })
  })
})

describe('search', () => {
  it('refuses an empty term and encodes the rest', () => {
    expect(searchPath('   ')).toBeNull()
    expect(searchPath(' rate & time ')).toBe('/search?q=rate+%26+time')
  })

  it('orders groups by their newest hit and hits newest first', () => {
    const hit = (event_id: string, timestamp: number) =>
      ({ event_id, timestamp, room_id: '', room_name: '', platform: '', sender_name: '', body: '' })
    const ordered = orderedSearchGroups({
      query: 'x', total: 3,
      groups: [
        { room_id: '!old', room_name: 'old', platform: 'signal', results: [hit('o1', 10)] },
        { room_id: '!new', room_name: 'new', platform: 'whatsapp', results: [hit('n1', 20), hit('n2', 30)] },
        { room_id: '!none', room_name: 'none', platform: 'discord', results: null },
      ],
    })
    expect(ordered.map(g => g.room_id)).toEqual(['!new', '!old', '!none'])
    expect(ordered[0].results.map(h => h.event_id)).toEqual(['n2', 'n1'])
  })

  it('reads a null group list as none', () => {
    expect(orderedSearchGroups({ query: 'x', total: 0, groups: null })).toEqual([])
  })

  it('marks every match in any case and treats the term as plain text', () => {
    expect(highlightSegments('Rate? rate. RATE', 'rate')).toEqual([
      { text: 'Rate', match: true }, { text: '? ', match: false }, { text: 'rate', match: true },
      { text: '. ', match: false }, { text: 'RATE', match: true },
    ])
    expect(highlightSegments('costs $5 (maybe)', '(maybe')).toEqual([
      { text: 'costs $5 ', match: false }, { text: '(maybe', match: true }, { text: ')', match: false },
    ])
    expect(highlightSegments('nothing here', '')).toEqual([{ text: 'nothing here', match: false }])
  })
})

describe('contacts and tags', () => {
  const alvaro: MultichatUnifiedContact = {
    display_name: 'Alvaro Lopez',
    identities: [
      { user_id: '@whatsapp_15614001607:chat.kayushkin.com', platform: 'whatsapp' },
      { user_id: '@meta_621211729:chat.kayushkin.com', platform: 'meta' },
    ],
  }
  const slava: MultichatUnifiedContact = {
    display_name: 'Slava',
    identities: [{ user_id: '@discord_110122051179687936:chat.kayushkin.com', platform: 'discord' }],
  }
  const tagMap: MultichatContactTagMap = {
    '@meta_621211729:chat.kayushkin.com': [tag(4, 'Green Card')],
    '@whatsapp_15614001607:chat.kayushkin.com': [tag(4, 'Green Card'), tag(1, 'LTL')],
  }
  const none = { text: '', platform: '', tagID: null, onSeveralApps: false }

  it("shows a contact's tags from all its identities, once each", () => {
    expect(contactTags(alvaro, tagMap).map(t => t.id)).toEqual([4, 1])
    expect(contactTags(slava, tagMap)).toEqual([])
  })

  it('filters by name or user id, app, tag and being on several apps', () => {
    const contacts = [alvaro, slava]
    expect(filterContacts(contacts, tagMap, none)).toHaveLength(2)
    expect(filterContacts(contacts, tagMap, { ...none, text: 'slav' })).toEqual([slava])
    expect(filterContacts(contacts, tagMap, { ...none, text: '15614001607' })).toEqual([alvaro])
    expect(filterContacts(contacts, tagMap, { ...none, platform: 'discord' })).toEqual([slava])
    expect(filterContacts(contacts, tagMap, { ...none, tagID: 1 })).toEqual([alvaro])
    expect(filterContacts(contacts, tagMap, { ...none, onSeveralApps: true })).toEqual([alvaro])
  })

  it('lists every app any contact is on', () => {
    expect(contactListPlatforms([alvaro, slava])).toEqual(['discord', 'meta', 'whatsapp'])
  })

  it('puts a tag on one identity by its user id and takes it off by query', () => {
    expect(contactTagAssignBody('@meta_621211729:chat.kayushkin.com', 4)).toEqual({ contact_user_id: '@meta_621211729:chat.kayushkin.com', tag_id: 4 })
    expect(contactTagRemovePath('@meta_621211729:chat.kayushkin.com', 4)).toBe('/contacts/tags?contact=%40meta_621211729%3Achat.kayushkin.com&tag=4')
    expect(tagDeletePath(7)).toBe('/tags?id=7')
  })

  it('names a new tag, and refuses an empty or taken name', () => {
    expect(tagCreateBodyOf('  Flatbed ', [tag(1, 'LTL')])).toEqual({ ok: true, body: { name: 'Flatbed' } })
    expect(tagCreateBodyOf(' ', [])).toEqual({ ok: false, error: 'A tag needs a name.' })
    expect(tagCreateBodyOf('LTL', [tag(1, 'LTL')])).toEqual({ ok: false, error: 'There is already a tag named "LTL".' })
  })
})

describe('messageTimeLabel', () => {
  it('shows the time alone for today and adds the date otherwise', () => {
    const now = new Date(2026, 8, 28, 15, 0)
    const today = messageTimeLabel(new Date(2026, 8, 28, 9, 30).getTime(), now)
    const earlier = messageTimeLabel(new Date(2026, 8, 20, 9, 30).getTime(), now)
    const lastYear = messageTimeLabel(new Date(2025, 8, 20, 9, 30).getTime(), now)
    expect(earlier.endsWith(today)).toBe(true)
    expect(earlier.length).toBeGreaterThan(today.length)
    expect(lastYear).toContain('2025')
  })
})

describe('the Messages group in the registry', () => {
  const config = (multichatBasePath: string) => ({ ...DEFAULT_CONFIG, multichatBasePath }) as BridgeConfig
  const flags = { showConformance: false, showServiceInventory: false }

  it('lists its pages only where the host proxies multichat', () => {
    const labelsOf = (base: string) => navEntriesFor(config(base), flags).filter(e => e.group === 'messages').map(e => e.label)
    expect(labelsOf('')).toEqual([])
    expect(labelsOf('/api/multichat')).toEqual(['Waiting on you', 'Conversations', 'Search', 'Contacts', 'Discord', 'Emoji', 'Auto-reactions'])
  })

  it('lights up Messages on every page of it, and Conversations on its own path and on a chat\'s short name', () => {
    expect(groupForPath('/messages', DEFAULT_BRIDGE_ROUTES)).toBe('messages')
    expect(groupForPath('/messages/search', DEFAULT_BRIDGE_ROUTES)).toBe('messages')
    expect(groupForPath('/messages/contacts', DEFAULT_BRIDGE_ROUTES)).toBe('messages')
    expect(groupForPath('/messages/20s30s/general', DEFAULT_BRIDGE_ROUTES)).toBe('messages')
    const row = navEntriesFor(config('/api/multichat'), flags).filter(e => e.group === 'messages')
    const lit = (path: string) => activeNavEntry(path, row)?.label
    expect(lit('/messages')).toBe('Conversations')
    expect(lit('/messages/kat')).toBe('Conversations')
    expect(lit('/messages/20s30s/general')).toBe('Conversations')
    expect(lit('/messages/search')).toBe('Search')
    expect(lit('/messages/auto-reactions')).toBe('Auto-reactions')
    expect(lit('/messagesx')).toBeUndefined()
  })
})

describe('foldEdits', () => {
  const edited = (event_id: string, timestamp: number, body: string, replaces_event_id = ''): MultichatMessage => ({
    event_id, sender: '@discord_1:x', body, msg_type: 'm.text', timestamp, replaces_event_id,
    format: 'org.matrix.custom.html', formatted_body: `<p>${body}</p>`,
    reactions: event_id === '$post' ? [{ key: '❤️', shortcode: '', count: 1, sender_display_names: ['Kai'] }] : [],
  })
  it('shows an edited message once, with its newest edit and its own reactions', () => {
    const shown = foldEdits([
      edited('$post', 1, 'Going (3)'),
      edited('$other', 2, 'hi'),
      edited('$edit2', 4, 'Going (5)', '$post'),
      edited('$edit1', 3, 'Going (4)', '$post'),
    ])
    expect(shown.map(m => [m.event_id, m.body, m.edited])).toEqual([['$post', 'Going (5)', true], ['$other', 'hi', false]])
    expect(shown[0].formatted_body).toBe('<p>Going (5)</p>')
    expect(shown[0].timestamp).toBe(1)
    expect(shown[0].reactions?.[0].key).toBe('❤️')
  })
  it('shows the newest edit alone when its original is on an older page', () => {
    const shown = foldEdits([edited('$edit1', 3, 'Going (4)', '$post'), edited('$edit2', 4, 'Going (5)', '$post')])
    expect(shown.map(m => [m.event_id, m.body, m.edited])).toEqual([['$edit2', 'Going (5)', true]])
  })
})

describe('conversationPreviewText', () => {
  it('reads Discord markdown as one plain line', () => {
    expect(conversationPreviewText('### Jazz Night at Lo-Bar!\n📍 Lo-Bar <#1554134165673353317>\n✅ **Going** (5): <@123> <:laugh:1366054428427157645>'))
      .toBe('Jazz Night at Lo-Bar! 📍 Lo-Bar #channel ✅ Going (5): @someone :laugh:')
  })
  it('leaves plain text alone', () => {
    expect(conversationPreviewText('Sure, that sounds good')).toBe('Sure, that sounds good')
  })
})

describe('reaction routes', () => {
  it('escapes the room and the reaction as single path segments', () => {
    expect(reactionPostPath('!a:chat.kayushkin.com')).toBe('/conversations/!a%3Achat.kayushkin.com/reactions')
    expect(reactionTakeBackPath('!a:x', '$abc/def')).toBe('/conversations/!a%3Ax/reactions/%24abc%2Fdef')
  })
  it('reacts to the original when an edit is shown alone', () => {
    expect(reactionTargetEventID({ event_id: '$edit', replaces_event_id: '$post' } as MultichatMessage)).toBe('$post')
    expect(reactionTargetEventID({ event_id: '$post' } as MultichatMessage)).toBe('$post')
  })
})

describe('linking one person across apps', () => {
  const texts = '@gmessages_1.630:chat.kayushkin.com'
  const midnaID = '@discord_493904201101869067:chat.kayushkin.com'
  const maleeha: MultichatUnifiedContact = {
    person_id: 'person_000040', display_name: 'Maleeha',
    identities: [{ user_id: texts, platform: 'gmessages', display_name: 'Maleeha' }],
  }
  const midna: MultichatUnifiedContact = {
    display_name: 'Twili Midna', identities: [{ user_id: midnaID, platform: 'discord', display_name: 'Twili Midna' }],
  }
  const mom: MultichatUnifiedContact = {
    display_name: 'Mom', identities: [{ user_id: '@whatsapp_19545474042:chat.kayushkin.com', platform: 'whatsapp', display_name: 'Mom' }],
  }
  const contacts = [maleeha, midna, mom]

  it('keys a person by people-store id, else by their one identity', () => {
    expect(contactKey(maleeha)).toBe('person_000040')
    expect(contactKey(midna)).toBe(midnaID)
  })

  it('finds the person an identity belongs to', () => {
    expect(personOfIdentity(contacts, texts)).toBe(maleeha)
    expect(personOfIdentity(contacts, '@nobody:x')).toBeUndefined()
  })

  it('offers other people by any of their names, and no one for an empty query', () => {
    expect(linkCandidates(contacts, texts, 'midna')).toEqual([midna])
    expect(linkCandidates(contacts, texts, 'male')).toEqual([])
    expect(linkCandidates(contacts, midnaID, 'MALE')).toEqual([maleeha])
    expect(linkCandidates(contacts, texts, '  ')).toEqual([])
  })

  it('links to an existing person by id, or to a lone identity by its user id', () => {
    expect(contactLinkBody(midnaID, maleeha)).toEqual({ user_id: midnaID, linked_by: 'operator', reason: OPERATOR_LINK_REASON, person_id: 'person_000040' })
    expect(contactLinkBody(texts, midna)).toEqual({ user_id: texts, linked_by: 'operator', reason: OPERATOR_LINK_REASON, with_user_id: midnaID })
    expect(contactUnlinkBody(texts)).toEqual({ user_id: texts, removed_by: 'operator' })
  })
})


describe('linked conversations', () => {
  const room = (room_id: string, platform: string, last_activity: number, name = room_id): MultichatConversation =>
    ({ room_id, name, platform, member_count: 4, last_activity })
  const texts = room('!texts:chat.kayushkin.com', 'gmessages', 300, 'Lillian, Loukic, Maleeha')
  const discord = room('!discord:chat.kayushkin.com', 'discord', 500, 'Logan, Twili Midna, KiLlersLuvbLaDe')
  const general = room('!general:chat.kayushkin.com', 'discord', 400, '#general')
  const all = [discord, general, texts]
  const links = [{ id: 1, room_ids: ['!discord:chat.kayushkin.com', '!texts:chat.kayushkin.com'] }]

  it('lists a link as one row named after its newest room, ordered by that room', () => {
    const entries = conversationEntries(all, all, links)
    expect(entries.map(entry => entry.conversations.map(c => c.room_id))).toEqual([
      ['!discord:chat.kayushkin.com', '!texts:chat.kayushkin.com'],
      ['!general:chat.kayushkin.com'],
    ])
    expect(entries[0].linkID).toBe(1)
    expect(entries[1].linkID).toBeNull()
  })

  it('brings in the whole link when only one of its rooms passes the filter', () => {
    const entries = conversationEntries(all, [texts], links)
    expect(entries).toHaveLength(1)
    expect(entries[0].conversations.map(c => c.platform)).toEqual(['discord', 'gmessages'])
  })

  it('leaves out a linked room multichat does not list', () => {
    const entries = conversationEntries([texts], [texts], links)
    expect(entries[0].conversations.map(c => c.room_id)).toEqual(['!texts:chat.kayushkin.com'])
  })

  it('finds the row of a room, labels tabs by app, and offers only rooms outside the link', () => {
    const entries = conversationEntries(all, all, links)
    const entry = conversationEntryOf(entries, '!texts:chat.kayushkin.com')!
    expect(entry.linkID).toBe(1)
    expect(conversationTabLabel(entry, texts)).toBe('gmessages')
    expect(conversationTabLabel({ linkID: 2, conversations: [discord, general] }, general)).toBe('discord · #general')
    expect(conversationLinkCandidates(all, entry).map(c => c.room_id)).toEqual(['!general:chat.kayushkin.com'])
    expect(conversationLinkBody('!a:x', '!b:x')).toEqual({ room_ids: ['!a:x', '!b:x'] })
    expect(conversationUnlinkBody('!a:x')).toEqual({ room_id: '!a:x' })
  })
})
