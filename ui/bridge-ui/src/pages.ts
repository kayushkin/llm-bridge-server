import type { ComponentType, ReactNode } from 'react'
import type { BridgeConfig, BridgeRoutes } from './context'
import { BridgeChat } from './components/chat/BridgeChat'
import { BridgeSignals } from './components/BridgeSignals'
import { BridgeInstances } from './components/BridgeInstances'
import { BridgeSessions } from './components/BridgeSessions'
import { BridgeAuth } from './components/BridgeAuth'
import { BridgePeers } from './components/BridgePeers'
import { BridgePeerConnection } from './components/BridgePeerConnection'
import { BridgeUsage } from './components/BridgeUsage'
import { BridgeMessageConversations } from './components/messages/BridgeMessageConversations'
import { BridgeMessagesWaiting } from './components/messages/BridgeMessagesWaiting'
import { AwaitingReplyCount } from './components/messages/AwaitingReplyCount'
import { BridgeMessageSearch } from './components/messages/BridgeMessageSearch'
import { BridgeMessageContacts } from './components/messages/BridgeMessageContacts'
import { BridgeSettings } from './components/BridgeSettings'
import { BridgeAgents } from './components/BridgeAgents'
import { BridgeFiles } from './components/BridgeFiles'
import { BridgeSkills } from './components/BridgeSkills'
import { BridgeTools } from './components/BridgeTools'
import { BridgePermissions } from './components/BridgePermissions'
import { BridgeKanban } from './components/BridgeKanban'
import { BridgeKanbanSettings } from './components/BridgeKanbanSettings'
import { BridgeEmailTickets } from './components/BridgeEmailTickets'
import { BridgeCardPage } from './components/BridgeCardPage'
import { BridgeReferencePage } from './components/BridgeReferencePage'
import { BridgePrincipals } from './components/BridgePrincipals'
import { BridgeGrants } from './components/BridgeGrants'
import { BridgeBundles } from './components/BridgeBundles'
import { BridgeServiceInventory } from './components/BridgeServiceInventory'
import { BridgeMachines } from './components/machines/BridgeMachines'
import { BridgeOrchestrator } from './components/BridgeOrchestrator'
import { BridgeConformance } from './components/BridgeConformance'
import { BridgeEffectiveConfig } from './components/BridgeEffectiveConfig'
import { BridgeServiceSettings } from './components/BridgeServiceSettings'
import { BridgeModels } from './components/BridgeModels'
import { BridgeHooks } from './components/BridgeHooks'
import { BridgePreviews } from './components/BridgePreviews'
import { BridgeThreeDimensionalModels } from './components/BridgeThreeDimensionalModels'
import { BridgeDiscord } from './components/BridgeDiscord'
import { BridgeEmoji } from './components/messages/BridgeEmoji'
import { BridgeAutoReactions } from './components/messages/BridgeAutoReactions'

/** The navigation groups, in the order they are drawn. A group is drawn only
 *  when at least one of its pages is available on this host.
 *
 *  Pages are grouped by what the person is doing, not by which service answers:
 *  - Work: the sessions and cards being worked, and the chat that drives them.
 *  - Messages: the operator's own conversations with people, through multichat's
 *    bridges — reading, answering, searching, and the contacts and their tags.
 *  - Agents: what runs — instances and machines, agent identities, the prompt,
 *    and the skills, tools and bundles they are given.
 *  - Access: who may do what — principals, grants, permission rules, credentials.
 *  - System: the host itself — settings, usage, its services, conformance.
 *  - Personal: pages the host brings that are about the operator, not the agents. */
export type PageGroupKey = 'work' | 'messages' | 'agents' | 'access' | 'system' | 'personal'

export interface PageGroup {
  key: PageGroupKey
  label: string
}

export const PAGE_GROUPS: readonly PageGroup[] = [
  { key: 'work', label: 'Work' },
  { key: 'messages', label: 'Messages' },
  { key: 'agents', label: 'Agents' },
  { key: 'access', label: 'Access' },
  { key: 'system', label: 'System' },
  { key: 'personal', label: 'Personal' },
]

/** The shell switches a host can set. */
export interface ShellFlags {
  showConformance: boolean
  showServiceInventory: boolean
  /** Only these of this library's pages are routed and listed. Absent means
   *  every page. A host gives a person who is not an administrator the pages
   *  their own principal can use, such as the chat and their sessions. */
  onlyBridgePages?: readonly (keyof BridgeRoutes)[]
}

/** This library's pages a host shows: all of them, or only those named. */
export function bridgePagesShown(onlyBridgePages?: readonly (keyof BridgeRoutes)[]): readonly BridgePage[] {
  if (!onlyBridgePages) return BRIDGE_PAGES
  return BRIDGE_PAGES.filter(p => onlyBridgePages.includes(p.route))
}

/** One page this library ships. `routes[route]` is where it is mounted. */
export interface BridgePage {
  route: keyof BridgeRoutes
  label: string
  group: PageGroupKey
  /** Drawn in the navigation. A sub-page — a card, a board's settings — is
   *  routed but not listed; the page it belongs to links to it. */
  listed: boolean
  /** NavLink `end`. The chat lives at the root and would otherwise be active on
   *  every path. */
  end?: boolean
  /** Appended to the route for the <Route> pattern, e.g. '/:cardId'. */
  routeSuffix?: string
  /** Whether this host can show the page at all. Absent means always. A page
   *  that is not available is still routed, so a stale link lands on a page
   *  that says what is missing rather than on nothing. */
  available?: (config: BridgeConfig, flags: ShellFlags) => boolean
  component: ComponentType
  /** Drawn inside the page's tab, after its label: a live count. */
  badge?: ComponentType
}

export const BRIDGE_PAGES: readonly BridgePage[] = [
  // Work
  { route: 'chat', label: 'Chat', group: 'work', listed: true, end: true, component: BridgeChat },
  // Routed, not listed: the chat sidebar's orchestrator row opens it.
  { route: 'signals', label: 'Signals', group: 'work', listed: false, component: BridgeSignals },
  { route: 'sessions', label: 'Sessions', group: 'work', listed: true, component: BridgeSessions },
  { route: 'kanban', label: 'Kanban', group: 'work', listed: true, available: c => !!c.kanbanStoreBasePath, component: BridgeKanban },
  { route: 'kanbanSettings', label: 'Board settings', group: 'work', listed: false, component: BridgeKanbanSettings },
  { route: 'card', label: 'Card', group: 'work', listed: false, routeSuffix: '/:cardId', component: BridgeCardPage },
  { route: 'reference', label: 'Record', group: 'work', listed: false, routeSuffix: '/:refId', available: c => !!c.referenceStoreBasePath, component: BridgeReferencePage },
  // Each email ticket beside the message it came from. Needs mail as well as
  // kanban, since the log is half emails.
  { route: 'emailTickets', label: 'Email tickets', group: 'work', listed: true, available: c => !!c.kanbanStoreBasePath && !!c.mailBasePath, component: BridgeEmailTickets },
  // Routed, not listed: the gear on the chat sidebar's orchestrator row links to it.
  { route: 'orchestrator', label: 'Orchestrator', group: 'work', listed: false, available: c => !!c.orchestratorBasePath, component: BridgeOrchestrator },
  // The dev servers agents are running. Only the host can see a machine's
  // listening ports, so a host that lists none gets no page.
  { route: 'previews', label: 'Previews', group: 'work', listed: true, available: c => !!c.previewsBasePath, component: BridgePreviews },
  // The 3D models agents are making, found by the host from their tool calls
  // and the folders they work in.
  { route: 'threeDimensionalModels', label: '3D models', group: 'work', listed: true, available: c => !!c.threeDimensionalModelsBasePath, component: BridgeThreeDimensionalModels },
  // Waiting on you comes first, so the Messages tab opens it: who wrote and
  // is waiting on a reply.
  { route: 'messageWaiting', label: 'Waiting on you', group: 'messages', listed: true, available: c => !!c.multichatBasePath, component: BridgeMessagesWaiting, badge: AwaitingReplyCount },
  // Messages — multichat's inbox. Conversations owns every path under
  // /messages that no sub-page claims: `/messages/kat` opens the chat
  // short-name-store calls messages/kat. React Router prefers a sub-page's
  // literal route to the splat, and `activeNavEntry` lights the longer tab.
  { route: 'messageConversations', label: 'Conversations', group: 'messages', listed: true, routeSuffix: '/*', available: c => !!c.multichatBasePath, component: BridgeMessageConversations },
  { route: 'messageSearch', label: 'Search', group: 'messages', listed: true, available: c => !!c.multichatBasePath, component: BridgeMessageSearch },
  { route: 'messageContacts', label: 'Contacts', group: 'messages', listed: true, available: c => !!c.multichatBasePath, component: BridgeMessageContacts },
  // The Discord message log, deleted messages with their text, and the bridge's
  // status. Read-only: GET /message-log and GET /discord/status.
  { route: 'discord', label: 'Discord', group: 'messages', listed: true, available: c => !!c.multichatBasePath, component: BridgeDiscord },
  // The emoji pickers' favorites and settings, and every bridged server's
  // custom emoji: GET /emoji and its writes.
  { route: 'emoji', label: 'Emoji', group: 'messages', listed: true, available: c => !!c.multichatBasePath, component: BridgeEmoji },
  // An emoji on everything one person sends: from the operator's own account
  // (multichat's rules), and from the Event-Manager Discord bot
  // (discord-signup-store's, shown when the host proxies it).
  { route: 'autoReactions', label: 'Auto-reactions', group: 'messages', listed: true, available: c => !!c.multichatBasePath, component: BridgeAutoReactions },
  // Agents
  { route: 'instances', label: 'Instances', group: 'agents', listed: true, component: BridgeInstances },
  { route: 'agents', label: 'Agents', group: 'agents', listed: true, component: BridgeAgents },
  { route: 'files', label: 'Files', group: 'agents', listed: true, component: BridgeFiles },
  { route: 'skills', label: 'Skills', group: 'agents', listed: true, available: c => !!c.skillStoreBasePath, component: BridgeSkills },
  { route: 'tools', label: 'Tools', group: 'agents', listed: true, available: c => !!c.toolStoreBasePath, component: BridgeTools },
  { route: 'bundles', label: 'Bundles', group: 'agents', listed: true, available: c => !!c.bundleStoreBasePath, component: BridgeBundles },
  { route: 'hooks', label: 'Hooks', group: 'agents', listed: true, component: BridgeHooks },
  // Access
  // The directory the Kanban assignees resolve against. Same gate as the
  // assignee UI itself: a host that proxies no principal-store gets no page.
  { route: 'principals', label: 'Principals', group: 'access', listed: true, available: c => !!c.principalStoreBasePath, component: BridgePrincipals },
  // Who may use what. Its own page, because "who holds a grant on this tool"
  // is a question about a resource, not a principal.
  { route: 'grants', label: 'Grants', group: 'access', listed: true, available: c => !!c.grantStoreBasePath, component: BridgeGrants },
  { route: 'permissions', label: 'Permissions', group: 'access', listed: true, available: c => !!c.permissionStoreBasePath, component: BridgePermissions },
  { route: 'auth', label: 'Auth', group: 'access', listed: true, component: BridgeAuth },
  // Other llm-bridge-servers: what this one may do there and they may do
  // here. Reads only the bridge server, so no store path gates it.
  { route: 'peers', label: 'Peers', group: 'access', listed: true, component: BridgePeers },
  { route: 'peerConnection', label: 'Connected server', group: 'access', listed: false, routeSuffix: '/:connectionId', component: BridgePeerConnection },
  // System
  { route: 'settings', label: 'Settings', group: 'system', listed: true, component: BridgeSettings },
  { route: 'usage', label: 'Usage', group: 'system', listed: true, component: BridgeUsage },
  { route: 'effectiveConfig', label: 'Effective config', group: 'system', listed: true, component: BridgeEffectiveConfig },
  { route: 'serviceSettings', label: 'Service settings', group: 'system', listed: true, component: BridgeServiceSettings },
  // Which model each background call uses: every model_role service setting,
  // and model-store's roles. The roles need model-store, so a host that does
  // not proxy it gets no tab.
  { route: 'models', label: 'Models', group: 'system', listed: true, available: c => !!c.modelStoreBasePath, component: BridgeModels },
  // Reads the bridge server itself (`GET /services` on basePath), so no store
  // base path gates it; a host whose server lacks the route turns it off.
  { route: 'serviceInventory', label: 'Service inventory', group: 'system', listed: true, available: (_, f) => f.showServiceInventory, component: BridgeServiceInventory },
  // Every machine's health on this server and each connected one. Reads the
  // bridge server itself, so no store path gates it; a server built before
  // machine stats says so on the page.
  { route: 'machines', label: 'Machines', group: 'system', listed: true, component: BridgeMachines },
  { route: 'conformance', label: 'Conformance', group: 'system', listed: true, available: (_, f) => f.showConformance, component: BridgeConformance },
]

/** A page the host brings into the same shell and navigation. `path` is
 *  absolute from the host's root, where `<Bridge>` is mounted. */
export interface HostPage {
  path: string
  label: string
  group: PageGroupKey
  element: ReactNode
  end?: boolean
  /** Drawn in the navigation; absent means listed. A host's sub-page — one
   *  day, one person's life — is routed but not listed, and lights up its
   *  group through `groupForPath` all the same. */
  listed?: boolean
  /** Appended to `path` for the <Route> pattern, e.g. '/*' or '/:date'. */
  routeSuffix?: string
}

/** One entry of the navigation, whichever side it came from. */
export interface NavEntry {
  to: string
  label: string
  group: PageGroupKey
  end: boolean
  badge?: ComponentType
}

/** The navigation for this host: every listed page that is available, in
 *  registry order, then the host's pages in the order given. */
export function navEntriesFor(config: BridgeConfig, flags: ShellFlags, hostPages: readonly HostPage[] = []): NavEntry[] {
  const own: NavEntry[] = bridgePagesShown(flags.onlyBridgePages)
    .filter(p => p.listed && (p.available ? p.available(config, flags) : true))
    .map(p => ({ to: config.routes[p.route], label: p.label, group: p.group, end: p.end ?? false, ...(p.badge ? { badge: p.badge } : {}) }))
  const host: NavEntry[] = hostPages.filter(p => p.listed !== false).map(p => ({ to: p.path, label: p.label, group: p.group, end: p.end ?? false }))
  return [...own, ...host]
}

/** Whether an entry's path covers this one: exactly, for an `end` entry,
 *  else as itself or a prefix ending at a segment boundary. */
function navEntryCovers(entry: { to: string; end: boolean }, pathname: string): boolean {
  if (!entry.to) return false
  if (entry.end) return pathname === entry.to
  return pathname === entry.to || pathname.startsWith(entry.to.endsWith('/') ? entry.to : entry.to + '/')
}

/** The tab to light in a row: of the entries whose path covers this one, the
 *  longest, so `/messages/search` lights Search and `/messages/kat` lights
 *  Conversations. Null when none covers it. */
export function activeNavEntry<T extends { to: string; end: boolean }>(pathname: string, entries: readonly T[]): T | null {
  let best: T | null = null
  for (const entry of entries) {
    if (navEntryCovers(entry, pathname) && (!best || entry.to.length > best.to.length)) best = entry
  }
  return best
}

/** Which group the current path belongs to: the group of the page whose route
 *  is the longest prefix of the path, listed or not, so a card page lights up
 *  Work even though no tab points at it. Null when nothing matches. */
export function groupForPath(pathname: string, routes: BridgeRoutes, hostPages: readonly HostPage[] = []): PageGroupKey | null {
  const candidates: { to: string; group: PageGroupKey; end: boolean }[] = [
    ...BRIDGE_PAGES.map(p => ({ to: routes[p.route], group: p.group, end: p.end ?? false })),
    ...hostPages.map(p => ({ to: p.path, group: p.group, end: p.end ?? false })),
  ]
  return activeNavEntry(pathname, candidates)?.group ?? null
}
