import { useCallback, useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import type { DiscordBridgeStatus, DiscordBridgedServer, LoggedMessage, MessageLogPage } from '@kayushkin/multichat-types'
import { useBridgeConfig } from '../context'
import {
  DISCORD_TABS, EMPTY_MESSAGE_LOG_FILTERS, appendMessageLogPage, archivedDiscordExtras, bridgeProblems, bridgedChannelsOf, channelLabel, channelOptions,
  channelProblems, currentVersionOf, deletedBeforeRead, discordTabOf, messageLogQuery, nextPageBefore, senderLabel, serverOptions, serverTotals,
  versionsOf, whereLabel, withServer, type DiscordTab, type MessageLogFilters,
} from '../discordLog'
import { discordNamesOf, type DiscordNames } from '../messageBody'
import { reactionsOfLoggedMessage } from '../messageReactions'
import { MessageContent, ReactionChips } from './messages/MessageContent'
import styles from './BridgeDiscord.module.css'
import { useDailyNitroCheck } from './messages/useEmojiCatalog'

/**
 * Discord, as multichat sees it through the mautrix-discord bridge.
 *
 * Messages and Deleted read multichat's message log (`GET /message-log`),
 * which keeps every bridged message as it arrives, so a message deleted on
 * Discord keeps its text here. Bridge status reads `GET /discord/status`: the
 * bridge's own database, which rooms @admin has joined, and the log's counts.
 * Nothing here calls Discord, and nothing writes.
 */
export function BridgeDiscord() {
  useDailyNitroCheck()
  const { fetch: apiFetch, multichatBasePath } = useBridgeConfig()
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = discordTabOf(searchParams.get('tab'))
  const [status, setStatus] = useState<DiscordBridgeStatus | null>(null)
  const [statusError, setStatusError] = useState<string | null>(null)

  const read = useCallback(async <T,>(path: string): Promise<T> => {
    const res = await apiFetch(`${multichatBasePath}${path}`)
    if (!res.ok) throw new Error(`GET ${path} → ${res.status}: ${(await res.text()).trim()}`)
    return await res.json() as T
  }, [apiFetch, multichatBasePath])

  const loadStatus = useCallback(async () => {
    try {
      setStatus(await read<DiscordBridgeStatus>('/discord/status'))
      setStatusError(null)
    } catch (err) {
      setStatusError(err instanceof Error ? err.message : String(err))
    }
  }, [read])
  useEffect(() => { void loadStatus() }, [loadStatus])

  const pickTab = (next: DiscordTab) => {
    const params = new URLSearchParams(searchParams)
    if (next === 'messages') params.delete('tab')
    else params.set('tab', next)
    setSearchParams(params, { replace: true })
  }
  const problems = status ? bridgeProblems(status) : []

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h2 className={styles.title}>Discord</h2>
        <p className={styles.subtitle}>
          Every message the Discord bridge brings into multichat is logged as it arrives, so a message deleted on Discord
          keeps its text here. The log starts when multichat began keeping it; older messages are not in it. Nothing on
          this page calls Discord or sends anything.
        </p>
      </header>
      <nav className={styles.tabs}>
        {DISCORD_TABS.map(t => (
          <button key={t.key} type="button" className={`${styles.tab} ${t.key === tab ? styles.tabActive : ''}`} onClick={() => pickTab(t.key)}>
            {t.label}
            {t.key === 'status' && (statusError || problems.length > 0) && <span className={styles.tabWarning} title="The bridge needs attention">!</span>}
          </button>
        ))}
      </nav>
      {tab === 'status'
        ? <BridgeStatusTab status={status} error={statusError} problems={problems} onReload={() => { void loadStatus() }} />
        : <MessageLogTab key={tab} deletedOnly={tab === 'deleted'} status={status} statusError={statusError} read={read} />}
    </div>
  )
}

type Read = <T>(path: string) => Promise<T>

function MessageLogTab({ deletedOnly, status, statusError, read }: {
  deletedOnly: boolean
  status: DiscordBridgeStatus | null
  statusError: string | null
  read: Read
}) {
  const [filters, setFilters] = useState<MessageLogFilters>(EMPTY_MESSAGE_LOG_FILTERS)
  const [textDraft, setTextDraft] = useState('')
  const [page, setPage] = useState<MessageLogPage | null>(null)
  const [olderFrom, setOlderFrom] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async (before?: string) => {
    setBusy(true)
    try {
      const next = await read<MessageLogPage>(messageLogQuery(filters, { deletedOnly, before }))
      setPage(shown => (before && shown ? appendMessageLogPage(shown, next) : next))
      setOlderFrom(nextPageBefore(next.messages))
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }, [read, filters, deletedOnly])
  useEffect(() => { void load() }, [load])

  const servers = useMemo(() => serverOptions(status), [status])
  const channels = useMemo(() => channelOptions(status, filters.serverID), [status, filters.serverID])
  const discordNames = useMemo(() => discordNamesOf(bridgedChannelsOf(status), page?.messages ?? []), [status, page])

  return (
    <section className={styles.section}>
      <form className={styles.filters} onSubmit={e => { e.preventDefault(); setFilters(f => ({ ...f, text: textDraft })) }}>
        <label className={styles.field}>
          <span>Server</span>
          <select value={filters.serverID} onChange={e => setFilters(f => withServer(f, status, e.target.value))}>
            <option value="">Every server</option>
            {servers.map(s => <option key={s.serverID} value={s.serverID}>{s.label}</option>)}
          </select>
        </label>
        <label className={styles.field}>
          <span>Channel</span>
          <select value={filters.roomID} onChange={e => setFilters(f => ({ ...f, roomID: e.target.value }))}>
            <option value="">Every channel</option>
            {channels.map(c => <option key={c.roomID} value={c.roomID}>{c.label}</option>)}
          </select>
        </label>
        <label className={`${styles.field} ${styles.fieldWide}`}>
          <span>Text contains</span>
          <input type="search" value={textDraft} placeholder="any text" onChange={e => setTextDraft(e.target.value)} />
        </label>
        <button type="submit" className={styles.button} disabled={busy}>Search</button>
      </form>
      {statusError && <div className={styles.muted}>Servers and channels cannot be listed for filtering: {statusError}</div>}
      {error && <pre className={styles.error}>{error}</pre>}
      {page && page.messages.length === 0 && !busy && (
        <div className={styles.empty}>{deletedOnly ? 'No deleted message is in the log for these filters.' : 'No message in the log matches.'}</div>
      )}
      {page && page.messages.length > 0 && (
        <ul className={styles.list}>
          {page.messages.map(message => deletedOnly
            ? <DeletedMessageRow key={message.id} message={message} page={page} discordNames={discordNames} />
            : <MessageRow key={message.id} message={message} page={page} discordNames={discordNames} />)}
        </ul>
      )}
      {busy && <div className={styles.muted}>Loading…</div>}
      {olderFrom && !busy && (
        <button type="button" className={styles.button} onClick={() => { void load(olderFrom) }}>Older messages</button>
      )}
    </section>
  )
}

interface RowProps {
  message: LoggedMessage
  page: MessageLogPage
  discordNames: DiscordNames
}

/** A message as it reads now — its newest edit, when it has one — with
 *  "edited" opening the versions it went through. Edits are not rows of their
 *  own: multichat lists them only under the message they edit. */
function MessageRow({ message, page, discordNames }: RowProps) {
  const versions = versionsOf(message, page.edits)
  const [showVersions, setShowVersions] = useState(false)
  const deleted = message.redacted_at !== null
  const current = currentVersionOf(message, page.edits)
  return (
    <li className={`${styles.row} ${deleted ? styles.rowDeleted : ''}`} data-log-id={message.id} data-event-id={message.event_id || undefined}>
      <div className={styles.rowHead}>
        <span className={styles.sender} title={message.sender_user_id}>{senderLabel(message)}</span>
        <ChannelLink message={message} />
        <time className={styles.muted} dateTime={message.sent_at}>{new Date(message.sent_at).toLocaleString()}</time>
        {versions.length > 1 && (
          <button type="button" className={styles.flagButton} aria-expanded={showVersions}
            title={`Last edited ${new Date(current.sent_at).toLocaleString()}`} onClick={() => setShowVersions(v => !v)}>
            edited{versions.length > 2 ? ` ${versions.length - 1}×` : ''}
          </button>
        )}
        {deleted && <span className={`${styles.flag} ${styles.flagWarning}`}>deleted {new Date(message.redacted_at!).toLocaleString()}</span>}
        {message.source === 'discord-archive' && <span className={styles.flag} title="Imported from Discord's history, not logged live">archive</span>}
      </div>
      {showVersions ? <VersionList versions={versions} discordNames={discordNames} /> : <LogMessageBody message={current} discordNames={discordNames} />}
      <ReactionChips reactions={reactionsOfLoggedMessage(message, page.reactions)} />
    </li>
  )
}

function DeletedMessageRow({ message, page, discordNames }: RowProps) {
  const versions = versionsOf(message, page.edits)
  return (
    <li className={`${styles.row} ${styles.rowDeleted}`} data-log-id={message.id} data-event-id={message.event_id || undefined}>
      <div className={styles.rowHead}>
        <span className={styles.sender} title={message.sender_user_id}>{senderLabel(message)}</span>
        <ChannelLink message={message} />
      </div>
      <dl className={styles.times}>
        <dt>Sent</dt><dd><time dateTime={message.sent_at}>{new Date(message.sent_at).toLocaleString()}</time></dd>
        <dt>Deleted</dt><dd><time dateTime={message.redacted_at ?? ''}>{message.redacted_at ? new Date(message.redacted_at).toLocaleString() : '—'}</time></dd>
      </dl>
      {versions.length > 1 ? <VersionList versions={versions} discordNames={discordNames} /> : <LogMessageBody message={message} discordNames={discordNames} />}
      <ReactionChips reactions={reactionsOfLoggedMessage(message, page.reactions)} />
      <span className={styles.id}>{message.event_id || `Discord message ${message.discord_message_id}`} · deleted by {message.redacted_by_user_id || 'unknown'} (on Discord the bridge relays every delete)</span>
    </li>
  )
}

function VersionList({ versions, discordNames }: { versions: ReturnType<typeof versionsOf>; discordNames: DiscordNames }) {
  return (
    <ol className={styles.versions}>
      {versions.map(v => (
        <li key={v.message.id}>
          <span className={styles.muted}>{v.isOriginal ? 'first sent' : 'edited'} {new Date(v.at).toLocaleString()}</span>
          <LogMessageBody message={v.message} discordNames={discordNames} />
        </li>
      ))}
    </ol>
  )
}

function LogMessageBody({ message, discordNames }: { message: LoggedMessage; discordNames: DiscordNames }) {
  if (deletedBeforeRead(message)) return <div className={styles.muted}>No text: it was deleted before multichat read it.</div>
  const extras = archivedDiscordExtras(message)
  if (extras.systemLabel) return <div className={styles.systemMessage}>{senderLabel(message)} {extras.systemLabel}</div>
  const hasContent = message.body !== '' || message.media_url !== ''
  return (
    <div className={styles.body}>
      {hasContent && <MessageContent message={message} messageType={message.message_type} discordNames={discordNames} />}
      {extras.stickers.map(sticker => (
        <div key={sticker.id} className={styles.muted} title={`Discord sticker ${sticker.id}`}>sticker: {sticker.name}</div>
      ))}
      {extras.attachments.map(attachment => (
        <div key={attachment.url} className={styles.muted}>
          attachment: <a href={attachment.url} target="_blank" rel="noopener noreferrer" title="Discord's link to the file, which expires">{attachment.filename}</a>
        </div>
      ))}
      {extras.unreadable && <div className={styles.error}>{extras.unreadable}</div>}
    </div>
  )
}

function ChannelLink({ message }: { message: LoggedMessage }) {
  const label = whereLabel(message)
  if (!message.channel_external_url) return <span className={styles.muted}>{label}</span>
  return <a className={styles.channel} href={message.channel_external_url} target="_blank" rel="noreferrer">{label}</a>
}

function BridgeStatusTab({ status, error, problems, onReload }: {
  status: DiscordBridgeStatus | null
  error: string | null
  problems: string[]
  onReload: () => void
}) {
  return (
    <section className={styles.section}>
      <div className={styles.rowHead}>
        <button type="button" className={styles.button} onClick={onReload}>Reload</button>
      </div>
      {error && <pre className={styles.error}>{error}</pre>}
      {!status && !error && <div className={styles.muted}>Loading…</div>}
      {status && (
        <>
          {problems.length > 0 && (
            <div className={styles.alert}>{problems.map(p => <div key={p}>{p}</div>)}</div>
          )}
          <h3 className={styles.heading}>Account</h3>
          <ul className={styles.list}>
            {status.accounts.map(account => (
              <li key={account.matrix_user_id} className={`${styles.row} ${account.logged_in ? '' : styles.rowAlert}`}>
                <div className={styles.rowHead}>
                  <span className={styles.sender}>{account.matrix_user_id}</span>
                  {account.logged_in
                    ? <span className={`${styles.flag} ${styles.flagGood}`}>logged in</span>
                    : <span className={`${styles.flag} ${styles.flagAlert}`}>logged out</span>}
                  {account.discord_user_id && <span className={styles.muted}>Discord user {account.discord_user_id}</span>}
                </div>
                {account.management_room_id && <span className={styles.id}>management room {account.management_room_id}</span>}
              </li>
            ))}
          </ul>
          <h3 className={styles.heading}>Bridged servers</h3>
          <p className={styles.muted}>
            The account is in {status.unbridged_server_count} more server{status.unbridged_server_count === 1 ? '' : 's'} that
            {status.unbridged_server_count === 1 ? ' is' : ' are'} not bridged, so nothing from {status.unbridged_server_count === 1 ? 'it' : 'them'} reaches multichat.
          </p>
          {status.bridged_servers.map(server => <ServerStatus key={server.discord_server_id} server={server} />)}
        </>
      )}
    </section>
  )
}

function ServerStatus({ server }: { server: DiscordBridgedServer }) {
  const totals = serverTotals(server)
  return (
    <div className={styles.server}>
      <div className={styles.rowHead}>
        <span className={styles.serverName}>{server.name}</span>
        <span className={styles.muted}>server {server.discord_server_id} · bridging {server.bridging_mode_name || `mode ${server.bridging_mode}`}</span>
        <span className={styles.muted}>{totals.channels} channels · {totals.logged} logged · {totals.deleted} deleted</span>
        {totals.unreadable > 0 && <span className={`${styles.flag} ${styles.flagAlert}`}>{totals.unreadable} unreadable</span>}
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Channel</th><th>Category</th><th>Joined</th><th>Bridge last saw a message</th>
              <th>Logged</th><th>Deleted</th><th>Last logged</th><th>Ids</th>
            </tr>
          </thead>
          <tbody>
            {server.channels.map(channel => {
              const channelIssues = channelProblems(channel)
              return (
                <tr key={channel.discord_channel_id} className={channelIssues.length > 0 ? styles.rowAlert : ''}>
                  <td>{channelLabel(channel)}</td>
                  <td>{channel.category_name || '—'}</td>
                  <td>
                    {channelIssues.length === 0
                      ? <span className={`${styles.flag} ${styles.flagGood}`}>joined</span>
                      : <span className={`${styles.flag} ${styles.flagAlert}`} title={channelIssues.join('; ')}>{channel.matrix_room_id ? 'not joined' : 'no room'}</span>}
                  </td>
                  <td>{channel.last_bridged_message_at ? new Date(channel.last_bridged_message_at).toLocaleString() : 'never'}</td>
                  <td>{channel.logged_message_count}</td>
                  <td>{channel.deleted_message_count}</td>
                  <td>{channel.last_logged_message_at ? new Date(channel.last_logged_message_at).toLocaleString() : 'never'}</td>
                  <td className={styles.id}>{channel.discord_channel_id}<br />{channel.matrix_room_id || '—'}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}
