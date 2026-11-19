import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useLocation, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useBridgeConfig } from '../../context'
import type { DiscordBridgeStatus } from '@kayushkin/multichat-types'
import { bridgedChannelsOf, DISCORD_PLATFORM } from '../../discordLog'
import { insertAtSelection } from '../../emojiPicker'
import { discordNamesOf, NO_DISCORD_NAMES, type DiscordNames } from '../../messageBody'
import {
  conversationMessagesPath, conversationOwnTags, conversationPartnerTags, conversationPlatforms, conversationSendFilePath, conversationSendPath, conversationShownName,
  conversationTagAssignBody, conversationTagRemovePath, conversationTags, filterConversations, type ConversationTagMaps,
  conversationLinkBody, conversationLinkCandidates, conversationUnlinkBody,
  conversationVisibilityPath, conversationVisibilityReason,
  conversationPreviewText, mergeNewestMessages, messageTimeLabel, pageMessages, prependOlderMessages, sameMessages, sendMessageBodyOf,
  reactionPostPath, reactionTakeBackPath, reactionTargetEventID,
} from '../../multichatMessages'
import { reactionChipAction, withReactionPosted, withReactionTakenBack } from '../../messageReactions'
import {
  allCustomEmoji, choiceNamed, colonQueryAt, colonQueryIsReactionCommand, reactionCommandOf, reactionGroupIsEmoji, roomDiscordServerID, type DiscordRoom,
  searchEmojiIndex, type EmojiChoice,
} from '../../emojiCatalog'
import type { ConversationLink, ConversationVisibility, ConversationVisibilitySettings, MessageReactionGroup } from '@kayushkin/multichat-types'
import type {
  MultichatContactTagMap, MultichatConversation, MultichatConversationTagMap, MultichatMessage, MultichatMessagePage,
  MultichatSendAnswer, MultichatSendFileAnswer, MultichatTag, MultichatUnifiedContact,
} from '../../types-multichat'
import {
  addToPersonChatsBody, chatLabel, conversationListRowName, conversationListRows, personParamOf, shownRowOf,
  storedChatLabel, storedChatSendAccount, storedChatsBehindRooms, historyMessageShown, historyMessagesPath, historyOnlyChats, personHistoryConversationsPath, defaultSendRoom, mergedMessages,
  peopleInRoom, personKey, personOtherChats, roomsToPageBack, type ConversationListRow, type LoadedRoom, type MergedMessage,
} from '../../conversationPeople'
import { errorText, useMultichat } from './useMultichat'
import { MultichatNotConfigured, TagChip, TagChips } from './messagesShared'
import { MessageContent, ReactionChips } from './MessageContent'
import { EmojiFace, EmojiPickerButton, EmojiPickerPanel, EmojiSuggestionList, useEmojiChoices, type EmojiSuggestion } from './EmojiPicker'
import { useEmojiCatalog, useDailyNitroCheck } from './useEmojiCatalog'
import { captionsForFiles, humanFileSize, pendingAttachment, type PendingAttachment } from '../../messageAttachments'
import { filesFromPaste } from '../../sessionFileAttachments'
import styles from './Messages.module.css'
import {
  conversationAddress, messagesAddressOf, MESSAGES_SHORT_NAME_NAMESPACE, MULTICHAT_ROOM_ENTITY_TYPE, roomShortNames,
  type RoomShortName,
} from '../../messageShortNames'
import type { ShortName } from '../../types-short-name-store'
import { useRoomShortNames } from './useRoomShortNames'
import { ConversationPerson, useUnifiedContacts } from './PersonLinks'
import type { Account, Conversation as StoreConversation, MessagePage as StoreMessagePage } from '@kayushkin/message-store-types'
import { BridgeMessageAccountView, MessageAccountPicker, useMessageStoreAccounts } from './BridgeMessageAccountView'
import {
  ACCOUNT_CONVERSATION_PARAMETER, ACCOUNT_PARAMETER, MESSAGE_STORE_THROUGH_MULTICHAT, accountConversationParameters, accountPickParameters, pickableBotAccounts, pickedBotAccount,
} from '../../messageAccounts'

/** A thread with no stored chats beside its rooms. */
const NO_HISTORIES: readonly StoreConversation[] = []
/** A stored chat and the live room that carries it. */
type StoredChatBehindRoom = { roomID: string; conversation: StoreConversation }
/** A thread with no stored chats behind its rooms. */
const NO_STORED_BEHIND: readonly StoredChatBehindRoom[] = []

const CONVERSATIONS_POLL_MS = 30_000
const MESSAGES_POLL_MS = 5_000
/** Distance from the bottom, in pixels, still counted as reading the newest. */
const AT_BOTTOM_PX = 40
/** How many `:` suggestions show at once. */
const SUGGESTION_LIMIT = 8
/** Why a custom emoji cannot be picked into a message. */
const CUSTOM_EMOJI_IN_MESSAGE_REASON =
  'The Discord bridge cannot send a custom emoji inside a message. Start the message with +: to react with it instead.'

/**
 * The Conversations page, for the account the picker at its top names in
 * `?account=`: none is "Me", the operator's own chats from multichat; a bot
 * account's id shows what that bot sent and received, read-only from
 * message-store (`BridgeMessageAccountView`), its open conversation in
 * `&conversation=`. A host with no message-store draws no picker.
 */
export function BridgeMessageConversations() {
  const { configured: messageStoreConfigured, accounts, error: accountsError } = useMessageStoreAccounts()
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { routes } = useBridgeConfig()
  const accountID = messageStoreConfigured ? searchParams.get(ACCOUNT_PARAMETER) ?? '' : ''
  const goTo = (parameters: URLSearchParams) => {
    const query = parameters.toString()
    navigate(query ? `${routes.messageConversations}?${query}` : routes.messageConversations)
  }
  const accountPicker = messageStoreConfigured
    ? <MessageAccountPicker accounts={accounts} error={accountsError} pickedAccountID={accountID}
        onPick={picked => goTo(accountPickParameters(picked))} />
    : null
  if (!accountID) return <OwnConversations accountPicker={accountPicker} messageStoreAccounts={accounts} messageStoreAccountsError={accountsError} />
  const account = accounts ? pickedBotAccount(accounts, accountID) : null
  if (!account) {
    return (
      <div className={styles.page}>
        <header className={styles.header}>
          <h2 className={styles.title}>Conversations</h2>
          {accountPicker}
        </header>
        {accountsError ? null : !accounts ? <div className={styles.empty}>Loading…</div>
          : <pre className={styles.error}>message-store lists no bot account {accountID}.</pre>}
      </div>
    )
  }
  return (
    <BridgeMessageAccountView account={account} accountPicker={accountPicker}
      conversationID={searchParams.get(ACCOUNT_CONVERSATION_PARAMETER) ?? ''}
      onOpenConversation={conversationID => goTo(accountConversationParameters(account.id, conversationID))} />
  )
}

/**
 * Every conversation multichat's bridges carry — WhatsApp, Telegram, Signal,
 * Messenger, Discord, Slack — and one of them open to read and answer.
 * `/messages/<short name>` opens the room short-name-store gives that name;
 * `?room=<room_id>` opens a room by its id, which is how a room with no name
 * yet is addressed, and wins over the path when both are given.
 *
 * A message sent here reaches a real person on that app.
 */
function OwnConversations({ accountPicker, messageStoreAccounts, messageStoreAccountsError }: {
  accountPicker: ReactNode
  /** message-store's accounts: its bots' chats are left out of a person's
   *  stored chats. Null while loading or when the read failed. */
  messageStoreAccounts: Account[] | null
  messageStoreAccountsError: string | null
}) {
  useDailyNitroCheck()
  const { read, write, configured } = useMultichat()
  const [conversations, setConversations] = useState<MultichatConversation[] | null>(null)
  const [tags, setTags] = useState<MultichatTag[]>([])
  const [tagMaps, setTagMaps] = useState<ConversationTagMaps>({ contactTagMap: {}, conversationTagMap: {} })
  const [loadError, setLoadError] = useState<string | null>(null)
  const [tagsError, setTagsError] = useState<string | null>(null)
  const [links, setLinks] = useState<ConversationLink[]>([])
  const [linksError, setLinksError] = useState<string | null>(null)
  const [platform, setPlatform] = useState('')
  const [text, setText] = useState('')
  const [tagID, setTagID] = useState<number | null>(null)
  const [showHidden, setShowHidden] = useState(false)
  const [visibilitySettings, setVisibilitySettings] = useState<ConversationVisibilitySettings | null>(null)
  const [visibilitySettingsError, setVisibilitySettingsError] = useState<string | null>(null)
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { search } = useLocation()
  const { routes } = useBridgeConfig()
  const roomShortNameRoutes = useRoomShortNames()
  const { listRoomNames, resolve: resolveShortName } = roomShortNameRoutes
  const { contacts, error: contactsError } = useUnifiedContacts()
  const roomParameter = searchParams.get('room') ?? ''
  const splat = useParams()['*'] ?? ''
  const address = useMemo(() => messagesAddressOf(splat), [splat])
  const addressedPath = address.kind === 'path' ? address.path : ''
  const [shortNames, setShortNames] = useState<ShortName[] | null>(null)
  const [shortNamesError, setShortNamesError] = useState<string | null>(null)
  const [lookup, setLookup] = useState<AddressLookup | null>(null)

  const reloadTags = useCallback(async () => {
    const [nextTags, contactTagMap, conversationTagMap] = await Promise.all([
      read<MultichatTag[] | null>('/tags'),
      read<MultichatContactTagMap | null>('/contacts/tags/bulk'),
      read<MultichatConversationTagMap | null>('/conversations/tags/bulk'),
    ])
    setTags(nextTags ?? [])
    setTagMaps({ contactTagMap: contactTagMap ?? {}, conversationTagMap: conversationTagMap ?? {} })
  }, [read])

  const reload = useCallback(async () => {
    try {
      setConversations(await read<MultichatConversation[] | null>('/conversations') ?? [])
      setLoadError(null)
    } catch (err) {
      setLoadError(errorText(err))
    }
    // Links only group the rows, so the list still shows, one row per room, when they fail.
    try {
      setLinks(await read<ConversationLink[] | null>('/conversations/links') ?? [])
      setLinksError(null)
    } catch (err) {
      setLinksError(errorText(err))
    }
    // The rule only explains why a room is hidden; multichat has already applied it.
    try {
      setVisibilitySettings(await read<ConversationVisibilitySettings>('/conversations/visibility-settings'))
      setVisibilitySettingsError(null)
    } catch (err) {
      setVisibilitySettingsError(errorText(err))
    }
    // Tags only label and filter the rows, so the list still shows when they fail.
    try {
      await reloadTags()
      setTagsError(null)
    } catch (err) {
      setTagsError(errorText(err))
    }
  }, [read, reloadTags])

  /** A link write, then the links read back from multichat; the refusal, or null. */
  const writeLinks = useCallback(async (path: string, body: unknown): Promise<string | null> => {
    const result = await write('POST', path, body)
    if (!result.ok) return result.error
    try {
      setLinks(await read<ConversationLink[] | null>('/conversations/links') ?? [])
      return null
    } catch (err) {
      return errorText(err)
    }
  }, [write, read])

  /** A tag write, then the tags read back from multichat; the refusal, or null. */
  const writeTags = useCallback(async (method: string, path: string, body?: unknown): Promise<string | null> => {
    const result = await write(method, path, body)
    if (!result.ok) return result.error
    try {
      await reloadTags()
      return null
    } catch (err) {
      return errorText(err)
    }
  }, [write, reloadTags])

  /** A visibility write, then the list read back from multichat, which
   *  decides what is hidden; the refusal, or null. */
  const writeVisibility = useCallback(async (path: string, body: unknown): Promise<string | null> => {
    const result = await write('PUT', path, body)
    if (!result.ok) return result.error
    try {
      setConversations(await read<MultichatConversation[] | null>('/conversations') ?? [])
      if (path === '/conversations/visibility-settings') {
        setVisibilitySettings(await read<ConversationVisibilitySettings>('/conversations/visibility-settings'))
      }
      return null
    } catch (err) {
      return errorText(err)
    }
  }, [write, read])

  useEffect(() => {
    if (!configured) return
    void reload()
    const timer = window.setInterval(() => { void reload() }, CONVERSATIONS_POLL_MS)
    return () => window.clearInterval(timer)
  }, [configured, reload])

  const reloadShortNames = useCallback(async () => {
    try {
      setShortNames(await listRoomNames())
      setShortNamesError(null)
    } catch (err) {
      setShortNamesError(errorText(err))
    }
  }, [listRoomNames])

  useEffect(() => {
    if (configured && roomShortNameRoutes.configured) void reloadShortNames()
  }, [configured, roomShortNameRoutes.configured, reloadShortNames])

  const roomNames = useMemo(() => roomShortNames(shortNames ?? [], links), [shortNames, links])
  /** Current path → the room that holds it, from the list already loaded. */
  const roomOfOwnPath = useMemo(() => {
    const byPath = new Map<string, string>()
    for (const [roomID, name] of roomNames) if (name.namedRoomID === roomID) byPath.set(name.path, roomID)
    return byPath
  }, [roomNames])
  const pathKnown = roomOfOwnPath.has(addressedPath)

  // A path the loaded list does not hold — superseded, new since the list
  // loaded, or not a name at all — is asked of the store. A superseded name
  // is replaced in the URL by the room's current one.
  useEffect(() => {
    if (!addressedPath || roomParameter || pathKnown || !roomShortNameRoutes.configured) return
    let canceled = false
    setLookup({ path: addressedPath, state: 'loading' })
    resolveShortName(addressedPath).then(answer => {
      if (canceled) return
      if (answer.kind === 'missing') { setLookup({ path: addressedPath, state: 'missing' }); return }
      const { current } = answer.resolution
      if (current.entity_type !== MULTICHAT_ROOM_ENTITY_TYPE) {
        setLookup({ path: addressedPath, state: 'error', error: `${MESSAGES_SHORT_NAME_NAMESPACE}/${addressedPath} names a ${current.entity_type}, not a chat.` })
        return
      }
      if (current.path !== addressedPath) {
        navigate(`${routes.messageConversations}/${current.path}${search}`, { replace: true })
        return
      }
      setLookup({ path: addressedPath, state: 'found', roomID: current.entity_id })
    }).catch(err => { if (!canceled) setLookup({ path: addressedPath, state: 'error', error: errorText(err) }) })
    return () => { canceled = true }
  }, [addressedPath, roomParameter, pathKnown, roomShortNameRoutes.configured, resolveShortName, navigate, routes.messageConversations, search])

  const currentLookup = lookup && lookup.path === addressedPath && !pathKnown && !roomParameter ? lookup : null
  const roomID = roomParameter || roomOfOwnPath.get(addressedPath) || (currentLookup?.state === 'found' ? currentLookup.roomID : '')

  // An old `?room=` link to a room that now holds a name moves to the name.
  useEffect(() => {
    if (address.kind !== 'none' || !roomParameter) return
    const name = roomNames.get(roomParameter)
    if (name && name.namedRoomID === roomParameter) {
      navigate(conversationAddress(routes.messageConversations, roomParameter, roomNames, searchParams), { replace: true })
    }
  }, [address.kind, roomParameter, roomNames, navigate, routes.messageConversations, searchParams])

  const platforms = useMemo(() => conversationPlatforms(conversations ?? []), [conversations])
  const shown = useMemo(
    () => filterConversations(conversations ?? [], tagMaps, { platform, text, tagID, showHidden }),
    [conversations, tagMaps, platform, text, tagID, showHidden],
  )
  const hiddenCount = useMemo(() => (conversations ?? []).filter(conversation => conversation.hidden).length, [conversations])
  const contactList = contacts ?? []
  const shownAllButText = useMemo(
    () => filterConversations(conversations ?? [], tagMaps, { platform, text: '', tagID, showHidden }),
    [conversations, tagMaps, platform, tagID, showHidden],
  )
  const rows = useMemo(
    () => conversationListRows(conversations ?? [], shown, links, contactList, { text, matchingAllButText: shownAllButText }),
    [conversations, shown, links, contactList, text, shownAllButText],
  )
  const allRows = useMemo(
    () => conversationListRows(conversations ?? [], conversations ?? [], links, contactList),
    [conversations, links, contactList],
  )
  /** `?person=`: whose page is open, kept while moving between their chats
   *  and the group chats they are in. */
  const personParam = searchParams.get('person')
  const pickedRow = useMemo(
    () => shownRowOf(allRows, roomID, personParam, conversations ?? [], contactList),
    [allRows, roomID, personParam, conversations, contactList],
  )
  const picked = conversations?.find(conversation => conversation.room_id === roomID) ?? null
  /** Every chat of the open row merged into one thread, by `?merged=1`. It
   *  stays in the URL as rows change, so merged is how every person opens
   *  until it is switched off. */
  const mergedWanted = searchParams.get('merged') === '1'
  const storedChats = usePersonStoredChats(pickedRow?.kind === 'person' ? pickedRow.person.person_id ?? '' : '')
  /** The person's one-to-one chats message-store keeps that no room carries. */
  const histories = useMemo(
    // Without the bot accounts no stored chat can be told apart from a bot's,
    // so none is shown until they are read; a failed read is reported below.
    () => (storedChats.conversations && messageStoreAccounts
      ? historyOnlyChats(storedChats.conversations, conversations ?? [], pickableBotAccounts(messageStoreAccounts)) : []),
    [storedChats.conversations, conversations, messageStoreAccounts],
  )
  /** `?history=`: the stored chat open in place of a room. */
  const historyParam = searchParams.get('history')
  const pickedHistory = histories.find(history => history.id === historyParam) ?? null
  const merged = mergedWanted && !!pickedRow && pickedRow.chats.length + histories.length > 1
    && pickedRow.chats.some(chat => chat.room_id === roomID) && !pickedHistory

  /** Opens a room. `person` is whose page to stay on: a person's key, null
   *  for none, or left out to keep the one open. */
  const pick = (nextRoomID: string, mergedNext: boolean = mergedWanted, person: string | null | undefined = undefined) => {
    const keep = new URLSearchParams(searchParams)
    if (mergedNext) keep.set('merged', '1')
    else keep.delete('merged')
    const nextPerson = person === undefined ? personParam : person
    if (nextPerson) keep.set('person', nextPerson)
    else keep.delete('person')
    keep.delete('history')
    navigate(conversationAddress(routes.messageConversations, nextRoomID, roomNames, keep), { replace: true })
  }

  /** The rooms the thread shows: none for a stored chat alone, every chat of
   *  the row when merged, else the open room. */
  const threadRooms = useMemo(
    () => (pickedHistory || !picked ? [] : merged && pickedRow ? pickedRow.chats : [picked]),
    [pickedHistory, picked, merged, pickedRow],
  )
  /** The stored past of those rooms, from before each was bridged. */
  const storedBehind = useMemo(
    () => storedChatsBehindRooms(storedChats.conversations ?? [], threadRooms),
    [storedChats.conversations, threadRooms],
  )
  /** A stored chat alone, answered from the person's account on its app:
   *  the first send opens its room. Undefined when no route opens one. */
  const openPickedHistory = useMemo(() => {
    if (!pickedHistory || pickedRow?.kind !== 'person') return undefined
    const account = storedChatSendAccount(pickedHistory, pickedRow.person)
    if (!account) return undefined
    return {
      label: `${pickedRow.person.display_name} on ${pickedHistory.platform}`,
      open: async () => {
        const opened = await write<{ room_id: string; opened: boolean }>('POST', '/discord/direct-messages/open', { user_id: account })
        if (!opened.ok) throw new Error(opened.error)
        return opened.value.room_id
      },
    }
  }, [pickedHistory, pickedRow, write])

  /** After a send: the list read again, and a chat opened by the send shown
   *  as its room, which now carries the stored chat. */
  const afterSend = async (sentRoomID: string) => {
    await reload()
    if (pickedHistory) pick(sentRoomID, false)
  }

  /** Opens a stored chat on the open person's page; the room stays the
   *  page's address. */
  const pickHistory = (conversationID: string) => {
    const keep = new URLSearchParams(searchParams)
    keep.delete('merged')
    keep.set('history', conversationID)
    navigate(conversationAddress(routes.messageConversations, roomID, roomNames, keep), { replace: true })
  }

  /** Opens a row: a person's page at their newest chat, or a room. */
  const openRow = (row: ConversationListRow) => pick(row.chats[0].room_id, mergedWanted, personParamOf(row))

  /** Opens the row of a person by their row key. */
  const openPerson = (key: string) => {
    const row = allRows.find(candidate => candidate.key === key)
    if (row) openRow(row)
  }

  /** Back to the list, which a narrow screen shows in place of the open room. */
  const closeRoom = () => {
    const next = new URLSearchParams(searchParams)
    next.delete('room')
    const rest = next.toString()
    navigate(rest ? `${routes.messageConversations}?${rest}` : routes.messageConversations, { replace: true })
  }

  /** After a rename: the names read back, then the room opened at its new one. */
  const renamed = async (renamedRoomID: string) => {
    let names: ShortName[]
    try {
      names = await listRoomNames()
    } catch (err) {
      setShortNamesError(errorText(err))
      return
    }
    setShortNames(names)
    setShortNamesError(null)
    navigate(conversationAddress(routes.messageConversations, renamedRoomID, roomShortNames(names, links), searchParams), { replace: true })
  }

  /** What the page says about the address it was given, when it opens no room. */
  const addressNotice = (() => {
    if (address.kind === 'too-long') return `No chat is called ${MESSAGES_SHORT_NAME_NAMESPACE}/${address.path}: a short name has at most three parts.`
    if (address.kind !== 'path' || roomID) return null
    if (!roomShortNameRoutes.configured) return `This host does not carry multichat, so ${MESSAGES_SHORT_NAME_NAMESPACE}/${address.path} cannot be looked up.`
    if (currentLookup?.state === 'missing') return `No chat is called ${MESSAGES_SHORT_NAME_NAMESPACE}/${address.path}.`
    if (currentLookup?.state === 'error') return `Could not look up ${MESSAGES_SHORT_NAME_NAMESPACE}/${address.path}: ${currentLookup.error}`
    return null
  })()
  const lookingUp = address.kind === 'path' && !roomID && (!currentLookup || currentLookup.state === 'loading') && roomShortNameRoutes.configured

  if (!configured) return <MultichatNotConfigured page="Conversations" />

  return (
    <div className={`${styles.page} ${roomID ? styles.pageRoomOpen : ''}`}>
      <header className={styles.header}>
        <h2 className={styles.title}>Conversations</h2>
        {accountPicker}
        <p className={styles.subtitle}>
          Everyone you talk with, newest first: a person&apos;s chats on every app are one row, and group chats have rows of their own. A message you send here reaches the person on that app.
        </p>
      </header>
      {loadError && <pre className={styles.error}>{loadError}</pre>}
      {tagsError && <pre className={styles.error}>Tags are not shown: {tagsError}</pre>}
      {linksError && <pre className={styles.error}>Linked conversations are listed apart: {linksError}</pre>}
      {visibilitySettingsError && <pre className={styles.error}>The auto-hide rule could not be read: {visibilitySettingsError}</pre>}
      {shortNamesError && <pre className={styles.error}>Short names are not shown: {shortNamesError}</pre>}
      {contactsError && <pre className={styles.error}>Chats are listed by room, not by person: {contactsError}</pre>}
      {addressNotice && <pre className={styles.error} data-short-name-notice="">{addressNotice}</pre>}
      {!conversations ? (loadError ? null : <div className={styles.empty}>Loading…</div>) : (
        <div className={`${styles.columns} ${roomID ? styles.columnsRoomOpen : ''}`}>
          <div className={styles.listColumn}>
            <div className={styles.filters}>
              <input className={styles.input} type="search" value={text} placeholder="Filter by name, member or last message"
                onChange={e => setText(e.target.value)} />
              <select className={styles.input} value={platform} onChange={e => setPlatform(e.target.value)}>
                <option value="">Every app ({conversations.length})</option>
                {platforms.map(p => <option key={p} value={p}>{p}</option>)}
              </select>
              <select className={styles.input} value={tagID ?? ''} title="A room's tags are its own, plus the other person's in a DM"
                onChange={e => setTagID(e.target.value ? Number(e.target.value) : null)}>
                <option value="">Any tag or none</option>
                {tags.map(tag => <option key={tag.id} value={tag.id}>{tag.name}</option>)}
              </select>
            </div>
            <div className={styles.filters}>
              <label className={styles.check}>
                <input type="checkbox" checked={showHidden} onChange={e => setShowHidden(e.target.checked)} /> Show hidden ({hiddenCount})
              </label>
              {visibilitySettings && <AutoHideRuleEditor settings={visibilitySettings} write={writeVisibility} />}
            </div>
            {rows.length === 0 && (
              <div className={styles.empty}>{conversations.length === 0 ? 'No conversations.' : 'No conversation matches.'}</div>
            )}
            <ul className={styles.list}>
              {rows.map(row => {
                const newest = row.chats[0]
                const selected = row.key === pickedRow?.key
                const rowTags = [...new Map(row.chats.flatMap(conversation => conversationTags(conversation, tagMaps)).map(tag => [tag.id, tag])).values()]
                const rowName = roomNames.get(newest.room_id)
                return (
                  <li key={row.key}>
                    <button type="button" data-room-id={newest.room_id} data-row-key={row.key} data-chat-count={row.chats.length}
                      data-link-id={row.kind === 'room' ? row.entry.linkID ?? undefined : undefined}
                      className={`${styles.row} ${styles.rowPick} ${selected ? styles.rowSelected : ''} ${row.chats.every(conversation => conversation.hidden) ? styles.rowHidden : ''}`}
                      onClick={() => { if (selected) pick(roomID); else openRow(row) }}>
                      <span className={styles.rowHead}>
                        <span className={styles.name} title={row.kind === 'person' ? row.person.display_name : newest.name}>
                          {row.kind === 'person' && <span className={styles.personMark} aria-label="Person">👤</span>}
                          {conversationListRowName(row)}
                        </span>
                        {newest.last_activity > 0 && <span className={styles.time}>{messageTimeLabel(newest.last_activity)}</span>}
                      </span>
                      <span className={styles.rowMeta}>
                        {rowName && <span className={styles.rowShortName}>/{rowName.path}</span>}
                        {row.chats.map(conversation => conversation.platform && (
                          <span key={conversation.room_id} className={styles.platform}>{conversation.platform}</span>
                        ))}
                        {row.kind === 'room' && newest.member_count > 2 && <span className={styles.muted}>{newest.member_count} members</span>}
                        {newest.hidden && <span className={styles.muted}>hidden</span>}
                        <TagChips tags={rowTags} />
                      </span>
                      {newest.last_message && (
                        <span className={styles.preview}>
                          {row.chats.length > 1 && newest.platform ? `${newest.platform}: ` : ''}{conversationPreviewText(newest.last_message)}
                        </span>
                      )}
                    </button>
                  </li>
                )
              })}
            </ul>
          </div>
          <div className={styles.viewer}>
            <button type="button" className={styles.backToList} onClick={closeRoom}>← Conversations</button>
            {picked && pickedRow && (
              <ConversationRowHeader row={pickedRow} roomID={roomID} merged={merged}
                histories={histories} historyID={pickedHistory?.id ?? null} onPickHistory={pickHistory}
                onPick={(nextRoomID, mergedNext) => pick(nextRoomID, mergedNext, personParamOf(pickedRow))}
                allRows={allRows} conversations={conversations} contacts={contactList} links={links}
                onOpenPerson={openPerson} writeLinks={writeLinks} />
            )}
            {storedChats.error && <pre className={styles.error}>Stored chats are not shown: {storedChats.error}</pre>}
            {storedChats.conversations && messageStoreAccountsError && (
              <pre className={styles.error}>Stored chats are not shown, because the bot accounts that would tell a bot&apos;s chats from yours could not be read: {messageStoreAccountsError}</pre>
            )}
            {picked ? (
              <ConversationThread
                key={pickedHistory ? pickedHistory.id
                  : [...threadRooms.map(chat => chat.room_id), ...(merged ? histories.map(history => history.id) : []), ...storedBehind.map(behind => behind.conversation.id)].join(' ')}
                conversations={threadRooms}
                histories={pickedHistory ? [pickedHistory] : merged ? histories : undefined}
                storedBehind={storedBehind}
                openSendRoom={openPickedHistory}
                onSent={sentRoomID => { void afterSend(sentRoomID) }}
                shortName={roomShortNameRoutes.configured
                  ? <ConversationShortName key={picked.room_id} roomID={picked.room_id} shown={roomNames.get(picked.room_id) ?? null} onRenamed={() => renamed(picked.room_id)} />
                  : null}
                tagEditor={merged || pickedHistory ? null : (
                  <details className={styles.roomSettings}>
                    <summary className={styles.muted}>⚙ Chat settings: tags, visibility, linking, who this is</summary>
                    <ConversationVisibilityEditor conversation={picked} settings={visibilitySettings} write={writeVisibility} />
                    <ConversationTagEditor conversation={picked} tags={tags} tagMaps={tagMaps} write={writeTags} />
                    {pickedRow && <ConversationLinkEditor key={picked.room_id} conversation={picked}
                      chats={pickedRow.chats.some(chat => chat.room_id === picked.room_id) ? pickedRow.chats : [picked]} conversations={conversations} write={writeLinks} />}
                    {picked.direct_message_partner_user_id && <ConversationPerson key={picked.direct_message_partner_user_id} partnerUserID={picked.direct_message_partner_user_id} roomID={picked.room_id} onSent={() => { void reload() }} />}
                  </details>
                )} />
            ) : (
              <div className={styles.empty}>
                {roomID ? `multichat lists no conversation ${roomID}.`
                  : lookingUp ? `Looking up ${MESSAGES_SHORT_NAME_NAMESPACE}/${addressedPath}…`
                  : addressNotice ?? 'Pick a conversation to read it here.'}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

/** One person's chats in message-store, read through multichat when their
 *  page opens. Empty when the row has no person id. */
function usePersonStoredChats(personID: string): { conversations: StoreConversation[] | null; error: string | null } {
  const { fetch: hostFetch, multichatBasePath } = useBridgeConfig()
  const [answer, setAnswer] = useState<{ personID: string; conversations: StoreConversation[] | null; error: string | null }>({ personID: '', conversations: null, error: null })
  useEffect(() => {
    if (!personID || !multichatBasePath) return
    let canceled = false
    hostFetch(`${multichatBasePath}${MESSAGE_STORE_THROUGH_MULTICHAT}${personHistoryConversationsPath(personID)}`)
      .then(async res => {
        if (!res.ok) throw new Error(`message-store: ${res.status} ${(await res.text()).trim()}`)
        return await res.json() as StoreConversation[] | null
      })
      .then(conversations => { if (!canceled) setAnswer({ personID, conversations: conversations ?? [], error: null }) })
      .catch(err => { if (!canceled) setAnswer({ personID, conversations: null, error: errorText(err) }) })
    return () => { canceled = true }
  }, [personID, multichatBasePath, hostFetch])
  return answer.personID === personID ? answer : { conversations: null, error: null }
}

/** What the store said about the path in the URL. */
type AddressLookup =
  | { path: string; state: 'loading' }
  | { path: string; state: 'found'; roomID: string }
  | { path: string; state: 'missing' }
  | { path: string; state: 'error'; error: string }

/** The room's short name as an address, `messages/kat`, and a control to give
 *  it a new one. The store keeps the old name working and refuses one another
 *  record holds; its refusal shows as it gave it. */
function ConversationShortName({ roomID, shown, onRenamed }: {
  roomID: string
  shown: RoomShortName | null
  onRenamed: () => Promise<void>
}) {
  const { rename } = useRoomShortNames()
  const [editing, setEditing] = useState(false)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const borrowed = shown !== null && shown.namedRoomID !== roomID
  const save = async () => {
    setBusy(true)
    const written = await rename(roomID, typed)
    setBusy(false)
    if (!written.ok) { setError(written.error); return }
    setError(null)
    setEditing(false)
    await onRenamed()
  }
  const start = () => {
    setTyped(shown && !borrowed ? shown.path : '')
    setError(null)
    setEditing(true)
  }
  return (
    <span className={styles.shortName} data-short-name={shown?.path ?? ''}>
      {editing ? (
        <form className={styles.shortNameForm} onSubmit={event => { event.preventDefault(); void save() }}>
          <span className={styles.muted}>{MESSAGES_SHORT_NAME_NAMESPACE}/</span>
          <input className={styles.shortNameInput} value={typed} autoFocus disabled={busy} placeholder="kat"
            aria-label="New short name" onChange={event => setTyped(event.target.value)}
            onKeyDown={event => { if (event.key === 'Escape') setEditing(false) }} />
          <button type="submit" className={styles.tagSelect} disabled={busy}>{busy ? 'Saving…' : 'Save'}</button>
          <button type="button" className={styles.tagSelect} disabled={busy} onClick={() => setEditing(false)}>Cancel</button>
        </form>
      ) : (
        <>
          {shown ? (
            <code className={styles.shortNamePath}
              title={borrowed ? 'The name of the chat this one is linked with; it opens that chat' : 'Type this after dash.kayushkin.com/ to open this chat'}>
              {MESSAGES_SHORT_NAME_NAMESPACE}/{shown.path}
            </code>
          ) : <span className={styles.muted}>No short name yet</span>}
          <button type="button" className={styles.tagSelect} onClick={start}>{shown && !borrowed ? 'Rename' : 'Name it'}</button>
        </>
      )}
      {error && <span className={styles.shortNameError}>{error}</span>}
    </span>
  )
}

/** What is loaded of one room in a thread: its messages oldest first, the
 *  token for the page before them, and whether multichat holds older ones. */
interface RoomPages {
  messages: MultichatMessage[]
  olderFrom: string | null
  hasOlder: boolean
}

/** A thread: one room's messages, or several rooms' merged in time order —
 *  one person's chats on different apps — with older pages loaded on request
 *  and the newest page of each polled; and the box to answer in. In a merged
 *  thread each message carries its app, and the Send button names the chat it
 *  sends to, which the reader picks beside it. Keyed by its rooms, so a change
 *  of rooms starts it afresh. */
export function ConversationThread({ conversations, histories = NO_HISTORIES, storedBehind = NO_STORED_BEHIND, openSendRoom, onSent, shortName, tagEditor }: {
  /** The live rooms: one, or the rooms to merge, newest first. */
  conversations: readonly MultichatConversation[]
  /** Stored chats no room carries, read from message-store: shown, merged
   *  with the rooms and paged back the same way, but read only. */
  histories?: readonly StoreConversation[]
  /** Stored chats behind the live rooms: each room's past from before it was
   *  bridged, shown under the room's own messages once those run out, as
   *  the room's. */
  storedBehind?: readonly StoredChatBehindRoom[]
  /** For a thread of a stored chat alone: opens the live room it is answered
   *  in, before the first send, and names it with what the box says. */
  openSendRoom?: { label: string; open: () => Promise<string> }
  /** After a send: the room it went to. */
  onSent: (roomID: string) => void
  /** Drawn at the end of the thread's header: the room's short name and rename control. */
  shortName: ReactNode
  /** Drawn under the header. */
  tagEditor: ReactNode
}) {
  const { read, write, writeForm } = useMultichat()
  const { fetch: hostFetch, multichatBasePath } = useBridgeConfig()
  const merged = conversations.length + histories.length > 1
  /** Every source's key: a room's id, or a stored conversation's id. */
  const roomIDs = useMemo(
    () => [
      ...conversations.map(conversation => conversation.room_id),
      ...histories.map(history => history.id),
      ...storedBehind.map(behind => behind.conversation.id),
    ],
    [conversations, histories, storedBehind],
  )
  /** Every stored chat in the thread, by its id: read from message-store. */
  const historyByID = useMemo(
    () => new Map([...histories, ...storedBehind.map(behind => behind.conversation)].map(history => [history.id, history])),
    [histories, storedBehind],
  )
  /** A stored chat behind a room → that room. */
  const roomInFrontOf = useMemo(() => new Map(storedBehind.map(behind => [behind.conversation.id, behind.roomID])), [storedBehind])
  /** One page of a source: a room's from multichat, a stored chat's from
   *  message-store, oldest first either way. */
  const sourcePage = useCallback(async (sourceID: string, from: string | null): Promise<RoomPages> => {
    if (!historyByID.has(sourceID)) {
      const page = await read<MultichatMessagePage>(conversationMessagesPath(sourceID, from))
      return { messages: pageMessages(page), olderFrom: page.end ?? null, hasOlder: page.has_more && !!page.end }
    }
    const res = await hostFetch(`${multichatBasePath}${MESSAGE_STORE_THROUGH_MULTICHAT}${historyMessagesPath(sourceID, from)}`)
    if (!res.ok) throw new Error(`message-store ${sourceID}: ${res.status} ${(await res.text()).trim()}`)
    const page = await res.json() as StoreMessagePage
    return { messages: (page.messages ?? []).map(historyMessageShown).reverse(), olderFrom: page.next || null, hasOlder: !!page.next }
  }, [read, hostFetch, multichatBasePath, historyByID])
  const roomKey = roomIDs.join(' ')
  /** Files waiting in the composer, sent with the next Send. */
  const [attachments, setAttachments] = useState<PendingAttachment[]>([])
  /** Which file of how many is going now, while files send. */
  const [fileProgress, setFileProgress] = useState<{ done: number; total: number } | null>(null)
  /** A drag carrying files is over the thread. */
  const [dragging, setDragging] = useState(false)
  const filePicker = useRef<HTMLInputElement | null>(null)
  const [pages, setPages] = useState<Record<string, RoomPages> | null>(null)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const [sendError, setSendError] = useState<string | null>(null)
  /** The room the reader picked to send to in a merged thread; null follows
   *  `defaultSendRoom`. */
  const [pickedSendRoomID, setPickedSendRoomID] = useState<string | null>(null)
  /** The message the reaction picker is open for, if any. */
  const [reactingTo, setReactingTo] = useState<MergedMessage | null>(null)
  const [reacting, setReacting] = useState(false)
  const [reactionError, setReactionError] = useState<string | null>(null)
  const scroller = useRef<HTMLDivElement | null>(null)
  const composer = useRef<HTMLTextAreaElement | null>(null)
  /** Where the cursor goes after an emoji is put in, applied once the draft redraws. */
  const pendingCursor = useRef<number | null>(null)

  const loadedRooms: LoadedRoom[] = useMemo(() => pages
    ? roomIDs.flatMap(roomID => pages[roomID] ? [{ roomID, messages: pages[roomID].messages, hasOlder: pages[roomID].hasOlder }] : [])
    : [], [pages, roomIDs])
  // A room's stored past shows only once the room's own pages are all loaded,
  // and only from before its oldest message, so nothing shows twice.
  const shownMessages = useMemo(() => mergedMessages(loadedRooms).filter(message => {
    const roomID = roomInFrontOf.get(message.roomID)
    if (!roomID) return true
    const room = pages?.[roomID]
    if (!room || room.hasOlder) return false
    return room.messages.length === 0 || message.timestamp < room.messages[0].timestamp
  }), [loadedRooms, roomInFrontOf, pages])
  const canLoadOlder = roomsToPageBack(loadedRooms).length > 0
  const liveMessages = useMemo(() => shownMessages.filter(message => !historyByID.has(message.roomID)), [shownMessages, historyByID])
  const sendRoomID = pickedSendRoomID ?? defaultSendRoom(conversations, liveMessages)
  /** The room a message goes to; none in a thread of history alone. */
  const sendConversation: MultichatConversation | null = conversations.find(conversation => conversation.room_id === sendRoomID) ?? conversations[0] ?? null
  /** What a message's badge says it came from. */
  const sourceLabel = (sourceID: string): { label: string; title: string } => {
    const front = roomInFrontOf.get(sourceID)
    if (front) return sourceLabel(front)
    const history = historyByID.get(sourceID)
    if (history) return { label: storedChatLabel(conversations, histories, history), title: `${history.title}, from before this chat came here` }
    const room = conversations.find(conversation => conversation.room_id === sourceID)
    return room ? { label: chatLabel(conversations, room), title: `Sent in ${conversationShownName(room)}. Click to answer there.` } : { label: '', title: '' }
  }

  const discordConversation = conversations.find(conversation => conversation.platform === DISCORD_PLATFORM) ?? null
  const discordStatus = useDiscordStatus(discordConversation !== null)
  const discordNames = useDiscordNames(discordConversation, discordConversation ? pages?.[discordConversation.room_id]?.messages ?? null : null, discordStatus)
  // The room sent to decides which emoji the pickers offer: a Discord server's
  // channel, a Discord DM (no server), or none.
  const discordRoom: DiscordRoom | null = useMemo(() => (sendConversation?.platform === DISCORD_PLATFORM
    ? { serverID: roomDiscordServerID(discordStatus, sendConversation.room_id) } : null), [sendConversation, discordStatus])
  const emoji = useEmojiChoices('reaction', discordRoom)
  const { catalog } = useEmojiCatalog()
  /** Where the cursor is in the text box, for the `:` suggestions. */
  const [cursor, setCursor] = useState(0)
  const [suggestionIndex, setSuggestionIndex] = useState(0)
  /** Where the `:query` the person shut with Escape starts; it stays shut. */
  const [dismissedColonAt, setDismissedColonAt] = useState<number | null>(null)
  /** What the next redraw of the list does to the scroll position: go to the
   *  newest message, keep the reader's distance from the bottom (an older page
   *  went in above), or nothing. */
  const pendingScroll = useRef<{ kind: 'bottom' } | { kind: 'keep'; fromBottom: number } | null>(null)
  /** The newest page of one room. The token for older pages is taken only from
   *  the first load and from older pages, never from the poll, so polling does
   *  not reset how far back the reader has gone. */
  const newestPage = useCallback((roomID: string) => read<MultichatMessagePage>(conversationMessagesPath(roomID)), [read])

  const atBottom = () => {
    const el = scroller.current
    return !el || el.scrollHeight - el.scrollTop - el.clientHeight <= AT_BOTTOM_PX
  }

  useEffect(() => {
    let canceled = false
    Promise.all(roomIDs.map(sourceID => sourcePage(sourceID, null))).then(answers => {
      if (canceled) return
      pendingScroll.current = { kind: 'bottom' }
      setPages(Object.fromEntries(answers.map((page, index) => [roomIDs[index], page])))
      setError(null)
    }).catch(err => { if (!canceled) setError(errorText(err)) })
    return () => { canceled = true }
    // roomKey stands for roomIDs, which is a new array on every list poll.
  }, [roomKey, sourcePage])

  const refreshNewest = useCallback(async (scrollToNewest: boolean) => {
    // A stored chat does not change while it is read, so only rooms are polled.
    const liveRoomIDs = roomIDs.filter(roomID => !historyByID.has(roomID))
    const answers = await Promise.all(liveRoomIDs.map(roomID => newestPage(roomID)))
    const stickToBottom = scrollToNewest || atBottom()
    setPages(current => {
      if (!current) return current
      let changed = false
      const next = { ...current }
      answers.forEach((page, index) => {
        const shown = current[liveRoomIDs[index]]
        if (!shown) return
        const mergedNewest = mergeNewestMessages(shown.messages, pageMessages(page))
        if (sameMessages(shown.messages, mergedNewest)) return
        next[liveRoomIDs[index]] = { ...shown, messages: mergedNewest }
        changed = true
      })
      if (!changed) return current
      if (stickToBottom) pendingScroll.current = { kind: 'bottom' }
      return next
    })
  }, [roomKey, newestPage])

  useEffect(() => {
    const timer = window.setInterval(() => {
      refreshNewest(false).then(() => setError(null)).catch(err => setError(errorText(err)))
    }, MESSAGES_POLL_MS)
    return () => window.clearInterval(timer)
  }, [refreshNewest])

  useLayoutEffect(() => {
    const el = scroller.current
    const pending = pendingScroll.current
    if (!el || !pending) return
    pendingScroll.current = null
    el.scrollTop = pending.kind === 'bottom' ? el.scrollHeight : el.scrollHeight - el.clientHeight - pending.fromBottom
  }, [pages])

  /** Pages back the rooms that hold the thread's start, so it moves back
   *  with no room showing a gap. */
  const loadOlder = async () => {
    if (!pages || loadingOlder) return
    const toPage = roomsToPageBack(loadedRooms)
    if (toPage.length === 0) return
    setLoadingOlder(true)
    try {
      const answers = await Promise.all(toPage.map(roomID => sourcePage(roomID, pages[roomID].olderFrom)))
      const el = scroller.current
      if (el) pendingScroll.current = { kind: 'keep', fromBottom: el.scrollHeight - el.clientHeight - el.scrollTop }
      setPages(current => {
        if (!current) return current
        const next = { ...current }
        answers.forEach((page, index) => {
          const shown = current[toPage[index]]
          next[toPage[index]] = { ...page, messages: prependOlderMessages(shown.messages, page.messages) }
        })
        return next
      })
      setError(null)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setLoadingOlder(false)
    }
  }

  useLayoutEffect(() => {
    const el = composer.current
    if (!el || pendingCursor.current === null) return
    el.focus()
    el.setSelectionRange(pendingCursor.current, pendingCursor.current)
    pendingCursor.current = null
  }, [draft])

  /** Puts an emoji where the cursor is (or over the selection). It only
   *  changes the text box; sending is still the person's Enter or Send. */
  const insertEmoji = (choice: EmojiChoice) => {
    const el = composer.current
    const next = insertAtSelection(draft, el?.selectionStart ?? draft.length, el?.selectionEnd ?? draft.length, choice.key)
    pendingCursor.current = next.cursor
    setCursor(next.cursor)
    setDraft(next.text)
  }

  const colonQuery = useMemo(() => colonQueryAt(draft, cursor), [draft, cursor])
  const inReactionCommand = !!colonQuery && colonQueryIsReactionCommand(draft, colonQuery.start)
  /** The `:` suggestions: in a react command (`+:par`) any emoji the room is
   *  offered; in a message the unicode ones, with matching custom ones listed
   *  but not pickable. */
  const suggestions: EmojiSuggestion[] = useMemo(() => {
    if (!colonQuery || colonQuery.start === dismissedColonAt) return []
    if (inReactionCommand) {
      return searchEmojiIndex(emoji.pickableIndex, colonQuery.query, emoji.favoriteKeys, SUGGESTION_LIMIT)
        .map(choice => ({ choice }))
    }
    return [
      ...searchEmojiIndex(emoji.unicodeIndex, colonQuery.query, emoji.favoriteKeys, SUGGESTION_LIMIT).map(choice => ({ choice })),
      ...searchEmojiIndex(emoji.customIndex, colonQuery.query, emoji.favoriteKeys, 3)
        .map(choice => ({ choice, disabledReason: CUSTOM_EMOJI_IN_MESSAGE_REASON })),
    ]
  }, [colonQuery, dismissedColonAt, inReactionCommand, emoji.pickableIndex, emoji.unicodeIndex, emoji.customIndex, emoji.favoriteKeys])
  const activeSuggestion = Math.min(suggestionIndex, Math.max(0, suggestions.length - 1))

  /** The newest message of the chat being sent to, which a react command reacts to. */
  const newestMessage = (): MergedMessage | null => {
    const inSendRoom = liveMessages.filter(message => message.roomID === sendRoomID)
    return inSendRoom.length > 0 ? inSendRoom[inSendRoom.length - 1] : null
  }

  const reactToNewest = async (choice: EmojiChoice) => {
    const newest = newestMessage()
    if (!newest) { setSendError('There is no message to react to.'); return }
    setDraft('')
    setCursor(0)
    setSendError(null)
    await postReaction(newest, choice)
  }

  const pickSuggestion = (suggestion: EmojiSuggestion) => {
    if (!colonQuery || suggestion.disabledReason) return
    if (inReactionCommand) { void reactToNewest(suggestion.choice); return }
    const next = insertAtSelection(draft, colonQuery.start, cursor, suggestion.choice.key)
    pendingCursor.current = next.cursor
    setCursor(next.cursor)
    setDraft(next.text)
  }

  const addFiles = (files: readonly File[], pasted: boolean) => {
    if (files.length === 0) return
    const now = new Date()
    setAttachments(current => [...current, ...files.map(file => pendingAttachment(file, pasted, now))])
    setSendError(null)
    composer.current?.focus()
  }

  /** Sends the waiting files one by one, the typed text as the first one's
   *  caption. A file that fails stops the rest; it and those after it stay in
   *  the composer, and the text stays unless it already went. */
  const sendFiles = async () => {
    const captions = captionsForFiles(draft, attachments.length)
    setSending(true)
    for (let index = 0; index < attachments.length; index++) {
      const attachment = attachments[index]
      setFileProgress({ done: index + 1, total: attachments.length })
      const form = new FormData()
      form.append('file', attachment.file, attachment.name)
      if (captions[index]) form.append('caption', captions[index])
      const sent = await writeForm<MultichatSendFileAnswer>(conversationSendFilePath(sendRoomID), form)
      if (!sent.ok) {
        setSendError(`${attachment.name} was not sent: ${sent.error}`)
        setAttachments(current => current.filter(pending => attachments.slice(index).some(a => a.id === pending.id)))
        break
      }
      if (index === 0) setDraft('')
      if (index === attachments.length - 1) {
        setSendError(null)
        setAttachments([])
      }
    }
    setFileProgress(null)
    setSending(false)
    setPickedSendRoomID(sendRoomID)
    onSent(sendRoomID)
    try { await refreshNewest(true) } catch (err) { setError(errorText(err)) }
  }

  const send = async () => {
    if (!sendConversation && openSendRoom) { await openAndSend(openSendRoom.open); return }
    if (attachments.length > 0) { await sendFiles(); return }
    const command = reactionCommandOf(draft)
    if (command) {
      const choice: EmojiChoice | null = 'name' in command
        ? choiceNamed([...emoji.custom, ...emoji.unicode], command.name)
        : { kind: 'unicode', key: command.emoji, name: command.emoji }
      if (!choice) { setSendError(`No emoji here is named :${'name' in command ? command.name : ''}:.`); return }
      await reactToNewest(choice)
      return
    }
    const result = sendMessageBodyOf(draft)
    if (!result.ok) { setSendError(result.error); return }
    setSending(true)
    const sent = await write<MultichatSendAnswer>('POST', conversationSendPath(sendRoomID), result.body)
    setSending(false)
    if (!sent.ok) { setSendError(sent.error); return }
    setSendError(null)
    setDraft('')
    // Our own message is now the newest in that room; keep sending there
    // rather than let the default move.
    setPickedSendRoomID(sendRoomID)
    onSent(sendRoomID)
    try { await refreshNewest(true) } catch (err) { setError(errorText(err)) }
  }

  /** The first message of a chat with no room yet: opens the room, then sends
   *  the text there. Files and reactions wait until the room is open. */
  const openAndSend = async (open: () => Promise<string>) => {
    if (attachments.length > 0 || reactionCommandOf(draft)) {
      setSendError('Send a text first: that opens this chat here, and files and reactions work after.')
      return
    }
    const result = sendMessageBodyOf(draft)
    if (!result.ok) { setSendError(result.error); return }
    setSending(true)
    let roomID: string
    try {
      roomID = await open()
    } catch (err) {
      setSending(false)
      setSendError(errorText(err))
      return
    }
    const sent = await write<MultichatSendAnswer>('POST', conversationSendPath(roomID), result.body)
    setSending(false)
    if (!sent.ok) { setSendError(`The chat is open, but the message was not sent: ${sent.error}`); return }
    setSendError(null)
    setDraft('')
    onSent(roomID)
  }

  /** Puts a change to one message's reactions on the shown list at once; the
   *  next poll brings the logged copy. */
  const updateReactions = (roomID: string, targetEventID: string, change: (reactions: MessageReactionGroup[]) => MessageReactionGroup[]) => {
    setPages(current => {
      const shown = current?.[roomID]
      if (!current || !shown) return current
      return {
        ...current,
        [roomID]: { ...shown, messages: shown.messages.map(message => message.event_id === targetEventID ? { ...message, reactions: change(message.reactions ?? []) } : message) },
      }
    })
  }

  /** Reacts to a message as our account, in the message's own room; the
   *  bridge passes it on to the people there. A custom emoji goes as its
   *  `discord-emoji:` key, and multichat answers the mxc:// key it reacted
   *  with, which is what the message's reactions carry from then on. */
  const postReaction = async (message: MergedMessage, choice: Pick<EmojiChoice, 'kind' | 'key' | 'name'>) => {
    const target = reactionTargetEventID(message)
    setReacting(true)
    const posted = await write<{ event_id: string; key: string }>('POST', reactionPostPath(message.roomID), { event_id: target, key: choice.key })
    setReacting(false)
    if (!posted.ok) { setReactionError(posted.error); return }
    setReactionError(null)
    const shortcode = choice.kind === 'custom' ? `:${choice.name}:` : ''
    updateReactions(message.roomID, target, reactions => withReactionPosted(reactions, posted.value.key || choice.key, posted.value.event_id, 'You', shortcode))
  }

  /** The favorites the quick-react bar under each message offers here. */
  const quickReactions = emoji.favorites.slice(0, catalog?.settings.quick_reaction_count ?? 0)

  /** A quick reaction adds ours, or takes back the one we posted from here. */
  const onQuickReaction = (message: MergedMessage, choice: EmojiChoice) => {
    const group = (message.reactions ?? []).find(g => reactionGroupIsEmoji(g, choice.key, allCustomEmoji(catalog)))
    if (group?.my_reaction_event_id) { void takeBackReaction(message, group.my_reaction_event_id); return }
    void postReaction(message, choice)
  }

  const takeBackReaction = async (message: MergedMessage, reactionEventID: string) => {
    const target = reactionTargetEventID(message)
    setReacting(true)
    const taken = await write<{ redaction_event_id: string }>('DELETE', reactionTakeBackPath(message.roomID, reactionEventID))
    setReacting(false)
    if (!taken.ok) { setReactionError(taken.error); return }
    setReactionError(null)
    updateReactions(message.roomID, target, reactions => withReactionTakenBack(reactions, reactionEventID, 'You'))
  }

  const onChipClick = (message: MergedMessage, reaction: MessageReactionGroup) => {
    const action = reactionChipAction(reaction)
    if (action.kind === 'post') void postReaction(message, { kind: 'unicode', key: action.key, name: reaction.shortcode || action.key })
    if (action.kind === 'take-back') void takeBackReaction(message, action.reactionEventID)
  }

  const sendLabel = sendConversation ? chatLabel(conversations, sendConversation) : ''
  const single: MultichatConversation | undefined = conversations[0]
  const onlyHistory = conversations.length === 0 ? histories[0] : undefined

  return (
    <div className={`${styles.thread} ${dragging ? styles.threadDragging : ''}`} data-merged={merged ? '' : undefined}
      onDragOver={event => {
        if (!event.dataTransfer.types.includes('Files')) return
        event.preventDefault()
        setDragging(true)
      }}
      onDragLeave={event => { if (event.currentTarget === event.target) setDragging(false) }}
      onDrop={event => {
        if (!event.dataTransfer.types.includes('Files')) return
        event.preventDefault()
        setDragging(false)
        addFiles(Array.from(event.dataTransfer.files), false)
      }}>
      {dragging && <div className={styles.dropHint}>Drop to attach — nothing is sent until you press Send</div>}
      {!merged && onlyHistory && (
        <div className={styles.threadBar} data-history-conversation={onlyHistory.id}>
          <span className={styles.name} title={onlyHistory.id}>{onlyHistory.title}</span>
          <span className={styles.platform}>{onlyHistory.platform}</span>
          <span className={styles.muted}>
            {openSendRoom ? 'Your first message here opens this chat in Messages; until then you see what was kept of it.'
              : 'What was kept of this chat; it is not connected to Messages, so it cannot be answered here.'}
          </span>
        </div>
      )}
      {!merged && single && (
        <div className={styles.threadBar}>
          <span className={styles.name} title={`${single.name}\nMatrix room id: ${single.room_id}`}>{conversationShownName(single)}</span>
          {single.platform && <span className={styles.platform}>{single.platform}</span>}
          <MemberList conversation={single} />
          {shortName}
        </div>
      )}
      {tagEditor}
      {error && <pre className={styles.error}>{error}</pre>}
      <div className={styles.messages} ref={scroller}>
        {canLoadOlder && (
          <button type="button" className={styles.olderButton} disabled={loadingOlder} onClick={() => { void loadOlder() }}>
            {loadingOlder ? 'Loading older messages…' : 'Load older messages'}
          </button>
        )}
        {!pages ? (error ? null : <div className={styles.empty}>Loading…</div>) : (
          shownMessages.length === 0 ? <div className={styles.empty}>No messages.</div> : shownMessages.map(message => (
            <div key={`${message.roomID} ${message.event_id}`} data-event-id={message.event_id} data-room-id={message.roomID}
              className={`${styles.message} ${message.is_me ? styles.messageMine : ''}`}>
              <div className={styles.messageHead}>
                <span className={styles.sender} title={message.sender}>{message.is_me ? 'You' : message.sender_name || message.sender}</span>
                {merged && (historyByID.has(message.roomID) ? (
                  <span className={`${styles.platform} ${styles.platformHistory}`} title={sourceLabel(message.roomID).title}>
                    {sourceLabel(message.roomID).label}
                  </span>
                ) : (
                  <button type="button" className={`${styles.platform} ${styles.platformPick} ${message.roomID === sendRoomID ? styles.platformPicked : ''}`}
                    title={sourceLabel(message.roomID).title}
                    onClick={() => setPickedSendRoomID(message.roomID)}>
                    {sourceLabel(message.roomID).label}
                  </button>
                ))}
                <span className={styles.time}>{messageTimeLabel(message.timestamp)}</span>
                {message.edited && <span className={styles.time}>edited</span>}
              </div>
              <div className={styles.messageBody}>
                <MessageContent message={message} messageType={message.msg_type}
                  discordNames={message.roomID === discordConversation?.room_id ? discordNames : NO_DISCORD_NAMES} />
              </div>
              {!historyByID.has(message.roomID) && <>
              <ReactionChips reactions={message.reactions} busy={reacting}
                onChipClick={reaction => onChipClick(message, reaction)} />
              <div className={styles.messageTools}>
                {quickReactions.map(choice => {
                  const group = (message.reactions ?? []).find(g => reactionGroupIsEmoji(g, choice.key, allCustomEmoji(catalog)))
                  const madeInTheApp = !!group?.reacted_by_me && !group.my_reaction_event_id
                  return (
                    <button key={choice.key} type="button" disabled={reacting || madeInTheApp}
                      className={`${styles.quickReaction} ${group?.reacted_by_me ? styles.quickReactionMine : ''}`}
                      title={madeInTheApp ? `You reacted with :${choice.name}: in the app; take it back there`
                        : group?.my_reaction_event_id ? `Take back your :${choice.name}:` : `React with :${choice.name}:`}
                      aria-label={`React with ${choice.name}`} aria-pressed={!!group?.reacted_by_me}
                      onClick={() => onQuickReaction(message, choice)}>
                      <EmojiFace choice={choice} />
                    </button>
                  )
                })}
                <button type="button" className={styles.reactButton} disabled={reacting}
                  aria-expanded={reactingTo?.event_id === message.event_id}
                  title="React to this message" aria-label="React to this message"
                  onClick={() => setReactingTo(current => current?.event_id === message.event_id ? null : message)}>
                  {'\u{1F642}'}
                </button>
              </div>
              </>}
            </div>
          ))
        )}
      </div>
      {attachments.length > 0 && (
        <ul className={styles.attachmentTray} aria-label="Files to send">
          {attachments.map(attachment => (
            <li key={attachment.id} className={styles.attachment}>
              <AttachmentPreview attachment={attachment} />
              <span className={styles.attachmentName} title={attachment.name}>{attachment.name}</span>
              <span className={styles.muted}>{humanFileSize(attachment.file.size)}</span>
              <button type="button" className={styles.emojiPanelClose} disabled={sending} aria-label={`Remove ${attachment.name}`}
                onClick={() => setAttachments(current => current.filter(pending => pending.id !== attachment.id))}>✕</button>
            </li>
          ))}
        </ul>
      )}
      {(sendConversation || openSendRoom) && <>
      {merged && sendConversation && (
        <div className={styles.sendTarget} data-send-room-id={sendRoomID}>
          <span className={styles.muted}>Send to</span>
          {conversations.map(conversation => (
            <button key={conversation.room_id} type="button" disabled={sending} aria-pressed={conversation.room_id === sendRoomID}
              className={`${styles.platform} ${styles.platformPick} ${conversation.room_id === sendRoomID ? styles.platformPicked : ''}`}
              title={conversationShownName(conversation)} onClick={() => setPickedSendRoomID(conversation.room_id)}>
              {chatLabel(conversations, conversation)}
            </button>
          ))}
        </div>
      )}
      <div className={styles.composer}>
        {reactingTo && (
          <EmojiPickerPanel
            heading={`React to ${reactingTo.is_me ? 'your' : `${reactingTo.sender_name || reactingTo.sender}'s`} message: “${reactingTo.body.slice(0, 60)}${reactingTo.body.length > 60 ? '…' : ''}”`}
            purpose="reaction" discordRoom={discordRoom}
            onPick={choice => { const message = reactingTo; setReactingTo(null); void postReaction(message, choice) }}
            onClose={() => setReactingTo(null)} />
        )}
        {suggestions.length > 0 && (
          <EmojiSuggestionList suggestions={suggestions} activeIndex={activeSuggestion}
            onPick={pickSuggestion} onHover={setSuggestionIndex} />
        )}
        <input ref={filePicker} type="file" multiple hidden
          onChange={event => { addFiles(Array.from(event.target.files ?? []), false); event.target.value = '' }} />
        <button type="button" className={styles.emojiButton} disabled={sending} title="Attach images or files (or paste or drop them here)"
          aria-label="Attach files" onClick={() => filePicker.current?.click()}>📎</button>
        <EmojiPickerButton onPick={insertEmoji} disabled={sending} discordRoom={discordRoom} />
        <textarea ref={composer} className={styles.input} rows={2} value={draft} disabled={sending}
          placeholder={sendConversation
            ? `Message ${sendConversation.name}${sendConversation.platform ? ` on ${sendConversation.platform}` : ''} — Enter sends, : finds an emoji, +:name: reacts to the newest message`
            : `Message ${openSendRoom?.label ?? ''} — Enter sends`}
          onChange={e => { setDraft(e.target.value); setCursor(e.target.selectionStart); setSuggestionIndex(0) }}
          onSelect={e => setCursor(e.currentTarget.selectionStart)}
          onPaste={e => {
            const { files, keepText } = filesFromPaste({ files: e.clipboardData.files, types: Array.from(e.clipboardData.types) })
            if (files.length === 0) return
            if (!keepText) e.preventDefault()
            addFiles(files, true)
          }}
          onKeyDown={e => {
            if (suggestions.length > 0 && !e.nativeEvent.isComposing) {
              if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                e.preventDefault()
                const step = e.key === 'ArrowDown' ? 1 : -1
                setSuggestionIndex((activeSuggestion + step + suggestions.length) % suggestions.length)
                return
              }
              if ((e.key === 'Enter' && !e.shiftKey) || e.key === 'Tab') {
                const suggestion = suggestions[activeSuggestion]
                if (!suggestion.disabledReason) { e.preventDefault(); pickSuggestion(suggestion); return }
                if (e.key === 'Tab') return
              }
              if (e.key === 'Escape') { e.preventDefault(); setDismissedColonAt(colonQuery?.start ?? null); return }
            }
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); void send() }
          }} />
        <button type="button" className="bi-save-btn" disabled={sending || (!draft.trim() && attachments.length === 0)} onClick={() => { void send() }}
          title={merged && sendConversation ? `Sends in ${conversationShownName(sendConversation)} on ${sendConversation.platform || 'unknown app'}` : undefined}>
          {fileProgress ? `Sending ${fileProgress.done} of ${fileProgress.total}…` : sending ? 'Sending…' : merged ? `Send on ${sendLabel}` : 'Send'}
        </button>
      </div>
      {sendError && <pre className={styles.error}>{sendError}</pre>}
      {reactionError && <pre className={styles.error}>{reactionError}</pre>}
      </>}
    </div>
  )
}

/** The room's member count, which opens to list every member but us: their
 *  display name, and their Matrix user id on hover. */
function MemberList({ conversation }: { conversation: MultichatConversation }) {
  const members = conversation.members ?? []
  const count = <>{conversation.member_count} members</>
  if (members.length === 0) return <span className={styles.muted}>{count}</span>
  return (
    <details className={styles.memberList}>
      <summary className={styles.muted}>{count}</summary>
      <ul>
        {members.map(member => (
          <li key={member.user_id} title={member.user_id}>{member.display_name || <code>{member.user_id}</code>}</li>
        ))}
      </ul>
    </details>
  )
}

/** The Discord bridge's status, read once when a thread with a Discord room
 *  opens: its channel names, and which server the room is a channel of (whose
 *  custom emoji the pickers offer). Null with no Discord room, and until it answers. */
function useDiscordStatus(isDiscord: boolean): DiscordBridgeStatus | null {
  const { read } = useMultichat()
  const [status, setStatus] = useState<DiscordBridgeStatus | null>(null)
  useEffect(() => {
    if (!isDiscord) return
    let canceled = false
    read<DiscordBridgeStatus>('/discord/status')
      .then(answer => { if (!canceled) setStatus(answer) })
      .catch(err => console.error('Discord channel names and custom emoji are not available:', err))
    return () => { canceled = true }
  }, [isDiscord, read])
  return status
}

/** Names for the Discord tokens a Discord room's messages can still carry
 *  (a channel mention the bridge leaves as `<#id>`): channels from the bridge
 *  status, read once when a Discord room opens, and users from the room's own
 *  senders. Other apps get none. A failed status read only leaves channel ids
 *  unnamed, so it is logged rather than shown over the thread. */
function useDiscordNames(conversation: MultichatConversation | null, messages: readonly MultichatMessage[] | null,
  status: DiscordBridgeStatus | null): DiscordNames {
  const isDiscord = conversation?.platform === DISCORD_PLATFORM
  return useMemo(() => {
    if (!isDiscord) return NO_DISCORD_NAMES
    const senders = (messages ?? []).map(m => ({ sender_user_id: m.sender, sender_display_name: m.sender_name ?? '' }))
    return discordNamesOf(bridgedChannelsOf(status), senders)
  }, [isDiscord, status, messages])
}

/** The open room's tags. Its own come off with ×, and the list adds one. In a
 *  DM the other person's tags show too, dashed; they belong to the contact and
 *  change on the Contacts page. */
function ConversationTagEditor({ conversation, tags, tagMaps, write }: {
  conversation: MultichatConversation
  tags: MultichatTag[]
  tagMaps: ConversationTagMaps
  write: (method: string, path: string, body?: unknown) => Promise<string | null>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const roomID = conversation.room_id
  const own = conversationOwnTags(conversation, tagMaps.conversationTagMap)
  const fromPartner = conversationPartnerTags(conversation, tagMaps.contactTagMap)
  const addable = tags.filter(tag => !own.some(on => on.id === tag.id))
  const run = async (method: string, path: string, body?: unknown) => {
    setBusy(true)
    setError(await write(method, path, body))
    setBusy(false)
  }
  return (
    <div className={styles.threadTags} data-room-tags={roomID}>
      <span className={styles.muted}>Tags</span>
      {own.map(tag => (
        <TagChip key={tag.id} tag={tag} disabled={busy}
          onRemove={() => { void run('DELETE', conversationTagRemovePath(roomID, tag.id)) }} />
      ))}
      {fromPartner.map(tag => (
        <span key={`contact-${tag.id}`} className={styles.tagFromContact} title="On the contact; change it on the Contacts page">
          <TagChip tag={tag} />
        </span>
      ))}
      {tags.length === 0 ? <span className={styles.muted}>No tags exist yet; add one on the Contacts page.</span> : addable.length > 0 && (
        <select className={styles.tagSelect} value="" disabled={busy}
          onChange={e => { if (e.target.value) void run('POST', '/conversations/tags', conversationTagAssignBody(roomID, Number(e.target.value))) }}>
          <option value="">+ Tag this conversation</option>
          {addable.map(tag => <option key={tag.id} value={tag.id}>{tag.name}</option>)}
        </select>
      )}
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** Hide the open room from the list, keep it shown whatever its size, or
 *  leave it to the auto-hide rule. */
function ConversationVisibilityEditor({ conversation, settings, write }: {
  conversation: MultichatConversation
  settings: ConversationVisibilitySettings | null
  write: (path: string, body: unknown) => Promise<string | null>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const choose = async (visibility: ConversationVisibility) => {
    setBusy(true)
    setError(await write(conversationVisibilityPath(conversation.room_id), { visibility }))
    setBusy(false)
  }
  return (
    <div className={styles.threadTags} data-room-visibility={conversation.room_id}>
      <span className={styles.muted}>In the list</span>
      <select className={styles.tagSelect} value={conversation.visibility} disabled={busy}
        onChange={e => { void choose(e.target.value) }}>
        <option value="auto">Auto (hide when large)</option>
        <option value="hidden">Hide</option>
        <option value="shown">Always show</option>
      </select>
      <span className={styles.muted}>{conversationVisibilityReason(conversation, settings)}</span>
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** The auto-hide rule: rooms left on Auto with more members than the
 *  threshold are hidden. Each change saves at once, as on the Emoji page. */
function AutoHideRuleEditor({ settings, write }: {
  settings: ConversationVisibilitySettings
  write: (path: string, body: unknown) => Promise<string | null>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [threshold, setThreshold] = useState(String(settings.auto_hide_member_count_threshold))
  useEffect(() => { setThreshold(String(settings.auto_hide_member_count_threshold)) }, [settings.auto_hide_member_count_threshold])
  const save = async (next: ConversationVisibilitySettings) => {
    setBusy(true)
    setError(await write('/conversations/visibility-settings', next))
    setBusy(false)
  }
  const saveThreshold = () => {
    const count = Number(threshold)
    if (threshold === String(settings.auto_hide_member_count_threshold)) return
    if (!Number.isInteger(count)) { setError(`${threshold} is not a whole number of members.`); return }
    void save({ ...settings, auto_hide_member_count_threshold: count })
  }
  return (
    <span className={styles.check} data-auto-hide-rule>
      <label>
        <input type="checkbox" checked={settings.auto_hide_enabled} disabled={busy}
          onChange={e => { void save({ ...settings, auto_hide_enabled: e.target.checked }) }} /> Auto-hide rooms with more than
      </label>{' '}
      <input type="number" min={1} value={threshold} disabled={busy || !settings.auto_hide_enabled}
        className={`${styles.input} ${styles.thresholdInput}`} aria-label="Auto-hide member threshold"
        onChange={e => setThreshold(e.target.value)} onBlur={saveThreshold}
        onKeyDown={e => { if (e.key === 'Enter') saveThreshold() }} /> members
      {error && <pre className={styles.error}>{error}</pre>}
    </span>
  )
}

/** How many of a person's other chats show before "more". */
const OTHER_CHATS_SHOWN = 8

/** Above the thread: who or what the open row is, a tab per chat with Merged
 *  first when there are several, and the other chats of the person — their
 *  group chats, each one click to open and one to make it a tab. A room that
 *  belongs to no one shows the people in it instead, to open their chats or
 *  make this room one of them. */
function ConversationRowHeader({ row, roomID, merged, histories, historyID, onPickHistory, onPick, allRows, conversations, contacts, links, onOpenPerson, writeLinks }: {
  row: ConversationListRow
  roomID: string
  merged: boolean
  /** The person's stored chats no room carries, each a read-only tab. */
  histories: readonly StoreConversation[]
  /** The stored chat open, if one is. */
  historyID: string | null
  onPickHistory: (conversationID: string) => void
  onPick: (roomID: string, merged?: boolean) => void
  allRows: readonly ConversationListRow[]
  conversations: readonly MultichatConversation[]
  contacts: readonly MultichatUnifiedContact[]
  links: readonly ConversationLink[]
  onOpenPerson: (key: string) => void
  writeLinks: (path: string, body: unknown) => Promise<string | null>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [showAll, setShowAll] = useState(false)
  const addToChats = async (chats: readonly MultichatConversation[], otherRoomID: string) => {
    const body = addToPersonChatsBody(otherRoomID, chats, links)
    if (!body) return
    setBusy(true)
    setError(await writeLinks('/conversations/links', body))
    setBusy(false)
  }
  const others = row.kind === 'person' ? personOtherChats(row.person, row.chats, conversations) : []
  const shownOthers = showAll ? others
    : [...others.slice(0, OTHER_CHATS_SHOWN), ...others.slice(OTHER_CHATS_SHOWN).filter(chat => chat.room_id === roomID)]
  const open = conversations.find(conversation => conversation.room_id === roomID)
  const peopleHere = row.kind === 'room' && open
    ? peopleInRoom(open, contacts).flatMap(person => {
      const personRow = allRows.find(candidate => candidate.key === `person:${personKey(person)}`)
      return personRow ? [{ person, personRow }] : []
    })
    : []
  const ownChat = row.chats.some(chat => chat.room_id === roomID)
  const openOther = others.find(chat => chat.room_id === roomID)
  return (
    <div className={styles.rowHeader} data-row-key={row.key}>
      {(row.kind === 'person' || row.chats.length > 1) && (
        <div className={styles.rowHeaderName}>
          <span className={styles.name}>{conversationListRowName(row)}</span>
          {row.kind === 'person' && row.person.person_id && <span className={styles.muted}>{row.person.person_id}</span>}
          {row.kind === 'person' && (
            <span className={styles.muted}>
              {row.chats.length} chat{row.chats.length === 1 ? '' : 's'} with {row.person.display_name}
              {others.length > 0 ? `, in ${others.length} other${others.length === 1 ? '' : 's'}` : ''}
            </span>
          )}
        </div>
      )}
      {(row.chats.length + histories.length > 1 || others.length > 0) && (
        <nav className={styles.tabs} aria-label="Chats">
          {row.chats.length + histories.length > 1 && (
            <button type="button" className={`${styles.tab} ${merged ? styles.tabActive : ''}`} aria-pressed={merged}
              title="Every chat with this person in one timeline; you pick which chat a message goes to"
              onClick={() => onPick(ownChat ? roomID : row.chats[0].room_id, true)}>
              Merged
            </button>
          )}
          {row.chats.map(conversation => (
            <button key={conversation.room_id} type="button" title={conversationShownName(conversation)}
              aria-pressed={!merged && !historyID && conversation.room_id === roomID}
              className={`${styles.tab} ${!merged && !historyID && conversation.room_id === roomID ? styles.tabActive : ''}`}
              onClick={() => onPick(conversation.room_id, false)}>
              {chatLabel(row.chats, conversation)}
            </button>
          ))}
          {histories.map(history => (
            <button key={history.id} type="button" data-history-conversation={history.id} aria-pressed={history.id === historyID}
              title={`${history.title}: ${history.message_count} messages kept in message-store, read only`}
              className={`${styles.tab} ${styles.tabHistory} ${history.id === historyID ? styles.tabActive : ''}`}
              onClick={() => onPickHistory(history.id)}>
              {storedChatLabel(row.chats, histories, history)}
            </button>
          ))}
          {others.length > 0 && <span className={styles.tabDivider} aria-hidden="true" />}
          {shownOthers.map(conversation => (
            <button key={conversation.room_id} type="button" aria-pressed={conversation.room_id === roomID}
              title={`${conversation.name}${conversation.platform ? ` on ${conversation.platform}` : ''}${conversation.member_count > 2 ? `, ${conversation.member_count} members` : ''}`}
              className={`${styles.tab} ${styles.tabOther} ${!historyID && conversation.room_id === roomID ? styles.tabActive : ''}`}
              onClick={() => onPick(conversation.room_id, false)}>
              {conversationShownName(conversation)}
              {conversation.platform && <span className={styles.tabApp}>{conversation.platform}</span>}
            </button>
          ))}
          {others.length > OTHER_CHATS_SHOWN && (
            <button type="button" className={`${styles.tab} ${styles.tabOther}`} onClick={() => setShowAll(current => !current)}>
              {showAll ? 'Fewer' : `${others.length - OTHER_CHATS_SHOWN} more…`}
            </button>
          )}
        </nav>
      )}
      {row.kind === 'person' && openOther && !historyID && (
        <div className={styles.otherChats}>
          <span className={styles.muted}>
            {openOther.member_count > 2 ? `A group chat ${row.person.display_name} is in.` : `Not counted as a chat with ${row.person.display_name} yet.`}
          </span>
          <button type="button" className={styles.tagSelect} disabled={busy}
            title={`Make this one of ${row.person.display_name}'s chats: a tab beside the others, and part of Merged`}
            onClick={() => { void addToChats(row.chats, openOther.room_id) }}>
            + Make it a chat with {row.person.display_name}
          </button>
        </div>
      )}
      {peopleHere.length > 0 && (
        <div className={styles.otherChats}>
          <span className={styles.muted}>People here</span>
          {peopleHere.map(({ person, personRow }) => (
            <span key={personRow.key} className={styles.otherChat}>
              <button type="button" className={styles.otherChatOpen} onClick={() => onOpenPerson(personRow.key)}>
                👤 {person.display_name}
              </button>
              <button type="button" className={styles.otherChatAdd} disabled={busy}
                title={`Make this chat one of ${person.display_name}'s: a tab beside the others, and part of Merged`}
                onClick={() => { void addToChats(personRow.chats, roomID) }}>+ make it {person.display_name}&apos;s</button>
            </span>
          ))}
        </div>
      )}
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** Link this room with another chat that is the same conversation, or take
 *  it out of its link. */
function ConversationLinkEditor({ conversation, chats, conversations, write }: {
  conversation: MultichatConversation
  /** The chats of the row the room is in. */
  chats: readonly MultichatConversation[]
  conversations: readonly MultichatConversation[]
  write: (path: string, body: unknown) => Promise<string | null>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const roomID = conversation.room_id
  const candidates = useMemo(() => conversationLinkCandidates(conversations, { linkID: null, conversations: [...chats] }), [conversations, chats])
  const inLink = chats.length > 1
  const run = async (path: string, body: unknown) => {
    setBusy(true)
    setError(await write(path, body))
    setBusy(false)
  }
  return (
    <div className={styles.threadTags} data-room-link={roomID}>
      <span className={styles.muted}>Same conversation</span>
      {candidates.length > 0 && (
        <select className={styles.tagSelect} value="" disabled={busy}
          onChange={e => { if (e.target.value) void run('/conversations/links', conversationLinkBody(roomID, e.target.value)) }}>
          <option value="">+ Link with another chat</option>
          {candidates.map(candidate => (
            <option key={candidate.room_id} value={candidate.room_id}>
              {conversationShownName(candidate)}{candidate.platform ? ` (${candidate.platform})` : ''}
            </option>
          ))}
        </select>
      )}
      {inLink && !conversation.direct_message_partner_user_id && (
        <button type="button" className={styles.tagSelect} disabled={busy}
          title="Takes this chat out of its link; a one-to-one chat stays with its person"
          onClick={() => { void run('/conversations/links/remove', conversationUnlinkBody(roomID)) }}>
          Unlink this chat
        </button>
      )}
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** A waiting file's thumbnail: the image itself, or what kind of file it is. */
function AttachmentPreview({ attachment }: { attachment: PendingAttachment }) {
  const [url, setURL] = useState<string | null>(null)
  useEffect(() => {
    if (attachment.kind !== 'image') return
    const objectURL = URL.createObjectURL(attachment.file)
    setURL(objectURL)
    return () => URL.revokeObjectURL(objectURL)
  }, [attachment])
  if (attachment.kind === 'image' && url) return <img className={styles.attachmentThumbnail} src={url} alt="" />
  const icon = attachment.kind === 'video' ? '🎬' : attachment.kind === 'audio' ? '🎵' : attachment.kind === 'image' ? '🖼️' : '📄'
  return <span className={styles.attachmentIcon} aria-hidden="true">{icon}</span>
}
