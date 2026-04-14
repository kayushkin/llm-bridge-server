# llm-bridge-server

Central HTTP gateway and session server for the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Spawns harness bridges as subprocesses, manages their lifecycle, and streams canonical `msg.Event` output to clients over SSE. Your application connects to this server and gets a uniform API regardless of which agent is running behind the harness.

```
  ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐
    Your Application  (dashboard, CLI, bot, anything)
  └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┬ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘
                          │ HTTP / SSE
  ╔═══════════════════════╪═══════════════════════════╗
  ║        llm-bridge-server                          ║
  ║                       │                           ║
  ║   Sessions ─── lifecycle, events, history         ║
  ║   Instances ── harness deployment registry        ║
  ║   Credentials ─ API key / token management        ║
  ║   Stores ──── agents, memory, models, logs        ║
  ║                       │                           ║
  ╚═══════════════════════╪═══════════════════════════╝
                          │ stdin/stdout NDJSON
  ┌───────────────────────▼─────────────────────────┐
  │              Harness Bridges                    │
  │   claudecode · jig · codex · hermes · aider     │
  │   goose · openclaw · nanoclaw · cline · inber   │
  │   roocode · kilocode · commander · autohand     │
  └─────────────────────────────────────────────────┘
```

## Quick start

### Build and run

```bash
go build -o llm-bridge ./cmd/llm-bridge-server
./llm-bridge
```

The server listens on `:8160` by default.

### Deploy as a systemd service

```bash
./deploy.sh
```

Builds the binary, installs to `/usr/local/bin/llm-bridge`, and restarts the `llm-bridge.service` unit.

### Start a session

```bash
# Create and auto-start a session
curl -X POST http://localhost:8160/sessions \
  -H 'Content-Type: application/json' \
  -d '{"harness": "claudecode", "prompt": "Fix the tests", "autoStart": true}'

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
        fmt.Println(event.Result.Message.Content)
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
| `GET` | `/sessions/{id}` | Get session details |
| `GET` | `/sessions/{id}/events` | SSE event stream (supports `Last-Event-ID` for reconnection) |
| `POST` | `/sessions/{id}/send` | Send a user message |
| `POST` | `/sessions/{id}/interrupt` | Interrupt mid-turn (SIGINT) |
| `POST` | `/sessions/{id}/resume` | Resume a paused session |
| `POST` | `/sessions/{id}/stop` | Terminate a session |
| `POST` | `/sessions/{id}/compact` | Compact context to stay within token limits |
| `POST` | `/sessions/{id}/fork` | Fork from a parent session |
| `POST` | `/sessions/{id}/config` | Update session config on the fly |
| `GET` | `/sessions/discover` | Discover on-disk sessions from harness CLIs |
| `GET` | `/sessions/{id}/messages` | Message history (proxied to log-store) |
| `GET` | `/sessions/{id}/history` | Full history (proxied to log-store) |

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

### Credentials

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/credentials` | List stored credentials (keys masked) |
| `POST` | `/credentials` | Create credential (API key or token) |
| `DELETE` | `/credentials/{id}` | Delete credential |

### Other

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/health` | Server health, available harnesses, session counts |
| `GET` | `/harnesses` | Harness metadata (name, label, emoji, image, capabilities) |
| `GET` | `/models` | Available models with credentials (requires model-store) |
| `GET` | `/bridge-prefs` | User preferences |
| `PUT` | `/bridge-prefs` | Update preferences |

When agent-store and memory-store are loaded, their HTTP handlers are also mounted on the server (see each library for endpoints).

## How it works

### Session lifecycle

1. **Create** — `POST /sessions` creates a session record. With `autoStart: true`, the server spawns the harness binary as a subprocess.
2. **Running** — The harness reads user messages from stdin (JSON) and writes `msg.Event` NDJSON to stdout. The server persists events and fans them out to SSE subscribers.
3. **Streaming** — `GET /sessions/{id}/events` opens an SSE connection. Replays current-turn events on connect, then streams live. Supports `Last-Event-ID` for reconnection.
4. **Interrupt** — `POST /sessions/{id}/interrupt` sends SIGINT. The session pauses and can be resumed.
5. **Resume** — `POST /sessions/{id}/resume` restarts the harness with resume context.
6. **Fork** — `POST /sessions/{id}/fork` creates a child session branching from a parent. The harness clones its state.
7. **Stop** — `POST /sessions/{id}/stop` terminates the subprocess.

### Credential slots

When running sessions on instances (via harness-store), each credential binding has a `maxConcurrent` limit. The server acquires a slot on session start and releases it on stop, preventing credential overuse.

### Auto-discovery

On startup, the server runs harness binaries with `-discover` to find existing on-disk sessions (e.g., Claude Code sessions from `~/.claude/projects/`). Discovered sessions are imported and their history is loaded into log-store.

## Configuration

All configuration is via environment variables with sensible defaults.

| Variable | Default | Description |
|----------|---------|-------------|
| `LLMBRIDGE_LISTEN_ADDR` | `:8160` | HTTP listen address |
| `LLMBRIDGE_DB_PATH` | `~/.llm-bridge/bridge.db` | Bridge SQLite database |
| `LLMBRIDGE_AGENT_DB` | `~/.config/agent-store/agents.db` | Agent store database |
| `LLMBRIDGE_MEMORY_DB` | `~/.config/memory-store/memory.db` | Memory store database |
| `LLMBRIDGE_HARNESS_DB` | `~/.config/harness-store/harness.db` | Harness store database |
| `LLMBRIDGE_MODEL_STORE_DB` | `~/.config/model-store/store.db` | Model store database |
| `LLMBRIDGE_LOG_STORE_URL` | `http://localhost:8175` | Log-store service URL |
| `LLMBRIDGE_BRIDGE_PREFS` | `~/.config/llm-bridge/bridge-prefs.json` | User preferences file |
| `LLMBRIDGE_IMAGES_DIR` | `images` | Static harness image directory |

## Optional stores

Every store is independently usable. The server degrades gracefully when any store is unavailable — it logs a warning and continues without that store's functionality.

| Store | What it adds |
|-------|-------------|
| [agent-store](https://github.com/kayushkin/agent-store) | Agent identity, config, tools, limits, memories |
| [harness-store](https://github.com/kayushkin/harness-store) | Instance registry, credential bindings, SSH transport config |
| [memory-store](https://github.com/kayushkin/memory-store) | Persistent vector memory with semantic search |
| [model-store](https://github.com/kayushkin/model-store) | Model registry, auth, usage tracking across providers |
| [log-store](https://github.com/kayushkin/log-store) | Durable event log, materialized message history |

## Part of the llm-bridge ecosystem

This server is one component of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. See the llm-bridge README for the full picture — harness bridges, provider bridges, stores, and example consumers.
