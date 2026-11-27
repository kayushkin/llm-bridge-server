// Peers: other llm-bridge-servers this one is connected to, and what each
// lets the other do.
//
// A connection says only how two servers reach each other. An administrator
// on a server with an https address makes a one-time passphrase; the other
// server's administrator enters that address and passphrase. If the entering
// server gives its own https address too, both call each other directly;
// otherwise it dials out and keeps a socket open, and requests both ways
// travel over it. A connection gives no access. Each server then grants the
// other a level on itself, and revokes it, on its own. llm-bridge-server's
// `peers.go` and `peer_connection_proxy.go` are the contract; design note
// d107154a-49ac-471b-a1b7-fc7975de37f3 is the why.
//
// The pure rules the Peers page and a connected server's page draw from.

import {
  PeerAccessLevelGroup,
  PeerTransportDialOut,
  PeerTransportDirect,
  type PeerAccessLevel,
  type PeerConnectRequest,
  type PeerConnection,
  type PeerConnectionInvite,
  type PeerConnectionInviteRequest,
  type PeerConnectionView,
  type PeerGrantRequest,
  type PeerReasonRequest,
  type PeerUserMapping,
  type PeerUserMappingRequest,
  type ServiceSettings,
} from '@kayushkin/llm-bridge-types'
import type { Principal } from '@kayushkin/principal-store-types'
import type { BridgeConfig, BridgeRoutes } from './context'

export const PEER_CONNECTION_INVITES_PATH = '/peer-connection-invites'
export const PEER_CONNECTIONS_PATH = '/peer-connections'
export const PEER_ACCESS_LEVELS_PATH = '/peer-access-levels'
export const PEER_USER_MAPPINGS_PATH = '/peer-user-mappings'

export function peerConnectionPath(connectionID: string): string {
  return `${PEER_CONNECTIONS_PATH}/${encodeURIComponent(connectionID)}`
}

export function peerConnectionRemovePath(connectionID: string): string {
  return `${peerConnectionPath(connectionID)}/remove`
}

export function peerConnectionGrantPath(connectionID: string): string {
  return `${peerConnectionPath(connectionID)}/grant`
}

export function peerConnectionRevokeGrantPath(connectionID: string): string {
  return `${peerConnectionPath(connectionID)}/revoke-grant`
}

export function peerConnectionCheckPath(connectionID: string): string {
  return `${peerConnectionPath(connectionID)}/check`
}

export function peerAccessLevelProvisionPath(levelKey: string): string {
  return `${PEER_ACCESS_LEVELS_PATH}/${encodeURIComponent(levelKey)}/provision`
}

/** The people of one connected server mapped to principals here, by the
 *  principal this server made for that server when it gave it access. */
export function peerUserMappingsListPath(peerPrincipalID: string): string {
  return `${PEER_USER_MAPPINGS_PATH}?peer_principal_id=${encodeURIComponent(peerPrincipalID)}`
}

export function peerUserMappingRemovePath(mappingID: string): string {
  return `${PEER_USER_MAPPINGS_PATH}/${encodeURIComponent(mappingID)}/remove`
}

/** The mapping form as typed, trimmed. The server checks that each id names
 *  a principal and says why when one does not, so nothing is checked here. */
export function peerUserMappingRequestBody(
  peerPrincipalID: string, remotePrincipalID: string, localPrincipalID: string,
): PeerUserMappingRequest {
  return {
    peer_principal_id: peerPrincipalID.trim(),
    remote_principal_id: remotePrincipalID.trim(),
    local_principal_id: localPrincipalID.trim(),
  }
}

export function peerUserMappingIsRemoved(mapping: PeerUserMapping): boolean {
  return !!mapping.removed_at
}

/** The principals here a person from another server may be mapped to: those
 *  principal-store says act as a caller (it refuses the others with
 *  `local_principal_cannot_act`), and are not disabled. */
export function peerUserMappingLocalChoices(principals: readonly Principal[]): Principal[] {
  return principals.filter(p => p.acts_as_caller && !p.disabled_at)
}

/** Who removed a connection, in words. The other server removing it and
 *  saying so over the connection leaves no administrator here. */
export function peerConnectionRemovedBy(connection: PeerConnection): string {
  if (connection.removed_by_other_server) return 'the other server removed it'
  return connection.removed_by ? `removed by ${connection.removed_by}` : 'removed'
}

/** Where the connected server answers: every route of it, under this server's
 *  proxy for the connection, which picks direct or the socket itself. */
export function peerConnectionProxyBasePath(basePath: string, connectionID: string): string {
  return `${basePath}${peerConnectionPath(connectionID)}/proxy`
}

/** How long a connection passphrase may wait to be used. These are the
 *  choices offered; the server keeps its own limit and says so past it. */
export const PEER_CONNECTION_INVITE_LIFETIMES: readonly { label: string; seconds: number }[] = [
  { label: '1 hour', seconds: 60 * 60 },
  { label: '1 day', seconds: 24 * 60 * 60 },
  { label: '7 days', seconds: 7 * 24 * 60 * 60 },
]

export function peerConnectionInviteRequestBody(expiresInSeconds: number): PeerConnectionInviteRequest {
  return { expires_in_seconds: expiresInSeconds }
}

export interface PeerConnectForm {
  baseURL: string
  passphrase: string
  displayName: string
  nameForThisServer: string
  thisServerBaseURL: string
}

export const EMPTY_PEER_CONNECT_FORM: PeerConnectForm = {
  baseURL: '', passphrase: '', displayName: '', nameForThisServer: '', thisServerBaseURL: '',
}

/** The connect form as typed, trimmed. An empty address for this server is
 *  left out, which is what makes the connection dial out. The server checks
 *  both addresses are https and says why when one is not, so nothing is
 *  checked here. */
export function peerConnectRequestBody(form: PeerConnectForm): PeerConnectRequest {
  const thisServerBaseURL = form.thisServerBaseURL.trim()
  return {
    base_url: form.baseURL.trim(),
    passphrase: form.passphrase.trim(),
    display_name: form.displayName.trim(),
    name_for_this_server: form.nameForThisServer.trim(),
    ...(thisServerBaseURL ? { this_server_base_url: thisServerBaseURL } : {}),
  }
}

/** A grant names a level key; only the `group` level names a group, and a
 *  group named with any other level would be ignored, so it is not sent. */
export function peerGrantRequestBody(levelKey: string, groupPrincipalID: string): PeerGrantRequest {
  return levelKey === PeerAccessLevelGroup
    ? { level: levelKey, group_principal_id: groupPrincipalID.trim() }
    : { level: levelKey }
}

export function peerReasonRequestBody(reason: string): PeerReasonRequest {
  return { reason: reason.trim() }
}

/** A level can be granted once it needs no group, or once its group exists. */
export function peerAccessLevelIsReady(level: PeerAccessLevel): boolean {
  return !level.needs_group || !!level.group_principal_id
}

/** The level's name as the server serves it, or the key when this server no
 *  longer offers that level. */
export function peerAccessLevelName(levels: readonly PeerAccessLevel[] | null, key: string): string {
  return levels?.find(level => level.key === key)?.display_name ?? key
}

/** What the page says about how two servers reach each other. The transport
 *  values come from llm-bridge's constants, not from a copy. */
export function peerTransportDescription(connection: PeerConnection): string {
  if (connection.transport === PeerTransportDirect) return 'direct: each server calls the other at its address'
  if (connection.transport === PeerTransportDialOut) {
    return connection.remote_base_url
      ? 'dial-out: this server keeps a socket open to the other'
      : 'dial-out: the other server keeps a socket open to this one'
  }
  return connection.transport
}

export type PeerConnectionReach = 'removed' | 'direct' | 'socket open' | 'not answering' | 'socket closed'

/** Whether requests can travel now. A direct connection has no socket; a
 *  dial-out one works only while its socket is open and the bridge behind it
 *  answers: the server probes that, since a socket can stay open, its pings
 *  answered, while every request sent over it hangs. */
export function peerConnectionReach(view: PeerConnectionView): PeerConnectionReach {
  if (view.connection.removed_at) return 'removed'
  if (view.connection.transport === PeerTransportDirect) return 'direct'
  if (!view.socket_connected) return 'socket closed'
  return view.answering === false ? 'not answering' : 'socket open'
}

/** The reach in plain words, with since when a server stopped answering. */
export function peerConnectionReachText(view: PeerConnectionView): string {
  const reach = peerConnectionReach(view)
  if (reach !== 'not answering') return reach
  return view.not_answering_since
    ? `connected, not answering since ${new Date(view.not_answering_since).toLocaleString()}`
    : 'connected, not answering'
}

export function peerConnectionIsRemoved(view: PeerConnectionView): boolean {
  return !!view.connection.removed_at
}

/** The live rows first, each part newest first, so what still works is at
 *  the top and what was taken away stays visible below it. */
export function liveFirstNewestFirst<T extends { created_at: string }>(rows: readonly T[], isLive: (row: T) => boolean): T[] {
  return [...rows].sort((a, b) => {
    const liveOrder = Number(isLive(b)) - Number(isLive(a))
    if (liveOrder !== 0) return liveOrder
    return new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
  })
}

export type PeerConnectionInviteState = 'waiting' | 'used' | 'expired'

export function peerConnectionInviteState(invite: PeerConnectionInvite, now: Date): PeerConnectionInviteState {
  if (invite.redeemed_at) return 'used'
  return new Date(invite.expires_at).getTime() <= now.getTime() ? 'expired' : 'waiting'
}

/** This server's own https address, from its `peer_connections.public_url` setting, which the
 *  server's `/settings` serves. Empty when it is not set: the page then says
 *  so rather than guess an address from the browser's location. */
export function thisServerPublicURL(settings: ServiceSettings | null): string {
  return settings?.settings.find(setting => setting.key === 'peer_connections.public_url')?.value.trim() ?? ''
}

/** Plain words for the refusals a person can act on, keyed by the code the
 *  server answers with. The server's own message follows, unchanged. */
const PEER_REFUSAL_EXPLANATIONS: Readonly<Record<string, string>> = {
  peer_access_not_delivered:
    'The other server did not take the access, so it was revoked here again. Check that the connection is up and try once more.',
  demo_login_enabled:
    'This server still has demo login on, which lets anyone who reaches it sign in as anyone, so it takes no call from another server. Switch demo login off first.',
  peer_access_not_held:
    'The other server has not given this one any access yet. Its administrator grants a level on its own Peers page.',
  peer_not_connected:
    'The other server is not connected now: its socket to this one is closed. It reconnects by itself when it is running and online.',
  peer_not_answering:
    'The other server is connected but not answering: its socket is open, yet its bridge has stopped answering requests. Nothing reaches it until it answers again; this server keeps checking.',
  mapping_incomplete:
    'A mapping needs all three ids: the connected server, its person, and the principal here they act as.',
  unknown_peer_principal:
    'This server has no principal for that connected server any more. Give it access again, then map its people.',
  unknown_local_principal:
    'No principal here has that id. Pick one from the list.',
  local_principal_cannot_act:
    'That principal cannot make requests (a group or a contact), so nobody can act as it. Pick a person or a server.',
  peer_user_mapping_exists:
    'That person from the connected server is already mapped. Remove the old mapping first to change it.',
  peer_user_mapping_not_found:
    'This server has no such mapping any more. Reload the page.',
  peer_user_not_mapped:
    'That person on the connected server is not mapped to anyone here, so this server refuses what they ask. Map them on this page.',
}

/** The error text of a refused peer route: llm-bridge-server answers
 *  `{"error":{"code","message"}}`, and the message says what to do. A code a
 *  person can act on gets its plain explanation first. */
export async function peerRouteErrorText(res: Response, verb: string): Promise<string> {
  const text = await res.text().catch(() => '')
  if (!text) return `${verb}: HTTP ${res.status}`
  try {
    const parsed = JSON.parse(text) as { error?: { code?: unknown; message?: unknown } | string }
    if (typeof parsed.error === 'string') return `${verb}: ${parsed.error}`
    if (parsed.error && typeof parsed.error.message === 'string') {
      const code = typeof parsed.error.code === 'string' ? parsed.error.code : ''
      const explanation = code ? PEER_REFUSAL_EXPLANATIONS[code] : undefined
      const serverWords = `${parsed.error.message}${code ? ` (${code})` : ''}`
      return explanation ? `${verb}: ${explanation} The server said: ${serverWords}` : `${verb}: ${serverWords}`
    }
  } catch {
    // Not JSON; the raw text is the message.
  }
  return `${verb}: ${text}`
}

/** Where a connected server's session reads each of `BridgeConfig`'s paths.
 *
 *  - `bridge`: llm-bridge-server's own routes, through the connection's proxy.
 *    The other server runs llm-bridge-server, so the session header, the
 *    composer, the panes and their controls ask it exactly what they ask this
 *    one.
 *  - `grant-store-gateway`: grant-store through the other server's
 *    `/grants/` gateway, which forwards `<rest>` to grant-store's `/<rest>`
 *    — the same shape as this host's grant-store path, so every call the
 *    library makes lands on the same route there.
 *  - `kanban-store-gateway`: kanban-store through the other server's
 *    `/issues/` gateway, which forwards `<rest>` to kanban-store's
 *    `/<rest>` unchanged, as this host's kanban path does. (Its `/kanban/`
 *    gateway adds `/api/` itself, which this library adds too.)
 *  - `reference-store-gateway`: reference-store through the other server's
 *    `/references/` gateway, so its chips are looked up by the server
 *    that owns them, as the person mapped there.
 *  - `not-there`: a path to one of THIS host's own stores or pages, or one whose
 *    shape the other server's gateways do not match. Left empty, which is the
 *    library's "not configured here": the feature behind it is hidden. Ids are
 *    numbered per server, so pointing a connected server's session at a store
 *    here would answer about a different record as if it were the same one.
 *
 *  A `Record` over every string field, so a path added to `BridgeConfig` does
 *  not compile until someone decides here where it goes. */
type BridgeConfigPathField = {
  [K in keyof BridgeConfig]: BridgeConfig[K] extends string ? K : never
}[keyof BridgeConfig]

export type ConnectedServerPathSource = 'bridge' | 'grant-store-gateway' | 'kanban-store-gateway' | 'reference-store-gateway' | 'not-there'

/** Where each gateway source sits under the connection's proxy. */
const GATEWAY_PATH_UNDER_PROXY: Readonly<Record<'grant-store-gateway' | 'kanban-store-gateway' | 'reference-store-gateway', string>> = {
  'grant-store-gateway': '/grants',
  'kanban-store-gateway': '/issues',
  'reference-store-gateway': '/references',
}

export const CONNECTED_SERVER_PATH_SOURCES: Readonly<Record<BridgeConfigPathField, ConnectedServerPathSource>> = {
  basePath: 'bridge',
  grantsBasePath: 'grant-store-gateway',
  issuesBasePath: 'kanban-store-gateway',
  // The other server's own reference-store, which looks its ids up under its
  // own access rules.
  referencesBasePath: 'reference-store-gateway',
  // llm-bridge-server has no principal-store gateway.
  principalsBasePath: 'not-there',
  // The other server's mailstack gateway, if it has a mailstack, is not this
  // host's mail page's shape; mail belongs to this host.
  mailBasePath: 'not-there',
  mailPagePath: 'not-there',
  // Everything below is one of this host's own proxies or pages.
  skillsBasePath: 'not-there',
  toolsBasePath: 'not-there',
  permissionsBasePath: 'not-there',
  bundlesBasePath: 'not-there',
  reposBasePath: 'not-there',
  noteboardBasePath: 'not-there',
  bridgeAdapterBasePath: 'not-there',
  orchestratorBasePath: 'not-there',
  usageBasePath: 'not-there',
  multichatBasePath: 'not-there',
  libraryInvitationsPath: 'not-there',
  schedulerBasePath: 'not-there',
  sessionLogBasePath: 'not-there',
  jobStoreBasePath: 'not-there',
  quoteStoreBasePath: 'not-there',
  predictionsBasePath: 'not-there',
  eventStoreBasePath: 'not-there',
  authStoreBasePath: 'not-there',
  previewsBasePath: 'not-there',
  threeDimensionalModelsBasePath: 'not-there',
  gitActivityBasePath: 'not-there',
  projectsBasePath: 'not-there',
  modelsBasePath: 'not-there',
  hostBasePath: 'not-there',
  peopleBasePath: 'not-there',
  journalBasePath: 'not-there',
  photosBasePath: 'not-there',
  messagesBasePath: 'not-there',
}

/** This host's pages that take one of this host's ids (`?session=`, a card,
 *  a port, a note). A link to one from a connected server's session would open
 *  this host's record with the other server's id, so those routes are emptied,
 *  and an empty route is not drawn as a link. Pages that take no id stay. */
const ROUTES_THAT_TAKE_THIS_HOSTS_IDS: readonly (keyof BridgeRoutes)[] = [
  'effectiveConfig',
  'card',
  'reference',
  'previews',
  'threeDimensionalModels',
  'orchestrator',
  'notes',
  'kanban',
  'kanbanSettings',
  'emailTickets',
]

/** The config a connected server's session runs with, path by path from
 *  `CONNECTED_SERVER_PATH_SOURCES`. */
export function peerConnectionBridgeConfig(config: BridgeConfig, connectionID: string): BridgeConfig {
  const proxy = peerConnectionProxyBasePath(config.basePath, connectionID)
  const paths = Object.fromEntries(
    (Object.entries(CONNECTED_SERVER_PATH_SOURCES) as [BridgeConfigPathField, ConnectedServerPathSource][]).map(
      ([field, source]) => [field, source === 'bridge' ? proxy : source === 'not-there' ? '' : `${proxy}${GATEWAY_PATH_UNDER_PROXY[source]}`],
    ),
  ) as Pick<BridgeConfig, BridgeConfigPathField>
  const routes = { ...config.routes }
  for (const route of ROUTES_THAT_TAKE_THIS_HOSTS_IDS) routes[route] = ''
  return {
    ...config,
    ...paths,
    routes,
    renderHarnessExtension: null,
    // This host's chips read this host's stores; a connected server's
    // records are drawn by this library alone.
    renderReferenceChip: null,
  }
}
