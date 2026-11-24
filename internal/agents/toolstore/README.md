# tool-store

A registry of tools that can be seeded into AI coding agents and harnesses. Written in Go, with a small SQLite-backed HTTP service.

AI agents need tools — shell, file editing, search, browsers, custom CLIs, MCP servers. Each harness (Claude Code, Codex, Aider, etc.) configures these differently. tool-store provides one place to register what's available, what each tool's input schema is, and how it's invoked. Harnesses and orchestrators read from tool-store on session bring-up and provision themselves.

Every component is part of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. Use it standalone, or as a store inside [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server).

## How it works

```
  ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐
    Your Application  (dashboard, agent, harness)
  └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┬ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘
                            │ HTTP (REST + JSON)
  ╔═════════════════════════╪═════════════════════════════╗
  ║                  tool-store                           ║
  ║                         │                             ║
  ║   ┌─────────────────────▼───────────────────────┐     ║
  ║   │           HTTP service (:8302)              │     ║
  ║   │                                             │     ║
  ║   │  Registry: list, upsert, enable/disable,    │     ║
  ║   │    fetch by id or name, fetch spec          │     ║
  ║   │                                             │     ║
  ║   │  Discovery: GET /locals                     │     ║
  ║   │  Invocation: POST /tools/by-name/{n}/invoke │     ║
  ║   └─────────────────────┬───────────────────────┘     ║
  ║                         │                             ║
  ║   ┌─────────────────────▼───────────────────────┐     ║
  ║   │              SQLite store                   │     ║
  ║   │   ~/.config/tool-store/tool-store.db        │     ║
  ║   └─────────────────────┬───────────────────────┘     ║
  ║                         │                             ║
  ║   ┌─────────────────────▼───────────────────────┐     ║
  ║   │          In-process tool registry           │     ║
  ║   │   Go-native impls registered via init()     │     ║
  ║   │   (shell, browser, web_search, fs, …)       │     ║
  ║   └─────────────────────────────────────────────┘     ║
  ╚═══════════════════════════════════════════════════════╝
```

A tool registered in tool-store can be one of four **kinds**, each with a different execution path. `GET /kinds` serves the list with a line on each; `Kinds` in `tool.go` is the one copy of it.

| Kind | What it is | How tool-store handles it |
|------|------------|---------------------------|
| `local` | A Go function compiled into the tool-store binary | Invoked in-process via `POST /tools/by-name/{name}/invoke` |
| `cli`   | An arbitrary command + argv template | tool-store substitutes `{{var}}` placeholders from the input JSON, execs the binary, and returns stdout |
| `mcp`   | An external MCP server (stdio / http / sse) | Not invoked here — clients fetch the launcher spec via `/tools/by-name/{name}/spec` and spawn it themselves (e.g. Claude Code via `--mcp-config`) |
| `harness` | A built-in tool of an agent harness (Claude Code's `Read`, Codex's `shell_tool`) | Not run here, and never — the harness runs it. tool-store records it and its `enabled` flag so a caller can switch it off. `/invoke` and `/spec` answer 409, `/provision` refuses it |

tool-store seeds its own registry on startup with every in-process tool the binary ships and a short list of MCP servers, and every such new row starts **disabled**. It also seeds the built-in tools of Claude Code and Codex as `kind=harness` rows, and those start **enabled**, because the harness offers them unless told otherwise; the seeder only creates harness rows that are missing and never changes one that exists, so the database owns each harness tool's tags, `enabled` flag and description; change a live row with `PATCH /tools/{id}`. Operators (or eventually a UI) explicitly enable a tool when they want it. Existing rows preserve whatever enabled state they have, so a deliberate enable or disable survives restarts. Discovery via `GET /locals` shows what's available regardless of registration state.

## What you get

| Capability | Description |
|------------|-------------|
| **Unified registry** | One source of truth for tools across harnesses, regardless of kind |
| **Multi-kind dispatch** | Local Go, arbitrary CLI, and external MCP all sit under one schema |
| **Safe templating** | `{{var}}` substitution in CLI argv with no shell expansion — no injection vector |
| **Discovery** | `GET /locals` lists every in-process tool the binary ships, ready to enable |
| **Idempotent CRUD** | Upsert by name, enable/disable, list with kind/tag/query filters |
| **No coupling to providers** | Schemas are plain JSON Schema — no Anthropic SDK or other LLM types in the wire format |
| **Single source of credentials** | Tools declare `env_keys`, never values — secrets resolve at provision time from your existing credential store |

## Packages

### `toolstore` — Registry types and HTTP handlers

```go
import toolstore "github.com/kayushkin/tool-store"
```

- `Tool`, `Kind` (`mcp` | `cli` | `local` | `harness`), `Kinds`, `KindDescriptor`, `MCPSpec`, `CLISpec`, `LocalSpec`, `HarnessToolRowName`
- `Open(dataDir) (*Store, error)` — opens the SQLite-backed store
- `UpsertTool`, `GetTool`, `GetToolByName`, `ListTools`, `DeleteTool`, `SetEnabled`, `PatchTool`, `RecordObservedHarnessTools`
- `RegisterHandlers(mux, store, HandlerOptions)` — wires the REST API onto an `*http.ServeMux`
- `HandlerOptions` — pluggable callbacks: `InvokeLocal`, `ListLocals`. Both optional.

### `schema` — JSON Schema helpers

```go
import "github.com/kayushkin/tool-store/schema"
```

A no-dependency builder for tool input schemas. `Props(required, properties)` returns an `InputSchema` that JSON-marshals to standard `{"type":"object","properties":{...},"required":[...]}` — accepted as-is by Anthropic, OpenAI, Google, and MCP. Helpers: `Str`, `Integer`, `Bool`, `Array`. `Parse[T]` unmarshals an input string into a typed struct.

### `tools` — In-process Go tool implementations

```go
import "github.com/kayushkin/tool-store/tools"
```

The Go-native tool implementations that ship inside `tool-store`. Each is an `Impl{ Name, Description, InputSchema, Run }`. Those that need nothing from their caller register themselves via `init()`. Consumers call `Register`, `ByName`, `All`.

| Tool | Description |
|------|-------------|
| `shell_commands` | Run one or more bash commands |
| `read_files`, `write_files`, `edit_files`, `list_files` | File operations |
| `ripgrep` | Pattern search via ripgrep |
| `web_search` | Brave Search API |
| `web_fetch` | Fetch URL and extract readable text |
| `browser` | Browser automation via PinchTab |
| `scheduler` | Manage cron jobs via the scheduler service |
| `end_turn` | Signal end of an agent turn |

`browser`, `web_search` and `scheduler` reach an outside service, and the package reads no environment variable to find it: build them with `Browser(PinchtabConnection)`, `WebSearch(braveAPIKey)` and `Scheduler(SchedulerConnection)`, or register all three with `RegisterToolsThatReachOutsideServices(OutsideServiceConnections)`.

Context-bearing tools (`recent_files`, `repo_map`, `scratchpad`, `task_plan`) are exposed as factories rather than auto-registered — they need a per-instance `repoRoot` / `agentName` and are typically wired from above (e.g. by [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) at session bring-up).

### `cmd/tool-store` — HTTP service

```bash
go build ./cmd/tool-store
./tool-store
# or via systemd (see deploy.sh)
```

Starts an HTTP server (default `:8302`) backed by SQLite at `~/.config/tool-store/tool-store.db`. Wires the `tools` package into `RegisterHandlers` so local tools are invokable in-process and discoverable via `GET /locals`.

Environment — every variable is declared once in `settings.go`, and `GET /settings` shows the values in force (a secret shows only whether it is set):
- `TOOL_STORE_ADDR` — listen address (default `:8302`)
- `TOOL_STORE_DATA_DIR` — data directory (default `~/.config/tool-store`)
- `AUTH_STORE_URL`, `AUTH_STORE_TOKEN` — where `POST /provision` resolves credentials (default `http://127.0.0.1:8303`)
- `PINCHTAB_URL`, `PINCHTAB_TOKEN` — the `browser` tool's PinchTab (default `http://localhost:9867`)
- `BRAVE_API_KEY` — the in-process `web_search` tool's key
- `SCHEDULER_URL`, `SCHEDULER_TOKEN` — the `scheduler` tool's scheduler (default `http://localhost:8092`)

A set variable that begins `TOOL_STORE_ADDR` or `TOOL_STORE_DATA_DIR` and is not one of those two stops the start: it is a misspelling.

## HTTP API

```
GET    /health
GET    /settings                               every environment variable the process reads, and the value in force
GET    /kinds                                  the tool kinds, each with a one-line description
GET    /tools                                  ?kind=&harness=&tag=&q=&enabled=true&not_seen_since=&limit=  (a bare JSON array of Tool)
POST   /tools                                  body: full Tool JSON (upsert by name; never writes last_seen_at)
GET    /tools/{id}
PATCH  /tools/{id}                             body: any of {"tags":[…],"description":"…","enabled":bool}; unknown field, null or {} is 400
DELETE /tools/{id}
POST   /tools/{id}/enable
POST   /tools/{id}/disable
GET    /tools/by-name/{name}
GET    /tools/by-name/{name}/spec              CLI/MCP launcher spec (for harnesses that run tools themselves); 409 for kind=harness
POST   /tools/by-name/{name}/invoke            body: input JSON; only valid for kind=local or kind=cli; 409 for kind=harness
GET    /locals                                 in-process registry (what's available to enable)

POST   /harness-tools/observed                 body: {"harness":"claude_code","tool_names":["Read",…]} → {"created":[…],"seen":N}

GET    /instances/{id}/tools                   tools opted-in for this harness instance
POST   /instances/{id}/tools/by-name/{name}    enable a tool for this instance
DELETE /instances/{id}/tools/by-name/{name}    disable a tool for this instance
GET    /tools/by-name/{name}/instances         every instance opted into this tool
```

### Harness tools

A `kind=harness` row is a built-in tool of an agent harness. Two fields carry it, and every other kind leaves both empty:

- `harness` — the llm-bridge harness id (`claude_code`, `codex`). llm-bridge-server owns these ids, so `?harness=` filters by the exact string and an unknown one matches nothing rather than failing.
- `harness_tool_name` — the name the harness itself uses (`Read`, `shell_tool`).

The row's `name` must be `<harness>.<harness_tool_name>` (`claude_code.Read`, `codex.web_search`), which keeps it apart from a local tool with the same bare name; both the store and a CHECK in `schema.sql` refuse anything else. Tags say what a tool does where that is plain: `read-only`, `effects` (changes files, runs commands, sends, schedules or reaches the network), and `runs-commands` on the tools that run shell commands (`claude_code.Bash`, `claude_code.Monitor`, `codex.shell_tool`, `codex.unified_exec`).

```bash
curl -s "http://localhost:8302/tools?kind=harness&harness=claude_code" | jq '.[] | {id, name, harness_tool_name, enabled, tags}'
```

A database made before the harness kind is rebuilt on the first open (`migrate.go`): every row keeps its id, and the `instance_tools` rows with it. A database made before `last_seen_at` gets that column, 0 on every row.

### Harness tools nobody registered

A harness reports the built-in tools it offers when a session starts; llm-bridge-server forwards Claude Code's list to `POST /harness-tools/observed`:

```bash
curl -s -X POST http://localhost:8302/harness-tools/observed -H 'content-type: application/json' \
  -d '{"harness":"claude_code","tool_names":["Read","Bash"]}'
# {"created":[],"seen":2}
```

- A name with a row has its `last_seen_at` (unix seconds; 0 means never reported) set to now, and nothing else changes.
- A name with no row gets one: `kind=harness`, **`enabled: false`**, tags `["unreviewed"]`, and a description naming the harness and the time. llm-bridge-server switches off every disabled harness tool in every session, so a tool nobody registered stays off until a person reviews it.
- `harness` is passed through as sent (llm-bridge-server owns harness ids). An empty harness, an empty list, or a name that is empty, contains `.` or whitespace, or starts with `mcp__` is a 400 and nothing is written. A name another kind already holds is a 409.
- One write transaction covers a report and each insert is `ON CONFLICT DO NOTHING`, so sessions reporting the same new name at once make one row and all get 200.

The seeder only creates missing rows from `cmd/tool-store/harness_seeds.go`, so it never re-enables or re-tags any existing row, reported or seeded. If a reported name is later added to the seeds, the row keeps its `enabled=false` and its `unreviewed` tag until a person reviews it.

To review a tool, replace its tags and, if it should be on, enable it:

```bash
curl -s "http://localhost:8302/tools?kind=harness&tag=unreviewed"
curl -s -X PATCH http://localhost:8302/tools/42 -H 'content-type: application/json' -d '{"tags":["read-only"]}'
curl -s -X POST  http://localhost:8302/tools/42/enable
```

`GET /tools?kind=harness&not_seen_since=<unix>` lists harness rows whose `last_seen_at` is older than that, never-reported rows included; it is a 400 without `kind=harness`.

`scripts/harness-tool-review.sh` is the daily report (scheduler shell job `harness-tool-review`, 08:30). It needs `TOOL_STORE_URL`, `NOTEBOARD_URL` and `codex` on `PATH`, and exits non-zero if it cannot reach any of them. It lists unreviewed rows, claude_code rows not reported for 14 days (a never-reported row only once reports have been arriving for 14 days), and features that appeared, disappeared or changed stage or default in `codex features list` since the last run. It keeps one open noteboard todo tagged `harness-tool-review`, rewriting its body while there is something to report, and keeps the last codex list in one workspace tagged `harness-tool-review-state`. It never creates tool-store rows for codex features.

### Per-instance opt-in

Tools have a global `enabled` flag and a per-instance opt-in. **Global is master**: a globally-disabled tool can't be opted in (the API returns 409). When a tool is globally enabled, no instance has it until you explicitly opt in:

```bash
# Enable globally
curl -s -X POST http://localhost:8302/tools/3/enable

# Opt instance "main-1" in
curl -s -X POST http://localhost:8302/instances/main-1/tools/by-name/brave-search

# What's enabled for this instance?
curl -s http://localhost:8302/instances/main-1/tools

# Where is brave-search active?
curl -s http://localhost:8302/tools/by-name/brave-search/instances
```

If a tool is globally re-disabled, its rows in `instance_tools` are preserved but `GET /instances/{id}/tools` filters them out — re-enabling globally restores the prior opt-ins.

## Quick start

### Discover what's available

```bash
curl -s http://localhost:8302/locals | jq '.[].name'
```

### Register a local tool

```bash
curl -s -X POST http://localhost:8302/tools -H 'content-type: application/json' -d '{
  "name": "shell_commands",
  "description": "Run one or more bash commands",
  "kind": "local",
  "local": {"symbol": "shell_commands"},
  "enabled": true
}'
```

### Register a CLI tool

```bash
curl -s -X POST http://localhost:8302/tools -H 'content-type: application/json' -d '{
  "name": "ls",
  "description": "List a directory",
  "kind": "cli",
  "cli": {
    "command": "ls",
    "args_template": ["-la", "{{path}}"],
    "timeout_ms": 5000
  },
  "input_schema": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]},
  "enabled": true
}'
```

### Register an MCP tool

```bash
curl -s -X POST http://localhost:8302/tools -H 'content-type: application/json' -d '{
  "name": "brave-search",
  "description": "Web search via Brave Search API",
  "kind": "mcp",
  "env_keys": ["BRAVE_API_KEY"],
  "mcp": {
    "transport": "stdio",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-brave-search"]
  },
  "enabled": true
}'
```

### Invoke a tool

```bash
curl -s -X POST http://localhost:8302/tools/by-name/shell_commands/invoke \
  -H 'content-type: application/json' \
  -d '{"command":"echo hello"}'
# {"output":"hello\n"}
```

### Fetch a launcher spec for an MCP

```bash
curl -s http://localhost:8302/tools/by-name/brave-search/spec
# {"transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-brave-search"]}
```

## llm-bridge-server integration

When [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) creates a Claude Code session, it can opt the session into tool-store provisioning by passing a `tool_store_tools` field inside `HarnessConfig`:

```jsonc
POST /sessions
{
  "harness": "claudecode",
  "harness_config": {
    "tool_store_tools": ["brave-search", "playwright"]
  }
}
```

llm-bridge-server's `injectMCPConfig` step calls `POST :8302/provision`, writes the response to a tmpfile, replaces the field with `mcp_config: <path>`, and the claudecode harness picks the path up via its existing `--mcp-config` flag. No msg-type changes required — the contract rides through the opaque `HarnessConfig` blob. Setting both `tool_store_tools` and `mcp_config` is rejected (single source of truth).

A session that names no tools of its own gets whatever its **instance** has been opted into — the rows the Tools page writes through `POST /instances/{id}/tools/by-name/{name}`. llm-bridge-server asks for those by instance instead of by name:

```jsonc
POST /provision
{ "instance_id": "inst-cc-local" }
// → {"mcpServers": { … the instance's opted-in MCP tools … }}
```

A caller that already holds tool-store ids — llm-bridge-server, intersecting a principal's grant-store grants with an instance's opt-ins, has ids on both sides — names them by id instead:

```jsonc
POST /provision
{ "tool_ids": [13, 14] }
```

Exactly one of `tools`, `tool_ids` and `instance_id` is required; a request carrying more than one is rejected, because merging a standing preference with a per-call list gives the same field two sources of truth. They differ in how they treat a tool that is not an MCP server: `tools` and `tool_ids` name it outright, so a CLI or in-process local there is an error, while an instance's opt-in list legitimately spans every kind of tool, so the MCP subset is selected out of it. An instance nobody has ticked anything for provisions nothing and says so with `200 {}`, which is how a caller tells "no opt-ins" apart from "the lookup broke".

## Design principles

- **Disabled by default, except what the harness already has.** The registry seeds itself with every in-process tool, and every new row of those starts disabled. Enabling is the deliberate act — no tool tool-store runs is silently active. Harness tools start enabled, because the harness offers them whatever tool-store says; the row makes switching one off possible.
- **Layers are transparent.** Input JSON, output strings, and launcher specs pass through unchanged. CLI substitution is literal — no shell, no quoting heuristics.
- **Single source of credentials.** Tools declare which env vars they need (`env_keys`); the values come from the canonical credential store at provision time, never from tool-store rows.
- **Four kinds, one shape.** `local`, `cli`, `mcp` and `harness` share the same `Tool` schema. Consumers branch on `kind` only when they have to.
- **Independent of any LLM SDK.** The schema is plain JSON Schema. Bring your own provider.
