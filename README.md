# llm-bridge-server

Central HTTP gateway and session server for the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

## Design docs (in this repo)

The cross-cutting design for the harness layer, agent rendering, tool/skill routing, and CLI surface lives at the root of this repo. Read in this order on first contact:

| Doc | Covers |
|---|---|
| [`HARNESS-LAYER.md`](./HARNESS-LAYER.md) | The abstraction: `AgentReconciler` interface (`EnsureAgent` / `PrepareSession` / `CleanupAgent`), per-harness implementations, subagent routing (CC `--agents` JSON inline, cross-harness via CLI delegation). |
| [`TOOL-ROUTING.md`](./TOOL-ROUTING.md) | Routing rule (`native > MCP > CLI > omit`), per-harness native tool catalog, capability registry, skills routing, **end-to-end setup walkthroughs for tools, skills, and agents**. |
| [`AGENT-MANAGEMENT.md`](./AGENT-MANAGEMENT.md) | Canonical agent shape, the rendering library (`llm-bridge/render`), per-harness rendering (CC: `--agents` JSON, no file), CRUD flows, `/agents` UI vs `/files` debug surface. |
| [`CLI-SURFACE.md`](./CLI-SURFACE.md) | Model-facing CLI surface. Unified `bridge` binary for cross-cutting capabilities (agent ask, memory, notes, bus, tools, skills); `inber` binary for runtime-specific only. Permission allowlist patterns. |
| [`CACHE-RULES.md`](./CACHE-RULES.md) | The seven cache-busting rules. What's allowed to bust, what's not, what's outside our control, and the diagnostic flow when caching regresses. |
| [`CC-VERIFIED.md`](./CC-VERIFIED.md) | Empirical reference for Claude Code 2.1.138 behavior — `--agents`, `--system-prompt` vs `--append`, `--bare`, `--settings`, init event surface. Sources cited from elsewhere in the design. |
| [`CONTEXT-MIGRATION.md`](./CONTEXT-MIGRATION.md) | Plan to extract inber's per-turn assembly (`engine/turn_*.go`, `conversation/`) into `llm-bridge/assembly/` shared library. Replaces this server's `agents_context.go`. |
| [`IMPLEMENTATION-ROADMAP.md`](./IMPLEMENTATION-ROADMAP.md) | Sequenced PRs across all affected repos. Critical path P1→P2→P3→P4→P6 (~3-4 weeks for the first end-to-end CC vertical). |
| [`TEAM-ORCHESTRATION.md`](./TEAM-ORCHESTRATION.md) | Dynamic skill-formed agent **teams** coordinating over a kanban board as a blackboard. Generalizes the `scheduler/cmd/kanban-*` loop: planner → team-former → assigner (slow/cron) + an in-server coordination engine (fast/events). Board-per-team, `team_id`/`board_id`/`role` on sessions, `bridge kanban` agent CLI. |

Operational docs:

| Doc | Covers |
|---|---|
| [`PTY-MODE.md`](./PTY-MODE.md) | PTY-mode harness operation. |
| [`CODEX-PARITY.md`](./CODEX-PARITY.md) | Plan to bring codex sessions to feature parity with CC on permissions, hooks, and tool-store; plus codex-specific extras (`PermissionRequest`, `Stop`/`UserPromptSubmit`/`SessionStart` hooks, output-schema). |
| [`TODO-jig-integration.md`](./TODO-jig-integration.md) | jig harness integration TODO list. |

External pointers (not in this repo, referenced by the design):

- `~/repos/inber/docs/cli-tool-surface.md` — `inber` CLI scope (runtime-only since the rescope; cross-harness lives in `CLI-SURFACE.md` here).
- `~/repos/inber/docs/pro-max-auth.md` — Pro/Max OAuth dual-refresh issue affecting inber's API path. Not specific to the harness layer.
- `~/repos/agent-store/AGENT-RENDER.md` — stub pointing at `AGENT-MANAGEMENT.md` here.

---


Spawns harness bridges as subprocesses, manages their lifecycle, and streams canonical `msg.Event` output to clients over SSE. Your application connects to this server and gets a uniform API regardless of which agent is running behind the harness.

```
  ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐
         Your Application  (dashboard, CLI, bot, anything)
  └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┬ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘
                                        │ HTTP / SSE
  ╔═════════════════════════════════════╪═════════════════════════════════════╗
  ║                    llm-bridge-server                                     ║
  ║                                     │                                    ║
  ║   Sessions ─── lifecycle, events, history                                ║
  ║   Instances ── harness deployment registry                               ║
  ║   Credentials ─ API key / token management                               ║
  ║   Stores ──── agents, memory, models, logs                               ║
  ║                                     │                                    ║
  ╚═════════════════════════════════════╪════════════════════════════════════╝
           stdin/stdout NDJSON          │           stdin/stdout NDJSON
       ┌────────────────────────────────┼──────────────────────────────┐
       │                                │                              │
       ▼                                ▼                              ▼
  ┌──────────┐                   ┌──────────┐                   ┌──────────┐
  │ harness  │                   │ harness  │                   │ harness  │
  │ bridge   │                   │ bridge   │                   │ bridge   │
  │          │                   │          │                   │          │
  │claudecode│                   │  codex   │                   │ hermes   │
  │  jig     │                   │  aider   │                   │ openclaw │
  │          │                   │  goose   │                   │ nanoclaw │
  └────┬─────┘                   └────┬─────┘                   └────┬─────┘
       │ spawns/connects              │ spawns/connects              │ spawns/connects
       ▼                              ▼                              ▼
  ┌──────────┐                   ┌──────────┐                   ┌──────────┐
  │  claude  │                   │  codex   │                   │  hermes  │
  │  code    │                   │  agent   │                   │  server  │
  │  CLI     │                   │  CLI     │                   │  (HTTP)  │
  └──────────┘                   └──────────┘                   └──────────┘
   subprocess                     subprocess                     HTTP/WS/Docker
```

Each harness bridge is a separate binary that the server spawns as a subprocess. The bridge in turn spawns or connects to the actual agent — whether that's a CLI subprocess, a local HTTP server, a WebSocket endpoint, or a Docker container. The bridge is the only thing that knows the agent's native protocol.

## Quick start

### Bootstrap (first run only)

`go.mod` uses `replace ../X` directives to pull a dozen sibling libraries
out of the parent directory rather than from the Go module proxy, so a
fresh `git clone` of this repo cannot build standalone. Run the
bootstrap script once to clone every sibling next to this repo:

```bash
./scripts/bootstrap.sh
```

This produces the layout the build expects:

```
<parent>/
  llm-bridge-server/   (this repo)
  llm-bridge/
  log-store/
  logstack/
  agent-store/
  …
```

Existing checkouts are left alone, and siblings without a public remote
(`snapshot-store`) are skipped — the server degrades gracefully without
those optional stores.

### Build and run

```bash
go build -o llm-bridge ./cmd/llm-bridge-server
./llm-bridge
```

The server listens on `:8160` by default. See [`.env.example`](./.env.example)
for every tunable.

### Deploy as a systemd service

```bash
./deploy.sh
```

Builds the binary, installs to `/usr/local/bin/llm-bridge`, and restarts the `llm-bridge.service` unit. The script auto-detaches via `systemd-run` so the deploy survives `systemctl stop llm-bridge.service` (the unit it's replacing).

### Run with Docker (with UI)

The compose stack ships a clickable end-to-end deploy: `llm-bridge-server` (full image with the upstream `claude` CLI baked in), `log-store`, and the `llmux` UI in front of it.

```bash
./scripts/bootstrap.sh                              # clone every sibling
echo "LLMUX_TOKEN=$(openssl rand -hex 16)" > .env   # token gates the UI
docker compose up --build
```

Default host-port mappings (deliberately offset from the canonical
8160/8170/8175 so the stack doesn't collide with a host-side server
that's already running):

| Service | Host port | Override |
|---|---|---|
| `llm-bridge-server` | `:18860` | `LLM_BRIDGE_HOST_PORT` |
| `llmux` (UI)        | `:18870` | `LLMUX_HOST_PORT` |
| `log-store`         | `:18875` | `LOG_STORE_HOST_PORT` |

Open `http://localhost:18870`, paste the `LLMUX_TOKEN` value from your `.env`, then use the UI to create a machine + instance + session. The image bakes in two `available:true` harnesses out of the box:

- **`mock`** — fake responses for protocol verification, no auth needed
- **`claude_code`** — real `claude` CLI bundled; needs your creds (next section)

#### Wiring real `claude_code` auth

The container ships with the upstream `claude` CLI installed, but credentials are user-specific. To share your host's existing Claude login, bind-mount `~/.claude` and override the container user so the mount lines up:

```yaml
# docker-compose.yml, llm-bridge-server service:
    volumes:
      - bridge-data:/data
      - ${HOME}/.claude:/data/home/.claude        # uncomment
    user: "${UID:-1000}:${GID:-1000}"             # uncomment + adjust if your uid ≠ 1000
```

Then `docker compose up --build` again. Without that mount the claude harness shows `available:true` (the binary is on PATH) but every actual request fails at auth — the mock harness still works fully, so the deploy is testable without any creds.

#### Building images directly

The multi-stage [`Dockerfile`](./Dockerfile) exposes four targets:

```bash
# distroless, mock-only (smallest)
docker build -f llm-bridge-server/Dockerfile --target server      -t llm-bridge-server      ..

# debian-slim + claude CLI + claudecode wrapper (used by compose default)
docker build -f llm-bridge-server/Dockerfile --target server-full -t llm-bridge-server:full ..

# durable event log sidecar
docker build -f llm-bridge-server/Dockerfile --target log-store   -t log-store              ..

# llmux UI (frontend dist + Go proxy with token auth)
docker build -f llm-bridge-server/Dockerfile --target llmux       -t llmux                  ..
```

Build context for every target is the *parent* directory, since the multi-stage build copies sibling repos in as `/src/<sibling>`.

### Start a session

> **Prerequisite — enroll a harness instance first.** `POST /sessions` resolves
> the session onto an *enabled instance* of the requested harness; with none
> enrolled it returns `503 harness-store not configured` (no harness-store DB) or
> `503 no enabled instance for harness: <name>`. The Docker/UI path above sets
> this up for you — the llmux UI creates a machine + instance in a couple of
> clicks. For the bare build/systemd path, enroll once via the API: `POST /machines`
> then `POST /instances` (`{"name": "...", "harness_type": "claude_code",
> "machine_id": "<id-from-POST-/machines>"}`; instances are enabled on create).
> See the [Instances](#instances-requires-harness-store) and
> [Machines](#machines-requires-harness-store) sections for the request shapes.
> The harness-store DB lives at `LLMBRIDGE_HARNESS_DB`
> (default `~/.config/harness-store/harness.db`; see [Configuration](#configuration)).

```bash
# Create and auto-start a session (returns {"id": "...", ...})
# Assumes an enabled instance of the harness is enrolled (see prerequisite above).
curl -X POST http://localhost:8160/sessions \
  -H 'Content-Type: application/json' \
  -d '{"harness": "claude_code", "auto_start": true}'

# Send the first instruction to the running session
curl -X POST http://localhost:8160/sessions/{id}/send \
  -H 'Content-Type: application/json' \
  -d '{"message": "Fix the tests"}'

# Stream events
curl -N http://localhost:8160/sessions/{id}/events
```

### Consume events (Go)

```go
import "github.com/kayushkin/llm-bridge/msg"

// GET /sessions/{id}/events returns an SSE stream of msg.Event
for event := range events {
    switch event.Type {
    case msg.EventResult:
        fmt.Println(event.Result.Text)
    case msg.EventToolCall:
        fmt.Println("Tool:", event.ToolCall.Name)
    case msg.EventApproval:
        // Surface permission request to user
    }
}
```

### Consume events (TypeScript)

```typescript
import type { Event } from '@kayushkin/llm-bridge-types'

const events = new EventSource(`${serverURL}/sessions/${id}/events`)
events.onmessage = (e) => {
    const event: Event = JSON.parse(e.data)
}
```

## API

### Sessions

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/sessions` | List all sessions |
| `POST` | `/sessions` | Create session (optionally auto-start) |
| `GET` | `/sessions/search` | Full-text search across sessions (proxied to log-store) |
| `GET` | `/sessions/discover` | Discover on-disk sessions from harness CLIs |
| `GET` | `/sessions/{id}` | Get session details |
| `GET` | `/sessions/{id}/events` | SSE event stream (supports `Last-Event-ID` for reconnection) |
| `GET` | `/sessions/{id}/attach` | WebSocket pty attach (pty-mode sessions only; rejected for events-mode) |
| `GET` | `/sessions/{id}/messages` | Message history (proxied to log-store) |
| `GET` | `/sessions/{id}/history` | Full history (proxied to log-store) |
| `POST` | `/sessions/{id}/send` | Send a user message |
| `POST` | `/sessions/{id}/interrupt` | Interrupt mid-turn (SIGINT) |
| `POST` | `/sessions/{id}/resume` | Resume a paused session |
| `POST` | `/sessions/{id}/stop` | Terminate a session |
| `POST` | `/sessions/{id}/compact` | Compact context to stay within token limits |
| `POST` | `/sessions/{id}/fork` | Fork from a parent session |
| `POST` | `/sessions/{id}/rename` | Set the session's display title |
| `POST` | `/sessions/{id}/auto-rename` | Generate a title from session content |
| `POST` | `/sessions/{id}/config` | Update session config on the fly |
| `PUT` | `/sessions/{id}/folder` | Move the session into a folder |
| `GET` | `/sessions/{id}/git/repos` | List git repos discovered for the session |
| `GET` | `/sessions/{id}/git` | Git status/diff for a repo (`?repo=<absolute-path>`; defaults to first discovered) |
| `GET` | `/sessions/{id}/hooks/pending` | List awaiting_resolution `HookEvent`s currently outstanding (used by UIs to hydrate the pending-hook banner without replaying the full SSE stream) |
| `POST` | `/sessions/{id}/hooks/{request_id}/resolve` | Deliver a decision for an awaiting_resolution hook. Body: `{behavior: "allow"\|"deny", updated_input?, message?, resolved_by?}`. Forwarded to the harness as a `resolve_hook` JSON-RPC request; the harness is responsible for closing the parked permission-prompt MCP call and emitting the matching `phase:"completed"` HookEvent |

### Folders

Sidebar organization for sessions, plus per-source default folders (e.g. all `scheduler`-created sessions land in `Scheduled`).

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/folders` | List folders |
| `POST` | `/folders` | Create folder |
| `PUT` | `/folders/{name}` | Rename folder |
| `DELETE` | `/folders/{name}` | Delete folder |
| `GET` | `/source-folders` | List source-folder overrides |
| `PUT` | `/source-folders/{source}` | Set folder for a session source |
| `DELETE` | `/source-folders/{source}` | Remove a source-folder override |

### Instances (requires harness-store)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/instances` | List harness instances |
| `POST` | `/instances` | Create instance (local or SSH) |
| `GET` | `/instances/{id}` | Get instance details |
| `PUT` | `/instances/{id}` | Update instance |
| `DELETE` | `/instances/{id}` | Delete instance |
| `GET` | `/instances/{id}/status` | Status with active sessions and credential availability |
| `GET` | `/instances/{id}/sessions` | Sessions running on this instance |
| `GET` | `/instances/{id}/credentials` | Credentials bound to this instance |
| `POST` | `/instances/{id}/credentials` | Bind a credential |
| `DELETE` | `/instances/{id}/credentials/{cred_id}` | Unbind a credential |

### Machines (requires harness-store)

Host-level configuration. Instances bind to a machine; the machine carries transport, SSH, and runner details.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/machines` | List machines |
| `POST` | `/machines` | Create machine |
| `GET` | `/machines/{id}` | Get machine details |
| `PUT` | `/machines/{id}` | Update machine |
| `DELETE` | `/machines/{id}` | Delete machine |

### Hooks (requires hook-store)

Bridge-managed harness hooks (event/matcher → shell command), bound to global, instance, or session scope.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/hooks` | List hooks (filterable by `harness`, `scope_kind`, `scope_id`, `enabled`) |
| `POST` | `/hooks` | Create hook |
| `GET` | `/hooks/{id}` | Get hook details |
| `PATCH` | `/hooks/{id}` | Partial update (e.g. toggle `enabled`) |
| `DELETE` | `/hooks/{id}` | Delete hook |
| `POST` | `/hooks/exec/{id}` | Execute a registered hook (called by harnesses for native-observed hooks) |

### Credentials

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/credentials` | List stored credentials (keys masked) |
| `POST` | `/credentials` | Create credential (API key or token) |
| `DELETE` | `/credentials/{id}` | Delete credential |

### Snapshots (requires snapshot-store)

Point-in-time file snapshots taken before/after Edit/Write tool calls; the UI reads these to render diffs.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/sessions/{id}/tools/{tool_use_id}/snapshots` | Snapshot metadata (before/after pairs) for a tool call |
| `GET` | `/snapshots/blob/{sha}` | Raw blob content (content-addressed by SHA; cacheable forever) |

### Conformance

Capability-matrix runs across all harnesses.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/conformance` | Latest conformance matrix and run state |
| `POST` | `/conformance/run` | Kick off a new conformance run |

### Runner (requires harness-store)

`/api/runner/*` powers `llm-bridge-runner` daemons on remote machines. The WebSocket multiplexes harness IO; the asset and enrollment endpoints bootstrap a fresh host.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/runner/ws` | Long-lived WebSocket from a runner (Bearer-auth against `machines.runner_token_hash`) |
| `POST` | `/api/runner/enroll` | Exchange a single-use enrollment passphrase for a durable runner token |
| `GET` | `/api/runner/install.sh` | Runner install script |
| `GET` | `/api/runner/binary` | Prebuilt runner / harness wrapper binary (`?os=&arch=&name=`) |
| `*` | `/api/harness-proxy/{harness}/{rest...}` | Reverse-proxy from runners to a service-style harness (inber, hermes…) hosted on the bridge |

### Admin

Housekeeping endpoints intended to be driven by a periodic scheduler job.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/admin/file-inactive` | File sessions that have gone inactive |
| `POST` | `/admin/archive-old` | Archive sessions older than the request's threshold |

### Other

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/health` | Server health, available harnesses, session counts |
| `GET` | `/harnesses` | Harness metadata (name, label, emoji, image, capabilities) |
| `GET` | `/harnesses/{name}/capabilities` | Capability descriptors for a single harness |
| `GET` | `/harnesses/{name}/agents` | Named-agent list (empty for harnesses without that concept) |
| `GET` | `/models` | Available models with credentials (requires model-store) |
| `GET` | `/bridge-prefs` | User preferences |
| `PUT` | `/bridge-prefs` | Update preferences |
| `GET` | `/images/...` | Static harness image directory |

When agent-store and memory-store are loaded, their HTTP handlers are also mounted on the server (see each library for endpoints).

## How it works

### Session lifecycle

1. **Create** — `POST /sessions` creates a session record. With `auto_start: true`, the server spawns the harness binary as a subprocess.
2. **Running** — The harness reads user messages from stdin (JSON) and writes `msg.Event` NDJSON to stdout. The server persists events and fans them out to SSE subscribers.
3. **Streaming** — `GET /sessions/{id}/events` opens an SSE connection. Replays current-turn events on connect, then streams live. Supports `Last-Event-ID` for reconnection.
4. **Interrupt** — `POST /sessions/{id}/interrupt` sends SIGINT. The session pauses and can be resumed.
5. **Resume** — `POST /sessions/{id}/resume` restarts the harness with resume context.
6. **Fork** — `POST /sessions/{id}/fork` creates a child session branching from a parent. The harness clones its state.
7. **Stop** — `POST /sessions/{id}/stop` terminates the subprocess.

### Instance concurrency cap

Each instance has a `max_concurrent_sessions` field on harness-store (default 1). Currently informational — server-side enforcement is not yet wired up.

### Auto-discovery

On startup, the server runs the discoverable harness binaries (`claudecode`, `codex`, `hermes`) with `-discover` to find existing on-disk sessions (e.g., Claude Code sessions from `~/.claude/projects/`). Discovered sessions are imported and their history is loaded into log-store.

## Configuration

All configuration is via environment variables with sensible defaults.

| Variable | Default | Description |
|----------|---------|-------------|
| `LLMBRIDGE_LISTEN_ADDR` | `:8160` | HTTP listen address |
| `LLMBRIDGE_PUBLIC_URL` | _(unset)_ | Externally-reachable bridge URL advertised to runners for binary/asset fetches; falls back to the runner's own `server_url` when empty |
| `LLMBRIDGE_DB_PATH` | `~/.llm-bridge/bridge.db` | Bridge SQLite database |
| `LLMBRIDGE_AGENT_DB` | `~/.config/agent-store/agents.db` | Agent store database |
| `LLMBRIDGE_MEMORY_DB` | `~/.config/memory-store/memory.db` | Memory store database |
| `LLMBRIDGE_HARNESS_DB` | `~/.config/harness-store/harness.db` | Harness store database |
| `LLMBRIDGE_HOOK_DB` | `~/.config/hook-store/hooks.db` | Hook store database |
| `LLMBRIDGE_MODEL_STORE_DB` | `~/.config/model-store/store.db` | Model store database |
| `LLMBRIDGE_SNAPSHOT_DB` | `~/.config/snapshot-store/snapshots.db` | Snapshot store SQLite metadata |
| `LLMBRIDGE_SNAPSHOT_GIT` | `~/.config/snapshot-store/snapshots.git` | Snapshot store git blob backend (bare repo) |
| `LLMBRIDGE_LOG_STORE_URL` | `http://localhost:8175` | Log-store service URL |
| `LLMBRIDGE_TOOL_STORE_URL` | `http://localhost:8302` | Tool-store service URL (used for MCP provisioning when a session sets `tool_store_tools` in its harness config) |
| `LLMBRIDGE_BRIDGE_PREFS` | `~/.config/llm-bridge/bridge-prefs.json` | User preferences file |
| `LLMBRIDGE_CONFORMANCE_PATH` | `~/.config/llm-bridge/conformance.json` | Conformance run state file (latest matrix + active run) |
| `LLMBRIDGE_IMAGES_DIR` | `images` | Static harness image directory |
| `LLMBRIDGE_SOURCE_FOLDERS` | `scheduler:Scheduled,autoworker:Scheduled,healthcheck:Scheduled,renamer:Auto-rename,conformance:Conformance` | Comma-separated `source:folder` map for auto-filing new sessions by their `source` field |
| `LLMBRIDGE_PTY_RING_BUFFER_BYTES` | `65536` | Per-session pty output ring buffer (bytes); late attachers receive a replay of this much screen state |
| `LLMBRIDGE_RUNNER_ASSETS_DIR` | `/usr/local/lib/llm-bridge-runner-binaries` | Directory of prebuilt runner + harness binaries served by `/api/runner/binary` |
| `LLMBRIDGE_RUNNER_INSTALL_SCRIPT` | _(unset)_ | Override path for the runner install script served by `/api/runner/install.sh` (falls back to `<assets-dir>/install.sh`, then `~/repos/llm-bridge-runner/scripts/install.sh`) |
| `LLMBRIDGE_HARNESS_PROXY_<NAME>` | _(per-harness default: `inber`=`http://localhost:8200`, `hermes`=`http://localhost:8500`)_ | Override URL for the `/api/harness-proxy/{harness}/...` reverse target; set to empty string to disable a harness's proxy |

## Testing

Three tiers, in increasing strictness about the host environment:

| Tier | Command | Needs | What it covers |
|---|---|---|---|
| **Unit + conformance** | `go test ./...` | nothing (auto-builds `cmd/mock-harness`) | Every package's unit tests + the full conformance feature matrix against mock-harness. |
| **Mock E2E** | `./scripts/e2e-smoke.sh` | go, curl, jq | Builds server + mock-harness + log-store, launches them against a temp data dir, drives a real session through HTTP/SSE, asserts the expected event stream. No LLM credentials required. |
| **Real-claude E2E** | `./scripts/e2e-claude.sh` | above + `claude` CLI + `llm-bridge-claudecode` on PATH | Same flow but bound to the live `claude_code` harness — exercises a real LLM round-trip end-to-end. Skips cleanly when either binary is missing, so it's safe in CI. |

Run all three to verify a deploy from scratch:

```bash
./scripts/bootstrap.sh && go test ./... && ./scripts/e2e-smoke.sh && ./scripts/e2e-claude.sh
```

The E2E scripts honor a couple of env knobs:

- `E2E_PORT` — server listen port (defaults: 18160 mock, 18161 claude)
- `E2E_LOG_STORE_PORT` — log-store listen port (defaults: 18175 / 18176)
- `E2E_KEEP=1` — leave the temp data dir + logs around for post-mortem
- `E2E_PROMPT` (claude-tier only) — override the prompt sent to claude

### Live pty-mode integration test

The end-to-end pty test in `internal/server/pty_integration_test.go` spawns the real `llm-bridge-claudecode` harness — which `exec`s into the upstream `claude` CLI — inside a pseudoterminal, attaches via WebSocket, round-trips a keystroke through the pty, and stops the session. It's slow (the claude binary takes a moment to come up) and assumes both binaries are installed locally, so it lives behind a build tag and is skipped by `go test ./...`.

Run it explicitly:

```bash
go test -tags pty_integration ./...
```

Prerequisites: `llm-bridge-claudecode` and `claude` must both be on `PATH`. The test skips with a clear message if either is missing — CI runners without claude installed are safe to pass the tag. The test does not assert what claude prints (an authenticated session and an auth-prompt session both produce output), only that bytes flow through the pty in both directions and the session row reaches a terminal state on `/stop`.

### Live convenience-events integration test

The end-to-end convenience-events test in `internal/server/convenience_events_integration_test.go` spawns a real claudecode session, sends a one-shot prompt, and asserts the derived `agent_state` / `usage_total` / `turn_complete` events flow in-band on the SSE feed alongside the raw event stream. Like the pty test, it's slow (claude takes a moment to come up) and assumes both `llm-bridge-claudecode` and `claude` are installed locally, so it lives behind a build tag and is skipped by `go test ./...`.

Run it explicitly:

```bash
go test -tags convenience_events_integration ./...
```

Prerequisites: `llm-bridge-claudecode` and `claude` must both be on `PATH`, and `claude`'s credential storage must be populated (the prompt does a real LLM round-trip). The test skips cleanly when either binary is missing. Assertion contract: at least one `agent_state` transition into `tool_running` and one back to `idle`, exactly one `usage_total` carrying non-zero token counts, and one `turn_complete` whose `turn_id` matches the user_message — it does not pin the interleaving order between `usage_total` and the closing `agent_state`, since the spec leaves that ordering free for consumers.

## Optional stores

Every store is independently usable. The server degrades gracefully when any store is unavailable — it logs a warning and continues without that store's functionality.

| Store | What it adds |
|-------|-------------|
| [agent-store](https://github.com/kayushkin/agent-store) | Agent identity, config, tools, limits, memories |
| [harness-store](https://github.com/kayushkin/harness-store) | Instance registry, credential bindings, SSH transport config |
| [hook-store](https://github.com/kayushkin/hook-store) | Bridge-managed harness hooks (event/matcher → shell command) bound to global, instance, or session scope |
| [memory-store](https://github.com/kayushkin/memory-store) | Persistent vector memory with semantic search |
| [model-store](https://github.com/kayushkin/model-store) | Model registry, aliases, pricing, and health tracking across providers |
| [snapshot-store](https://github.com/kayushkin/snapshot-store) | Point-in-time file snapshots before/after tool calls (Edit/Write) for diff rendering |
| [log-store](https://github.com/kayushkin/log-store) | Durable event log, materialized message history |

## Part of the llm-bridge ecosystem

This server is one component of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. See the llm-bridge README for the full picture — harness bridges, provider bridges, stores, and example consumers.
