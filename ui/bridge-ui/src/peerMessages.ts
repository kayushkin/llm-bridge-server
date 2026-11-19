// Messages between connected bridge servers: what one server asks of the
// other, and what each tells the other when it is done.
//
// Four kinds, each carried over the connection and kept by both servers:
//   - `request`: the other server asks this one to start a session. Its title
//     and prompt are what that session would be told. Nothing runs until a
//     person here approves it, choosing an instance (or handing it to the
//     orchestrator); the session then starts with the prompt and a note of
//     where it came from.
//   - `notice`: the other server says something is ready, naming repos and
//     commits, usually "update to these". Updating waits for approval here.
//   - `status`: what came of a request or a notice, threaded to it.
//   - `patch`: a change to one repo as `git format-patch` files, made against a
//     base commit. Approving it starts a session here that lands it.
//
// Any kind may carry files. They are never part of the message's text: the
// sender uploads each one (`POST /peer-message-attachments`) and the message
// names them; the receiving server fetches the bytes over the connection and
// keeps them only when their size and SHA-256 match. A file is downloaded,
// never drawn as a page.
//
// ⚠️ A message is text another machine wrote. It is drawn as plain text —
// never as markdown, never with an image or link that loads by itself — and an
// approved request's prompt reaches the new session marked as a request from
// that server, never as the operator's own words.
//
// The routes are llm-bridge-server's (`POST /peer-connections/{id}/messages`,
// `GET /peer-messages`, `POST /peer-messages/{id}/approve` and `/decline`),
// and the types are its own, generated into `@kayushkin/llm-bridge-types`
// (`msg.PeerMessage*`). A message's text is in its `body`, whose fields
// depend on the kind: `title` and `prompt` for a request, `summary` and
// `commits` for a notice, `result`, `detail` and `versions` for a status,
// `repo`, `base_commit` and `summary` for a patch, whose verified files the
// receiving server reads into `patch` (each commit's subject, each file's
// insertions and deletions).

import {
  PeerMessageAttachmentFailed,
  PeerMessageAttachmentPending,
  PeerMessageDirectionIncoming,
  PeerMessageKindNotice,
  PeerMessageKindPatch,
  PeerMessageKindRequest,
  PeerMessageKindStatus,
  PeerMessageStatePending,
  SignalSourcePeerMessage,
  TransportLocal,
  type Instance,
  type PeerMessage,
  type PeerMessageAttachment,
  type ServiceSettings,
  type PeerMessageApproveRequest,
  type PeerMessageCreateRequest,
  type PeerMessageDirection,
  type PeerRepoCommit,
} from '@kayushkin/llm-bridge-types'

export type { PeerMessage, PeerMessageAttachment, PeerMessageApproveRequest, PeerMessageCreateRequest, PeerRepoCommit }

export const PEER_MESSAGES_PATH = '/peer-messages'

/** The signal source the server raises for an incoming message. Such a
 *  signal is drawn by the message's own card, not as a question. */
export const PEER_MESSAGE_SIGNAL_SOURCE = SignalSourcePeerMessage

export function peerConnectionMessagesPath(connectionID: string): string {
  return `/peer-connections/${encodeURIComponent(connectionID)}/messages`
}

export function peerMessagesListPath(filter: { connectionID?: string; direction?: PeerMessageDirection; state?: string } = {}): string {
  const query = new URLSearchParams()
  if (filter.connectionID) query.set('connection_id', filter.connectionID)
  if (filter.direction) query.set('direction', filter.direction)
  if (filter.state) query.set('state', filter.state)
  const encoded = query.toString()
  return encoded ? `${PEER_MESSAGES_PATH}?${encoded}` : PEER_MESSAGES_PATH
}

export function peerMessageApprovePath(messageID: string): string {
  return `${PEER_MESSAGES_PATH}/${encodeURIComponent(messageID)}/approve`
}

export function peerMessageDeclinePath(messageID: string): string {
  return `${PEER_MESSAGES_PATH}/${encodeURIComponent(messageID)}/decline`
}

/** Where a file is uploaded before the message that sends it names it. The
 *  browser's own media type for it goes along when it knows one; the server
 *  records application/octet-stream otherwise. */
export function peerMessageAttachmentUploadPath(name: string, mediaType?: string): string {
  const query: Record<string, string> = { name }
  if (mediaType) query.media_type = mediaType
  return `/peer-message-attachments?${new URLSearchParams(query).toString()}`
}

export function peerMessageAttachmentPath(messageID: string, attachmentID: string): string {
  return `${PEER_MESSAGES_PATH}/${encodeURIComponent(messageID)}/attachments/${encodeURIComponent(attachmentID)}`
}

export function peerMessageAttachmentRetryPath(messageID: string, attachmentID: string): string {
  return `${peerMessageAttachmentPath(messageID, attachmentID)}/retry`
}

/** Whether a person can ask this server to fetch an incoming file again now. */
export function peerAttachmentCanRetry(message: PeerMessage, attachment: PeerMessageAttachment): boolean {
  return message.direction === PeerMessageDirectionIncoming &&
    (attachment.state === PeerMessageAttachmentFailed || attachment.state === PeerMessageAttachmentPending)
}

/** Whether a file's bytes are here and verified, so it can be downloaded. */
export function peerAttachmentDownloadable(attachment: PeerMessageAttachment): boolean {
  return attachment.state === 'stored' || attachment.state === 'fetched'
}

/** A size in bytes, as a person reads it. */
export function formatAttachmentSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`
}

/** Whether an incoming message is waiting on a person here. */
export function peerMessageNeedsDecision(message: PeerMessage): boolean {
  return message.direction === PeerMessageDirectionIncoming && message.state === PeerMessageStatePending && message.kind !== PeerMessageKindStatus
}

/** Whether a signal is one the server raised for a waiting peer message, which
 *  its card draws instead. */
export function isPeerMessageSignal(signal: { source: string }): boolean {
  return signal.source === PEER_MESSAGE_SIGNAL_SOURCE
}

/** An attention group without the signals raised for peer messages, which
 *  their own cards draw: a request whose every signal is one is dropped, and
 *  the group's count no longer includes them. */
export function withoutPeerMessageSignals<
  S extends { source: string },
  G extends { requests: readonly { signals: readonly S[] }[]; signals: readonly S[] },
>(group: G): G {
  return {
    ...group,
    requests: group.requests.filter(request => !request.signals.every(isPeerMessageSignal)),
    signals: group.signals.filter(signal => !isPeerMessageSignal(signal)),
  }
}

/** A `repo@commit` per line, as typed; blank lines are skipped. A line that is
 *  not `repo@commit` is returned as an error naming it, never guessed at. */
export function parsePeerMessageCommits(text: string): { commits: PeerRepoCommit[]; error: string | null } {
  const commits: PeerRepoCommit[] = []
  for (const raw of text.split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const at = line.lastIndexOf('@')
    const repo = at > 0 ? line.slice(0, at).trim() : ''
    const commit = at > 0 ? line.slice(at + 1).trim() : ''
    if (!repo || !/^[0-9a-f]{7,40}$/i.test(commit)) {
      return { commits: [], error: `“${line}” is not repo@commit, with a commit of at least seven hex characters` }
    }
    commits.push({ repo, commit })
  }
  return { commits, error: null }
}

function withReplyAndFiles(request: PeerMessageCreateRequest, inReplyTo?: string, attachmentIDs?: readonly string[]): PeerMessageCreateRequest {
  if (inReplyTo) request.in_reply_to = inReplyTo
  if (attachmentIDs && attachmentIDs.length > 0) request.attachment_ids = [...attachmentIDs]
  return request
}

export function peerMessageRequestBody(title: string, prompt: string, inReplyTo?: string, attachmentIDs?: readonly string[]): PeerMessageCreateRequest {
  return withReplyAndFiles({ kind: PeerMessageKindRequest, body: { title: title.trim(), prompt: prompt.trim() } }, inReplyTo, attachmentIDs)
}

export function peerMessageNoticeBody(summary: string, commits: PeerRepoCommit[], inReplyTo?: string, attachmentIDs?: readonly string[]): PeerMessageCreateRequest {
  return withReplyAndFiles({ kind: PeerMessageKindNotice, body: { summary: summary.trim(), commits } }, inReplyTo, attachmentIDs)
}

/** A patch: the repo it changes, the commit it was made against, and its
 *  `git format-patch` files, which must already be uploaded. */
export function peerMessagePatchBody(repo: string, baseCommit: string, summary: string, attachmentIDs: readonly string[], inReplyTo?: string): PeerMessageCreateRequest {
  const body: PeerMessageCreateRequest['body'] = { repo: repo.trim(), base_commit: baseCommit.trim() }
  if (summary.trim()) body.summary = summary.trim()
  return withReplyAndFiles({ kind: PeerMessageKindPatch, body }, inReplyTo, attachmentIDs)
}

/** One piece of a message's text: words as they are, or a web address shown
 *  as text the reader may copy. Nothing in a message is fetched or opened by
 *  drawing it. */
export type UntrustedTextPiece = { kind: 'text'; text: string } | { kind: 'address'; text: string }

const WEB_ADDRESS = /\bhttps?:\/\/[^\s<>"']+/g

/** A message's text, split so the card can draw addresses distinctly without
 *  making them load anything. */
export function untrustedTextPieces(text: string): UntrustedTextPiece[] {
  const pieces: UntrustedTextPiece[] = []
  let last = 0
  for (const match of text.matchAll(WEB_ADDRESS)) {
    const start = match.index ?? 0
    // A closing bracket with no opening one in the address belongs to the
    // text around it, as in markdown's `(https://…)`.
    let address = match[0]
    while (address.endsWith(')') && (address.match(/\(/g)?.length ?? 0) < (address.match(/\)/g)?.length ?? 0)) {
      address = address.slice(0, -1)
    }
    if (start > last) pieces.push({ kind: 'text', text: text.slice(last, start) })
    pieces.push({ kind: 'address', text: address })
    last = start + address.length
  }
  if (last < text.length) pieces.push({ kind: 'text', text: text.slice(last) })
  return pieces
}

/** A connection's messages as threads: each message that answers none starts
 *  one, and its answers follow it oldest first. An answer whose first message
 *  this list does not hold starts its own thread. Threads come newest first. */
export function peerMessageThreads(messages: readonly PeerMessage[]): PeerMessage[][] {
  const byID = new Map(messages.map(message => [message.id, message]))
  const rootOf = (message: PeerMessage): PeerMessage => {
    let current = message
    const seen = new Set<string>()
    while (current.in_reply_to && byID.has(current.in_reply_to) && !seen.has(current.id)) {
      seen.add(current.id)
      current = byID.get(current.in_reply_to)!
    }
    return current
  }
  const threads = new Map<string, PeerMessage[]>()
  for (const message of messages) {
    const root = rootOf(message)
    const thread = threads.get(root.id) ?? []
    thread.push(message)
    threads.set(root.id, thread)
  }
  const byTime = (a: PeerMessage, b: PeerMessage) => a.created_at.localeCompare(b.created_at)
  return [...threads.values()]
    .map(thread => [...thread].sort(byTime))
    .sort((a, b) => b[b.length - 1].created_at.localeCompare(a[a.length - 1].created_at))
}

/** What a message is, in a few words, for a thread row. */
export function peerMessageHeadline(message: PeerMessage, serverName: string): string {
  const from = message.direction === PeerMessageDirectionIncoming ? serverName : 'This server'
  switch (message.kind) {
    case PeerMessageKindRequest:
      return `${from} asks to start a session: ${message.body.title || '(untitled)'}`
    case PeerMessageKindNotice:
      return `${from} says this is ready: ${message.body.summary || peerMessageCommitList(message.body.commits)}`
    case PeerMessageKindStatus:
      return `${from}: ${message.body.result || message.state}`
    case PeerMessageKindPatch:
      return `${from} sends a patch for ${message.body.repo || '(no repo)'}`
    default:
      return `${from}: ${message.kind}`
  }
}

export function peerMessageCommitList(commits: readonly PeerRepoCommit[] | undefined): string {
  return (commits ?? []).map(c => `${c.repo}@${c.commit.slice(0, 9)}`).join(', ')
}

const PEER_MESSAGE_REFUSALS: Readonly<Record<string, string>> = {
  peer_messages_not_granted:
    'The other server has not given this one leave to send it messages. Its administrator gives that on its own Peers page.',
  peer_message_not_pending: 'Someone has already decided this message.',
  peer_not_connected: 'The other server is not connected right now. The message waits here and goes when it reconnects.',
  self_update_not_configured: 'This server has no update command set (self_update.command on its settings), so it cannot take an update.',
  peer_message_not_found: 'This server has no such message.',
  unknown_instance: 'That instance is not one of this server’s.',
  orchestrator_refused: 'The orchestrator did not take it (it may be over its weekly limit), so the message waits here again.',
  orchestrator_unreachable: 'The orchestrator did not answer, so the message waits here again.',
  orchestrator_places_sessions: 'This server hands approved work to its orchestrator, which chooses the machine. Reload and approve again.',
  patch_needs_this_server: 'A patch lands only on this server, where its files and repos are. Choose an instance on this server.',
  peer_attachments_not_fetched: 'Its files have not all arrived and been checked here yet. Approve it once they have; a file that failed can be fetched again.',
  attachment_too_large: 'A file is larger than this server takes (peer_messages.attachment_maximum_bytes on its settings).',
  empty_attachment: 'A file is empty.',
  patch_needs_attachments: 'A patch needs at least one git format-patch file attached.',
  peer_attachment_not_fetched: 'That file has not arrived and been checked here yet.',
}

/** Plain words for a refusal a person can act on, else nothing (the caller
 *  shows the server's own message). */
export function peerMessageRefusalText(code: string): string | null {
  return PEER_MESSAGE_REFUSALS[code] ?? null
}

/** Whether a server hands approved requests and patches to its orchestrator,
 *  which chooses the machine, read from the server's own `orchestrator.url`
 *  setting. Then the approval card has no instance picker; a server with
 *  none keeps the picker, since nothing else can start the session. */
export function orchestratorPlacesApprovals(settings: ServiceSettings): boolean {
  return (settings.settings.find(setting => setting.key === 'orchestrator.url')?.value.trim() ?? '') !== ''
}

/** What an approved request or patch handed to the orchestrator is waiting
 *  on, or where it was placed and why, for the card; nothing for a message
 *  that was never handed over. */
export function peerMessagePlacementText(message: PeerMessage): string | null {
  if (!message.placement_session_id) return null
  if (!message.placed_by) {
    return message.state === 'approved' ? `Waiting for the orchestrator (${message.placement_session_id}) to choose a machine.` : null
  }
  const by = message.placed_by === `session:${message.placement_session_id}` ? 'the orchestrator' : message.placed_by
  return `Placed by ${by}: ${message.placement_reason ?? ''}`.trim()
}

/** One machine's instances in the approval picker. `machineID` is empty, and
 *  `machineName` says so, for an instance whose answer carried no machine. */
export interface ApprovalMachineGroup {
  machineID: string
  machineName: string
  /** The machine this server runs on (transport `local`), where its repos are. */
  isThisServer: boolean
  instances: Instance[]
}

/** The enabled instances a request or patch may be approved onto, grouped by
 *  the machine each runs on: this server's own machine first, then the others
 *  by name. On 2026-11-03 two patches were approved onto
 *  `sscitadel/claude_code`, listed beside this server's `Clawd` with nothing
 *  to say it was another machine, and landed nothing. */
export function approvalMachineGroups(instances: readonly Instance[]): ApprovalMachineGroup[] {
  const groups = new Map<string, ApprovalMachineGroup>()
  for (const instance of instances) {
    if (!instance.enabled) continue
    const machine = instance.machine
    const key = machine?.id ?? ''
    let group = groups.get(key)
    if (!group) {
      group = {
        machineID: key,
        machineName: machine?.name ?? 'machine not reported',
        isThisServer: machine?.transport === TransportLocal,
        instances: [],
      }
      groups.set(key, group)
    }
    group.instances.push(instance)
  }
  const ordered = [...groups.values()]
  for (const group of ordered) group.instances.sort((a, b) => a.name.localeCompare(b.name))
  ordered.sort((a, b) => Number(b.isThisServer) - Number(a.isThisServer) || a.machineName.localeCompare(b.machineName))
  return ordered
}

/** Where an approval would run, said in words under the picker. */
export function approvalTargetDescription(group: ApprovalMachineGroup): string {
  if (group.isThisServer) return `Runs on ${group.machineName}, this server.`
  if (!group.machineID) return 'The server did not say which machine this instance runs on.'
  return `Runs on ${group.machineName}, another machine, not this server.`
}

/** The warning shown when a patch is about to be approved onto a machine
 *  other than this server's: a patch lands in this server's repos, which a
 *  session elsewhere does not have. Null when there is nothing to warn of.
 *  Approving stays possible: the person decides. */
export function approvalTargetWarning(kind: string, group: ApprovalMachineGroup): string | null {
  if (kind !== PeerMessageKindPatch || group.isThisServer) return null
  return `This patch lands in ${group.machineID ? "this server's" : 'its'} repos, but the session would run on ${group.machineName}, which does not hold them. Choose an instance on this server unless you know the repo is there.`
}
