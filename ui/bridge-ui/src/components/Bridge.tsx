import { createElement, useMemo, type JSX } from 'react'
import { Route, Routes, useLocation } from 'react-router-dom'
import { ChatProvider } from '@kayushkin/chat-core'
import { BridgeProvider, type BridgeProviderProps } from '../provider'
import { DEFAULT_BRIDGE_ROUTES, useBridgeConfig, type BridgeRoutes } from '../context'
import { chatFetchOver } from '../chatFetch'
import { BridgeLayout } from './BridgeLayout'
import { bridgePagesShown, type HostPage } from '../pages'

export interface BridgeProps extends Omit<BridgeProviderProps, 'children' | 'routes'> {
  /** The host's notes page, for `[todo:<id>]` references. Empty means the host has
   *  none and those references render as plain text. */
  notesPath?: string
  /** Draw the Conformance tab. Default true. */
  showConformance?: boolean
  /** Draw the Service inventory tab. Default true. Its page reads `GET /services` on
   *  `basePath`, so a host whose bridge server predates that route hides it. */
  showServiceInventory?: boolean
  /** Pages the host brings into the same shell: routed beside this library's own
   *  and listed in the group each names. Paths are absolute from the root this
   *  component is mounted at. */
  hostPages?: readonly HostPage[]
  /** Only these of this library's pages are routed and listed; absent means
   *  every page. A path to a page left out matches no route. */
  onlyBridgePages?: readonly (keyof BridgeRoutes)[]
  /** The host's brand, drawn at the start of the top bar, before the groups. */
  navStart?: React.ReactNode
  /** The host's own controls (theme, logout), drawn at the end of the top bar.
   *  A host that fills both slots needs no header of its own. */
  navEnd?: React.ReactNode
}

/** The bridge, whole: every page this library ships (`BRIDGE_PAGES`), the
 *  host's own pages if it brings any, the grouped navigation, and the two
 *  providers they read — mounted by a host as ONE thing at the root of a router.
 *
 *  dash mounts it under a splat route and puts its own pages beside it; the
 *  standalone launcher mounts nothing else. Either way this component owns its
 *  host's root: the routes in `DEFAULT_BRIDGE_ROUTES` are what it renders, so a
 *  `?session=` deeplink, a card link or a reference chip built from them lands on
 *  a page this component serves. A host that instead composes pages by hand
 *  under a prefix passes `BridgeProvider` its own `routes` and does not use this.
 *
 *  `ChatProvider` sits above the router on purpose. It is chat-core's session
 *  store and sync engine, and holding it here rather than inside the chat page
 *  means the store survives a switch to Instances or Kanban and back — and that
 *  every page can render a reference chip, which throws without it. */
export function Bridge({ notesPath = '', showConformance, showServiceInventory, hostPages = [], onlyBridgePages, navStart, navEnd, ...provider }: BridgeProps): JSX.Element {
  const routes = useMemo(() => ({ notes: notesPath }), [notesPath])
  return (
    <BridgeProvider {...provider} routes={routes}>
      <ChatStore>
        <Routes>
          <Route element={<BridgeLayout showConformance={showConformance} showServiceInventory={showServiceInventory} hostPages={hostPages} onlyBridgePages={onlyBridgePages} navStart={navStart} navEnd={navEnd} />}>
            {bridgePagesShown(onlyBridgePages).map(p => {
              const path = p.route === 'chat' ? '' : DEFAULT_BRIDGE_ROUTES[p.route].replace(/^\//, '') + (p.routeSuffix ?? '')
              return p.route === 'chat'
                ? <Route key={p.route} index element={createElement(p.component)} />
                : <Route key={p.route} path={path} element={createElement(p.component)} />
            })}
            {hostPages.map(p => (
              <Route key={p.path + (p.routeSuffix ?? '')} path={p.path.replace(/^\//, '') + (p.routeSuffix ?? '')} element={p.element} />
            ))}
            {/* A path no page claims: a record's page on another host (reference-store's
                page_urls name dash's pages too), or a stale link. Saying so beats a
                blank page under the navigation. */}
            <Route path="*" element={<NoPageAtThisAddress />} />
          </Route>
        </Routes>
      </ChatStore>
    </BridgeProvider>
  )
}

/** chat-core's provider, fed from the same `BridgeConfig` every page reads, so
 *  the chat reaches noteboard and the resolver through whatever the host proxies. */
function ChatStore({ children }: { children: React.ReactNode }): JSX.Element {
  const { fetch: fetchFn, basePath, noteboardBasePath, referencesBasePath } = useBridgeConfig()
  const chatFetch = useMemo(() => chatFetchOver(fetchFn), [fetchFn])
  return (
    <ChatProvider
      fetch={chatFetch}
      basePath={basePath}
      noteboardBasePath={noteboardBasePath}
      referencesBasePath={referencesBasePath}
    >
      {children}
    </ChatProvider>
  )
}

function NoPageAtThisAddress(): JSX.Element {
  const location = useLocation()
  return (
    <div className="bridge-page">
      <div className="bi-empty">This site has no page at <code>{location.pathname}</code>.</div>
    </div>
  )
}
