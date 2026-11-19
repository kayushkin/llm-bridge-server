import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import type { Account, Button, Conversation, Embed, Message, MessagePage } from '@kayushkin/message-store-types'
import { useBridgeConfig } from '../../context'
import { type DiscordNames } from '../../messageBody'
import { messageTimeLabel } from '../../multichatMessages'
import {
  ACCOUNTS_PATH, DISCORD_MESSAGE_LIMIT, MESSAGE_LIMIT_WARNING_AT, accountConversationKind, accountConversationMessagesPath, accountConversationName,
  MESSAGE_STORE_THROUGH_MULTICHAT, accountConversationsPath, accountDiscordNames, accountDraftProblem, accountSendPath, attachmentNamesOf, buttonChipOf, countLabel, embedHasContent,
  messageSentByAccount, pickableBotAccounts, safeWebAddress, storedMessageFormat, storedMessageIsEmpty,
} from '../../messageAccounts'
import { errorText } from './useMultichat'
import { MessageContent } from './MessageContent'
import styles from './Messages.module.css'

/** One GET of message-store's records through multichat; a refusal throws in its words. */
function useMessageStoreRead() {
  const { fetch: hostFetch, multichatBasePath } = useBridgeConfig()
  return useCallback(async <T,>(path: string): Promise<T> => {
    const res = await hostFetch(`${multichatBasePath}${MESSAGE_STORE_THROUGH_MULTICHAT}${path}`)
    if (!res.ok) throw new Error(`message-store GET ${path} → ${res.status}: ${(await res.text()).trim()}`)
    return await res.json() as T
  }, [hostFetch, multichatBasePath])
}

/** message-store's accounts through multichat, read once; `configured` is false on a host with no multichat. */
export function useMessageStoreAccounts(): { configured: boolean; accounts: Account[] | null; error: string | null } {
  const { multichatBasePath } = useBridgeConfig()
  const readMessageStore = useMessageStoreRead()
  const [answer, setAnswer] = useState<{ accounts: Account[] | null; error: string | null }>({ accounts: null, error: null })
  useEffect(() => {
    if (!multichatBasePath) return
    let canceled = false
    readMessageStore<Account[] | null>(ACCOUNTS_PATH)
      .then(accounts => { if (!canceled) setAnswer({ accounts: accounts ?? [], error: null }) })
      .catch(err => { if (!canceled) setAnswer({ accounts: null, error: errorText(err) }) })
    return () => { canceled = true }
  }, [multichatBasePath, readMessageStore])
  return { configured: !!multichatBasePath, ...answer }
}

/** "Me" and one entry per bot account; the picked one is pressed. */
export function MessageAccountPicker({ accounts, error, pickedAccountID, onPick }: {
  accounts: readonly Account[] | null
  error: string | null
  pickedAccountID: string
  onPick: (accountID: string) => void
}) {
  const bots = pickableBotAccounts(accounts ?? [])
  const entries = [{ id: '', label: 'Me', title: 'Your own chats, from multichat' },
    ...bots.map(bot => ({ id: bot.id, label: bot.display_name || bot.handle, title: `What the ${bot.platform} bot ${bot.display_name} sent and received, from message-store` }))]
  return (
    <div className={styles.accountPicker} role="group" aria-label="Whose messages">
      <span className={styles.muted}>Account</span>
      {entries.map(entry => (
        <button key={entry.id || 'me'} type="button" data-account-id={entry.id} title={entry.title} aria-pressed={entry.id === pickedAccountID}
          className={`${styles.accountPick} ${entry.id === pickedAccountID ? styles.accountPickPicked : ''}`}
          onClick={() => onPick(entry.id)}>
          {entry.label}
        </button>
      ))}
      {!accounts && !error && <span className={styles.muted}>loading accounts…</span>}
      {error && <span className={styles.accountPickerError}>Bot accounts are not listed: {error}</span>}
    </div>
  )
}

/**
 * What one bot account sent and received, read-only from message-store: its
 * conversations (server channels, threads and direct messages, a direct
 * message named by the other person) and the open one's messages in time
 * order, each marked as the bot's or not, with its cards and buttons.
 */
export function BridgeMessageAccountView({ account, accountPicker, conversationID, onOpenConversation }: {
  account: Account
  accountPicker: ReactNode
  /** The open conversation, from the URL; empty for none. */
  conversationID: string
  onOpenConversation: (conversationID: string) => void
}) {
  const readMessageStore = useMessageStoreRead()
  const [conversations, setConversations] = useState<Conversation[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [text, setText] = useState('')

  useEffect(() => {
    let canceled = false
    setConversations(null)
    readMessageStore<Conversation[] | null>(accountConversationsPath(account.id))
      .then(answer => { if (!canceled) { setConversations(answer ?? []); setLoadError(null) } })
      .catch(err => { if (!canceled) setLoadError(errorText(err)) })
    return () => { canceled = true }
  }, [account.id, readMessageStore])

  const needle = text.trim().toLowerCase()
  const shown = (conversations ?? []).filter(conversation => !needle || accountConversationName(conversation, account, conversations ?? []).toLowerCase().includes(needle))
  const picked = conversations?.find(conversation => conversation.id === conversationID) ?? null

  return (
    <div className={`${styles.page} ${conversationID ? styles.pageRoomOpen : ''}`} data-account-view={account.id}>
      <header className={styles.header}>
        <h2 className={styles.title}>Conversations</h2>
        {accountPicker}
        <p className={styles.subtitle}>
          What {account.display_name} sent and received on {account.platform}, as message-store keeps it, newest first. A reply sends as {account.display_name}.
        </p>
      </header>
      {loadError && <pre className={styles.error}>{loadError}</pre>}
      {!conversations ? (loadError ? null : <div className={styles.empty}>Loading…</div>) : (
        <div className={`${styles.columns} ${conversationID ? styles.columnsRoomOpen : ''}`}>
          <div className={styles.listColumn}>
            <div className={styles.filters}>
              <input className={styles.input} type="search" value={text} placeholder="Filter by name"
                onChange={event => setText(event.target.value)} />
            </div>
            {shown.length === 0 && (
              <div className={styles.empty}>{conversations.length === 0 ? `message-store holds no conversation of ${account.display_name}.` : 'No conversation matches.'}</div>
            )}
            <ul className={styles.list}>
              {shown.map(conversation => {
                const name = accountConversationName(conversation, account, conversations)
                const selected = conversation.id === conversationID
                return (
                  <li key={conversation.id}>
                    <button type="button" data-conversation-id={conversation.id} data-is-group={conversation.is_group ? 'true' : 'false'}
                      className={`${styles.row} ${styles.rowPick} ${selected ? styles.rowSelected : ''}`}
                      onClick={() => onOpenConversation(conversation.id)}>
                      <span className={`${styles.rowHead} ${styles.rowHeadOneLine}`}>
                        <span className={styles.name} title={name}>{name}</span>
                        {conversation.last_message_at > 0 && <span className={styles.time}>{messageTimeLabel(conversation.last_message_at)}</span>}
                      </span>
                      <span className={styles.rowMeta}>
                        <span className={styles.platform}>{accountConversationKind(conversation)}</span>
                        <span className={styles.muted}>{countLabel(conversation.message_count, 'message', 'messages')}</span>
                        {conversation.is_group && <span className={styles.muted}>{countLabel((conversation.participants ?? []).length, 'person', 'people')}</span>}
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          </div>
          <div className={styles.viewer}>
            <button type="button" className={styles.backToList} onClick={() => onOpenConversation('')}>← Conversations</button>
            {picked ? <AccountConversationThread key={picked.id} account={account} conversation={picked} conversations={conversations} />
              : <div className={styles.empty}>{conversationID ? `${account.display_name} takes no part in conversation ${conversationID}.` : 'Pick a conversation to read it here.'}</div>}
          </div>
        </div>
      )}
    </div>
  )
}

/** One conversation of a bot account, oldest at the top, paged back by
 *  message-store's cursor. Keyed by conversation, so a switch starts afresh. */
function AccountConversationThread({ account, conversation, conversations }: {
  account: Account
  conversation: Conversation
  /** The account's whole list, which names channels and a thread's parent. */
  conversations: readonly Conversation[]
}) {
  const readMessageStore = useMessageStoreRead()
  const discordNames = useMemo(() => accountDiscordNames(conversation, conversations), [conversation, conversations])
  /** Loaded messages, oldest first. */
  const [messages, setMessages] = useState<Message[] | null>(null)
  const [olderCursor, setOlderCursor] = useState<string>('')
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const scroller = useRef<HTMLDivElement | null>(null)
  /** What the next redraw does to the scroll: go to the newest, or keep the
   *  reader's distance from the bottom after an older page went in above. */
  const pendingScroll = useRef<{ kind: 'bottom' } | { kind: 'keep'; fromBottom: number } | null>(null)

  useEffect(() => {
    let canceled = false
    readMessageStore<MessagePage>(accountConversationMessagesPath(conversation.id))
      .then(page => {
        if (canceled) return
        pendingScroll.current = { kind: 'bottom' }
        setMessages([...(page.messages ?? [])].reverse())
        setOlderCursor(page.next)
        setError(null)
      })
      .catch(err => { if (!canceled) setError(errorText(err)) })
    return () => { canceled = true }
  }, [conversation.id, readMessageStore])

  useLayoutEffect(() => {
    const el = scroller.current
    const pending = pendingScroll.current
    if (!el || !pending) return
    pendingScroll.current = null
    el.scrollTop = pending.kind === 'bottom' ? el.scrollHeight : el.scrollHeight - el.clientHeight - pending.fromBottom
  }, [messages])

  const loadOlder = async () => {
    if (!olderCursor || loadingOlder) return
    setLoadingOlder(true)
    try {
      const page = await readMessageStore<MessagePage>(accountConversationMessagesPath(conversation.id, olderCursor))
      const el = scroller.current
      if (el) pendingScroll.current = { kind: 'keep', fromBottom: el.scrollHeight - el.clientHeight - el.scrollTop }
      setMessages(current => [...[...(page.messages ?? [])].reverse(), ...(current ?? [])])
      setOlderCursor(page.next)
      setError(null)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setLoadingOlder(false)
    }
  }

  const name = accountConversationName(conversation, account, conversations)
  const appendSent = (message: Message) => {
    pendingScroll.current = { kind: 'bottom' }
    setMessages(current => [...(current ?? []), message])
  }
  return (
    <div className={styles.thread} data-conversation-id={conversation.id}>
      <div className={styles.threadBar}>
        <span className={styles.name} title={`${name}\n${conversation.id}`}>{name}</span>
        <span className={styles.platform}>{accountConversationKind(conversation)}</span>
        <span className={styles.muted}>{countLabel(conversation.message_count, 'message', 'messages')}</span>
      </div>
      {error && <pre className={styles.error}>{error}</pre>}
      <div className={styles.messages} ref={scroller}>
        {olderCursor && messages && (
          <button type="button" className={styles.olderButton} disabled={loadingOlder} onClick={() => { void loadOlder() }}>
            {loadingOlder ? 'Loading older messages…' : 'Load older messages'}
          </button>
        )}
        {!messages ? (error ? null : <div className={styles.empty}>Loading…</div>) : (
          messages.length === 0 ? <div className={styles.empty}>No messages.</div> : messages.map(message => (
            <AccountMessage key={message.id} message={message} sentByAccount={messageSentByAccount(message, account)} account={account} discordNames={discordNames} />
          ))
        )}
      </div>
      <AccountComposer account={account} conversation={conversation} onSent={appendSent} />
    </div>
  )
}

/** Sends as the account into the open conversation through multichat.
 *  A refusal shows in the route's words and the draft stays; a send adds the
 *  stored row to the thread. */
function AccountComposer({ account, conversation, onSent }: { account: Account; conversation: Conversation; onSent: (message: Message) => void }) {
  const { fetch: hostFetch, multichatBasePath } = useBridgeConfig()
  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const length = [...draft].length
  const send = async () => {
    const problem = accountDraftProblem(draft)
    if (problem) { setError(problem); return }
    setSending(true)
    setError(null)
    try {
      const res = await hostFetch(`${multichatBasePath}${accountSendPath(account.id, conversation.id)}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body: draft }),
      })
      const text = await res.text()
      if (!res.ok) {
        let reason = text.trim()
        try { reason = (JSON.parse(text) as { error?: string }).error || reason } catch { /* not JSON: show it as it came */ }
        throw new Error(`Not sent (${res.status}): ${reason}`)
      }
      onSent(JSON.parse(text) as Message)
      setDraft('')
    } catch (err) {
      setError(errorText(err))
    } finally {
      setSending(false)
    }
  }
  return (
    <div className={styles.accountComposer} data-account-composer={account.id}>
      <div className={styles.accountComposerHead}>
        <span>Sending as <strong>{account.display_name || account.handle}</strong></span>
        {length >= MESSAGE_LIMIT_WARNING_AT && (
          <span className={length > DISCORD_MESSAGE_LIMIT ? styles.accountPickerError : styles.muted}>{length} / {DISCORD_MESSAGE_LIMIT}</span>
        )}
      </div>
      <div className={styles.accountComposerRow}>
        <textarea className={styles.input} rows={2} value={draft} disabled={sending}
          placeholder={`Message as ${account.display_name || account.handle}`}
          onChange={event => setDraft(event.target.value)}
          onKeyDown={event => {
            if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void send() }
          }} />
        <button type="button" disabled={sending || !draft.trim()} onClick={() => { void send() }}>{sending ? 'Sending…' : 'Send'}</button>
      </div>
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** One stored message: its text through the shared renderer, its files' names, its cards and its buttons. */
function AccountMessage({ message, sentByAccount, account, discordNames }: { message: Message; sentByAccount: boolean; account: Account; discordNames: DiscordNames }) {
  const attachmentNames = attachmentNamesOf(message)
  const embeds = (message.embeds ?? []).filter(embedHasContent)
  const buttons = message.buttons ?? []
  return (
    <div data-message-id={message.id} data-sent-by-account={sentByAccount ? 'true' : 'false'}
      className={`${styles.message} ${sentByAccount ? styles.messageMine : ''}`}>
      <div className={styles.messageHead}>
        <span className={styles.sender} title={message.sender_handle}>{message.sender_name || message.sender_handle}</span>
        <span className={styles.muted}>{sentByAccount ? `sent by ${account.display_name}` : `received by ${account.display_name}`}</span>
        <span className={styles.time}>{messageTimeLabel(message.sent_at)}</span>
        {message.edited && <span className={styles.time}>edited</span>}
        {message.deleted_on_platform && <span className={styles.time}>deleted on {message.source}</span>}
      </div>
      <div className={`${styles.messageBody} ${message.kind === 'system' ? styles.accountSystemLine : ''}`} data-kind={message.kind}>
        {message.body && (
          <MessageContent message={{ body: message.body, format: storedMessageFormat(message) }} messageType={message.kind} discordNames={discordNames} />
        )}
        {attachmentNames.length > 0 && <div className={styles.muted}>📎 {attachmentNames.join(', ')}</div>}
        {embeds.map((embed, index) => <EmbedCard key={index} embed={embed} format={storedMessageFormat(message)} discordNames={discordNames} />)}
        {buttons.length > 0 && (
          <div className={styles.buttonChips}>
            {buttons.map((button, index) => <ButtonChip key={index} button={button} />)}
          </div>
        )}
        {storedMessageIsEmpty(message) && <span className={styles.muted}>(no text kept)</span>}
      </div>
    </div>
  )
}

/** A bot's card: author, title (a link when it has one), description,
 *  fields side by side when inline, picture, footer. */
function EmbedCard({ embed, format, discordNames }: { embed: Embed; format: string; discordNames: DiscordNames }) {
  const titleLink = safeWebAddress(embed.url)
  const image = safeWebAddress(embed.image_url)
  const thumbnail = safeWebAddress(embed.thumbnail_url)
  return (
    <div className={styles.embedCard} data-embed="">
      <div className={styles.embedMain}>
        {embed.author_name && <div className={styles.embedAuthor}>{embed.author_name}</div>}
        {embed.title && (
          <div className={styles.embedTitle}>
            {titleLink ? <a href={titleLink} target="_blank" rel="noopener noreferrer">{embed.title}</a> : embed.title}
          </div>
        )}
        {embed.description && <MessageContent message={{ body: embed.description, format }} messageType="m.text" discordNames={discordNames} />}
        {(embed.fields ?? []).length > 0 && (
          <div className={styles.embedFields}>
            {(embed.fields ?? []).map((field, index) => (
              <div key={index} data-embed-field={field.name} className={`${styles.embedField} ${field.inline ? styles.embedFieldInline : ''}`}>
                <div className={styles.embedFieldName}>{field.name}</div>
                <MessageContent message={{ body: field.value, format }} messageType="m.text" discordNames={discordNames} />
              </div>
            ))}
          </div>
        )}
        {image && <img className={styles.embedImage} src={image} alt={embed.title || 'card picture'} loading="lazy" referrerPolicy="no-referrer" />}
        {embed.footer_text && <div className={styles.embedFooter}>{embed.footer_text}</div>}
      </div>
      {thumbnail && <img className={styles.embedThumbnail} src={thumbnail} alt="" loading="lazy" referrerPolicy="no-referrer" />}
    </div>
  )
}

/** A button under a bot's message. A link button links; one that runs
 *  something on the bot is drawn but cannot be pressed from here. */
function ButtonChip({ button }: { button: Button }) {
  const chip = buttonChipOf(button)
  if (chip.link) {
    return <a className={styles.buttonChip} href={chip.link} target="_blank" rel="noopener noreferrer" title={chip.link}>{chip.text} ↗</a>
  }
  return (
    <span className={`${styles.buttonChip} ${chip.disabled ? styles.buttonChipDisabled : ''}`}
      title={chip.disabled ? 'A button the bot had switched off' : 'A button that acts in the app; it cannot be pressed here'}>
      {chip.text}
    </span>
  )
}
