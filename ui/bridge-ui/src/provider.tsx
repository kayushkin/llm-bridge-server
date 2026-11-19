import type { ReactNode } from 'react'
import { useMemo } from 'react'
import { BridgeContext, DEFAULT_BRIDGE_ROUTES, type BridgeConfig, type BridgeRoutes } from './context'
import type { ReferenceChipRenderer } from './context'
import type { FetchFn } from './types'
import { MinimalChromeProvider } from './components/minimal/MinimalChromeContext'

export interface BridgeProviderProps {
  /** Auth'd fetch function */
  fetch: FetchFn
  /** Base path for bridge API (default: "/api/bridge") */
  basePath?: string
  /** Base path for skill-store API. If omitted, the Skills tab is hidden. */
  skillStoreBasePath?: string
  /** Base path for tool-store API. If omitted, the Tools tab is hidden. */
  toolStoreBasePath?: string
  /** Base path for permission-store API. If omitted, the Permissions tab is hidden. */
  permissionStoreBasePath?: string
  /** Base path for kanban-store API. If omitted, the Kanban tab is hidden. */
  kanbanStoreBasePath?: string
  /** Base path for principal-store API. If omitted, kanban cards show no
   * assignee chips and the card drawer has no assignee editor. */
  principalStoreBasePath?: string
  /** Base path for grant-store API. If omitted, the Grants tab is hidden, a
   * principal shows no grants section, and the kanban "Runs on" picker has no
   * assignee lists to put first. */
  grantStoreBasePath?: string
  /** Base path for bundle-store API. If omitted, the Bundles tab is hidden. */
  bundleStoreBasePath?: string
  /** Base path for repo-store API. If omitted, the Bundles page lists bundles
   * but has no repos to preview a resolution for. */
  repoStoreBasePath?: string
  /** Base path for the noteboard API. If omitted, chat todo chips can't resolve
   * an item's title/status and say so. */
  noteboardBasePath?: string
  /** reference-store through the bridge, e.g. "/api/bridge/reference-store". */
  referenceStoreBasePath?: string
  /** Base path for llm-bridge-adapter API. If omitted, bus_session links can't
   * resolve to a bridge_id and the chat button on those cards stays disabled. */
  bridgeAdapterBasePath?: string
  /** Base path for the orchestrator API. If omitted, the sidebar's
   * Orchestrator row and the in-chat orchestrator-context pane say the
   * orchestrator isn't configured. */
  orchestratorBasePath?: string
  /** Base path for the mailstack API as the host proxies it (dash: "/api/mail").
   * If omitted, the kanban card drawer hides its email preview and deep link. */
  mailBasePath?: string
  /** Base path for multichat's API as the host proxies it (dash:
   * "/api/multichat"). If omitted, the Messages group is hidden. */
  multichatBasePath?: string
  /** The host's route that invites a person to its library (dash:
   * "/api/kayushkin-viewer/invitations"). If omitted, no Invite button. */
  libraryInvitationsPath?: string
  /** Base path for discord-signup-store's API as the host proxies it (dash:
   * "/api/discord-signup"). If omitted, the Auto-reactions page leaves out the bot's rules. */
  /** Base path for short-name-store's API as the host proxies it (dash:
   * "/api/short-names"). If omitted, chats are addressed only by room id. */
  /** Path to the host's own mail page (dash: "/mail"). Empty hides the deep link. */
  mailPagePath?: string
  /** people-store and journal-store as the host proxies them (dash:
   * "/api/people", "/api/journal"), and the host's own pages for them (dash:
   * "/people", "/journal"), which person and entry chips link to. */
  peopleStoreBasePath?: string
  journalStoreBasePath?: string
  photoStoreBasePath?: string
  messageStoreBasePath?: string
  /** Base path for usage-store API. If omitted, spend/limits sections of the
   * Usage tab are hidden and only per-session aggregates are shown. */
  usageStoreBasePath?: string
  /** Base paths for seven services the settings page asks and nothing else
   * does (dash: "/api/scheduler", "/api/log-store", "/api/jobs",
   * "/api/quotes", "/api/predictions", "/api/events", "/api/authstore"). If
   * one is omitted, the page does not list that service. */
  schedulerBasePath?: string
  logStoreBasePath?: string
  jobStoreBasePath?: string
  quoteStoreBasePath?: string
  predictionStoreBasePath?: string
  eventStoreBasePath?: string
  authStoreBasePath?: string
  /** Base path of the host's own routes, read only by the service settings
   * page (dash: "/api/dash"). If omitted, the page does not list the host. */
  hostBasePath?: string
  /** The host's list of agents' dev servers (dash: "/api/previews"). If
   * omitted, the Previews page is hidden and Bash calls show no View button. */
  previewsBasePath?: string
  /** The host's list of .glb files agents make (dash: "/api/3d-models"). If
   * omitted, the 3D models page is hidden. */
  threeDimensionalModelsBasePath?: string
  /** Base path for work-graph-store (dash: "/api/work-graph"). If omitted,
   * the chat's work-graph view says it is not configured and nothing links to
   * it. */
  workGraphStoreBasePath?: string
  projectStoreBasePath?: string
  /** Base path for model-store (dash: "/api/model-store"). If omitted, the
   * Models page is hidden and a model_role setting cannot list the roles. */
  modelStoreBasePath?: string
  /** Optional render hook for per-harness Settings-tab extensions. Return null
   * for harnesses that don't need a custom panel. */
  renderHarnessExtension?: (harnessName: string) => ReactNode
  /** The host's chips for its own stores' records (see BridgeConfig). */
  renderReferenceChip?: ReferenceChipRenderer
  /** Route overrides. Any unspecified routes fall back to DEFAULT_BRIDGE_ROUTES,
   * which means `notes` and `orchestrator` — pages this library doesn't ship —
   * stay empty and their links aren't rendered until a host names them. */
  routes?: Partial<BridgeRoutes>
  children: ReactNode
}

export function BridgeProvider({
  fetch: fetchFn,
  basePath = '/api/bridge',
  skillStoreBasePath = '',
  toolStoreBasePath = '',
  permissionStoreBasePath = '',
  kanbanStoreBasePath = '',
  principalStoreBasePath = '',
  grantStoreBasePath = '',
  bundleStoreBasePath = '',
  repoStoreBasePath = '',
  mailBasePath = '',
  multichatBasePath = '',
  libraryInvitationsPath = '',
  mailPagePath = '',
  peopleStoreBasePath = '',
  journalStoreBasePath = '',
  photoStoreBasePath = '',
  messageStoreBasePath = '',
  noteboardBasePath = '',
  referenceStoreBasePath = '',
  bridgeAdapterBasePath = '',
  orchestratorBasePath = '',
  usageStoreBasePath = '',
  schedulerBasePath = '',
  logStoreBasePath = '',
  jobStoreBasePath = '',
  quoteStoreBasePath = '',
  predictionStoreBasePath = '',
  eventStoreBasePath = '',
  authStoreBasePath = '',
  hostBasePath = '',
  previewsBasePath = '',
  threeDimensionalModelsBasePath = '',
  workGraphStoreBasePath = '',
  projectStoreBasePath = '',
  modelStoreBasePath = '',
  renderHarnessExtension,
  renderReferenceChip,
  routes,
  children,
}: BridgeProviderProps) {
  const config = useMemo<BridgeConfig>(() => ({
    fetch: fetchFn,
    basePath,
    skillStoreBasePath,
    toolStoreBasePath,
    permissionStoreBasePath,
    kanbanStoreBasePath,
    principalStoreBasePath,
    grantStoreBasePath,
    bundleStoreBasePath,
    repoStoreBasePath,
    mailBasePath,
    multichatBasePath,
    libraryInvitationsPath,
    mailPagePath,
    peopleStoreBasePath,
    journalStoreBasePath,
    photoStoreBasePath,
    messageStoreBasePath,
    noteboardBasePath,
    referenceStoreBasePath,
    bridgeAdapterBasePath,
    orchestratorBasePath,
    usageStoreBasePath,
    schedulerBasePath,
    logStoreBasePath,
    jobStoreBasePath,
    quoteStoreBasePath,
    predictionStoreBasePath,
    eventStoreBasePath,
    authStoreBasePath,
    hostBasePath,
    previewsBasePath,
    threeDimensionalModelsBasePath,
    workGraphStoreBasePath,
    projectStoreBasePath,
    modelStoreBasePath,
    renderHarnessExtension: renderHarnessExtension ?? null,
    renderReferenceChip: renderReferenceChip ?? null,
    routes: { ...DEFAULT_BRIDGE_ROUTES, ...routes },
  }), [fetchFn, basePath, skillStoreBasePath, toolStoreBasePath, permissionStoreBasePath, kanbanStoreBasePath, principalStoreBasePath, grantStoreBasePath, bundleStoreBasePath, repoStoreBasePath, mailBasePath, multichatBasePath, libraryInvitationsPath, mailPagePath, peopleStoreBasePath, journalStoreBasePath, photoStoreBasePath, messageStoreBasePath, noteboardBasePath, referenceStoreBasePath, bridgeAdapterBasePath, orchestratorBasePath, usageStoreBasePath, schedulerBasePath, logStoreBasePath, jobStoreBasePath, quoteStoreBasePath, predictionStoreBasePath, eventStoreBasePath, authStoreBasePath, hostBasePath, previewsBasePath, threeDimensionalModelsBasePath, workGraphStoreBasePath, projectStoreBasePath, modelStoreBasePath, renderHarnessExtension, renderReferenceChip, routes])

  return (
    <BridgeContext value={config}>
      <MinimalChromeProvider>{children}</MinimalChromeProvider>
    </BridgeContext>
  )
}
