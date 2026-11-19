import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  contactLinkBody, contactUnlinkBody, linkCandidates, linkedByLabel, personOfIdentity,
} from '../../multichatMessages'
import type { MultichatContactIdentity, MultichatUnifiedContact } from '../../types-multichat'
import { errorText, useMultichat } from './useMultichat'
import { useBridgeConfig } from '../../context'
import { conversationSendPath } from '../../multichatMessages'
import { libraryInvitationMessage, type LibraryInvitation } from '../../libraryInvitation'
import styles from './Messages.module.css'

/** Everyone, one row per person, and a reload after a link changes. */
export function useUnifiedContacts(enabled = true) {
  const { read, configured } = useMultichat()
  const [contacts, setContacts] = useState<MultichatUnifiedContact[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const reload = useCallback(async () => {
    try {
      setContacts((await read<MultichatUnifiedContact[] | null>('/contacts/unified')) ?? [])
      setError(null)
    } catch (err) {
      setError(errorText(err))
    }
  }, [read])
  useEffect(() => {
    if (configured && enabled) void reload()
  }, [configured, enabled, reload])
  return { contacts, error, reload }
}

/** A search box that links one identity to another person: type part of any
 *  of their names, pick the row. The link goes live at once. */
export function PersonLinkPicker({ userID, contacts, onLinked }: {
  userID: string
  contacts: readonly MultichatUnifiedContact[]
  onLinked: () => Promise<void> | void
}) {
  const { write } = useMultichat()
  const [query, setQuery] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const candidates = useMemo(() => linkCandidates(contacts, userID, query), [contacts, userID, query])
  const link = async (target: MultichatUnifiedContact) => {
    setBusy(true)
    const result = await write('POST', '/contacts/links', contactLinkBody(userID, target))
    setBusy(false)
    if (!result.ok) { setError(result.error); return }
    setError(null)
    setQuery('')
    await onLinked()
  }
  return (
    <div className={styles.personLinkPicker} data-link-picker={userID}>
      <input className={styles.input} type="search" value={query} disabled={busy}
        placeholder="Same person as… (type a name)" onChange={e => setQuery(e.target.value)} />
      {candidates.length > 0 && (
        <ul className={styles.personLinkCandidates}>
          {candidates.map(candidate => (
            <li key={candidate.person_id ?? candidate.identities[0]?.user_id}>
              <button type="button" className={styles.tagAdd} disabled={busy} onClick={() => { void link(candidate) }}>
                {candidate.display_name}
                {candidate.identities.map(identity => (
                  <span key={identity.user_id} className={styles.platform}>
                    {identity.platform}{identity.display_name && identity.display_name !== candidate.display_name ? ` · ${identity.display_name}` : ''}
                  </span>
                ))}
              </button>
            </li>
          ))}
        </ul>
      )}
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

/** Who linked an identity and why, with a button that separates it from its
 *  person. Separating also tells the phone matcher and the agent never to link
 *  it back. */
export function IdentityLink({ identity, onChanged }: {
  identity: MultichatContactIdentity
  onChanged: () => Promise<void> | void
}) {
  const { write } = useMultichat()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  if (!identity.linked_by) return null
  const unlink = async () => {
    setBusy(true)
    const result = await write('POST', '/contacts/links/remove', contactUnlinkBody(identity.user_id))
    setBusy(false)
    if (!result.ok) { setError(result.error); return }
    setError(null)
    await onChanged()
  }
  return (
    <span className={styles.identityLink} title={identity.link_reason}>
      <span className={styles.muted}>{linkedByLabel(identity.linked_by)}</span>
      <button type="button" className={styles.tagAdd} disabled={busy} onClick={() => { void unlink() }}>Not this person</button>
      {error && <pre className={styles.error}>{error}</pre>}
    </span>
  )
}

/** Invites a person to the host's library and posts the sign-in link into
 *  this conversation, which reaches them on that app. Drawn only when the host
 *  names its invitation route. */
function InviteToLibrary({ personID, displayName, roomID, onSent }: {
  personID: string
  displayName: string
  roomID: string
  onSent: () => void
}) {
  const { fetch: apiFetch, libraryInvitationsPath } = useBridgeConfig()
  const { write } = useMultichat()
  const [state, setState] = useState<'idle' | 'busy' | 'sent'>('idle')
  const [error, setError] = useState<string | null>(null)
  if (!libraryInvitationsPath) return null
  const invite = async () => {
    setState('busy')
    setError(null)
    try {
      const res = await apiFetch(libraryInvitationsPath, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ person_id: personID }),
      })
      if (!res.ok) throw new Error(`invite ${displayName}: ${res.status} ${(await res.text()).trim()}`)
      const invitation = await res.json() as LibraryInvitation
      const sent = await write('POST', conversationSendPath(roomID), { body: libraryInvitationMessage(invitation.url) })
      if (!sent.ok) throw new Error(`made the link, but could not send it here: ${sent.error}`)
      setState('sent')
      onSent()
    } catch (err) {
      setError(errorText(err))
      setState('idle')
    }
  }
  return (
    <span className={styles.identityLink}>
      <button type="button" className={styles.tagAdd} disabled={state === 'busy'}
        title={`Give ${displayName} a login with access to the whole library, and send them a one-time sign-in link here`}
        onClick={() => { void invite() }}>
        {state === 'busy' ? 'Inviting…' : state === 'sent' ? 'Link sent · send another' : 'Invite to library'}
      </button>
      {error && <pre className={styles.error}>{error}</pre>}
    </span>
  )
}

/** In a one-to-one conversation: who the other person is across apps, a
 *  picker to link this account to someone else, and the library invite. */
export function ConversationPerson({ partnerUserID, roomID, onSent }: { partnerUserID: string; roomID: string; onSent: () => void }) {
  const { contacts, error, reload } = useUnifiedContacts()
  if (error) return <pre className={styles.error}>{error}</pre>
  if (!contacts) return null
  const person = personOfIdentity(contacts, partnerUserID)
  const identity = person?.identities.find(i => i.user_id === partnerUserID)
  const elsewhere = person?.identities.filter(i => i.user_id !== partnerUserID) ?? []
  return (
    <div className={styles.threadTags} data-conversation-person={partnerUserID}>
      <span className={styles.muted}>Person</span>
      <span className={styles.name}>{person?.display_name ?? partnerUserID}</span>
      {identity?.linked_by && (
        <span className={styles.identityLink}>
          <span className={styles.muted}>this {identity.platform} account:</span>
          <IdentityLink identity={identity} onChanged={reload} />
        </span>
      )}
      {elsewhere.length > 0 && <span className={styles.muted}>also</span>}
      {elsewhere.map(other => (
        <span key={other.user_id} className={styles.platform} title={`${linkedByLabel(other.linked_by)}: ${other.link_reason ?? ''}`}>
          {other.platform}{other.display_name ? ` · ${other.display_name}` : ''}
        </span>
      ))}
      {person?.person_id && <InviteToLibrary personID={person.person_id} displayName={person.display_name} roomID={roomID} onSent={onSent} />}
      <PersonLinkPicker userID={partnerUserID} contacts={contacts} onLinked={reload} />
    </div>
  )
}
