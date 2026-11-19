import { useDeferredValue, useEffect, useMemo, useState } from 'react'
import type { EmojiSettings } from '@kayushkin/multichat-types'
import {
  allCustomEmoji, customChoice, customEmojiSectionName, emojiSearchIndex, serverSweepSummary, favoriteChoices, SKIN_TONES, searchEmojiIndex, unicodeChoices, unicodeNamesOf, withFavoriteMoved,
  withFavoriteToggled, withSkinTone, type EmojiChoice,
} from '../../emojiCatalog'
import { messageTimeLabel } from '../../multichatMessages'
import { EmojiFace, emojiSections, VirtualEmojiGrid } from './EmojiPicker'
import { MultichatNotConfigured } from './messagesShared'
import { useEmojiCatalog, useEmojiGroups, useDailyNitroCheck } from './useEmojiCatalog'
import { useMultichat } from './useMultichat'
import styles from './Messages.module.css'

/**
 * The emoji pickers' settings, stored by multichat (`GET /emoji`,
 * `PUT /emoji/favorites`, `PUT /emoji/settings`): the favorites, which the
 * quick-react bar under each message offers first; the skin tone; and which
 * servers' custom emoji a Discord room is offered. Below them, every bridged
 * server's custom emoji and every unicode emoji, each a click away from being
 * a favorite, and the refresh that lists the custom ones again from Discord.
 */
export function BridgeEmoji() {
  useDailyNitroCheck()
  const { configured } = useMultichat()
  const { catalog, error, saveFavorites, saveSettings, refreshFromDiscord, checkNitroNow, startServerSweep, reload } = useEmojiCatalog()
  const sweepRunning = !!catalog?.discord_server_sweep.running
  // While the listing runs, read its progress (and the emoji it has added) every 30 seconds.
  useEffect(() => {
    if (!sweepRunning) return
    const timer = window.setInterval(() => { void reload() }, 30_000)
    return () => window.clearInterval(timer)
  }, [sweepRunning, reload])
  const [checkingNitro, setCheckingNitro] = useState(false)
  const { groups, error: groupsError } = useEmojiGroups()
  const [query, setQuery] = useState('')
  const [writeError, setWriteError] = useState<string | null>(null)
  const [refreshing, setRefreshing] = useState(false)

  const tone = catalog?.settings.skin_tone ?? ''
  const favoriteKeys = useMemo(() => catalog?.favorite_keys ?? [], [catalog])
  const favoriteSet = useMemo(() => new Set(favoriteKeys), [favoriteKeys])
  const unicode = useMemo(() => (groups ? unicodeChoices(groups, tone) : []), [groups, tone])
  // The bridged servers' emoji, then the ones seen in messages.
  const custom = useMemo(() => allCustomEmoji(catalog).map(customChoice), [catalog])
  const favorites = useMemo(() => favoriteChoices(favoriteKeys, allCustomEmoji(catalog),
    groups ? unicodeNamesOf(groups) : new Map()), [favoriteKeys, catalog, groups])
  const index = useMemo(() => emojiSearchIndex([...custom, ...unicode]), [custom, unicode])
  const deferredQuery = useDeferredValue(query)
  const results = useMemo(() => searchEmojiIndex(index, deferredQuery, favoriteKeys), [index, deferredQuery, favoriteKeys])
  const resultSections = useMemo(() => [{ id: 'matches', name: 'Matches', choices: results }], [results])
  // The favorites have their own list above, so the grid starts at the servers.
  const sections = useMemo(() => emojiSections(groups, unicode, custom, []), [groups, unicode, custom])

  if (!configured) return <MultichatNotConfigured page="Emoji" />

  const writeFavorites = async (keys: string[]) => setWriteError(await saveFavorites(keys))
  const writeSettings = async (settings: EmojiSettings) => setWriteError(await saveSettings(settings))
  const checkNitro = async () => {
    setCheckingNitro(true)
    setWriteError(await checkNitroNow())
    setCheckingNitro(false)
  }
  const refresh = async () => {
    setRefreshing(true)
    setWriteError(await refreshFromDiscord())
    setRefreshing(false)
  }
  const toggle = (choice: EmojiChoice) => { void writeFavorites(withFavoriteToggled(favoriteKeys, choice.key)) }

  const cell = (choice: EmojiChoice) => {
    const favorite = favoriteSet.has(choice.key)
    const notTaught = choice.kind === 'custom' && !choice.emoji.bridge_mxc
    return (
      <button key={choice.key} type="button" aria-pressed={favorite}
        className={`${styles.emojiCell} ${favorite ? styles.emojiCellFavorite : ''}`}
        title={`:${choice.name}:${choice.kind === 'custom' ? ` — ${customEmojiSectionName(choice.emoji)}` : ''}${notTaught ? '\nThe bridge learns this one the first time you react with it.' : ''}\n${favorite ? 'Click to take it out of your favorites' : 'Click to make it a favorite'}`}
        onClick={() => toggle(choice)}>
        <EmojiFace choice={choice} />
      </button>
    )
  }

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h2 className={styles.title}>Emoji</h2>
        <p className={styles.subtitle}>
          Your favorites come first in every picker, and the first few sit beside each message as one-click reactions.
          In a message, type <code>:</code> and part of a name; start with <code>+:</code> to react to the newest message instead.
        </p>
      </header>
      {error && <pre className={styles.error}>{error}</pre>}
      {groupsError && <pre className={styles.error}>The unicode emoji did not load: {groupsError}</pre>}
      {writeError && <pre className={styles.error}>{writeError}</pre>}
      {!catalog ? (error ? null : <div className={styles.empty}>Loading…</div>) : (
        <>
          <section className={styles.emojiPageSection}>
            <h3 className={styles.emojiPageHeading}>Settings</h3>
            <div className={styles.emojiSetting}>
              <span>Skin tone</span>
              <div className={styles.emojiToneRow} role="radiogroup" aria-label="Skin tone">
                {SKIN_TONES.map(({ tone: option, label }) => (
                  <button key={label} type="button" role="radio" aria-checked={tone === option} title={label}
                    className={`${styles.emojiCell} ${tone === option ? styles.emojiCellFavorite : ''}`}
                    onClick={() => { void writeSettings({ ...catalog.settings, skin_tone: option }) }}>
                    {withSkinTone('\u{1F44B}', true, option)}
                  </button>
                ))}
              </div>
            </div>
            <label className={styles.emojiSetting}>
              <span>Quick reactions beside each message</span>
              <input className={styles.input} type="number" min={0} value={catalog.settings.quick_reaction_count}
                onChange={event => {
                  const count = Number(event.target.value)
                  if (Number.isInteger(count)) void writeSettings({ ...catalog.settings, quick_reaction_count: count })
                }} />
            </label>
            <div className={styles.emojiSetting}>
              <span>
                {!catalog.discord_nitro ? 'Nitro not checked yet: a Discord room is offered only its own server\'s emoji.'
                  : catalog.discord_nitro.has_nitro ? 'Your Discord account has Nitro, so a Discord room is offered every server\'s emoji and the ones seen in messages.'
                    : 'Your Discord account has no Nitro, so a Discord room is offered only its own server\'s emoji.'}
                <span className={styles.muted}>
                  {catalog.discord_nitro && ` Checked ${messageTimeLabel(Date.parse(catalog.discord_nitro.checked_at))}; checked again the first time you open Messages each day.`}
                </span>
              </span>
              <button type="button" className="bi-save-btn" disabled={checkingNitro} onClick={() => { void checkNitro() }}>
                {checkingNitro ? 'Asking Discord…' : 'Check now'}
              </button>
            </div>
            {catalog.discord_nitro_error && <pre className={styles.error}>The last Nitro check failed: {catalog.discord_nitro_error}</pre>}
          </section>

          <section className={styles.emojiPageSection}>
            <h3 className={styles.emojiPageHeading}>Favorites</h3>
            {favorites.length === 0 ? <div className={styles.empty}>No favorites yet. Click any emoji below to add it.</div> : (
              <ol className={styles.emojiFavoriteList}>
                {favorites.map((choice, index) => (
                  <li key={choice.key} className={styles.emojiFavorite}>
                    <EmojiFace choice={choice} className={styles.emojiSuggestionFace} />
                    <span>:{choice.name}:</span>
                    {index < catalog.settings.quick_reaction_count && <span className={styles.muted}>quick reaction</span>}
                    <span className={styles.emojiFavoriteButtons}>
                      <button type="button" className={styles.emojiPanelClose} disabled={index === 0} aria-label={`Move ${choice.name} earlier`}
                        onClick={() => { void writeFavorites(withFavoriteMoved(favoriteKeys, choice.key, -1)) }}>↑</button>
                      <button type="button" className={styles.emojiPanelClose} disabled={index === favorites.length - 1} aria-label={`Move ${choice.name} later`}
                        onClick={() => { void writeFavorites(withFavoriteMoved(favoriteKeys, choice.key, 1)) }}>↓</button>
                      <button type="button" className={styles.emojiPanelClose} aria-label={`Remove ${choice.name}`}
                        onClick={() => toggle(choice)}>✕</button>
                    </span>
                  </li>
                ))}
              </ol>
            )}
          </section>

          <section className={styles.emojiPageSection}>
            <div className={styles.emojiPageHeadingRow}>
              <h3 className={styles.emojiPageHeading}>All emoji</h3>
              <input className={styles.input} type="search" value={query} placeholder="Search by name or server — :par, pretend, reno kerm"
                onChange={event => setQuery(event.target.value)} />
            </div>
            <div className={styles.emojiPageHeadingRow}>
              <span className={styles.muted}>
                Custom emoji of your Discord servers, and the ones seen in messages from servers you are not in.{' '}
                {serverSweepSummary(catalog.discord_server_sweep, catalog.discord_refreshes.length)}
              </span>
              <button type="button" className="bi-save-btn" disabled={refreshing} onClick={() => { void refresh() }}
                title="Lists the bridged servers' emoji again now, with the bot. The other servers are listed by the weekly listing.">
                {refreshing ? 'Asking Discord…' : 'Refresh bridged servers'}
              </button>
              <button type="button" className="bi-save-btn" disabled={sweepRunning}
                title="Lists the emoji of every server you are in that was not listed in the past week, one server every 30 seconds."
                onClick={() => { void startServerSweep().then(setWriteError) }}>
                {sweepRunning ? 'Listing…' : 'List all my servers now'}
              </button>
            </div>
            {catalog.discord_server_sweep.error && (
              <pre className={styles.error}>The listing of your servers&apos; emoji: {catalog.discord_server_sweep.error}</pre>
            )}
            {custom.length === 0 && <div className={styles.empty}>No custom emoji yet. Refresh from Discord to list each server&apos;s.</div>}
            {deferredQuery.trim() && results.length === 0 && <div className={styles.empty}>No emoji matches.</div>}
            <div className={styles.emojiPageGrid}>
              <VirtualEmojiGrid sections={deferredQuery.trim() ? resultSections : sections} jumpBar={!deferredQuery.trim()} renderCell={cell} />
            </div>
          </section>
        </>
      )}
    </div>
  )
}
