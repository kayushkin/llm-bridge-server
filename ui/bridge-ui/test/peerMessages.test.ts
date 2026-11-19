import { describe, expect, it } from 'vitest'
import {
  approvalMachineGroups,
  orchestratorPlacesApprovals,
  peerMessagePlacementText,
  approvalTargetDescription,
  approvalTargetWarning,
  formatAttachmentSize,
  isPeerMessageSignal,
  peerAttachmentCanRetry,
  peerAttachmentDownloadable,
  peerMessageAttachmentPath,
  peerMessageAttachmentRetryPath,
  peerMessageAttachmentUploadPath,
  peerMessagePatchBody,
  parsePeerMessageCommits,
  peerConnectionMessagesPath,
  peerMessageApprovePath,
  peerMessageDeclinePath,
  peerMessageHeadline,
  peerMessageNeedsDecision,
  peerMessageNoticeBody,
  peerMessageRefusalText,
  peerMessageRequestBody,
  peerMessageThreads,
  peerMessagesListPath,
  untrustedTextPieces,
  withoutPeerMessageSignals,
  type PeerMessage,
  type PeerMessageAttachment,
} from '../src/peerMessages'

const message = (over: Partial<PeerMessage>): PeerMessage => ({
  id: 'peer_message_000001',
  connection_id: 'peer_connection_000001',
  direction: 'incoming',
  kind: 'request',
  state: 'pending',
  body: {},
  created_at: '2026-10-29T10:00:00Z',
  updated_at: '2026-10-29T10:00:00Z',
  ...over,
})

describe('peer message paths', () => {
  it('escapes ids and adds only the filters given', () => {
    expect(peerConnectionMessagesPath('peer_connection_000001')).toBe('/peer-connections/peer_connection_000001/messages')
    expect(peerMessageApprovePath('a/b')).toBe('/peer-messages/a%2Fb/approve')
    expect(peerMessageDeclinePath('m1')).toBe('/peer-messages/m1/decline')
    expect(peerMessagesListPath()).toBe('/peer-messages')
    expect(peerMessagesListPath({ direction: 'incoming', state: 'pending' })).toBe('/peer-messages?direction=incoming&state=pending')
    expect(peerMessagesListPath({ connectionID: 'peer_connection_000001' })).toBe('/peer-messages?connection_id=peer_connection_000001')
  })
})

describe('which messages need a person here', () => {
  it('is an incoming request or notice that is still pending', () => {
    expect(peerMessageNeedsDecision(message({}))).toBe(true)
    expect(peerMessageNeedsDecision(message({ kind: 'notice' }))).toBe(true)
    expect(peerMessageNeedsDecision(message({ kind: 'status' }))).toBe(false)
    expect(peerMessageNeedsDecision(message({ direction: 'outgoing' }))).toBe(false)
    expect(peerMessageNeedsDecision(message({ state: 'approved' }))).toBe(false)
  })
})

describe('signals raised for peer messages', () => {
  it('are taken out of the attention group, request and count alike', () => {
    const peer = { id: 's1', source: 'peer_message' }
    const question = { id: 's2', source: 'hook' }
    const group = {
      requests: [{ requestId: 'r1', signals: [peer] }, { requestId: 'r2', signals: [question] }],
      signals: [peer, question],
    }
    const filtered = withoutPeerMessageSignals(group)
    expect(filtered.requests.map(r => r.requestId)).toEqual(['r2'])
    expect(filtered.signals).toEqual([question])
    expect(isPeerMessageSignal(peer)).toBe(true)
    expect(isPeerMessageSignal(question)).toBe(false)
  })
})

describe('commit rows for a notice', () => {
  it('reads repo@commit per line and skips blank lines', () => {
    expect(parsePeerMessageCommits('llm-bridge-server@98bd56d\n\n llm-bridge-codex@50b8e69 ')).toEqual({
      commits: [
        { repo: 'llm-bridge-server', commit: '98bd56d' },
        { repo: 'llm-bridge-codex', commit: '50b8e69' },
      ],
      error: null,
    })
  })

  it('names a line that is not repo@commit instead of guessing', () => {
    expect(parsePeerMessageCommits('llm-bridge-server').error).toContain('llm-bridge-server')
    expect(parsePeerMessageCommits('repo@xyz').error).toContain('repo@xyz')
    expect(parsePeerMessageCommits('@98bd56d').error).not.toBeNull()
  })
})

describe('request bodies', () => {
  it('trims what was typed and carries the message answered only when there is one', () => {
    expect(peerMessageRequestBody('  Fix it ', ' do the thing\n')).toEqual({ kind: 'request', body: { title: 'Fix it', prompt: 'do the thing' } })
    expect(peerMessageNoticeBody('Ready', [{ repo: 'r', commit: 'abcdef1' }], 'peer_message_000002')).toEqual({
      kind: 'notice',
      body: { summary: 'Ready', commits: [{ repo: 'r', commit: 'abcdef1' }] },
      in_reply_to: 'peer_message_000002',
    })
  })
})

describe('text another server wrote', () => {
  it('splits out web addresses as text, keeping everything else as it is', () => {
    expect(untrustedTextPieces('see https://example.test/x?y=1 now\nand ![img](https://example.test/p.png)')).toEqual([
      { kind: 'text', text: 'see ' },
      { kind: 'address', text: 'https://example.test/x?y=1' },
      { kind: 'text', text: ' now\nand ![img](' },
      { kind: 'address', text: 'https://example.test/p.png' },
      { kind: 'text', text: ')' },
    ])
    expect(untrustedTextPieces('https://example.test/wiki/A_(b)')).toEqual([{ kind: 'address', text: 'https://example.test/wiki/A_(b)' }])
    expect(untrustedTextPieces('<script>alert(1)</script>')).toEqual([{ kind: 'text', text: '<script>alert(1)</script>' }])
  })
})

describe('threads', () => {
  it('puts each answer under the message it answers, oldest first, newest thread first', () => {
    const ask = message({ id: 'a', created_at: '2026-10-29T10:00:00Z' })
    const done = message({ id: 'b', direction: 'outgoing', kind: 'notice', in_reply_to: 'a', created_at: '2026-10-29T12:00:00Z' })
    const updated = message({ id: 'c', kind: 'status', in_reply_to: 'b', created_at: '2026-10-29T13:00:00Z' })
    const other = message({ id: 'd', created_at: '2026-10-29T11:00:00Z' })
    const orphan = message({ id: 'e', in_reply_to: 'not-here', created_at: '2026-10-29T09:00:00Z' })
    const threads = peerMessageThreads([updated, other, ask, orphan, done])
    expect(threads.map(thread => thread.map(m => m.id))).toEqual([['a', 'b', 'c'], ['d'], ['e']])
  })
})

describe('headlines and refusals', () => {
  it('says who wrote it and what it is', () => {
    expect(peerMessageHeadline(message({ body: { title: 'Fix resume' } }), 'its-skayushkin')).toBe('its-skayushkin asks to start a session: Fix resume')
    expect(
      peerMessageHeadline(message({ direction: 'outgoing', kind: 'notice', body: { commits: [{ repo: 'r', commit: '0123456789abcdef' }] } }), 'x'),
    ).toBe('This server says this is ready: r@012345678')
  })

  it('has plain words only for refusals a person can act on', () => {
    expect(peerMessageRefusalText('peer_messages_not_granted')).toContain('leave')
    expect(peerMessageRefusalText('something_else')).toBeNull()
  })
})

const attachment = (over: Partial<PeerMessageAttachment>): PeerMessageAttachment => ({
  id: 'peer_attachment_000001',
  name: 'work.patch',
  media_type: 'text/x-patch',
  size: 2048,
  sha256: 'ab'.repeat(32),
  state: 'fetched',
  created_at: '2026-10-29T10:00:00Z',
  ...over,
})

describe('attachments', () => {
  it('builds the upload, download and retry paths, escaping names and ids', () => {
    expect(peerMessageAttachmentUploadPath('my work & notes.patch')).toBe('/peer-message-attachments?name=my+work+%26+notes.patch')
    expect(peerMessageAttachmentUploadPath('fix.patch', 'text/x-patch')).toBe('/peer-message-attachments?name=fix.patch&media_type=text%2Fx-patch')
    expect(peerMessageAttachmentUploadPath('fix.patch', '')).toBe('/peer-message-attachments?name=fix.patch')
    expect(peerMessageAttachmentPath('peer_message_000001', 'a/b')).toBe('/peer-messages/peer_message_000001/attachments/a%2Fb')
    expect(peerMessageAttachmentRetryPath('m', 'a')).toBe('/peer-messages/m/attachments/a/retry')
  })

  it('offers a download only once the bytes are here and checked, and a retry only for an incoming file that is not', () => {
    expect(peerAttachmentDownloadable(attachment({ state: 'fetched' }))).toBe(true)
    expect(peerAttachmentDownloadable(attachment({ state: 'stored' }))).toBe(true)
    expect(peerAttachmentDownloadable(attachment({ state: 'pending' }))).toBe(false)
    expect(peerAttachmentCanRetry(message({}), attachment({ state: 'failed' }))).toBe(true)
    expect(peerAttachmentCanRetry(message({}), attachment({ state: 'fetched' }))).toBe(false)
    expect(peerAttachmentCanRetry(message({ direction: 'outgoing' }), attachment({ state: 'failed' }))).toBe(false)
  })

  it('reads sizes the way a person does', () => {
    expect(formatAttachmentSize(512)).toBe('512 B')
    expect(formatAttachmentSize(2048)).toBe('2.0 KB')
    expect(formatAttachmentSize(105570)).toBe('103 KB')
    expect(formatAttachmentSize(52428800)).toBe('50 MB')
  })
})

describe('patches', () => {
  it('carries the repo, the base commit, the uploads and a summary only when one was typed', () => {
    expect(peerMessagePatchBody(' llm-bridge-server ', ' 3cd2cec ', '  ', ['peer_attachment_000002'])).toEqual({
      kind: 'patch',
      body: { repo: 'llm-bridge-server', base_commit: '3cd2cec' },
      attachment_ids: ['peer_attachment_000002'],
    })
    expect(peerMessagePatchBody('r', 'c', 'Add history import', ['a'], 'peer_message_000003').body.summary).toBe('Add history import')
  })

  it('names a patch by the repo it changes', () => {
    expect(peerMessageHeadline(message({ kind: 'patch', body: { repo: 'llm-bridge-codex', base_commit: 'x' } }), 'its-skayushkin')).toBe(
      'its-skayushkin sends a patch for llm-bridge-codex',
    )
  })

  it('carries attachments on a request or notice only when there are some', () => {
    expect(peerMessageRequestBody('t', 'p').attachment_ids).toBeUndefined()
    expect(peerMessageRequestBody('t', 'p', undefined, ['a']).attachment_ids).toEqual(['a'])
    expect(peerMessageNoticeBody('s', [], 'm', []).attachment_ids).toBeUndefined()
  })
})

describe('the approval picker says which machine an instance runs on', () => {
  const machine = (id: string, name: string, transport: string) => ({ id, name, transport, computer_use_allowed: false, created_at: '', updated_at: '' })
  const here = machine('m_1', 'server', 'local')
  const mediaServer = machine('m_2', 'media-server', 'runner')
  const instance = (id: string, name: string, onMachine: ReturnType<typeof machine> | undefined, enabled = true) => ({
    id, name, harness_type: 'claude_code', machine_id: onMachine?.id ?? '', enabled, created_at: '', updated_at: '', machine: onMachine,
  })
  const instances = [
    instance('inst_3', 'media-server/claude_code', mediaServer),
    instance('inst_2', 'Codex', here),
    instance('inst_1', 'Claude', here),
    instance('inst_4', 'Off', here, false),
    instance('inst_5', 'Orphan', undefined),
  ]

  it('groups by machine, this server first, and leaves out disabled instances', () => {
    const groups = approvalMachineGroups(instances)
    expect(groups.map(group => [group.machineName, group.isThisServer, group.instances.map(i => i.id)])).toEqual([
      ['server', true, ['inst_1', 'inst_2']],
      ['machine not reported', false, ['inst_5']],
      ['media-server', false, ['inst_3']],
    ])
  })

  it('says where the approval runs, and warns only for a patch on another machine', () => {
    const [server, unreported, media] = approvalMachineGroups(instances)
    expect(approvalTargetDescription(server)).toBe('Runs on server, this server.')
    expect(approvalTargetDescription(media)).toBe('Runs on media-server, another machine, not this server.')
    expect(approvalTargetDescription(unreported)).toMatch(/did not say/)
    expect(approvalTargetWarning('patch', server)).toBeNull()
    expect(approvalTargetWarning('request', media)).toBeNull()
    expect(approvalTargetWarning('patch', media)).toMatch(/media-server, which does not hold them/)
  })
})

describe('the orchestrator places approved work', () => {
  const settingsWith = (value: string) => ({
    service: 'llm-bridge-server',
    settings: [{ key: 'orchestrator.url', value, environment_variable: 'LLMBRIDGE_ORCHESTRATOR_URL', kind: 'behavior', value_type: 'text', description: '', source: 'stored' }],
  }) as unknown as Parameters<typeof orchestratorPlacesApprovals>[0]

  it('reads the server’s own orchestrator.url', () => {
    expect(orchestratorPlacesApprovals(settingsWith('http://localhost:9999'))).toBe(true)
    expect(orchestratorPlacesApprovals(settingsWith('  '))).toBe(false)
    expect(orchestratorPlacesApprovals({ service: 'llm-bridge-server', settings: [] } as unknown as Parameters<typeof orchestratorPlacesApprovals>[0])).toBe(false)
  })

  const handed = {
    id: 'peer_message_000002', connection_id: 'peer_connection_000001', direction: 'incoming', kind: 'patch',
    body: { repo: 'kanban-store' }, state: 'approved', created_at: '', updated_at: '',
    placement_session_id: 'br_1',
  } as unknown as PeerMessage

  it('says what an approved message waits on, and where it went', () => {
    expect(peerMessagePlacementText({ ...handed, placement_session_id: undefined })).toBeNull()
    expect(peerMessagePlacementText(handed)).toBe('Waiting for the orchestrator (br_1) to choose a machine.')
    expect(peerMessagePlacementText({ ...handed, placed_by: 'session:br_1', placement_reason: 'a patch lands here' }))
      .toBe('Placed by the orchestrator: a patch lands here')
    expect(peerMessagePlacementText({ ...handed, placed_by: 'principal_000001', placement_reason: 'by hand' }))
      .toBe('Placed by principal_000001: by hand')
  })
})
