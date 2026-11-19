import { useCallback, useEffect, useMemo, useState, type FormEvent, type JSX } from 'react'
import type { PeerConnectionView } from '@kayushkin/llm-bridge-types'
import { useBridgeConfig } from '../context'
import { SharedPoll, failureOfResponse, sharedPoll, useSharedPoll, type LoadResult } from '../sharedPoll'
import type { FetchFn } from '../types'
import { useBridgeInstances } from '../useBridgeInstances'
import { PEER_CONNECTIONS_PATH, peerRouteErrorText } from '../peers'
import { connectedServerLabel } from '../serverListings'
import { fetchServiceSettings } from '../serviceSettings'
import {
  approvalMachineGroups,
  orchestratorPlacesApprovals,
  peerMessagePlacementText,
  approvalTargetDescription,
  approvalTargetWarning,
  formatAttachmentSize,
  peerAttachmentCanRetry,
  peerAttachmentDownloadable,
  peerConnectionMessagesPath,
  peerMessageAttachmentPath,
  peerMessageAttachmentRetryPath,
  peerMessageAttachmentUploadPath,
  peerMessageApprovePath,
  peerMessageCommitList,
  peerMessageDeclinePath,
  peerMessageHeadline,
  peerMessageNeedsDecision,
  peerMessageNoticeBody,
  peerMessagePatchBody,
  peerMessageRefusalText,
  peerMessageRequestBody,
  peerMessageThreads,
  peerMessagesListPath,
  parsePeerMessageCommits,
  untrustedTextPieces,
  type PeerMessage,
  type PeerMessageAttachment,
  type PeerMessageApproveRequest,
  type PeerMessageCreateRequest,
} from '../peerMessages'

// The cards for messages from connected bridge servers: a request to start a
// session, a notice that something is ready, a patch to land, what came of
// any of them, and the files a message carries. Rules
// live in `src/peerMessages.ts`; this file fetches and draws.

const jsonPost = (body: unknown): RequestInit => ({
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})

/** The server's refusal in plain words when it is one a person can act on,
 *  followed by the server's own text. */
async function peerMessageErrorText(res: Response, verb: string): Promise<string> {
  const text = await peerRouteErrorText(res.clone(), verb)
  try {
    const code = ((await res.json()) as { error?: { code?: string } }).error?.code ?? ''
    const plain = peerMessageRefusalText(code)
    return plain ? `${plain} (${text})` : text
  } catch {
    return text
  }
}

/** The `error.code` of a refusal, or '' when the body carries none. */
async function refusalCodeOf(res: Response): Promise<string> {
  try {
    return ((await res.json()) as { error?: { code?: string } }).error?.code ?? ''
  } catch {
    return ''
  }
}

interface PeerMessageList {
  /** False when this server answers 404: it has no message routes, which is
   *  not "no messages". */
  available: boolean
  messages: PeerMessage[]
}

async function loadPeerMessages(fetchFn: FetchFn, url: string): Promise<LoadResult<PeerMessageList>> {
  let res: Response
  try {
    res = await fetchFn(url)
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err), failure: 'unreachable' }
  }
  try {
    if (res.status === 404) return { ok: true, value: { available: false, messages: [] } }
    // A person who is not an administrator is refused the route as an operator
    // route: the messages are not theirs to see, which is not a failure.
    if (res.status === 403 && (await refusalCodeOf(res.clone())) === 'operator_route') {
      return { ok: true, value: { available: false, messages: [] } }
    }
    if (!res.ok) {
      return {
        ok: false,
        error: await peerMessageErrorText(res, 'read messages from connected servers'),
        failure: failureOfResponse(res),
      }
    }
    return { ok: true, value: { available: true, messages: (await res.json()) as PeerMessage[] } }
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err), failure: 'refused' }
  }
}

const NO_PEER_MESSAGES_YET: PeerMessageList = { available: true, messages: [] }

/** Every message the filter names, re-read every 30 seconds, when asked, and
 *  when the page comes back online or into view. One read per filter however
 *  many components show it: the orchestrator row, the signals inbox and the
 *  signals page all ask for the waiting ones. A dropped request is retried
 *  within seconds and reported only if the retries fail too. */
export function usePeerMessages(filter: Parameters<typeof peerMessagesListPath>[0]): {
  messages: PeerMessage[]
  available: boolean
  error: string | null
  reload: () => void
} {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const url = `${basePath}${peerMessagesListPath(filter)}`
  const poll = useMemo(
    () => sharedPoll(fetchFn, `peer-messages ${url}`, () =>
      new SharedPoll<PeerMessageList>(() => loadPeerMessages(fetchFn, url), NO_PEER_MESSAGES_YET)),
    [fetchFn, url],
  )
  const { data, error } = useSharedPoll(poll)
  const reload = useCallback(() => { void poll.refresh() }, [poll])
  return { messages: data.messages, available: data.available, error, reload }
}

/** Each connection's server name, by connection id, for card headings. */
export function usePeerConnectionNames(): (connectionID: string) => string {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [views, setViews] = useState<PeerConnectionView[]>([])
  useEffect(() => {
    let canceled = false
    void (async () => {
      const res = await fetchFn(`${basePath}${PEER_CONNECTIONS_PATH}`).catch(() => null)
      if (!canceled && res?.ok) setViews((await res.json()) as PeerConnectionView[])
    })()
    return () => { canceled = true }
  }, [fetchFn, basePath])
  return useMemo(() => {
    const names = new Map(views.map(view => [view.connection.id, connectedServerLabel(view).label]))
    return (connectionID: string) => names.get(connectionID) ?? 'A connected server'
  }, [views])
}

/** Text another machine wrote, drawn as text: line breaks kept, a web address
 *  shown distinctly but never as a link that loads or opens by itself. */
export function UntrustedText({ text, className }: { text: string; className?: string }): JSX.Element {
  return (
    <div className={`pm-untrusted ${className ?? ''}`}>
      {untrustedTextPieces(text).map((piece, index) =>
        piece.kind === 'address' ? (
          <span key={index} className="pm-address" title="An address from the other server; copy it to open it">
            {piece.text}
          </span>
        ) : (
          <span key={index}>{piece.text}</span>
        ),
      )}
    </div>
  )
}

/** Every incoming request and notice waiting on a person here, newest first,
 *  read once for a surface that shows both a count and the cards. */
export function useWaitingPeerMessages(): {
  waiting: PeerMessage[]
  available: boolean
  error: string | null
  reload: () => void
} {
  const { messages, available, error, reload } = usePeerMessages({ direction: 'incoming', state: 'pending' })
  const waiting = useMemo(
    () => messages.filter(peerMessageNeedsDecision).sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [messages],
  )
  return { waiting, available, error, reload }
}

/** "Needs you" for messages from connected servers: a card per waiting
 *  message. Draws nothing when none is waiting and nothing failed. */
export function PeerMessagesNeedingYou({ waiting, error, reload, onSelectSession }: {
  waiting: readonly PeerMessage[]
  error: string | null
  reload: () => void
  onSelectSession: (sessionID: string) => void
}): JSX.Element | null {
  const nameOf = usePeerConnectionNames()
  if (waiting.length === 0 && error === null) return null
  return (
    <section className="pm-needs-you" aria-label="Requests from connected servers">
      {error !== null && <p className="bridge-error pm-error">{error}</p>}
      {waiting.map(message => (
        <PeerMessageCard
          key={message.id}
          message={message}
          serverName={nameOf(message.connection_id)}
          onDecided={reload}
          onSelectSession={onSelectSession}
        />
      ))}
    </section>
  )
}

/** One message from a connected server, with what a person here may do about
 *  it. */
export function PeerMessageCard({ message, serverName, onDecided, onSelectSession }: {
  message: PeerMessage
  serverName: string
  onDecided: () => void
  onSelectSession?: (sessionID: string) => void
}): JSX.Element {
  const decidable = peerMessageNeedsDecision(message)
  return (
    <article className={`pm-card pm-card-${message.kind}`} data-state={message.state}>
      <header className="pm-card-head">
        <span className="pm-kind">{message.kind}</span>
        <span className="pm-headline">
          {message.kind === 'request' && message.direction === 'incoming'
            ? `${serverName} asks to start a session`
            : message.kind === 'notice' && message.direction === 'incoming'
              ? `${serverName} says these are ready`
              : peerMessageHeadline(message, serverName)}
        </span>
        <span className="pm-when">{new Date(message.created_at).toLocaleString()}</span>
      </header>
      {message.kind === 'request' && (
        <>
          <div className="pm-title">{message.body.title || '(untitled)'}</div>
          <p className="pm-hint">
            What the new session would be told, written by {serverName}. It reaches the session marked as a request
            from that server, not as your words.
          </p>
          <UntrustedText text={message.body.prompt ?? ''} className="pm-prompt" />
        </>
      )}
      {message.kind === 'notice' && (
        <>
          {message.body.summary && <UntrustedText text={message.body.summary} className="pm-summary" />}
          {(message.body.commits ?? []).length > 0 && (
            <ul className="pm-commits">
              {(message.body.commits ?? []).map(commit => (
                <li key={`${commit.repo}@${commit.commit}`}>
                  <code>{commit.repo}@{commit.commit}</code>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {message.kind === 'patch' && <PeerPatchDetails message={message} serverName={serverName} />}
      {message.kind === 'status' && message.body.detail && <UntrustedText text={message.body.detail} className="pm-summary" />}
      {message.kind === 'status' && (message.body.versions ?? []).length > 0 && (
        <p className="pm-versions">Now at {peerMessageCommitList(message.body.versions)}</p>
      )}
      {(message.attachments ?? []).length > 0 && <PeerMessageAttachments message={message} onChanged={onDecided} />}
      {message.decision_reason && <p className="pm-reason">Reason: <UntrustedText text={message.decision_reason} /></p>}
      {message.state === 'failed' && message.last_error && (
        <p className="pm-reason">Not delivered: <UntrustedText text={message.last_error} /></p>
      )}
      {message.session_id && (
        <p className="pm-session">
          Started{' '}
          {onSelectSession && message.direction === 'incoming' ? (
            <button type="button" className="bc-ctrl-btn" onClick={() => onSelectSession(message.session_id!)}>
              {message.session_id}
            </button>
          ) : (
            <code>{message.session_id}</code>
          )}
        </p>
      )}
      {peerMessagePlacementText(message) !== null && <p className="pm-session">{peerMessagePlacementText(message)}</p>}
      {decidable && <PeerMessageDecision message={message} onDecided={onDecided} onSelectSession={onSelectSession} />}
    </article>
  )
}

/** A patch's repo, base and what its verified files change, as this server
 *  read them. The sender's summary is drawn as text from another machine. */
function PeerPatchDetails({ message, serverName }: { message: PeerMessage; serverName: string }): JSX.Element {
  const read = message.patch
  return (
    <div className="pm-patch">
      <p className="pm-patch-target">
        <code>{message.body.repo}</code> against <code>{(message.body.base_commit ?? '').slice(0, 12)}</code>
      </p>
      {message.body.summary && <UntrustedText text={message.body.summary} className="pm-summary" />}
      {read ? (
        <>
          {read.subjects.length > 0 && (
            <ul className="pm-patch-subjects">
              {read.subjects.map((subject, index) => (
                <li key={index}><UntrustedText text={subject} /></li>
              ))}
            </ul>
          )}
          {read.files.length > 0 && (
            <ul className="pm-patch-files">
              {read.files.map(file => (
                <li key={file.path}>
                  <code>{file.path}</code> <span className="pm-patch-plus">+{file.insertions}</span>{' '}
                  <span className="pm-patch-minus">−{file.deletions}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        message.direction === 'incoming' && (
          <p className="pm-hint">What it changes shows here once its files have arrived from {serverName} and been checked.</p>
        )
      )}
      {message.direction === 'incoming' && (
        <p className="pm-hint">
          Approving starts a session here that reads the files, applies them in a worktree under this server&apos;s own git
          identity, runs the tests, and lands them as this server&apos;s prompt says.
        </p>
      )}
    </div>
  )
}

/** The files a message carries: name, size, checksum and where each stands.
 *  A file is only ever downloaded, never opened as a page here. */
function PeerMessageAttachments({ message, onChanged }: { message: PeerMessage; onChanged: () => void }): JSX.Element {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const download = async (attachment: PeerMessageAttachment) => {
    setBusy(attachment.id)
    setError(null)
    try {
      const res = await fetchFn(`${basePath}${peerMessageAttachmentPath(message.id, attachment.id)}`)
      if (!res.ok) {
        setError(await peerMessageErrorText(res, `download ${attachment.name}`))
        return
      }
      const url = URL.createObjectURL(new Blob([await res.arrayBuffer()], { type: 'application/octet-stream' }))
      const link = document.createElement('a')
      link.href = url
      link.download = attachment.name
      link.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(null)
    }
  }

  const retry = async (attachment: PeerMessageAttachment) => {
    setBusy(attachment.id)
    setError(null)
    try {
      const res = await fetchFn(`${basePath}${peerMessageAttachmentRetryPath(message.id, attachment.id)}`, jsonPost({}))
      if (!res.ok) {
        setError(await peerMessageErrorText(res, `fetch ${attachment.name} again`))
        return
      }
      onChanged()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="pm-attachments">
      <ul>
        {(message.attachments ?? []).map(attachment => (
          <li key={attachment.id} className="pm-attachment" data-state={attachment.state}>
            <span className="pm-attachment-name">{attachment.name}</span>{' '}
            <span className="pm-attachment-meta">
              {formatAttachmentSize(attachment.size)} · sha256 <code title={attachment.sha256}>{attachment.sha256.slice(0, 12)}</code> · {attachment.state}
            </span>
            {attachment.last_error && <span className="pm-attachment-error"> — <UntrustedText text={attachment.last_error} /></span>}{' '}
            {peerAttachmentDownloadable(attachment) && (
              <button type="button" className="bc-ctrl-btn" disabled={busy !== null} onClick={() => void download(attachment)}>
                Download
              </button>
            )}
            {peerAttachmentCanRetry(message, attachment) && (
              <button type="button" className="bc-ctrl-btn" disabled={busy !== null} onClick={() => void retry(attachment)}>
                Fetch again
              </button>
            )}
          </li>
        ))}
      </ul>
      {error !== null && <p className="bridge-error pm-error">{error}</p>}
    </div>
  )
}

/** Whether a server hands approved requests and patches to its orchestrator
 *  (true) or a person picks the instance here (false), read from that
 *  server's own settings, so a connected server's card follows that server.
 *  Null until the first read. One shared read per server, every five minutes. */
export function approvalWayPoll(fetchFn: FetchFn, basePath: string): SharedPoll<boolean | null> {
  return sharedPoll(fetchFn, `approval-way ${basePath}`, () =>
    new SharedPoll<boolean | null>(async () => {
      const answer = await fetchServiceSettings(fetchFn, basePath)
      if (answer.phase === 'described') return { ok: true, value: orchestratorPlacesApprovals(answer.described) }
      if (answer.phase === 'failed') return { ok: false, error: answer.message, failure: 'unreachable' }
      return { ok: false, error: `GET ${basePath}/settings answered HTTP ${answer.status}`, failure: 'refused' }
    }, null, 5 * 60_000))
}

function PeerMessageDecision({ message, onDecided, onSelectSession }: {
  message: PeerMessage
  onDecided: () => void
  onSelectSession?: (sessionID: string) => void
}): JSX.Element {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const approvalWay = useSharedPoll(approvalWayPoll(fetchFn, basePath))
  const { instances } = useBridgeInstances()
  const machineGroups = useMemo(() => approvalMachineGroups(instances), [instances])
  const [instanceID, setInstanceID] = useState('')
  const chosenGroup = machineGroups.find(group => group.instances.some(instance => instance.id === instanceID))
  const targetWarning = chosenGroup ? approvalTargetWarning(message.kind, chosenGroup) : null
  const [declining, setDeclining] = useState(false)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const post = async (path: string, body: unknown, verb: string): Promise<PeerMessage | null> => {
    setBusy(true)
    setError(null)
    try {
      const res = await fetchFn(`${basePath}${path}`, jsonPost(body))
      if (!res.ok) {
        setError(await peerMessageErrorText(res, verb))
        return null
      }
      return (await res.json()) as PeerMessage
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      return null
    } finally {
      setBusy(false)
    }
  }

  const approve = async (body: PeerMessageApproveRequest) => {
    const decided = await post(peerMessageApprovePath(message.id), body, 'approve the message')
    if (!decided) return
    onDecided()
    if (decided.session_id && onSelectSession) onSelectSession(decided.session_id)
  }

  const decline = async (event: FormEvent) => {
    event.preventDefault()
    if (await post(peerMessageDeclinePath(message.id), { reason: reason.trim() }, 'decline the message')) onDecided()
  }

  return (
    <div className="pm-decision">
      {message.kind === 'request' || message.kind === 'patch' ? (
        approvalWay.data === null ? (
          approvalWay.error !== null
            ? <p className="bridge-error pm-error">Cannot approve: {approvalWay.error}</p>
            : <p className="pm-approve">Reading how this server starts approved work…</p>
        ) : approvalWay.data ? (
          <div className="pm-approve">
            <button type="button" className="bc-ctrl-btn bc-ctrl-btn-active" disabled={busy} onClick={() => void approve({})}>
              {message.kind === 'patch' ? 'Approve and land it' : 'Approve'}
            </button>
            <span className="pm-approve-target">The orchestrator chooses the machine it runs on.</span>
          </div>
        ) : (
        <div className="pm-approve">
          <label>
            Start it on{' '}
            <select value={instanceID} onChange={event => setInstanceID(event.target.value)} disabled={busy}>
              <option value="">Choose an instance…</option>
              {machineGroups.map(group => (
                <optgroup key={group.machineID || 'unreported'} label={group.isThisServer ? `${group.machineName} (this server)` : group.machineName}>
                  {group.instances.map(instance => (
                    <option key={instance.id} value={instance.id}>
                      {instance.name} ({instance.harness_type})
                    </option>
                  ))}
                </optgroup>
              ))}
            </select>
          </label>
          {chosenGroup && <span className="pm-approve-target">{approvalTargetDescription(chosenGroup)}</span>}
          {targetWarning && <span className="pm-approve-warning" role="alert">{targetWarning}</span>}
          <button
            type="button"
            className="bc-ctrl-btn bc-ctrl-btn-active"
            disabled={busy || !instanceID}
            onClick={() => void approve({ instance_id: instanceID })}
          >
            {message.kind === 'patch' ? 'Approve and land it' : 'Approve'}
          </button>
        </div>
        )
      ) : (
        <div className="pm-approve">
          <button type="button" className="bc-ctrl-btn bc-ctrl-btn-active" disabled={busy} onClick={() => void approve({})}>
            Update now
          </button>
        </div>
      )}
      {declining ? (
        <form className="pm-decline" onSubmit={decline}>
          <input
            value={reason}
            onChange={event => setReason(event.target.value)}
            placeholder="Why (sent to the other server)"
            disabled={busy}
          />
          <button type="submit" className="bc-ctrl-btn" disabled={busy || !reason.trim()}>
            {message.kind === 'notice' ? 'Not now' : 'Decline'}
          </button>
          <button type="button" className="bc-ctrl-btn" disabled={busy} onClick={() => setDeclining(false)}>
            Cancel
          </button>
        </form>
      ) : (
        <button type="button" className="bc-ctrl-btn" disabled={busy} onClick={() => setDeclining(true)}>
          {message.kind === 'notice' ? 'Not now…' : 'Decline…'}
        </button>
      )}
      {error !== null && <p className="bridge-error pm-error">{error}</p>}
    </div>
  )
}

/** One connection's messages both ways, threaded, with the forms to send a
 *  request or a notice. For the Peers page. */
export function PeerConnectionMessages({ connectionID, serverName }: { connectionID: string; serverName: string }): JSX.Element {
  const { messages, available, error, reload } = usePeerMessages({ connectionID })
  const threads = useMemo(() => peerMessageThreads(messages), [messages])
  if (!available) {
    return <p className="bp-hint">This server has no message routes yet, so it cannot send or show messages.</p>
  }
  return (
    <div className="pm-connection">
      {error !== null && <p className="bridge-error pm-error">{error}</p>}
      {threads.length === 0 ? (
        <p className="bp-hint">No messages with {serverName} yet.</p>
      ) : (
        threads.map(thread => (
          <div key={thread[0].id} className="pm-thread">
            {thread.map(message => (
              <div key={message.id} className={`pm-thread-row pm-${message.direction}`}>
                <PeerMessageCard message={message} serverName={serverName} onDecided={reload} />
                <span className="pm-state">{message.direction === 'incoming' ? 'received' : 'sent'} · {message.state}</span>
              </div>
            ))}
          </div>
        ))
      )}
      <SendPeerMessageForm connectionID={connectionID} serverName={serverName} onSent={reload} />
    </div>
  )
}

type SendKind = 'request' | 'notice' | 'patch'

function SendPeerMessageForm({ connectionID, serverName, onSent }: {
  connectionID: string
  serverName: string
  onSent: () => void
}): JSX.Element {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  const [kind, setKind] = useState<SendKind>('request')
  const [title, setTitle] = useState('')
  const [prompt, setPrompt] = useState('')
  const [summary, setSummary] = useState('')
  const [commitText, setCommitText] = useState('')
  const [repo, setRepo] = useState('')
  const [baseCommit, setBaseCommit] = useState('')
  const [files, setFiles] = useState<File[]>([])
  const [fileInputKey, setFileInputKey] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Each file is uploaded to this server first; the message then names the
  // uploads, and the other server fetches their bytes over the connection.
  const uploadFiles = async (): Promise<string[] | null> => {
    const ids: string[] = []
    for (const file of files) {
      const res = await fetchFn(`${basePath}${peerMessageAttachmentUploadPath(file.name, file.type)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: file,
      })
      if (!res.ok) {
        setError(await peerMessageErrorText(res, `upload ${file.name}`))
        return null
      }
      ids.push(((await res.json()) as PeerMessageAttachment).id)
    }
    return ids
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    let commits: ReturnType<typeof parsePeerMessageCommits>['commits'] = []
    if (kind === 'notice') {
      const parsed = parsePeerMessageCommits(commitText)
      if (parsed.error) {
        setError(parsed.error)
        return
      }
      commits = parsed.commits
    }
    if (kind === 'patch' && files.length === 0) {
      setError('A patch needs at least one git format-patch file.')
      return
    }
    setBusy(true)
    setError(null)
    try {
      const attachmentIDs = await uploadFiles()
      if (attachmentIDs === null) return
      const body: PeerMessageCreateRequest =
        kind === 'request'
          ? peerMessageRequestBody(title, prompt, undefined, attachmentIDs)
          : kind === 'notice'
            ? peerMessageNoticeBody(summary, commits, undefined, attachmentIDs)
            : peerMessagePatchBody(repo, baseCommit, summary, attachmentIDs)
      const res = await fetchFn(`${basePath}${peerConnectionMessagesPath(connectionID)}`, jsonPost(body))
      if (!res.ok) {
        setError(await peerMessageErrorText(res, `send the ${kind} to ${serverName}`))
        return
      }
      setTitle('')
      setPrompt('')
      setSummary('')
      setCommitText('')
      setRepo('')
      setBaseCommit('')
      setFiles([])
      setFileInputKey(key => key + 1)
      onSent()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const parsedCommits = kind === 'notice' ? parsePeerMessageCommits(commitText) : null
  return (
    <form className="pm-send" onSubmit={submit}>
      <div className="pm-send-kind" role="radiogroup" aria-label="What to send">
        <label>
          <input type="radio" checked={kind === 'request'} onChange={() => setKind('request')} /> Ask {serverName} to start a session
        </label>
        <label>
          <input type="radio" checked={kind === 'notice'} onChange={() => setKind('notice')} /> Tell {serverName} something is ready
        </label>
        <label>
          <input type="radio" checked={kind === 'patch'} onChange={() => setKind('patch')} /> Send {serverName} a patch to land
        </label>
      </div>
      {kind === 'patch' ? (
        <>
          <input value={repo} onChange={event => setRepo(event.target.value)} placeholder="Repo it changes, e.g. llm-bridge-server" disabled={busy} />
          <input value={baseCommit} onChange={event => setBaseCommit(event.target.value)} placeholder="Commit it was made against" disabled={busy} />
          <input value={summary} onChange={event => setSummary(event.target.value)} placeholder="What it does (optional)" disabled={busy} />
          <p className="bp-hint">
            Attach its <code>git format-patch</code> file below (from an agent, <code>bridge-peer patch</code> makes and sends it in one step).
            {` ${serverName}`} lands it only when a person there approves it.
          </p>
        </>
      ) : kind === 'request' ? (
        <>
          <input value={title} onChange={event => setTitle(event.target.value)} placeholder="Title" disabled={busy} />
          <textarea
            value={prompt}
            onChange={event => setPrompt(event.target.value)}
            placeholder="What the session should do — what you would type as its first message"
            rows={5}
            disabled={busy}
          />
          <p className="bp-hint">Nothing runs there until a person on {serverName} approves it.</p>
        </>
      ) : (
        <>
          <input value={summary} onChange={event => setSummary(event.target.value)} placeholder="What is ready" disabled={busy} />
          <textarea
            value={commitText}
            onChange={event => setCommitText(event.target.value)}
            placeholder={'repo@commit, one per line\nllm-bridge-server@98bd56d'}
            rows={3}
            disabled={busy}
          />
          {parsedCommits && parsedCommits.commits.length > 0 && (
            <p className="bp-hint">{peerMessageCommitList(parsedCommits.commits)}</p>
          )}
          <p className="bp-hint">{serverName} updates only when a person there approves it.</p>
        </>
      )}
      <label className="pm-send-files">
        Files{kind === 'patch' ? '' : ' (optional)'}:{' '}
        <input
          key={fileInputKey}
          type="file"
          multiple
          disabled={busy}
          onChange={event => setFiles(Array.from(event.target.files ?? []))}
        />
      </label>
      {files.length > 0 && (
        <p className="bp-hint">{files.map(file => `${file.name} (${formatAttachmentSize(file.size)})`).join(', ')}</p>
      )}
      <button
        type="submit"
        className="bc-ctrl-btn bc-ctrl-btn-active"
        disabled={
          busy ||
          (kind === 'request'
            ? !title.trim() || !prompt.trim()
            : kind === 'notice'
              ? !summary.trim() || !commitText.trim()
              : !repo.trim() || !baseCommit.trim() || files.length === 0)
        }
      >
        Send
      </button>
      {error !== null && <p className="bridge-error pm-error">{error}</p>}
    </form>
  )
}
