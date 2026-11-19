import { useDeferredValue, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import type { DiscordCustomEmoji } from '@kayushkin/multichat-types'
import { useBridgeConfig } from '../../context'
import {
  allCustomEmoji, customChoice, type DiscordRoom, customEmojiImageURL, customEmojiOffered, customEmojiSectionName, emojiSearchIndex, favoriteChoices, searchEmojiIndex, unicodeChoices,
  unicodeNamesOf, type EmojiChoice,
} from '../../emojiCatalog'
import { emojiGridLayout, rowsInView, sectionAt } from '../../emojiGridLayout'
import { useEmojiCatalog, useEmojiGroups } from './useEmojiCatalog'
import styles from './Messages.module.css'

/** What a pick is for. A reaction can be any emoji the room is offered; a
 *  message can carry only unicode ones, because the Discord bridge cannot send
 *  a custom emoji inside a message. */
export type EmojiPurpose = 'reaction' | 'message'

/** How one emoji is drawn: its text, or a custom emoji's image. */
export function EmojiFace({ choice, className }: { choice: EmojiChoice; className?: string }) {
  if (choice.kind === 'custom') {
    return <img className={`${styles.customEmoji} ${className ?? ''}`} src={customEmojiImageURL(choice.emoji)} alt={`:${choice.name}:`} loading="lazy" decoding="async" />
  }
  return <span className={className}>{choice.key}</span>
}

/**
 * What a room's pickers offer: the unicode emoji in the chosen skin tone, the
 * custom emoji of the room's Discord server (and of every other bridged
 * server when the setting says so), and the favorites among those.
 */
export function useEmojiChoices(purpose: EmojiPurpose, discordRoom: DiscordRoom | null) {
  const { groups, error: groupsError } = useEmojiGroups()
  const { catalog, error: catalogError } = useEmojiCatalog()
  const tone = catalog?.settings.skin_tone ?? ''
  const unicode = useMemo(() => (groups ? unicodeChoices(groups, tone) : []), [groups, tone])
  const unicodeNames = useMemo(() => (groups ? unicodeNamesOf(groups) : new Map<string, string>()), [groups])
  const offered: DiscordCustomEmoji[] = useMemo(() => (catalog
    ? customEmojiOffered(catalog.discord_custom_emoji, catalog.discord_seen_emoji ?? [], discordRoom, !!catalog.discord_nitro?.has_nitro)
    : []), [catalog, discordRoom])
  const custom = useMemo(() => offered.map(customChoice), [offered])
  const favoriteKeys = useMemo(() => catalog?.favorite_keys ?? [], [catalog])
  const favorites = useMemo(() => {
    const offeredKeys = new Set(offered.map(emoji => emoji.reaction_key))
    return favoriteChoices(favoriteKeys, allCustomEmoji(catalog), unicodeNames)
      .filter(choice => choice.kind === 'unicode' || (purpose === 'reaction' && offeredKeys.has(choice.key)))
  }, [favoriteKeys, catalog, unicodeNames, offered, purpose])
  const sectionsOf = useMemo(() => emojiSections(groups, unicode, purpose === 'reaction' ? custom : [], favorites),
    [groups, unicode, custom, favorites, purpose])
  // Search indexes, built once per list rather than on every keystroke.
  const unicodeIndex = useMemo(() => emojiSearchIndex(unicode), [unicode])
  const customIndex = useMemo(() => emojiSearchIndex(custom), [custom])
  const pickableIndex = useMemo(() => (purpose === 'reaction' ? [...customIndex, ...unicodeIndex] : unicodeIndex),
    [purpose, customIndex, unicodeIndex])
  return {
    groups, unicode, custom: purpose === 'reaction' ? custom : [], customNotSendable: purpose === 'message' ? custom : [],
    favorites, favoriteKeys, tone, error: groupsError ?? catalogError,
    sections: sectionsOf, unicodeIndex, customIndex, pickableIndex,
  }
}

/**
 * A button that opens the emoji panel for the composer. Picking one calls
 * `onPick` and nothing else: the composer puts it in its text box, and
 * nothing is sent until the person sends.
 */
export function EmojiPickerButton({ onPick, disabled, discordRoom }: {
  onPick: (choice: EmojiChoice) => void
  disabled?: boolean
  discordRoom: DiscordRoom | null
}) {
  const [open, setOpen] = useState(false)
  const wrapper = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    if (!open) return
    const closeOnOutsideClick = (event: MouseEvent) => {
      if (wrapper.current && !wrapper.current.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', closeOnOutsideClick)
    return () => document.removeEventListener('mousedown', closeOnOutsideClick)
  }, [open])

  return (
    <div className={styles.emojiPickerWrap} ref={wrapper}>
      <button type="button" className={styles.emojiButton} disabled={disabled} aria-expanded={open}
        title="Insert an emoji (or type : and a name)" aria-label="Insert an emoji" onClick={() => setOpen(o => !o)}>
        {'\u{1F642}'}
      </button>
      {open && <EmojiPickerPanel purpose="message" discordRoom={discordRoom} onPick={onPick} onClose={() => setOpen(false)} />}
    </div>
  )
}

/** How many results a search shows. */
const SEARCH_RESULT_LIMIT = 300

/** One titled run of emoji in a grid: favorites, a server's custom emoji, a
 *  unicode group, or a search's matches. */
export interface EmojiGridSection {
  id: string
  name: string
  choices: readonly EmojiChoice[]
}

/** Favorites, then each server's custom emoji, then each unicode group. */
export function emojiSections(groups: readonly { name: string; emojis: readonly unknown[] }[] | null,
  unicode: readonly EmojiChoice[], custom: readonly EmojiChoice[], favorites: readonly EmojiChoice[]): EmojiGridSection[] {
  const sections: EmojiGridSection[] = [{ id: 'favorites', name: 'Favorites', choices: favorites }]
  // By server id: two servers can share a name. The seen emoji have none, so
  // they share one section.
  const byServer = new Map<string, { name: string; choices: EmojiChoice[] }>()
  for (const choice of custom) {
    if (choice.kind !== 'custom') continue
    const serverID = choice.emoji.discord_server_id
    const section = byServer.get(serverID) ?? { name: customEmojiSectionName(choice.emoji), choices: [] }
    section.choices.push(choice)
    byServer.set(serverID, section)
  }
  for (const [serverID, { name, choices }] of byServer) sections.push({ id: `server:${serverID}`, name, choices })
  let start = 0
  for (const group of groups ?? []) {
    sections.push({ id: `group:${group.name}`, name: group.name, choices: unicode.slice(start, start + group.emojis.length) })
    start += group.emojis.length
  }
  return sections
}

const EMOJI_CELL_WIDTH = 36
const EMOJI_ROW_HEIGHT = 36
const HEADING_HEIGHT = 24
/** Rows drawn beyond the view on each side, so a fast scroll shows no gap. */
const OVERSCAN_PX = 240

/**
 * A scrolling grid of emoji sections that draws only the rows in view, so
 * ~2,000 emoji scroll and search smoothly. `jumpBar` adds a row of section
 * icons above it that scroll to each section and mark the one in view.
 */
export function VirtualEmojiGrid({ sections, renderCell, jumpBar }: {
  sections: readonly EmojiGridSection[]
  renderCell: (choice: EmojiChoice) => ReactNode
  jumpBar?: boolean
}) {
  const scroller = useRef<HTMLDivElement | null>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [size, setSize] = useState({ width: 0, height: 0 })
  useLayoutEffect(() => {
    const el = scroller.current
    if (!el) return
    const measure = () => setSize({ width: el.clientWidth, height: el.clientHeight })
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(el)
    return () => observer.disconnect()
  }, [])
  // New sections (a search) start at the top.
  useLayoutEffect(() => {
    if (scroller.current) scroller.current.scrollTop = 0
    setScrollTop(0)
  }, [sections])
  const columns = Math.max(1, Math.floor(size.width / EMOJI_CELL_WIDTH))
  const layout = useMemo(() => emojiGridLayout(sections.map(section => section.choices.length), columns, EMOJI_ROW_HEIGHT, HEADING_HEIGHT),
    [sections, columns])
  const shown = rowsInView(layout.rows, scrollTop, size.height, OVERSCAN_PX)
  const current = sectionAt(layout.sectionTops, scrollTop)

  return (
    <>
      {jumpBar && (
        <div className={styles.emojiJumpBar} role="toolbar" aria-label="Jump to a section">
          {sections.map((section, index) => section.choices.length > 0 && (
            <button key={section.id} type="button" title={section.name} aria-label={`Jump to ${section.name}`}
              className={`${styles.emojiJumpButton} ${index === current ? styles.emojiJumpButtonCurrent : ''}`}
              onClick={() => { if (scroller.current) scroller.current.scrollTop = layout.sectionTops[index] }}>
              <EmojiFace choice={section.choices[0]} />
            </button>
          ))}
        </div>
      )}
      <div ref={scroller} className={styles.emojiGroups} onScroll={event => setScrollTop(event.currentTarget.scrollTop)}>
        <div className={styles.emojiGridCanvas} style={{ height: layout.height }}>
          {shown.map(row => row.kind === 'heading' ? (
            <div key={`h${row.sectionIndex}`} className={styles.emojiGridHeading} style={{ top: row.top, height: row.height }}>
              {sections[row.sectionIndex].name} ({sections[row.sectionIndex].choices.length})
            </div>
          ) : (
            <div key={`r${row.sectionIndex}-${row.start}`} className={styles.emojiGridRow}
              style={{ top: row.top, height: row.height, gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))` }}>
              {sections[row.sectionIndex].choices.slice(row.start, row.end).map(renderCell)}
            </div>
          ))}
        </div>
      </div>
    </>
  )
}

/** The searchable emoji panel: a jump bar, then favorites, the room's custom
 *  emoji and every unicode group. A search matches any part of a name (`:par`
 *  finds parrot and partyParrot) or a server's name (`pretend`, `reno kerm`),
 *  and Enter picks the first match. `heading`, when given, says what a pick
 *  is for. */
export function EmojiPickerPanel({ onPick, onClose, heading, purpose, discordRoom }: {
  onPick: (choice: EmojiChoice) => void
  onClose: () => void
  heading?: string
  purpose: EmojiPurpose
  discordRoom: DiscordRoom | null
}) {
  const { routes } = useBridgeConfig()
  const { groups, sections, pickableIndex, favoriteKeys, tone, error } = useEmojiChoices(purpose, discordRoom)
  const [query, setQuery] = useState('')
  // The box shows every keystroke at once; the results follow when React has time.
  const deferredQuery = useDeferredValue(query)
  const results = useMemo(() => searchEmojiIndex(pickableIndex, deferredQuery, favoriteKeys, SEARCH_RESULT_LIMIT),
    [pickableIndex, deferredQuery, favoriteKeys])
  const searching = deferredQuery.trim() !== ''
  const resultSections = useMemo(() => [{ id: 'matches', name: 'Matches', choices: results }], [results])

  return (
    <div className={styles.emojiPanel} role="dialog" aria-label="Emoji"
      onKeyDown={event => { if (event.key === 'Escape') { event.stopPropagation(); onClose() } }}>
      {heading && (
        <div className={styles.emojiPanelHeading}>
          <span>{heading}</span>
          <button type="button" className={styles.emojiPanelClose} aria-label="Close" onClick={onClose}>✕</button>
        </div>
      )}
      <input className={styles.input} type="search" value={query} placeholder="Search by name or server — :par, pretend, reno kerm" autoFocus
        onChange={event => setQuery(event.target.value)}
        onKeyDown={event => {
          if (event.key !== 'Enter') return
          event.preventDefault()
          const first = searchEmojiIndex(pickableIndex, query, favoriteKeys, 1)[0]
          if (first) onPick(first)
        }} />
      {error && <pre className={styles.error}>{error}</pre>}
      {!groups && !error && <div className={styles.empty}>Loading…</div>}
      {searching && results.length === 0 && <div className={styles.empty}>No emoji matches.</div>}
      <VirtualEmojiGrid sections={searching ? resultSections : sections} jumpBar={!searching}
        renderCell={choice => <EmojiCell key={choice.key} choice={choice} onPick={onPick} />} />
      <div className={styles.emojiPanelFoot}>
        {tone && <span className={styles.muted}>Skin tone {tone}</span>}
        {routes.emoji && <Link to={routes.emoji} className={styles.muted}>Favorites and settings</Link>}
      </div>
    </div>
  )
}

function EmojiCell({ choice, onPick }: { choice: EmojiChoice; onPick: (choice: EmojiChoice) => void }) {
  return (
    <button type="button" className={styles.emojiCell}
      title={`:${choice.name}:${choice.kind === 'custom' ? ` — ${customEmojiSectionName(choice.emoji)}` : ''}`} aria-label={choice.name}
      onClick={() => onPick(choice)}>
      <EmojiFace choice={choice} />
    </button>
  )
}

/** One row of the composer's `:` suggestions. A custom emoji outside a react
 *  command is listed but cannot be picked, with the reason. */
export interface EmojiSuggestion {
  choice: EmojiChoice
  disabledReason?: string
}

/** The suggestions a `:query` in the composer opens, above the text box.
 *  The composer owns the keys: arrows move, Enter or Tab picks, Escape shuts. */
export function EmojiSuggestionList({ suggestions, activeIndex, onPick, onHover }: {
  suggestions: readonly EmojiSuggestion[]
  activeIndex: number
  onPick: (suggestion: EmojiSuggestion) => void
  onHover: (index: number) => void
}) {
  return (
    <ul className={styles.emojiSuggestions} role="listbox" aria-label="Emoji suggestions">
      {suggestions.map((suggestion, index) => (
        <li key={suggestion.choice.key} role="option" aria-selected={index === activeIndex} aria-disabled={!!suggestion.disabledReason}
          className={`${styles.emojiSuggestion} ${index === activeIndex ? styles.emojiSuggestionActive : ''} ${suggestion.disabledReason ? styles.emojiSuggestionDisabled : ''}`}
          title={suggestion.disabledReason}
          onMouseEnter={() => onHover(index)}
          // mousedown, not click, so the text box keeps its focus and cursor.
          onMouseDown={event => { event.preventDefault(); if (!suggestion.disabledReason) onPick(suggestion) }}>
          <EmojiFace choice={suggestion.choice} className={styles.emojiSuggestionFace} />
          <span>:{suggestion.choice.name}:</span>
          {suggestion.choice.kind === 'custom' && <span className={styles.muted}>{customEmojiSectionName(suggestion.choice.emoji)}</span>}
          {suggestion.disabledReason && <span className={styles.muted}>react only</span>}
        </li>
      ))}
    </ul>
  )
}
