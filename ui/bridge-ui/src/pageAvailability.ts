import type { BridgeConfig, BridgeRoutes } from './context'
import type { ShellFlags } from './pages'

/** When each of this library's pages can work on a host: the base paths it
 *  needs, or the shell switch that turns it on. A page not named here needs
 *  nothing but the bridge. The navigation (`pages.ts`) and the links other
 *  pages draw to a page (the Settings page's "open" links) both read this, so
 *  neither links to a page the host cannot serve.
 *
 *  It lives apart from `pages.ts` because that module imports every page
 *  component, and a page that imported it back would load before the list it
 *  is part of. */
export const BRIDGE_PAGE_AVAILABILITY: Readonly<Partial<Record<keyof BridgeRoutes, (config: BridgeConfig, flags: ShellFlags) => boolean>>> = {
  kanban: c => !!c.issuesBasePath,
  reference: c => !!c.referencesBasePath,
  emailTickets: c => !!c.issuesBasePath && !!c.mailBasePath,
  orchestrator: c => !!c.orchestratorBasePath,
  previews: c => !!c.previewsBasePath,
  threeDimensionalModels: c => !!c.threeDimensionalModelsBasePath,
  messageWaiting: c => !!c.multichatBasePath,
  messageConversations: c => !!c.multichatBasePath,
  messageSearch: c => !!c.multichatBasePath,
  messageContacts: c => !!c.multichatBasePath,
  discord: c => !!c.multichatBasePath,
  emoji: c => !!c.multichatBasePath,
  autoReactions: c => !!c.multichatBasePath,
  skills: c => !!c.skillsBasePath,
  tools: c => !!c.toolsBasePath,
  bundles: c => !!c.bundlesBasePath,
  principals: c => !!c.principalsBasePath,
  grants: c => !!c.grantsBasePath,
  permissions: c => !!c.permissionsBasePath,
  models: c => !!c.modelsBasePath,
  serviceInventory: (_, f) => f.showServiceInventory,
  conformance: (_, f) => f.showConformance,
}

/** Whether a page can work on this host, for a caller that has no shell
 *  switches: a page that needs one counts as unavailable. */
export function bridgePageAvailableWithoutShellFlags(route: keyof BridgeRoutes, config: BridgeConfig): boolean {
  const available = BRIDGE_PAGE_AVAILABILITY[route]
  return available ? available(config, { showConformance: false, showServiceInventory: false }) : true
}
