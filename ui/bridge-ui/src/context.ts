import type { ResolvedRefMatch } from '@kayushkin/chat-core'
import { createContext, useContext } from 'react'
import type { ReactNode } from 'react'
import type { FetchFn } from './types'

/** What a host's chip is given: the id as written and the one record
 *  reference-store found for it. */
export interface ReferenceChipRendererProps {
  refId: string
  match: ResolvedRefMatch
  className?: string
}

export type ReferenceChipRenderer = (props: ReferenceChipRendererProps) => ReactNode | null

export interface BridgeRoutes {
  // Where each page this library ships is mounted. `<Bridge>` owns its host's
  // root and routes these itself, so the defaults are the truth for every host
  // that mounts it; a host composing pages by hand under a prefix overrides them.
  chat: string
  /** Every open signal across sessions, on a page of its own. */
  signals: string
  instances: string
  sessions: string
  auth: string
  /** Other llm-bridge-servers: the connections this server has, the access
   *  given and held each way, and the passphrases that let others connect. */
  peers: string
  /** One connected server's sessions, through the connection's proxy.
   *  Mounted at `peerConnection/:connectionId`. */
  peerConnection: string
  usage: string
  settings: string
  agents: string
  files: string
  skills: string
  tools: string
  permissions: string
  conformance: string
  kanban: string
  /** The per-board settings page (defaults, classifier, priority ladder,
   *  business hours). `?board=<id>` picks the board; without it, the board the
   *  kanban page last opened. */
  kanbanSettings: string
  principals: string
  grants: string
  bundles: string
  /** The host's services, their databases, and a read-only look inside.
   *  Not `/services`: dash owns that path for its topology page and matches
   *  it before the bridge's splat, so the bridge page there was unreachable. */
  serviceInventory: string
  /** The health of every machine this server and each connected server
   *  knows: CPU, memory, drives, batteries, uptime, with history. Backed by
   *  llm-bridge-server's `/machines/stats` routes. */
  machines: string
  /** Hooks: the shell commands the bridge wires into a harness's hook
   *  mechanism at spawn, by scope. llm-bridge-server's /hooks routes. */
  hooks: string
  /** What a session is given, setting by setting, with the layer that decided
   *  each — for a stored session (`?session=`) or a dry run (`?harness=…`,
   *  `?board_id=…`). Backed by llm-bridge-server's effective-config routes. */
  effectiveConfig: string
  /** Every backend the host proxies, describing its own configuration: what
   *  each setting is, the value in force and what decided it. Backed by each
   *  service's `GET /settings`. */
  serviceSettings: string
  /** Which models the background calls use: every service setting that names a
   *  model-store role, and each role's ordered model list, editable. Needs
   *  `modelStoreBasePath`. */
  models: string
  /** The orchestrator's full review surface (WAL, prior versions, filters), linked
   *  from the sidebar's Orchestrator row and the in-chat orchestrator pane. */
  orchestrator: string
  /** The single-card page. The card id is appended as a path segment. */
  card: string
  /** The generic record page, `/ref/<id>`: any record reference-store can
   *  look up, for a chip whose type has no page of its own. */
  reference: string
  /** Every ticket that came from an email, and the email it came from. */
  emailTickets: string
  /** multichat's conversations, one open to read and answer (`?room=<room_id>`).
   *  Needs `multichatBasePath`. Its own path is a prefix of the other two
   *  Messages pages, so its tab is `end`. */
  messageConversations: string
  messageWaiting: string
  /** Search across the messages of every multichat conversation (`?q=`). */
  messageSearch: string
  /** multichat's contacts, merged across apps, and the tags on them. */
  messageContacts: string
  /** The dev servers agents are running, each shown in a frame. `?port=<n>`
   *  opens the one listening on that port. Needs `previewsBasePath`. */
  previews: string
  /** The 3D models agents are making and have made, with the renders they
   *  opened. `?session=<id>` or `?repository=<folder>`, and `&model=<path>`.
   *  Needs `threeDimensionalModelsBasePath`. */
  threeDimensionalModels: string
  /** Discord through multichat: the message log with deleted messages kept,
   *  and the bridge's status. `?tab=deleted|status`. Needs `multichatBasePath`. */
  discord: string
  /** The emoji pickers' settings: favorites (the quick-react bar under each
   *  message), skin tone, which servers' custom emoji a Discord room is
   *  offered, and every bridged server's custom emoji with a refresh from
   *  Discord. Needs `multichatBasePath`. */
  emoji: string
  /** Auto-reactions: an emoji on every message one person sends, from the
   *  operator's own account and from the Event-Manager Discord bot, both
   *  through multichat. Needs `multichatBasePath`. */
  autoReactions: string

  // Pages the HOST owns and this library does not provide. No sensible default
  // exists, so it is empty, and a link to an empty route is not rendered at all —
  // the library never guesses a path for a page it doesn't ship.
  /** The host's notes page, for `[todo:<id>]` references. Empty means none. */
  notes: string
}

export const DEFAULT_BRIDGE_ROUTES: BridgeRoutes = {
  chat: '/',
  signals: '/signals',
  instances: '/instances',
  sessions: '/sessions',
  auth: '/auth',
  peers: '/peers',
  peerConnection: '/peers',
  usage: '/usage',
  settings: '/settings',
  agents: '/agents',
  files: '/files',
  skills: '/skills',
  tools: '/tools',
  permissions: '/permissions',
  conformance: '/conformance',
  kanban: '/kanban',
  kanbanSettings: '/kanban/settings',
  principals: '/principals',
  grants: '/grants',
  bundles: '/bundles',
  serviceInventory: '/service-inventory',
  machines: '/machines',
  effectiveConfig: '/effective-config',
  serviceSettings: '/service-settings',
  models: '/models',
  hooks: '/hooks',
  orchestrator: '/orchestrator',
  card: '/card',
  reference: '/ref',
  emailTickets: '/email-tickets',
  messageConversations: '/messages',
  messageWaiting: '/messages/waiting',
  messageSearch: '/messages/search',
  messageContacts: '/messages/contacts',
  previews: '/previews',
  threeDimensionalModels: '/3d-models',
  discord: '/messages/discord',
  emoji: '/messages/emoji',
  autoReactions: '/messages/auto-reactions',
  notes: '',
}

export interface BridgeConfig {
  /** Auth'd fetch function — consumers provide their own (e.g. with cookies or bearer tokens). */
  fetch: FetchFn
  /** Base path for bridge API (e.g. "/api/bridge"). No trailing slash. */
  basePath: string
  /** Base path for skill-store API (e.g. "/api/skill-store"). No trailing
   * slash. If empty, the Skills tab is hidden. */
  skillStoreBasePath: string
  /** Base path for tool-store API (e.g. "/api/tool-store"). No trailing
   * slash. If empty, the Tools tab is hidden. */
  toolStoreBasePath: string
  /** Base path for permission-store API (e.g. "/api/permission-store"). No
   * trailing slash. If empty, the Permissions tab is hidden. */
  permissionStoreBasePath: string
  /** Base path for kanban-store API (e.g. "/api/kanban"). No trailing slash.
   * If empty, the Kanban tab is hidden. */
  kanbanStoreBasePath: string
  /** Base path for principal-store API (e.g. "/api/principals"). No trailing
   * slash. If empty, the Principals tab, assignee chips and the assignee
   * editor are hidden — the same convention as `kanbanStoreBasePath` hiding
   * the Kanban tab. */
  principalStoreBasePath: string
  /** Base path for grant-store API (e.g. "/api/grants"). No trailing slash.
   * If empty, the Grants tab is hidden, a principal shows no grants section,
   * and the kanban "Runs on" picker has no assignee lists to put first. */
  grantStoreBasePath: string
  /** Base path for bundle-store API (e.g. "/api/bundle-store"). No trailing
   * slash. If empty, the Bundles tab is hidden. */
  bundleStoreBasePath: string
  /** Base path for repo-store API (e.g. "/api/repo-store"). No trailing slash.
   * The Bundles page reads its repos to preview a resolution for one. If
   * empty, the page lists bundles and says there are no repos to preview. */
  repoStoreBasePath: string
  /** Base path for the noteboard API (e.g. "/api/noteboard"). No trailing
   * slash. Used by chat reference chips to resolve a todo/item id to its
   * title/status. If empty, todo chips render but say lookup isn't configured. */
  noteboardBasePath: string
  /** reference-store through the bridge (dash: "/api/bridge/reference-store"):
   * the one list of reference types and lookups for the signed-in person.
   * The generic chip and the record page (`routes.reference`) need it. */
  referenceStoreBasePath: string
  /** Base path for llm-bridge-adapter API (e.g. "/api/llm-bridge-adapter"). No
   * trailing slash. Used to resolve bus_session_id → bridge_id when a kanban
   * card is linked by entity_type=bus_session. If empty, those cards' chat
   * deeplinks are disabled. */
  bridgeAdapterBasePath: string
  /** Base path for the orchestrator API (e.g. "/api/orchestrator"). No
   * trailing slash. Used by the sidebar's Orchestrator row and the in-chat
   * orchestrator-context pane. If empty, both say the orchestrator isn't
   * configured instead of guessing a path. */
  orchestratorBasePath: string
  /** Base path for usage-store API (e.g. "/api/usage"). No trailing slash.
   * If empty, the spend/limits sections of the Usage tab are hidden and only
   * per-session aggregates from llm-bridge-server are shown. */
  usageStoreBasePath: string
  /** Base path for the mailstack API, as the host proxies it. If omitted, the
   * card drawer cannot read a linked email and hides those controls — offering a button that 404s is worse than
   * offering none. */
  mailBasePath: string
  /** Base path for multichat's API as the host proxies it (dash:
   * "/api/multichat", which forwards only the routes it names). The Messages
   * pages call the routes `MULTICHAT_ROUTES_CALLED` in `src/multichatMessages.ts`
   * lists, including a send that reaches a real person, and the Discord page
   * those in `DISCORD_ROUTES_CALLED` in `src/discordLog.ts`. If empty, the
   * Messages group is hidden. */
  multichatBasePath: string
  /** The host's route that invites a people-store person to its library and
   *  answers a one-time sign-in link (dash: "/api/kayushkin-viewer/invitations",
   *  kayushkin.com's `POST /api/viewer/invitations {person_id}` → `{url,
   *  expires_at, display_name}`). A one-to-one conversation then posts the link
   *  to the person. If empty, no Invite button is drawn. */
  libraryInvitationsPath: string
  /** Base paths the host proxies seven more services at, read only by the
   * service settings page, which asks `{base}/settings`. dash: scheduler
   * "/api/scheduler", log-store "/api/log-store", job-store "/api/jobs",
   * quote-store "/api/quotes", prediction-store "/api/predictions",
   * event-store "/api/events", auth-store "/api/authstore". If one is empty,
   * the page does not list that service. */
  schedulerBasePath: string
  logStoreBasePath: string
  jobStoreBasePath: string
  quoteStoreBasePath: string
  predictionStoreBasePath: string
  eventStoreBasePath: string
  authStoreBasePath: string
  /** The HOST's list of ports agents' processes listen on, each with the
   * public port that shows it (dash: "/api/previews"). If empty, the Previews
   * page is hidden and Bash calls show no View button. */
  previewsBasePath: string
  /** The HOST's list of .glb files agents are making and the renders they
   * opened, with `/file?path=` serving each one it listed (dash:
   * "/api/3d-models"). If empty, the 3D models page is hidden. */
  threeDimensionalModelsBasePath: string
  /** Base path for work-graph-store as the host proxies it (dash:
   * "/api/work-graph"): which agent session moved which git ref, drawn as
   * each repo's commit graph by the chat's `?view=work-graph`. If empty, the
   * view says the store is not configured and nothing links to it. */
  workGraphStoreBasePath: string
  /** Base path for project-store as the host proxies it (dash:
   * "/api/projects"): what each project is for and what it owns, filed
   * sessions included. Read by the chat's `?view=projects`, the sidebar's
   * "Group by project" toggle and the session header's filing control. If
   * empty, none of those is drawn. */
  projectStoreBasePath: string
  /** Base path for model-store as the host proxies it (dash:
   * "/api/model-store"; model-store roots its API at /api, so the page asks
   * `{base}/api/roles` and `{base}/api/models`). Read by the Models page and by
   * the service settings page's drop-down for a `model_role` setting. If
   * empty, the Models page is hidden and a `model_role` setting says the roles
   * cannot be listed. */
  modelStoreBasePath: string
  /** Base path of the HOST's own routes, read only by the service settings
   * page, which asks `{base}/settings` — the host describes its own settings
   * there like any backend (dash: "/api/dash"). The host is not proxied under a
   * base path, so it needs a field of its own. If empty, the page does not list
   * the host. */
  hostBasePath: string
  /** Path to the HOST's own mail page, which is not one of this library's
   * exported pages — hence a plain path rather than an entry in BridgeRoutes,
   * whose contract is that every route has a component here. Empty hides the
   * "open in Mail" deep link while leaving the inline preview available. */
  mailPagePath: string
  /** Base path for people-store as the host proxies it (dash: "/api/people"):
   * the people in the operator's own life, `person_000001`. Read by the
   * service settings page. A person id in prose resolves through the host's
   * resolver, not through this. Empty means the host does not carry it. */
  peopleStoreBasePath: string
  /** Base path for journal-store as the host proxies it (dash:
   * "/api/journal"): the operator's own writing, `entry_000001`. Read by the
   * service settings page. Empty means the host does not carry it. */
  journalStoreBasePath: string
  /** Base path for photo-store as the host proxies it (dash: "/api/photos"):
   * the operator's own photos, `photo_000001`. A photo chip draws its
   * thumbnail from `{base}/photos/{id}/thumbnail`. Empty means the host does
   * not carry it, and a photo chip shows no picture. */
  photoStoreBasePath: string
  /** Base path for message-store as the host proxies it (dash:
   * "/api/message-store"), read only by `/service-settings`. The Messages
   * pages reach message-store's records through multichat instead
   * (`multichatBasePath` + `/message-store`), so the host's front end calls
   * multichat alone for messages. */
  messageStoreBasePath: string
  /** The base path fields whose service answers `{base}/settings`, which the
   * Service settings and Models pages ask. Null means every base path the host
   * set: dash proxies each service whole, so each answers there. A host that
   * mounts stores without their settings route names the ones that have it —
   * llm-bridge-server serves its stores in process, and only its own
   * `/settings` and the scheduler's `/scheduler/settings` exist, so asking the
   * rest only adds a 404 for each. */
  serviceSettingsSourceKeys: readonly (keyof BridgeConfig)[] | null
  /** Optional render hook called per harness in the Settings tab. Hosts can
   * use this to inject harness-specific configuration UI keyed on harness
   * name. Return null for harnesses without an extension. */
  renderHarnessExtension: ((harnessName: string) => ReactNode) | null
  /** The host's chips, for records of the host's own stores (a journal
   * entry, a photo, a possession…): called with each record a chip resolved
   * to, before this library's own chips. Return null to let this library draw
   * it (its own chip for the bridge's records, or the generic chip). It is
   * never called on a connected server's records, which this host's chips
   * would look up in its own stores with another server's ids. */
  renderReferenceChip: ReferenceChipRenderer | null
  /** Route paths for navigation between bridge pages. */
  routes: BridgeRoutes
}

export const BridgeContext = createContext<BridgeConfig | null>(null)

export function useBridgeConfig(): BridgeConfig {
  const ctx = useContext(BridgeContext)
  if (!ctx) throw new Error('useBridgeConfig: wrap your component tree in <BridgeProvider>')
  return ctx
}
