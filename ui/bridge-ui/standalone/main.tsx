import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import {
  Bridge,
  BridgeSignInGate,
  BridgeThemeProvider,
  ShellBrand,
  ShellControls,
  type BridgeProps,
} from '@kayushkin/bridge-ui'

// Default theme (CSS variables) + component styles. The theme import is what
// lets bridge-ui render correctly with no host app providing a palette.
import '../theme.css'
import '../styles.css'
import '../zine.css'

// The standalone app: bridge-ui on its own over one llm-bridge-server, signed
// in to that server's own login (BridgeSignInGate) — no host server of its own.
// A connected server such as a laptop serves this build and keeps it current by
// following GitHub main of this repo and llm-bridge-server.
//
// VITE_BRIDGE_BASE points it at the bridge (default /api/bridge, which the dev
// server proxies to localhost:8160). The page must be on the bridge's origin
// for its login cookie, so a build is served behind a proxy that forwards that
// prefix to the bridge.
//
// `npm run build:served-by-the-bridge` makes the build llm-bridge-server serves
// itself, at /ui/ on its own address (its `ui` package). There the bridge's
// routes sit at the root of the page's origin, so the base is empty, and the
// pages live under Vite's base (/ui/), which the router takes as its basename.
const SERVED_BY_THE_BRIDGE = import.meta.env.VITE_SERVED_BY_THE_BRIDGE === 'true'
const BASE = SERVED_BY_THE_BRIDGE ? '' : (import.meta.env.VITE_BRIDGE_BASE ?? '/api/bridge')
const ROUTER_BASENAME = import.meta.env.BASE_URL === '/' ? undefined : import.meta.env.BASE_URL.replace(/\/$/, '')

/** Stores reached through the bridge's own gateway, as the signed-in person:
 *  every host that runs the bridge carries these. */
const BRIDGE_GATEWAY_PATHS: Partial<BridgeProps> = {
  issuesBasePath: `${BASE}/issues`,
  grantsBasePath: `${BASE}/grants`,
  referencesBasePath: `${BASE}/references`,
}

/** Stores the bridge does not proxy yet, reached at /api/<store> on the page's
 *  own origin. The dev server proxies them (vite.config.ts); a build gets them
 *  only when VITE_HOST_PROXIES_STORES=true says whatever serves it forwards
 *  them too. Left out, bridge-ui hides their pages rather than erroring. */
const HOST_STORE_PATHS: Partial<BridgeProps> = {
  skillsBasePath: '/api/skill-store',
  toolsBasePath: '/api/tool-store',
  principalsBasePath: '/api/principals',
  bundlesBasePath: '/api/bundle-store',
  reposBasePath: '/api/repo-store',
  gitActivityBasePath: '/api/work-graph',
  projectsBasePath: '/api/projects',
  modelsBasePath: '/api/model-store',
}

/** Stores the bridge serves in its own process, at the prefixes it mounts
 *  them under (each client's paths checked against the server's route table).
 *  Only the build the bridge serves uses these: there the page is on the
 *  bridge's own origin, so every prefix is reachable as the signed-in person.
 *  multichat is not one of them, so the Messages pages stay hidden. */
const STORES_THE_BRIDGE_SERVES: Partial<BridgeProps> = {
  toolsBasePath: '/tools',
  skillsBasePath: '/skills',
  bundlesBasePath: '/bundles',
  principalsBasePath: '/principals',
  permissionsBasePath: '/permissions',
  modelsBasePath: '/models',
  reposBasePath: '/repos',
  gitActivityBasePath: '/git-activity',
  projectsBasePath: '/projects',
  notesBasePath: '/notes',
  schedulerBasePath: '/scheduler',
  photosBasePath: '/photos',
  // usage-store roots its routes at /api/usage; the Usage page asks
  // `{base}/limits`, `{base}/spend/keys` and so on.
  usageBasePath: '/usage/api/usage',
  // mailstack roots its routes at /api; the card drawer asks `{base}/accounts`
  // and `{base}/messages/{id}`.
  mailBasePath: '/mail/api',
  // Of the stores above, only the scheduler serves `/settings` in process;
  // the bridge's own `/settings` describes the rest of the server.
  serviceSettingsSourceKeys: ['basePath', 'schedulerBasePath'],
}

const hostProxiesStores = import.meta.env.DEV || import.meta.env.VITE_HOST_PROXIES_STORES === 'true'
const STORE_PATHS: Partial<BridgeProps> = SERVED_BY_THE_BRIDGE
  ? { ...BRIDGE_GATEWAY_PATHS, ...STORES_THE_BRIDGE_SERVES }
  : { ...BRIDGE_GATEWAY_PATHS, ...(hostProxiesStores ? HOST_STORE_PATHS : {}) }

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter basename={ROUTER_BASENAME}>
      <BridgeThemeProvider storageKey="bridge-standalone-theme" defaultTheme="dark">
        <BridgeSignInGate basePath={BASE} title="Bridge">
          {({ fetch, logOut }) => (
            <Bridge
              fetch={fetch}
              basePath={BASE}
              {...STORE_PATHS}
              navStart={<ShellBrand name="Bridge" mark="🌉" />}
              navEnd={<ShellControls onLogout={logOut} />}
            />
          )}
        </BridgeSignInGate>
      </BridgeThemeProvider>
    </BrowserRouter>
  </StrictMode>,
)
