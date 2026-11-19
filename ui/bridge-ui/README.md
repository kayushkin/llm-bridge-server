# @kayushkin/bridge-ui

The whole bridge surface for an [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) backend, as one React component. `<Bridge>` routes its own pages — the chat at `/` (on [`@kayushkin/chat-core`](https://github.com/kayushkin/chat-core)), instances, sessions, auth, usage, settings, agents, files, skills, tools, permissions, kanban, a card page, principals, bundles, the orchestrator and conformance — draws the tab row, and mounts both providers. A host wires auth and where it proxies each backend, mounts `<Bridge>` at the root of a router, and is done.

llm-bridge-server does not serve this package: it has no embedded UI. To get a working UI on top of it, mount `<Bridge>` in a React app of your own, which is the host and owns the chrome, the auth and the proxying to each backend.

## Install

```bash
npm install @kayushkin/bridge-ui
```

Peer dependencies (host app must provide):

- `react` ≥ 18
- `react-dom` ≥ 18
- `react-router-dom` ≥ 6

The library is ESM-only (`"type": "module"`) and ships three stylesheets:

- `@kayushkin/bridge-ui/styles.css` — component styles (always import this). It only
  *consumes* theme variables (`--bg`, `--bg-surface`, `--border`, `--text`, `--accent`,
  `--success`, …); it defines none.
- `@kayushkin/bridge-ui/theme.css` — **optional** default theme: defines those variables
  (dark + a `[data-theme="light"]` variant) plus a base reset, scrollbars, and body
  defaults. Import it to get a working look with zero config. Hosts that already define
  their own palette (dash does) should **skip** it and set the same variables at
  `:root` to override.
- `@kayushkin/bridge-ui/zine.css` — **optional** second skin: a two-ink risograph print
  (cream paper, navy ink, orange and pink, halftone dots) with a painted crab mascot. Import
  it after `styles.css` and put `data-theme="zine"` on a parent element. It sets the same
  variables, loads its own fonts, and draws its pictures from `zine/` in this package, so a
  bundler that rebases CSS `url()`s (Vite does) needs nothing more. A host page title gets
  its pink headline by carrying class `page-title`; a host can use a picture itself with
  `import star from '@kayushkin/bridge-ui/zine/star.png'`.

## Usage

Mount `<Bridge>` at the root of a router. It owns the root: `/` is its chat and every other
page is a route it renders itself, so a `?session=` deeplink or a card link built by one page
lands on another. A host with pages of its own puts them beside it as static routes — they rank
above the splat.

```tsx
import { Routes, Route } from 'react-router-dom'
import { Bridge } from '@kayushkin/bridge-ui'
import '@kayushkin/bridge-ui/styles.css'

// Your auth'd fetch — adds cookies, bearer tokens, etc. before every request.
const apiFetch: typeof fetch = (url, init) =>
  fetch(url, { credentials: 'include', ...init })

export default function App() {
  return (
    <Routes>
      <Route path="settings" element={<HostSettings />} />
      <Route path="*" element={
        <Bridge
          fetch={apiFetch}
          basePath="/api/bridge"
          skillStoreBasePath="/api/skill-store"
          kanbanStoreBasePath="/api/kanban"
          notesPath="/notes"
        />
      } />
    </Routes>
  )
}
```

`<Bridge>` takes every `BridgeProvider` prop except `routes` and `children`, plus `notesPath` (the
host's notes page, for `[todo:…]` references) and `showConformance`. Each `*BasePath` names where
the host proxies that backend; omit one and the feature that needs it is hidden rather than
pointed at a 404.

`BridgeProvider` remains the single configuration point underneath. Every hook and page pulls its
`fetch`, `basePath` and route map from context — nothing reads a global or a hardcoded URL — so a
host that would rather compose pages by hand mounts `BridgeProvider` + `BridgeLayout` and the page
components itself, passing its own `routes`.

### `BridgeProvider` props

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `fetch` | `(url, init?) => Promise<Response>` | required | Auth'd fetch function. Add cookies / bearer tokens here. |
| `basePath` | `string` | `/api/bridge` | Base path for the llm-bridge-server API (no trailing slash). |
| `skillStoreBasePath` | `string` | `""` | Base path for skill-store API. If empty, the Skills tab is hidden. |
| `bundleStoreBasePath` | `string` | `""` | Base path for bundle-store API. If empty, the Bundles tab is hidden. |
| `repoStoreBasePath` | `string` | `""` | Base path for repo-store API, which gives the Bundles page repos to preview a resolution for. If empty, the page lists bundles only. |
| `previewsBasePath` | `string` | `""` | The host's list of ports agents' processes listen on, each with the public port that shows it (dash: `/api/previews`). If empty, the Previews tab and the View link on Bash calls are hidden. |
| `routes` | `Partial<BridgeRoutes>` | `DEFAULT_BRIDGE_ROUTES` | Override individual route paths used by inter-page navigation. `<Bridge>` sets only `notes`. |

`DEFAULT_BRIDGE_ROUTES` is what `<Bridge>` renders:

```ts
{
  chat: '/', instances: '/instances', sessions: '/sessions', auth: '/auth', usage: '/usage',
  settings: '/settings', agents: '/agents', files: '/files', skills: '/skills', tools: '/tools',
  permissions: '/permissions', conformance: '/conformance', kanban: '/kanban',
  principals: '/principals', bundles: '/bundles', orchestrator: '/orchestrator', card: '/card', previews: '/previews',
  notes: '',   // the host's, if it has one
}
```

## Exports

### Page components

| Component | Purpose |
|-----------|---------|
| `Bridge` | The whole surface: both providers, the tab row and every page below, routed. |
| `BridgeChat` | The chat — sidebar, thread, panes, composer — on `@kayushkin/chat-core`. |
| `BridgeLayout` | Outer shell with the tab nav. Renders the `<Outlet/>` of nested routes. |
| `BridgeSessions` | Session browser across all instances/harnesses. |
| `BridgeInstances` | Instance + machine management (create, edit, bind credentials). |
| `BridgeAuth` | Credential management (Anthropic, OpenAI, Google, etc.). |
| `BridgeUsage` | Token / cost rollups across sessions. |
| `BridgeSettings` | User-level prefs (default model, layout, etc.). |
| `BridgeSkills` | Skill-store browser — hidden when `skillStoreBasePath` is empty. |
| `BridgeConformance` | Per-harness conformance matrix (which features pass). |

### Hooks

| Hook | Returns | Notes |
|------|---------|-------|
| `useBridgeConfig()` | `BridgeConfig` | The current provider config. Throws if not under `BridgeProvider`. |
| `useBridgeInstances()` | `{ instances, loading, error, ... }` | Polls `/instances` every 30 s; create/update/delete helpers. |
| `useBridgeMachines()` | `{ machines, loading, error, ... }` | Host registry instances bind to. Same poll-and-snapshot shape as instances. |
| `useBridgePrefs(opts?)` | `[prefs, setPrefs]`-style object | Server-synced (when `fetch`+`endpoint` provided) or `localStorage`-only. |
| `useBridgeFolders()` | `UseBridgeFoldersReturn` | Folder ordering + session→folder assignment. |
| `useStickyBottomScroll(ref)` | `StickyBottomScroll` | Auto-stick chat scroll to bottom unless the user scrolls up. |

### SSE & utilities

- `connectSSE(fetch, basePath, sessionId, lastEventId?, signal?)` — async generator yielding `BridgeEvent`s, for consumers that need a raw event stream. The chat itself reads through chat-core's sync engine.
- `formatTokens`, `formatCost`, `formatDuration`, `timeAgo` — display helpers used by the built-in components.
- `TRANSPORT_LABEL` — display name lookup for credential transports.

### Tool renderers

The Tools panel renders `ToolEvent`s through a registry. Built-in renderers cover File reads, Edit, Bash, Grep, and Web tools; everything else falls back to `DefaultRenderer`. Register custom renderers from your host app:

```tsx
import { registerToolRenderer, type ToolRendererProps } from '@kayushkin/bridge-ui'

function MyToolRenderer({ tool, running }: ToolRendererProps) {
  return <pre>{JSON.stringify(tool.input, null, 2)}</pre>
}

registerToolRenderer('MyTool', MyToolRenderer)
```

`getToolRenderer(name)` resolves a renderer by tool name, returning `DefaultRenderer` if none is registered.

### Types

All canonical types come from `@kayushkin/llm-bridge-types` (auto-generated from the Go structs in `llm-bridge/msg/`) and are re-exported here. UI-specific types (`Message`, `LogRow`, `BridgeEvent`, `SessionUIState`, `ActivityKind`, etc.) are defined in `src/types.ts`.

Do **not** copy these types into your host app — they are a single source of truth for the wire protocol; importing them from this package keeps host code in lock-step with the server.

## Architecture

```
Host app
  │
  ├─ <Bridge fetch=… basePath=… skillStoreBasePath=… notesPath=…>
  │     │
  │     ├─ <BridgeProvider>        ← config context + the one MinimalChromeProvider
  │     │   └─ <ChatProvider>      ← chat-core's session store + sync engine, above the router
  │     │       └─ <Routes>
  │     │           └─ <BridgeLayout/>     ← tab row
  │     │               └─ <Outlet/>       ← one of:
  │     │                   BridgeChat (index) | BridgeInstances | BridgeSessions | BridgeAuth |
  │     │                   BridgeUsage | BridgeSettings | BridgeAgents | BridgeFiles | BridgeSkills |
  │     │                   BridgeTools | BridgePermissions | BridgeKanban | BridgeCardPage |
  │     │                   BridgePrincipals | BridgeOrchestrator | BridgeConformance
  │     │
  │     └─ hooks read config from BridgeContext:
  │         useBridgeInstances → /instances
  │         useBridgeMachines  → /machines
  │         useBridgeFolders   → /folders
  │         useBridgePrefs     → /session-meta/bridge (or localStorage)
  │         useKanban          → kanban-store, via kanbanStoreBasePath
  │
  └─ apiFetch — host's job: cookies, bearer tokens, error handling
```

The library never opens a connection that isn't routed through the host's `fetch`. SSE uses fetch + `ReadableStream` (rather than the native `EventSource`) so the same auth applies to chat events as to REST calls.

## Build / dev

```bash
npm install
npm run build    # tsc → dist/, then scripts/copy-css.mjs puts every src/**/*.css beside its .js
npm run dev      # tsc --watch
```

`tsconfig.json` emits ESM (`module: ESNext`) with declarations and source maps into `dist/`. tsc emits no CSS, so the build copies every stylesheet under `src/` into `dist/` at the same path — the chat's `Chat.module.css` included — and the consumer's bundler resolves the relative import as it would in a source tree. The published package contains `dist/`, `styles.css`, `theme.css`, `zine.css` and the `zine/` pictures only (`files` field in `package.json`). `dist/` is not committed: `npm install` builds it (`prepare`), and a host's deploy rebuilds every linked library before bundling.

## Standalone launcher

A minimal Vite app under `standalone/` runs the full UI on its own — useful for
developing the library or driving a bare llm-bridge-server without a host app. It mounts
`<Bridge>` at the root against `src/` directly (no prior build needed) and imports both
`theme.css`, `styles.css` and `zine.css`, so it's fully styled out of the box; add
`?theme=zine` (or `?theme=light`) to the URL to switch skins.

```bash
npm install
npm start                 # dev server; proxies /api/bridge → localhost:8160,
                          # plus skill-store/tool-store/kanban (see standalone/vite.config.ts)
npm run build:standalone  # static bundle → standalone/dist (gitignored, not published)
```

Point it at a different bridge without the dev proxy via `VITE_BRIDGE_BASE`
(e.g. an absolute URL). The launcher lives outside `src/`, so it never enters the
published library build.

## License

[Apache License 2.0](./LICENSE).
