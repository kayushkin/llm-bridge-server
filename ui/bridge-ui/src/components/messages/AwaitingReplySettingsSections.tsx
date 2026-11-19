import { useEffect, useState } from 'react'
import type { AwaitingReplySettings } from '@kayushkin/multichat-types'
import { SettingsSection, useSectionSave } from '../settings/SettingsSection'
import { ALERT_SOUNDS, browserAlertSettingsProblem, settingsWriteBody, type MessageAlertBrowserSettings } from '../../awaitingReply'
import { useAwaitingReply } from '../../useAwaitingReply'
import { useBrowserAlertSettings } from '../../useBrowserAlertSettings'
import { playAlertSound } from '../../alertSound'
import type { MultichatTag } from '../../types-multichat'
import { errorText, useMultichat } from './useMultichat'
import { TagChip } from './messagesShared'
import styles from './Messages.module.css'

/** The Waiting on you settings: who counts and what clears a room (kept in
 *  multichat, the same on every device), and the sound and pop-ups for a new
 *  message (kept in this browser). */
export function AwaitingReplySettingsSections() {
  return (
    <div className={styles.awaitingSettings}>
      <WhoCountsSection />
      <BrowserAlertsSection />
    </div>
  )
}

function WhoCountsSection() {
  const { overview, saveSettings } = useAwaitingReply()
  const { read } = useMultichat()
  const [tags, setTags] = useState<MultichatTag[] | null>(null)
  const [tagsError, setTagsError] = useState<string | null>(null)
  const [draft, setDraft] = useState<AwaitingReplySettings | null>(null)
  const [anySizeTagsText, setAnySizeTagsText] = useState('')
  const [neverCountTagsText, setNeverCountTagsText] = useState('')
  const save = useSectionSave()
  const stored = overview?.settings ?? null

  useEffect(() => {
    read<MultichatTag[] | null>('/tags').then(next => setTags(next ?? [])).catch(err => setTagsError(errorText(err)))
  }, [read])
  useEffect(() => {
    if (stored && !draft) {
      setDraft(stored)
      setAnySizeTagsText(stored.any_size_person_tags.join(', '))
      setNeverCountTagsText(stored.never_count_person_tags.join(', '))
    }
  }, [stored, draft])

  if (!draft || !stored) return null
  const dirty = JSON.stringify(settingsWriteBody(draft)) !== JSON.stringify(settingsWriteBody(stored))
  const chosen = new Set(draft.tag_ids)
  const toggleTag = (id: number) => setDraft({ ...draft, tag_ids: chosen.has(id) ? draft.tag_ids.filter(t => t !== id) : [...draft.tag_ids, id] })

  return (
    <SettingsSection id="awaiting-reply-who-counts" title="Who counts and what clears it" scope="global" storedBy="multichat · awaiting_reply_settings"
      help="In a direct message or a small group, a person counts when you count everyone people-store knows, or when any of their accounts carries one of these tags; then a message from any of their accounts counts. In a bigger group only people with one of the people-store tags below count. A room carrying a tag counts everyone in it."
      save={{ dirty, state: save, onSave: () => void save.run(() => saveSettings(draft)) }}>
      <div className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Tags</span>
        {tagsError && <span className={styles.error}>Tags could not be read: {tagsError}</span>}
        {tags?.map(tag => (
          <label key={tag.id} className={styles.check}>
            <input type="checkbox" checked={chosen.has(tag.id)} onChange={() => toggleTag(tag.id)} /> <TagChip tag={tag} />
          </label>
        ))}
      </div>
      <label className={styles.check}>
        <input type="checkbox" checked={draft.count_every_known_person} onChange={e => setDraft({ ...draft, count_every_known_person: e.target.checked })} />
        Count everyone in people-store, tagged or not. Accounts no person owns (short codes, unknown numbers, bots) still need a tag.
      </label>
      <label className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Groups up to</span>
        <input className={`${styles.input} ${styles.thresholdInput}`} type="number" min={2} value={draft.max_group_member_count}
          onChange={e => setDraft({ ...draft, max_group_member_count: Number(e.target.value) })} />
        <span className={styles.muted}>people, you included. Discord server channels never count, unless you tag the room.</span>
      </label>
      <label className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Any size for people tagged</span>
        <input className={styles.input} type="text" value={anySizeTagsText} placeholder="work"
          onChange={e => setAnySizeTagsText(e.target.value)}
          onBlur={() => setDraft({ ...draft, any_size_person_tags: personTagsFromText(anySizeTagsText) })} />
        <span className={styles.muted}>people-store tags, comma-separated. Their messages count in a group of any size.</span>
      </label>
      <label className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Never count people tagged</span>
        <input className={styles.input} type="text" value={neverCountTagsText} placeholder="political, spam"
          onChange={e => setNeverCountTagsText(e.target.value)}
          onBlur={() => setDraft({ ...draft, never_count_person_tags: personTagsFromText(neverCountTagsText) })} />
        <span className={styles.muted}>people-store tags, comma-separated. Wins over every rule above except a tagged room.</span>
      </label>
      <label className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Ignore messages older than</span>
        <input className={`${styles.input} ${styles.thresholdInput}`} type="number" min={1} value={draft.max_message_age_days}
          onChange={e => setDraft({ ...draft, max_message_age_days: Number(e.target.value) })} />
        <span className={styles.muted}>days</span>
      </label>
      <label className={styles.check}>
        <input type="checkbox" checked={draft.clear_when_opened} onChange={e => setDraft({ ...draft, clear_when_opened: e.target.checked })} />
        Opening a conversation in dash clears it
      </label>
      <label className={styles.check}>
        <input type="checkbox" checked={draft.clear_when_replied} onChange={e => setDraft({ ...draft, clear_when_replied: e.target.checked })} />
        Replying from any app or device clears what came before it
      </label>
    </SettingsSection>
  )
}

/** People-store tags typed as "work, ge": trimmed, blanks dropped. */
function personTagsFromText(text: string): string[] {
  return text.split(',').map(tag => tag.trim()).filter(tag => tag !== '')
}

function BrowserAlertsSection() {
  const { settings, problem, save } = useBrowserAlertSettings()
  const [testError, setTestError] = useState<string | null>(null)
  const [permission, setPermission] = useState<NotificationPermission | 'unsupported'>(
    typeof Notification === 'undefined' ? 'unsupported' : Notification.permission)
  const [refused, setRefused] = useState<string | null>(null)
  const set = (change: Partial<MessageAlertBrowserSettings>) => {
    const next = { ...settings, ...change }
    const wrong = browserAlertSettingsProblem(next)
    setRefused(wrong && `Not saved: ${wrong}`)
    if (!wrong) save(next)
  }
  const test = () => {
    setTestError(null)
    playAlertSound(settings.sound, settings.volume).catch(err => setTestError(errorText(err)))
  }
  const askForNotifications = async (on: boolean) => {
    if (on && permission !== 'granted' && permission !== 'unsupported') setPermission(await Notification.requestPermission())
    set({ desktopNotification: on })
  }

  return (
    <SettingsSection id="awaiting-reply-browser-alerts" title="Sound and pop-ups" scope="browser" storedBy="this browser · localStorage"
      help="What a new message from someone you tagged does on this device. Another browser or phone keeps its own."
      status={{ error: refused ?? problem ?? testError, note: 'Saved as you change it.' }}>
      <label className={styles.check}>
        <input type="checkbox" checked={settings.playSound} onChange={e => set({ playSound: e.target.checked })} /> Play a sound
      </label>
      <div className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Sound</span>
        <select className={styles.input} value={settings.sound} onChange={e => set({ sound: e.target.value })}>
          {ALERT_SOUNDS.map(sound => <option key={sound.name} value={sound.name}>{sound.label}</option>)}
        </select>
        <button type="button" className={styles.olderButton} onClick={test}>▶ Test</button>
      </div>
      <label className={styles.awaitingSettingsRow}>
        <span className={styles.awaitingSettingsLabel}>Volume</span>
        <input type="range" min={0} max={1} step={0.05} value={settings.volume} onChange={e => set({ volume: Number(e.target.value) })} />
        <span className={styles.muted}>{Math.round(settings.volume * 100)}%</span>
      </label>
      <label className={styles.check}>
        <input type="checkbox" checked={settings.popUp} onChange={e => set({ popUp: e.target.checked })} /> Pop-up in dash
      </label>
      <label className={styles.check}>
        <input type="checkbox" checked={settings.desktopNotification} disabled={permission === 'unsupported'}
          onChange={e => void askForNotifications(e.target.checked)} />
        Desktop notification when dash is in a background tab
        {permission === 'denied' && <span className={styles.muted}> — the browser blocks notifications for this site</span>}
        {permission === 'unsupported' && <span className={styles.muted}> — this browser has none</span>}
      </label>
      <div className={styles.awaitingSettingsRow}>
        <label className={styles.check}>
          <input type="checkbox" checked={settings.quietHours.enabled}
            onChange={e => set({ quietHours: { ...settings.quietHours, enabled: e.target.checked } })} /> Quiet hours
        </label>
        <input className={styles.input} type="time" value={settings.quietHours.start}
          onChange={e => set({ quietHours: { ...settings.quietHours, start: e.target.value } })} />
        –
        <input className={styles.input} type="time" value={settings.quietHours.end}
          onChange={e => set({ quietHours: { ...settings.quietHours, end: e.target.value } })} />
        <span className={styles.muted}>no sound or pop-up; the list still updates</span>
      </div>
    </SettingsSection>
  )
}
