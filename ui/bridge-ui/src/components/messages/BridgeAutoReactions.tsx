import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useBridgeConfig } from '../../context'
import {
  DISCORD_SIGNUP_STORE_THROUGH_MULTICHAT, accountAutoReactionPlaceLabel, accountReactionKeyOf, autoReactionEmojiLabel, autoReactionEmojiOf, autoReactionPersonLabel, memberSearchPath,
  type AccountAutoReaction, type AutoReaction, type CreateAccountAutoReactionBody, type CreateAutoReactionBody,
  type DiscordSignupGuild, type DiscordSignupMember,
} from '../../autoReactions'
import type { EmojiChoice } from '../../emojiCatalog'
import type { MultichatConversation } from '../../types-multichat'
import { EmojiPickerPanel } from './EmojiPicker'
import { MultichatNotConfigured } from './messagesShared'
import { useMultichat } from './useMultichat'
import styles from './Messages.module.css'

/** Requests to discord-signup-store through multichat. They throw the
 *  store's refusal in its own words, so the page can show it. */
function useDiscordSignup() {
  const { fetch: apiFetch, multichatBasePath } = useBridgeConfig()
  return useCallback(async <T,>(method: string, path: string, body?: unknown): Promise<T> => {
    const res = await apiFetch(`${multichatBasePath}${DISCORD_SIGNUP_STORE_THROUGH_MULTICHAT}${path}`, {
      method,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) throw new Error(`${method} ${path} → ${res.status}: ${(await res.text()).trim()}`)
    return (res.status === 204 ? undefined : await res.json()) as T
  }, [apiFetch, multichatBasePath])
}

/** The same over multichat's proxy, for the account rules. */
function useMultichatCall() {
  const { fetch: apiFetch, multichatBasePath } = useBridgeConfig()
  return useCallback(async <T,>(method: string, path: string, body?: unknown): Promise<T> => {
    const res = await apiFetch(`${multichatBasePath}${path}`, {
      method,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) throw new Error(`${method} ${path} → ${res.status}: ${(await res.text()).trim()}`)
    return (res.status === 204 ? undefined : await res.json()) as T
  }, [apiFetch, multichatBasePath])
}

function timeLabel(unixSeconds: number): string {
  return new Date(unixSeconds * 1000).toLocaleString()
}

/** Runs a write, then reloads, keeping the error to show. */
function useWrites(reload: () => Promise<void>) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const act = async (work: () => Promise<unknown>) => {
    setBusy(true)
    setError(null)
    try {
      await work()
      await reload()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }
  return { error, setError, busy, act }
}

/** The emoji panel's size, as Messages.module.css draws it. */
const EMOJI_PANEL_WIDTH = 360
const EMOJI_PANEL_HEIGHT = 320
const EMOJI_PANEL_MARGIN = 8

/** Where the panel goes for a button at `button`: below it, or above when
 *  there is no room below, moved and narrowed as needed so that all of it
 *  stays inside the window. */
export function emojiPanelBox(button: { left: number; top: number; bottom: number }, windowWidth: number, windowHeight: number) {
  const width = Math.min(EMOJI_PANEL_WIDTH, windowWidth - 2 * EMOJI_PANEL_MARGIN)
  const left = Math.min(Math.max(button.left, EMOJI_PANEL_MARGIN), windowWidth - EMOJI_PANEL_MARGIN - width)
  const below = button.bottom + 4
  const above = button.top - 4 - EMOJI_PANEL_HEIGHT
  const top = below + EMOJI_PANEL_HEIGHT <= windowHeight - EMOJI_PANEL_MARGIN ? below
    : Math.max(EMOJI_PANEL_MARGIN, Math.min(above, windowHeight - EMOJI_PANEL_MARGIN - EMOJI_PANEL_HEIGHT))
  return { left, top, width }
}

/** A button that opens the emoji panel, placed in the window by
 *  `emojiPanelBox` wherever the button sits on the page. */
function EmojiChooser({ label, onPick, disabled }: { label: string; onPick: (choice: EmojiChoice) => void; disabled?: boolean }) {
  const [box, setBox] = useState<{ left: number; top: number; width: number } | null>(null)
  const wrapper = useRef<HTMLDivElement | null>(null)
  const button = useRef<HTMLButtonElement | null>(null)
  const close = () => setBox(null)
  const isOpen = !!box
  useEffect(() => {
    if (!isOpen) return
    const closeOnOutsideClick = (event: MouseEvent) => {
      if (wrapper.current && !wrapper.current.contains(event.target as Node)) setBox(null)
    }
    // Follow the button when the page scrolls or the window changes size,
    // still inside the window. Closing on scroll instead shut the panel the
    // moment it opened, since opening it can itself scroll the page a little.
    const follow = () => {
      if (!button.current) return
      const next = emojiPanelBox(button.current.getBoundingClientRect(), window.innerWidth, window.innerHeight)
      // Scrolling the emoji grid reaches here too, and moves nothing.
      setBox(current => current && current.left === next.left && current.top === next.top && current.width === next.width ? current : next)
    }
    document.addEventListener('mousedown', closeOnOutsideClick)
    window.addEventListener('resize', follow)
    document.addEventListener('scroll', follow, true)
    return () => {
      document.removeEventListener('mousedown', closeOnOutsideClick)
      window.removeEventListener('resize', follow)
      document.removeEventListener('scroll', follow, true)
    }
  }, [isOpen])
  const toggle = () => {
    if (box || !button.current) { setBox(null); return }
    setBox(emojiPanelBox(button.current.getBoundingClientRect(), window.innerWidth, window.innerHeight))
  }
  return (
    <div ref={wrapper}>
      <button type="button" ref={button} disabled={disabled} aria-expanded={!!box} onClick={toggle}>{label}</button>
      {box && (
        <div className={styles.autoReactionEmojiFloat} style={{ left: box.left, top: box.top, width: box.width }}>
          <EmojiPickerPanel purpose="reaction" discordRoom={null} heading="React with"
            onPick={(choice: EmojiChoice) => { onPick(choice); close() }}
            onClose={close} />
        </div>
      )}
    </div>
  )
}

/** One rule's row: emoji, who, where, on/off, counts, and its last refusal. */
function RuleRow({ emoji, person, place, enabled, reactionCount, lastReactedAt, lastError, lastErrorAt, busy, onToggle, onEmoji, onDelete }: {
  emoji: string; person: string; place: string; enabled: boolean
  reactionCount: number; lastReactedAt: number; lastError: string; lastErrorAt: number
  busy: boolean; onToggle: (enabled: boolean) => void; onEmoji: (choice: EmojiChoice) => void; onDelete: () => void
}) {
  return (
    <div className={styles.row}>
      <div className={styles.rowHead}>
        <span style={{ fontSize: '1.4em' }}>{emoji}</span>
        <strong>{person}</strong>
        <span className={styles.muted}>{place}</span>
      </div>
      <div className={styles.rowMeta}>
        <label>
          <input type="checkbox" checked={enabled} disabled={busy} onChange={e => onToggle(e.target.checked)} />
          {' '}{enabled ? 'On' : 'Off'}
        </label>
        <span className={styles.muted}>
          {reactionCount} reaction{reactionCount === 1 ? '' : 's'}
          {lastReactedAt > 0 && `, last ${timeLabel(lastReactedAt)}`}
        </span>
        <EmojiChooser label="Change emoji" disabled={busy} onPick={onEmoji} />
        <button type="button" disabled={busy}
          onClick={() => { if (window.confirm(`Delete the ${emoji} rule for ${person}?`)) onDelete() }}>
          Delete
        </button>
      </div>
      {lastError && <div className={styles.error}>The last reaction failed ({timeLabel(lastErrorAt)}): {lastError}</div>}
    </div>
  )
}

function Section({ title, explanation, children }: { title: string; explanation: string; children: ReactNode }) {
  return (
    <section className={styles.header}>
      <h3 className={styles.title}>{title}</h3>
      <span className={styles.muted}>{explanation}</span>
      {children}
    </section>
  )
}

/** Rules multichat applies: the operator's own account reacts, through the
 *  bridge, so the people in the chat see the operator react. */
function AccountAutoReactions() {
  const call = useMultichatCall()
  const { read } = useMultichat()
  const [rules, setRules] = useState<AccountAutoReaction[] | null>(null)
  const [conversations, setConversations] = useState<MultichatConversation[]>([])
  const reload = useCallback(async () => {
    const list = await call<{ auto_reactions: AccountAutoReaction[] }>('GET', '/auto-reactions')
    setRules(list.auto_reactions)
  }, [call])
  const { error, setError, busy, act } = useWrites(reload)
  useEffect(() => {
    reload().catch(err => setError(err instanceof Error ? err.message : String(err)))
    read<MultichatConversation[] | null>('/conversations')
      .then(list => setConversations(list ?? []))
      .catch(err => setError(err instanceof Error ? err.message : String(err)))
  }, [reload, read, setError])

  const [roomID, setRoomID] = useState('')
  const [senderUserID, setSenderUserID] = useState('')
  const [everyChat, setEveryChat] = useState(false)
  const [reactionKey, setReactionKey] = useState('')
  const conversation = conversations.find(c => c.room_id === roomID)

  const pickKey = (choice: EmojiChoice, apply: (key: string) => void) => {
    const key = accountReactionKeyOf(choice)
    if (key === null) setError('Reacting from your account takes a standard emoji; custom server emoji are not supported here yet.')
    else apply(key)
  }
  const add = () => act(async () => {
    const body: CreateAccountAutoReactionBody = { sender_user_id: senderUserID, room_id: everyChat ? '' : roomID, reaction_key: reactionKey, enabled: true }
    await call('POST', '/auto-reactions', body)
    setSenderUserID('')
    setReactionKey('')
    setEveryChat(false)
  })

  return (
    <Section title="From your account"
      explanation="Your own account reacts, on any app multichat bridges (Discord, Google Messages, WhatsApp…), so people see you react. A rule only reacts to messages sent after it was made.">
      {error && <div className={styles.error}>{error}</div>}
      {rules === null ? <div className={styles.empty}>Loading…</div>
        : rules.length === 0 ? <div className={styles.empty}>No rules yet.</div>
          : rules.map(rule => (
            <RuleRow key={rule.id} emoji={rule.reaction_key} person={rule.sender_display_name || rule.sender_user_id}
              place={accountAutoReactionPlaceLabel(rule)} enabled={rule.enabled}
              reactionCount={rule.reaction_count} lastReactedAt={rule.last_reacted_at} lastError={rule.last_error} lastErrorAt={rule.last_error_at}
              busy={busy}
              onToggle={enabled => { void act(() => call('PATCH', `/auto-reactions/${rule.id}`, { enabled })) }}
              onEmoji={choice => pickKey(choice, key => { void act(() => call('PATCH', `/auto-reactions/${rule.id}`, { reaction_key: key })) })}
              onDelete={() => { void act(() => call('DELETE', `/auto-reactions/${rule.id}`)) }} />
          ))}
      <div className={styles.row}>
        <strong>New rule</strong>
        <div className={styles.filters}>
          <select className={styles.input} value={roomID} disabled={busy}
            onChange={e => { setRoomID(e.target.value); setSenderUserID('') }}>
            <option value="">Chat…</option>
            {conversations.map(c => <option key={c.room_id} value={c.room_id}>{c.name}{c.platform ? ` (${c.platform})` : ''}</option>)}
          </select>
          <select className={styles.input} value={senderUserID} disabled={busy || !conversation}
            onChange={e => setSenderUserID(e.target.value)}>
            <option value="">{conversation ? 'Person…' : 'Pick a chat first'}</option>
            {(conversation?.members ?? []).map(m => <option key={m.user_id} value={m.user_id}>{m.display_name || m.user_id}</option>)}
          </select>
          <label title="Their messages in every chat you share on that app, not only this one">
            <input type="checkbox" checked={everyChat} disabled={busy} onChange={e => setEveryChat(e.target.checked)} /> every chat
          </label>
          <EmojiChooser label={reactionKey ? `Emoji: ${reactionKey}` : 'Pick emoji'} disabled={busy}
            onPick={choice => pickKey(choice, setReactionKey)} />
          <button type="button" disabled={busy || !senderUserID || !reactionKey} onClick={() => { void add() }}>Add</button>
        </div>
      </div>
    </Section>
  )
}

/** Rules discord-signup-store applies: the Event-Manager bot reacts on
 *  Discord, where a channel lets it. */
function BotAutoReactions() {
  const call = useDiscordSignup()
  const [rules, setRules] = useState<AutoReaction[] | null>(null)
  const [guilds, setGuilds] = useState<DiscordSignupGuild[]>([])
  const reload = useCallback(async () => {
    const [list, guildList] = await Promise.all([
      call<{ auto_reactions: AutoReaction[] }>('GET', '/auto-reactions'),
      call<{ guilds: DiscordSignupGuild[] }>('GET', '/guilds'),
    ])
    setRules(list.auto_reactions)
    setGuilds(guildList.guilds)
  }, [call])
  const { error, setError, busy, act } = useWrites(reload)
  useEffect(() => { reload().catch(err => setError(err instanceof Error ? err.message : String(err))) }, [reload, setError])

  const [guildID, setGuildID] = useState('')
  const [query, setQuery] = useState('')
  const [members, setMembers] = useState<DiscordSignupMember[]>([])
  const [member, setMember] = useState<DiscordSignupMember | null>(null)
  const [emoji, setEmoji] = useState('')

  // The member search runs as someone types, a moment after they stop.
  useEffect(() => {
    const trimmed = query.trim()
    if (!guildID || trimmed === '' || member) { setMembers([]); return }
    let canceled = false
    const timer = window.setTimeout(() => {
      call<{ members: DiscordSignupMember[] }>('GET', memberSearchPath(guildID, trimmed))
        .then(found => { if (!canceled) setMembers(found.members) })
        .catch(err => { if (!canceled) setError(err instanceof Error ? err.message : String(err)) })
    }, 250)
    return () => { canceled = true; window.clearTimeout(timer) }
  }, [call, guildID, query, member, setError])

  const add = () => act(async () => {
    if (!member) return
    const body: CreateAutoReactionBody = {
      guild_id: guildID, discord_user_id: member.user_id, member_display_name: member.display_name, emoji, enabled: true,
    }
    await call('POST', '/auto-reactions', body)
    setMember(null)
    setQuery('')
    setEmoji('')
  })

  return (
    <Section title="From the Event-Manager bot"
      explanation="The bot reacts on Discord, in every channel and thread it can see. A channel that does not give it Add Reactions refuses it, and the refusal shows on the rule.">
      {error && <div className={styles.error}>{error}</div>}
      {rules === null ? <div className={styles.empty}>Loading…</div>
        : rules.length === 0 ? <div className={styles.empty}>No rules yet.</div>
          : rules.map(rule => (
            <RuleRow key={rule.id} emoji={autoReactionEmojiLabel(rule.emoji)} person={autoReactionPersonLabel(rule)}
              place={`in ${rule.guild_name || rule.guild_id}`} enabled={rule.enabled}
              reactionCount={rule.reaction_count} lastReactedAt={rule.last_reacted_at} lastError={rule.last_error} lastErrorAt={rule.last_error_at}
              busy={busy}
              onToggle={enabled => { void act(() => call('PATCH', `/auto-reactions/${rule.id}`, { enabled })) }}
              onEmoji={choice => { void act(() => call('PATCH', `/auto-reactions/${rule.id}`, { emoji: autoReactionEmojiOf(choice) })) }}
              onDelete={() => { void act(() => call('DELETE', `/auto-reactions/${rule.id}`)) }} />
          ))}
      <div className={styles.row}>
        <strong>New rule</strong>
        <div className={styles.filters}>
          <select className={styles.input} value={guildID} disabled={busy}
            onChange={e => { setGuildID(e.target.value); setMember(null); setQuery('') }}>
            <option value="">Server…</option>
            {guilds.map(g => <option key={g.id} value={g.id}>{g.name || g.id}</option>)}
          </select>
          {member ? (
            <span>
              <strong>{member.display_name}</strong> <span className={styles.muted}>@{member.username}</span>{' '}
              <button type="button" onClick={() => { setMember(null); setQuery('') }}>Change</button>
            </span>
          ) : (
            <input className={styles.input} placeholder={guildID ? 'Person (start of their name)' : 'Pick a server first'}
              disabled={!guildID || busy} value={query} onChange={e => setQuery(e.target.value)} />
          )}
          <EmojiChooser label={emoji ? `Emoji: ${autoReactionEmojiLabel(emoji)}` : 'Pick emoji'} disabled={busy}
            onPick={choice => setEmoji(autoReactionEmojiOf(choice))} />
          <button type="button" disabled={busy || !guildID || !member || !emoji} onClick={() => { void add() }}>Add</button>
        </div>
        {!member && members.length > 0 && (
          <div className={styles.rowMeta}>
            {members.map(m => (
              <button type="button" key={m.user_id} onClick={() => { setMember(m); setMembers([]) }}>
                {m.display_name} <span className={styles.muted}>@{m.username}</span>
              </button>
            ))}
          </div>
        )}
      </div>
    </Section>
  )
}

/**
 * Auto-reactions: an emoji on every message one person sends. Rules that react
 * from the operator's own account live in multichat; rules for the
 * Event-Manager Discord bot live in discord-signup-store, reached through
 * multichat.
 */
export function BridgeAutoReactions() {
  const { configured } = useMultichat()
  if (!configured) return <MultichatNotConfigured page="Auto-reactions" />
  return (
    <div className={styles.page} style={{ overflowY: 'auto' }}>
      <header className={styles.header}>
        <h2 className={styles.title}>Auto-reactions</h2>
        <span className={styles.muted}>An emoji on every message one person sends.</span>
      </header>
      <AccountAutoReactions />
      <BotAutoReactions />
    </div>
  )
}
