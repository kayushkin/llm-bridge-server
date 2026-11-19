import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import {
  Bridge,
  BridgeSignInGate,
  BridgeThemeProvider,
  ShellBrand,
  ShellControls,
  type BridgeConfig,
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
const BASE = import.meta.env.VITE_BRIDGE_BASE ?? '/api/bridge'

/** Stores reached through the bridge's own gateway, as the signed-in person:
 *  every host that runs the bridge carries these. */
const BRIDGE_GATEWAY_PATHS: Partial<BridgeConfig> = {
  kanbanStoreBasePath: `${BASE}/kanban-store`,
  grantStoreBasePath: `${BASE}/grant-store`,
  referenceStoreBasePath: `${BASE}/reference-store`,
}

/** Stores the bridge does not proxy yet, reached at /api/<store> on the page's
 *  own origin. The dev server proxies them (vite.config.ts); a build gets them
 *  only when VITE_HOST_PROXIES_STORES=true says whatever serves it forwards
 *  them too. Left out, bridge-ui hides their pages rather than erroring. */
const HOST_STORE_PATHS: Partial<BridgeConfig> = {
  skillStoreBasePath: '/api/skill-store',
  toolStoreBasePath: '/api/tool-store',
  principalStoreBasePath: '/api/principals',
  bundleStoreBasePath: '/api/bundle-store',
  repoStoreBasePath: '/api/repo-store',
  workGraphStoreBasePath: '/api/work-graph',
  projectStoreBasePath: '/api/projects',
  modelStoreBasePath: '/api/model-store',
}

const hostProxiesStores = import.meta.env.DEV || import.meta.env.VITE_HOST_PROXIES_STORES === 'true'
const STORE_PATHS: Partial<BridgeConfig> = {
  ...BRIDGE_GATEWAY_PATHS,
  ...(hostProxiesStores ? HOST_STORE_PATHS : {}),
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
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
