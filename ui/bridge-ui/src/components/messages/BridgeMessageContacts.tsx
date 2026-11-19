import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  contactKey, contactListPlatforms, contactPlatforms, contactTagAssignBody, contactTagRemovePath, contactTags, filterContacts,
  tagCreateBodyOf, tagDeletePath,
} from '../../multichatMessages'
import type { MultichatContactTagMap, MultichatTag, MultichatUnifiedContact } from '../../types-multichat'
import { errorText, useMultichat } from './useMultichat'
import { MultichatNotConfigured, TagChip } from './messagesShared'
import styles from './Messages.module.css'
import { useDailyNitroCheck } from './useEmojiCatalog'
import { IdentityLink, PersonLinkPicker } from './PersonLinks'

/**
 * Everyone multichat's bridges know, one row per person: the app identities
 * linked to one principal-store id, by phone number, by the review agent or by
 * hand here. Tags are multichat's own and sit
 * on one identity — one Matrix user id — so each is put on or taken off an
 * identity, and a person's row shows every tag on any of theirs.
 */
export function BridgeMessageContacts() {
  useDailyNitroCheck()
  const { read, write, configured } = useMultichat()
  const [contacts, setContacts] = useState<MultichatUnifiedContact[] | null>(null)
  const [tags, setTags] = useState<MultichatTag[]>([])
  const [tagMap, setTagMap] = useState<MultichatContactTagMap>({})
  const [loadError, setLoadError] = useState<string | null>(null)
  const [text, setText] = useState('')
  const [platform, setPlatform] = useState('')
  const [tagID, setTagID] = useState<number | null>(null)
  const [onSeveralApps, setOnSeveralApps] = useState(false)
  const [expanded, setExpanded] = useState<string | null>(null)

  const reloadTags = useCallback(async () => {
    const [nextTags, nextMap] = await Promise.all([
      read<MultichatTag[] | null>('/tags'),
      read<MultichatContactTagMap | null>('/contacts/tags/bulk'),
    ])
    setTags(nextTags ?? [])
    setTagMap(nextMap ?? {})
  }, [read])

  const reloadContacts = useCallback(async () => {
    try {
      setContacts((await read<MultichatUnifiedContact[] | null>('/contacts/unified')) ?? [])
      setLoadError(null)
    } catch (err) {
      setLoadError(errorText(err))
    }
  }, [read])

  useEffect(() => {
    if (!configured) return
    Promise.all([reloadContacts(), reloadTags()]).catch(err => setLoadError(errorText(err)))
  }, [configured, reloadContacts, reloadTags])

  /** A write, then the tags read back from multichat; the refusal, or null. */
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

  const platforms = useMemo(() => contactListPlatforms(contacts ?? []), [contacts])
  const shown = useMemo(
    () => filterContacts(contacts ?? [], tagMap, { text, platform, tagID, onSeveralApps }),
    [contacts, tagMap, text, platform, tagID, onSeveralApps],
  )
  const onSeveralAppsCount = useMemo(() => (contacts ?? []).filter(c => contactPlatforms(c).length > 1).length, [contacts])

  if (!configured) return <MultichatNotConfigured page="Contacts" />

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h2 className={styles.title}>Contacts</h2>
        <p className={styles.subtitle}>
          Everyone multichat&apos;s bridges know, one row per person. Accounts that share a phone number are linked on their
          own, and the review agent links people it is sure of; open a row to link an account to someone else, or to
          separate one that is not them. People and their links live in people-store; the links the agent was not sure
          of wait as suggestions on the People page.
        </p>
      </header>
      {loadError && <pre className={styles.error}>{loadError}</pre>}
      {!contacts ? (loadError ? null : <div className={styles.empty}>Loading…</div>) : (
        <>
          <TagManager tags={tags} write={writeTags} onDeleted={id => { if (tagID === id) setTagID(null) }} />
          <div className={styles.filters}>
            <input className={styles.input} type="search" value={text} placeholder="Filter by name or user id"
              onChange={e => setText(e.target.value)} />
            <select className={styles.input} value={platform} onChange={e => setPlatform(e.target.value)}>
              <option value="">Every app</option>
              {platforms.map(p => <option key={p} value={p}>{p}</option>)}
            </select>
            <select className={styles.input} value={tagID ?? ''} onChange={e => setTagID(e.target.value ? Number(e.target.value) : null)}>
              <option value="">Any tag or none</option>
              {tags.map(tag => <option key={tag.id} value={tag.id}>{tag.name}</option>)}
            </select>
            <label className={styles.check}>
              <input type="checkbox" checked={onSeveralApps} onChange={e => setOnSeveralApps(e.target.checked)} />
              On more than one app ({onSeveralAppsCount})
            </label>
          </div>
          <div className={styles.muted}>{shown.length} of {contacts.length} contacts</div>
          <ul className={styles.list}>
            {shown.map(contact => {
              const key = contactKey(contact)
              const open = expanded === key
              return (
                <li key={key} className={styles.row}>
                  <button type="button" className={`${styles.rowPick} ${styles.rowHead}`} aria-expanded={open}
                    onClick={() => setExpanded(open ? null : key)}>
                    <span className={styles.name}>{contact.display_name}</span>
                    {contactPlatforms(contact).map(p => <span key={p} className={styles.platform}>{p}</span>)}
                    {contactTags(contact, tagMap).map(tag => <TagChip key={tag.id} tag={tag} />)}
                    <span className={styles.chevron}>{open ? '▾' : '▸'}</span>
                  </button>
                  {open && <ContactIdentities contact={contact} contacts={contacts} tags={tags} tagMap={tagMap} write={writeTags} onLinksChanged={reloadContacts} />}
                </li>
              )
            })}
          </ul>
        </>
      )}
    </div>
  )
}

/** Each app identity of one person, with the tags on it: × takes one off, and
 *  the dashed ones put a tag on. */
function ContactIdentities({ contact, contacts, tags, tagMap, write, onLinksChanged }: {
  contact: MultichatUnifiedContact
  contacts: readonly MultichatUnifiedContact[]
  onLinksChanged: () => Promise<void>
  tags: MultichatTag[]
  tagMap: MultichatContactTagMap
  write: (method: string, path: string, body?: unknown) => Promise<string | null>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = async (method: string, path: string, body?: unknown) => {
    setBusy(true)
    setError(await write(method, path, body))
    setBusy(false)
  }
  return (
    <div className={styles.identities}>
      {contact.identities.map(identity => {
        const onIdentity = tagMap[identity.user_id] ?? []
        const addable = tags.filter(tag => !onIdentity.some(on => on.id === tag.id))
        return (
          <div key={identity.user_id} className={styles.identity} data-user-id={identity.user_id}>
            <span className={styles.platform}>{identity.platform}</span>
            <span>
              {identity.display_name && identity.display_name !== contact.display_name && <span>{identity.display_name} </span>}
              <code className={styles.roomID}>{identity.user_id}</code>{' '}
              <IdentityLink identity={identity} onChanged={onLinksChanged} />
            </span>
            <span className={styles.tagRow}>
              {onIdentity.map(tag => (
                <TagChip key={tag.id} tag={tag} disabled={busy}
                  onRemove={() => { void run('DELETE', contactTagRemovePath(identity.user_id, tag.id)) }} />
              ))}
              {addable.map(tag => (
                <button key={tag.id} type="button" className={styles.tagAdd} disabled={busy}
                  onClick={() => { void run('POST', '/contacts/tags', contactTagAssignBody(identity.user_id, tag.id)) }}>
                  + {tag.name}
                </button>
              ))}
              {tags.length === 0 && <span className={styles.muted}>No tags exist yet; add one above.</span>}
            </span>
          </div>
        )
      })}
      {contact.identities[0] && (
        <PersonLinkPicker userID={contact.identities[0].user_id} contacts={contacts} onLinked={onLinksChanged} />
      )}
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** Every tag, with a box to add one and × to delete one. Deleting a tag takes
 *  it off every contact that has it. */
function TagManager({ tags, write, onDeleted }: {
  tags: MultichatTag[]
  write: (method: string, path: string, body?: unknown) => Promise<string | null>
  onDeleted: (tagID: number) => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const create = async () => {
    const result = tagCreateBodyOf(name, tags)
    if (!result.ok) { setError(result.error); return }
    setBusy(true)
    const refusal = await write('POST', '/tags', result.body)
    setBusy(false)
    setError(refusal)
    if (refusal === null) setName('')
  }
  const remove = async (tag: MultichatTag) => {
    if (!window.confirm(`Delete the tag "${tag.name}"? It is taken off every contact that has it.`)) return
    setBusy(true)
    const refusal = await write('DELETE', tagDeletePath(tag.id))
    setBusy(false)
    setError(refusal)
    if (refusal === null) onDeleted(tag.id)
  }

  return (
    <section className={styles.tagManager}>
      <span className={styles.muted}>Tags</span>
      <span className={styles.tagRow}>
        {tags.length === 0 && <span className={styles.muted}>none yet</span>}
        {tags.map(tag => <TagChip key={tag.id} tag={tag} disabled={busy} onRemove={() => { void remove(tag) }} />)}
      </span>
      <form className={styles.filters} onSubmit={e => { e.preventDefault(); void create() }}>
        <input className={styles.input} value={name} placeholder="New tag" onChange={e => setName(e.target.value)} />
        <button type="submit" className="bp-cancel" disabled={busy || !name.trim()}>Add tag</button>
      </form>
      {error && <pre className={styles.error}>{error}</pre>}
    </section>
  )
}
