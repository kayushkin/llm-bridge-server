# llm-bridge-server

The session server of the llm-bridge system, on `:8160`. It starts agent CLIs through harness wrappers, keeps a record of every session, and streams each session's events to clients as `msg.Event` over SSE. A client gets one API whichever agent runs behind a session. bridge-ui (served by dash) is its main client.

For the whole system — which repo owns what, and how the pieces talk — read the [llm-bridge README](https://github.com/kayushkin/llm-bridge#readme) first. `AGENTS.md` here explains why the server works the way it does, for anyone changing its code.

## Boundaries

```
  dash / bridge-ui · scheduler · bridge-agent · llm-bridge-tui
                    │ HTTP, SSE, WebSocket
                    ▼
          ┌──────────────────────┐   HTTP   principal-store, grant-store, kanban-store,
          │  llm-bridge-server   │ ───────► log-store, auth-store, tool-store, bundle-store,
          │        :8160         │          permission-store, skill-store, healthcheck
          └──────────────────────┘
            │ stdin/stdout NDJSON, pty, SSH, or a runner's WebSocket
            ▼
  harness wrapper: llm-bridge-claudecode, -codex, -hermes, …   (one repo each)
            │
            ▼
  the agent: claude, codex, a hermes server, …
```

**It owns** these records, in its own SQLite database (`~/.llm-bridge/bridge.db`): sessions, signals, folders, bridge preferences and its stored settings. Change them only through this server's routes.

**It embeds** six stores as Go libraries. Each keeps its own database and lives in its own repo, so a change to what they store is made there:

| Store | Holds | Required |
|---|---|---|
| model-store | Models, roles, prices | yes |
| agent-store | Agents, tracked context files, the prompt source every harness receives | no |
| harness-store | Machines, instances and their credential bindings | no |
| hook-store | Hooks the bridge wires into a harness | no |
| snapshot-store | File contents before and after each Edit or Write | no |
| memory-store | Agent memories | no |

A store that will not open is logged and its routes are not mounted.

**It asks** these services over HTTP and keeps none of their data:

| Service | Asked for |
|---|---|
| principal-store | Who a caller is, and whether they are an administrator |
| grant-store | Which instances and agents a principal may use, and which tools |
| kanban-store | A card's tags and a board's defaults; also proxied at `/kanban/` |
| log-store | Every session's event history, which it stores and projects |
| auth-store | The credentials bound to an instance |
| tool-store, bundle-store | The MCP servers a session is offered |
| permission-store | Allow or deny on each tool call |
| skill-store | Nothing itself; runners read it through `/api/skill-store/` |
| healthcheck | The service list behind the Services page |

**It does not know** any agent's native protocol. That lives in the harness wrapper, which is the only thing to change when an agent CLI changes. Adding a harness means a new wrapper repo and an entry in `msg.AllHarnesses` in llm-bridge.

## Run it

### Build

```bash
go build -o llm-bridge ./cmd/llm-bridge-server
```

`go.mod` points ten `replace` directives at sibling clones (`../llm-bridge`, `../agent-store`, …), so it builds only next to them, as in `~/repos`. `scripts/bootstrap.sh` clones them on a new machine.

### Settings

The server will not start without these five:

| Variable | What it is |
|---|---|
| `LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY` | At least 32 bytes. Signs login cookies and session agent tokens. |
| `LLMBRIDGE_SERVICE_TOKEN` | At least 32 bytes. Internal callers send it as `X-LLM-Bridge-Service-Token`. |
| `LLMBRIDGE_PRINCIPAL_STORE_URL` | principal-store |
| `LLMBRIDGE_KANBAN_STORE_URL` | kanban-store |
| `LLMBRIDGE_GRANT_STORE_URL` | grant-store |

Every other setting is declared once, in `internal/config/settings.go`, with its default and what it changes. `GET /settings` shows each value in force and what decided it. The server refuses to start on a value that does not parse, and on an `LLMBRIDGE_` variable nothing declares.

On this host the unit is the system unit `llm-bridge.service`; its drop-ins in `/etc/systemd/system/llm-bridge.service.d/` set these.

### Deploy

```bash
./deploy.sh
```

Run it from the main clone on `main`. It passes `~/bin/deploy-gate`, builds, installs `/usr/local/bin/llm-bridge` and restarts the unit, from a separate systemd unit so the restart does not stop it. It logs to `~/.cache/llm-bridge-deploy.log`. ⚠️ **A deploy ends every live bridge session**, including the one that ran it.

### Add a remote machine

```bash
llm-bridge mint-enroll -ttl 15m
```

This prints a single-use passphrase. `llm-bridge-runner` on the other machine trades it for a lasting token at `POST /api/runner/enroll`, then holds a WebSocket open to the server, which starts harnesses through it.

## Use it

A session runs on an enabled *instance* of its harness, so a machine and an instance must exist first (`POST /machines`, then `POST /instances` with `name`, `harness_type` and `machine_id`). Without one, `POST /sessions` answers 503.

```bash
H="X-LLM-Bridge-Service-Token: $LLMBRIDGE_SERVICE_TOKEN"

curl -s -X POST localhost:8160/sessions -H "$H" -H 'Content-Type: application/json' \
  -d '{"harness":"claude_code","auto_start":true}'           # returns {"id": …}

curl -s -X POST localhost:8160/sessions/<id>/send -H "$H" -H 'Content-Type: application/json' \
  -d '{"message":"Fix the tests"}'

curl -sN localhost:8160/sessions/<id>/events -H "$H"         # SSE stream of msg.Event
```

Each SSE `data:` line is one `msg.Event` (`github.com/kayushkin/llm-bridge/msg`; `Event` in `@kayushkin/llm-bridge-types` for TypeScript). The stream replays the current turn on connect and honours `Last-Event-ID`.

`cmd/bridge-agent` wraps these calls: it hands one prompt to a new session with a chosen MCP bundle and prints the final answer.

## Who may call what

Every request is checked before a handler sees it (`internal/server/request_authorization.go`). The last column of the route table below names the rule for each route.

### Callers

1. **Operator**: the service token, or a principal principal-store marks `is_administrator`. May call anything. A wrong token is 401.
2. **The service token on behalf of a person**: the token plus `X-Principal-Id`. dash sends this for its logged-in user, and the request is judged as that person's. `X-Principal-Id` without the token is 401 on every route.
3. **Principal**: a login cookie from `POST /auth/demo-login`, or, on the two store proxies only, a session agent token.
4. **Nobody**: 401 everywhere but the routes open to anyone and the harness callbacks.

Every principal is looked up in principal-store: unknown is 400 `unknown_principal`, a group is 400 `principal_not_human`, disabled is 403 `principal_disabled`, principal-store down is 502. Answers are cached for 30 seconds, so disabling someone takes up to 30 seconds to take effect.

### The rules in the route table

| Rule | Meaning |
|---|---|
| anyone | No credential needed |
| harness callback | Called by a process the server started, or by a runner; carries no login. The handler makes whatever check it makes. ⚠️ The prehooks, sidecar, hook exec and harness proxy check nothing, so do not publish them beyond this host. |
| operator | Service token or administrator; a principal gets 403 `operator_route` |
| principal | Any principal. `GET /instances` lists only the instances its `can_dispatch_on` grants allow |
| principal, as itself | `POST /sessions` creates the session as the caller; naming someone else is 403 `principal_mismatch` |
| principal, own sessions only | The answer holds only the caller's sessions |
| owner | Only the principal who owns the session or signal. Anyone else's, one with no principal and a missing one all answer the same 404 |
| principal or session agent | A login cookie or a session agent token; the request reaches the store as that principal |

A route with no rule is 403 `route_not_classified` to everyone but the service token, and `TestEveryRegisteredRouteIsClassified` fails until it has one. A fork or promoted subagent takes its parent's principal.

### Demo login

⚠️ **A stand-in for a real login, with no password.** Anyone who can reach `POST /auth/demo-login` can sign in as any active human principal by naming its id. Never publish it beyond a network you control.

```bash
curl -s -c jar -X POST localhost:8160/auth/demo-login -H 'Content-Type: application/json' \
  -d '{"principal_id":"principal_000001"}'
```

The `llm_bridge_principal_session` cookie holds the principal id and a 12-hour expiry, signed with HMAC-SHA256. It holds no server state, so a copy taken before logout works until it expires.

### Store proxies

`/kanban/<rest>` goes to kanban-store `/api/<rest>`, and `/grant-store/<rest>` to grant-store `/<rest>`. Each deletes the caller's `X-Principal-Id`, store tokens, service token, `Authorization` header and cookie, then sets `X-Principal-Id` to the verified principal. ⚠️ **kanban-store believes `X-Principal-Id`**, so users must reach it only through here.

### Session agent tokens

A session started as a principal gives its harness process `LLM_BRIDGE_GATEWAY_URL` and `LLM_BRIDGE_PRINCIPAL_TOKEN`, so a tool call inside the session can reach the stores as that principal:

```bash
curl -s "$LLM_BRIDGE_GATEWAY_URL/kanban/boards" -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN"
```

The token lasts 24 hours, is minted at each spawn, and works only on the two store proxies while its session exists and has not ended. Only the local transport can pass these variables, so a principal's session on an SSH or runner instance is refused at spawn. ⚠️ In deployment, kanban-store and grant-store must not be reachable from agent processes directly.

### Secrets never reach a child process

Every process the server starts gets its environment from `internal/childprocessenv`, which removes the signing key, the service token, `GRANT_STORE_SERVICE_TOKEN` and `KANBAN_STORE_SERVICE_TOKEN`; a test fails on any `exec.Command` that skips it. The server also marks itself non-dumpable, so a process running as the same user cannot read its memory. Keep the unit's environment file unreadable by the user agents run as.

## Routes

A route marked *(harness-store)*, *(hook-store)* or *(snapshot-store)* exists only when that store opened. This table is generated from `routeDescriptions` (`internal/server/route_descriptions.go`) and `routeAccessRules`; a test fails when it is out of date. After changing a route, run:

```bash
go test ./internal/server -run TestReadmeRouteTable -update-readme-route-table
```

<!-- route-table:begin (generated by TestReadmeRouteTableMatchesTheRoutes; do not edit by hand) -->
### Sessions

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/effective-config` | Every setting a session would get, and which layer decides it, before it exists | operator |
| `GET` | `/session-events` | SSE stream of changes to the session list | principal, own sessions only |
| `GET` | `/sessions` | List sessions | principal, own sessions only |
| `POST` | `/sessions` | Create a session; `auto_start` starts it | principal, as itself |
| `GET` | `/sessions/aggregates` | Totals across sessions, answered by log-store | operator |
| `GET` | `/sessions/discover` | Import sessions the harness CLIs have on this host's disk | operator |
| `GET` | `/sessions/recent-bundle` | Recent sessions, to warm the chat page | principal, own sessions only |
| `GET` | `/sessions/search` | Full-text search, answered by log-store | operator |
| `GET` | `/sessions/summary` | The chat sidebar's list, projected small | principal, own sessions only |
| `POST` | `/sessions/summary` | The same, with the id lists in the body, since a long query string makes nginx drop the HTTP/2 connection | principal, own sessions only |
| `GET` | `/sessions/validators` | Cheap check of which cached sessions changed | principal, own sessions only |
| `POST` | `/sessions/validators` | The same, with the ids in the body | principal, own sessions only |
| `GET` | `/snapshots/blob/{sha}` | One file snapshot's content (snapshot-store) | operator |

### One session

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/sessions/{id}` | The session record | owner |
| `GET` | `/sessions/{id}/attach` | WebSocket onto a pty-mode session's terminal | owner |
| `GET` | `/sessions/{id}/attach-token` | The token the attach WebSocket needs | owner |
| `POST` | `/sessions/{id}/auto-rename` | The renamer session posts the title it wrote | harness callback |
| `PUT` | `/sessions/{id}/bypass-permissions` | Old boolean form of the above | operator |
| `POST` | `/sessions/{id}/compact` | Compact the context | owner |
| `POST` | `/sessions/{id}/config` | Change model, effort, budget or disabled tools | owner |
| `GET` | `/sessions/{id}/effective-config` | Every setting the session runs with, and which layer decided it | owner |
| `GET` | `/sessions/{id}/entries/{eventId}` | One history entry with its tool input and output in full | owner |
| `GET` | `/sessions/{id}/events` | SSE stream of `msg.Event`; replays the current turn and honours `Last-Event-ID` | owner |
| `PUT` | `/sessions/{id}/folder` | Move into a folder | operator |
| `POST` | `/sessions/{id}/fork` | Branch a new session from this one | owner |
| `GET` | `/sessions/{id}/git` | Status and diff of one of them (`?repo=`) | owner |
| `GET` | `/sessions/{id}/git/repos` | Git repos in the session's directory | owner |
| `GET` | `/sessions/{id}/hooks/pending` | Hooks waiting on a human decision | owner |
| `POST` | `/sessions/{id}/hooks/{request_id}/resolve` | Allow or deny one | owner |
| `POST` | `/sessions/{id}/interrupt` | Stop the current turn | owner |
| `POST` | `/sessions/{id}/mark-done` | Mark done and move to Archive | owner |
| `GET` | `/sessions/{id}/messages` | History, projected by log-store for reading | owner |
| `GET` | `/sessions/{id}/messages/raw` | History with nothing left out, about ten times larger | owner |
| `POST` | `/sessions/{id}/mode` | Switch between events mode and pty mode | owner |
| `PUT` | `/sessions/{id}/permission-mode` | `ask`, `auto` or `bypass` for this session | operator |
| `POST` | `/sessions/{id}/rename` | Set the title | owner |
| `POST` | `/sessions/{id}/resume` | Restart a stopped session with its history | owner |
| `POST` | `/sessions/{id}/send` | Send a message | owner |
| `POST` | `/sessions/{id}/stop` | End the session | owner |
| `GET` | `/sessions/{id}/tools/{tool_use_id}/snapshots` | File snapshots before and after an Edit or Write (snapshot-store) | owner |

### Signals

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/sessions/{id}/signals` | One session's signals | owner |
| `POST` | `/sessions/{id}/signals` | A session raises a notice about itself | owner |
| `GET` | `/signals` | The inbox across sessions (`?state=open`) | principal, own sessions only |
| `POST` | `/signals/{id}/answer` | Answer a question, whether or not its session still runs | owner |
| `POST` | `/signals/{id}/resolve` | Acknowledge or dismiss | owner |

### Machines, instances and credentials

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/credentials` | List auth-store credentials, keys masked | operator |
| `POST` | `/credentials` | Add a credential to auth-store | operator |
| `DELETE` | `/credentials/{id}` | Delete one | operator |
| `GET` | `/instances` | List instances (harness-store) | principal |
| `POST` | `/instances` | Add an instance (harness-store) | operator |
| `DELETE` | `/instances/{id}` | Remove an instance (harness-store) | operator |
| `GET` | `/instances/{id}` | One instance (harness-store) | operator |
| `PUT` | `/instances/{id}` | Change an instance (harness-store) | operator |
| `GET` | `/instances/{id}/credentials` | Bound credentials (harness-store) | operator |
| `POST` | `/instances/{id}/credentials` | Bind a credential (harness-store) | operator |
| `DELETE` | `/instances/{id}/credentials/{cred_id}` | Unbind one (harness-store) | operator |
| `POST` | `/instances/{id}/oneshot` | One model call on the instance, with no session (harness-store) | operator |
| `GET` | `/instances/{id}/sessions` | Its sessions (harness-store) | operator |
| `GET` | `/instances/{id}/status` | Live sessions and credential state (harness-store) | operator |
| `GET` | `/machines` | List machines (harness-store) | operator |
| `POST` | `/machines` | Add a machine (harness-store) | operator |
| `DELETE` | `/machines/{id}` | Remove a machine (harness-store) | operator |
| `GET` | `/machines/{id}` | One machine (harness-store) | operator |
| `PUT` | `/machines/{id}` | Change a machine (harness-store) | operator |

### Hooks

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/hook-options` | Which harnesses take hooks, their events and scopes (hook-store) | operator |
| `GET` | `/hooks` | List hooks (hook-store) | operator |
| `POST` | `/hooks` | Create a hook (hook-store) | operator |
| `DELETE` | `/hooks/{id}` | Delete one (hook-store) | operator |
| `GET` | `/hooks/{id}` | One hook (hook-store) | operator |
| `PATCH` | `/hooks/{id}` | Change one (hook-store) | operator |

### Harnesses, models and settings

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/bridge-prefs` | Stored preferences: per-harness defaults, default principal, … | operator |
| `PUT` | `/bridge-prefs` | Change them | operator |
| `POST` | `/bridge/bypass-permissions` | Old boolean form of the above | operator |
| `POST` | `/bridge/permission-mode` | Default permission mode for new sessions | operator |
| `GET` | `/conformance` | Latest capability matrix across harnesses | operator |
| `POST` | `/conformance/run` | Start a conformance run | operator |
| `GET` | `/harnesses` | Each harness's name, label, image, capabilities and hook events | principal |
| `GET` | `/harnesses/{name}/agents` | Its named agents | principal |
| `GET` | `/harnesses/{name}/capabilities` | One harness's capabilities | principal |
| `GET` | `/health` | Health, harnesses present, session counts | anyone |
| `*` | `/images/` | Harness images | principal |
| `GET` | `/models` | Models there are credentials for | operator |
| `GET` | `/settings` | Every server setting, its value and what decided it | operator |
| `PUT` | `/settings/{key}` | Change a stored setting without a restart | operator |

### Folders, labels and housekeeping

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `POST` | `/admin/archive-old` | Archive old sessions; a scheduler job calls it | operator |
| `POST` | `/admin/file-inactive` | File inactive sessions; a scheduler job calls it | operator |
| `GET` | `/folders` | List folders | operator |
| `POST` | `/folders` | Create one | operator |
| `DELETE` | `/folders/{name}` | Delete one | operator |
| `PUT` | `/folders/{name}` | Rename one | operator |
| `GET` | `/session-taxonomy` | The types and purposes a session may carry | principal |
| `GET` | `/session-taxonomy/report` | Sessions whose labels break that vocabulary | operator |
| `GET` | `/source-folders` | Which folder each session `source` files into | operator |
| `DELETE` | `/source-folders/{source}` | Clear one | operator |
| `PUT` | `/source-folders/{source}` | Set one | operator |

### Services page

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/services` | Each service healthcheck watches, its status, processes and open SQLite files | operator |
| `GET` | `/services/databases/rows` | Newest rows of one table (`?path=&table=&filter=col:op:value`) | operator |
| `GET` | `/services/databases/schema` | Tables of one open file (`?path=`) | operator |

### Login and store proxies

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `POST` | `/auth/demo-login` | Sign in as a principal by id, with no password | anyone |
| `POST` | `/auth/logout` | Clear the cookie | anyone |
| `GET` | `/auth/principal` | Who the login cookie names | anyone |
| `*` | `/grant-store/` | grant-store `/…`, as the caller | principal or session agent |
| `*` | `/kanban/` | kanban-store `/api/…`, as the caller | principal or session agent |

### Runners and harness callbacks

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `*` | `/api/agent-store/` | Runners read agent-store through this | harness callback |
| `*` | `/api/harness-proxy/{harness}/{rest...}` | Forward to a harness backend (inber, hermes) on this host. Checks no credential; nothing calls it | harness callback |
| `GET` | `/api/runner/binary` | Runner and wrapper binaries (`?os=&arch=&name=`) | harness callback |
| `POST` | `/api/runner/enroll` | Trade a passphrase for a runner token | harness callback |
| `GET` | `/api/runner/install.sh` | Runner install script | harness callback |
| `POST` | `/api/runner/seed/broadcast` | Tell every runner to re-sync agent and skill files | operator |
| `GET` | `/api/runner/ws` | A runner's WebSocket | harness callback |
| `*` | `/api/skill-store/` | Runners read skill-store through this | harness callback |
| `POST` | `/hooks/exec/{id}` | A harness runs a registered hook (hook-store) | harness callback |
| `POST` | `/permission/cc-prehook/{bridge_id}` | Claude Code's permission check before each tool call | harness callback |
| `POST` | `/permission/codex-prehook/{bridge_id}` | The same for codex | harness callback |
| `POST` | `/sidecar/event/{bridge_id}` | Events from a pty-mode session's sidecar | harness callback |

### agent-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/agents` | List agents | principal |
| `POST` | `/agents` | Create an agent | operator |
| `DELETE` | `/agents/{slug}` | Delete one | operator |
| `GET` | `/agents/{slug}` | One agent | principal |
| `PUT` | `/agents/{slug}` | Change one | operator |
| `GET` | `/agents/{slug}/config` | An agent's runtime config | operator |
| `GET` | `/agents/{slug}/harnesses` | Harnesses an agent runs on | operator |
| `POST` | `/agents/{slug}/harnesses` | Bind an agent to a harness | operator |
| `GET` | `/configs` | Every runtime config | operator |
| `GET` | `/context/resolve` | The prompt a harness gets for a directory and tags (`?harness=&work_dir=&tag=`) | operator |
| `GET` | `/files` | Tracked context files | operator |
| `POST` | `/files/scan` | Scan disk for context files | operator |
| `GET` | `/files/{id}` | One tracked file | operator |
| `GET` | `/files/{id}/content` | Its content | operator |
| `PUT` | `/files/{id}/content` | Write its content | operator |
| `POST` | `/files/{id}/disable` | Stop tracking it | operator |
| `POST` | `/files/{id}/enable` | Track a file | operator |
| `GET` | `/files/{id}/versions` | Its saved versions | operator |
| `GET` | `/reconcile` | Problems in each agent's harness bindings | operator |
| `POST` | `/seed/drift` | A runner saves its local edit before overwriting the file | operator |
| `GET` | `/seed/manifest` | The files a runner should hold | operator |
| `POST` | `/seed/observe` | A runner reports the hash of a file it holds | operator |
| `GET` | `/seed/profile` | One runner's seed profile | operator |
| `PUT` | `/seed/profile` | Set it | operator |
| `GET` | `/seed/profiles` | Runner seed profiles | operator |
| `GET` | `/seed/state` | Seed state across runners | operator |
| `GET` | `/tracked-file-ignore-rules` | Paths the scan skips | operator |
| `POST` | `/tracked-file-ignore-rules` | Add one | operator |
| `POST` | `/tracked-file-ignore-rules/{id}/disable` | Turn one off | operator |
| `POST` | `/tracked-file-ignore-rules/{id}/enable` | Turn one on | operator |
| `GET` | `/versions/{vid}/content` | One version's content | operator |

### agent-store prompt source, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/prompt-collections` | Prompt collections: the host one and one per repo | operator |
| `POST` | `/prompt-collections` | Create one | operator |
| `POST` | `/prompt-collections/import-untracked` | Import prompt files not yet in a collection | operator |
| `GET` | `/prompt-collections/{id}` | One collection and its sections | operator |
| `POST` | `/prompt-collections/{id}/outputs` | Add a file the collection renders to | operator |
| `POST` | `/prompt-collections/{id}/render` | Write the collection's files to disk | operator |
| `GET` | `/prompt-collections/{id}/revisions` | Its history | operator |
| `POST` | `/prompt-collections/{id}/sections` | Add a section | operator |
| `GET` | `/prompt-delivery-options` | The ways a harness can get it | operator |
| `GET` | `/prompt-drifts` | Edits found in rendered files | operator |
| `POST` | `/prompt-drifts/reconcile` | Compare rendered files with the sections now | operator |
| `GET` | `/prompt-drifts/{id}` | One drift | operator |
| `PUT` | `/prompt-drifts/{id}/annotation` | Note on a drift | operator |
| `POST` | `/prompt-drifts/{id}/apply` | Take the file's edit into the sections | operator |
| `GET` | `/prompt-drifts/{id}/disk-content` | The file as it is on disk | operator |
| `POST` | `/prompt-drifts/{id}/dismiss` | Drop the edit | operator |
| `GET` | `/prompt-harness-deliveries` | How each harness gets the prompt | operator |
| `PUT` | `/prompt-harness-deliveries/{harness}` | Set it for one harness | operator |
| `POST` | `/prompt-outputs/{id}/disable` | Turn it off | operator |
| `POST` | `/prompt-outputs/{id}/enable` | Turn a rendered file on | operator |
| `DELETE` | `/prompt-sections/{id}` | Delete a section | operator |
| `PUT` | `/prompt-sections/{id}` | Change a section | operator |
| `GET` | `/prompt-sections/{id}/revisions` | A section's history | operator |

### memory-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `POST` | `/memories` | Save a memory | operator |
| `POST` | `/memories/compact` | Compact old memories | operator |
| `POST` | `/memories/context` | Memories to put in a prompt | operator |
| `POST` | `/memories/decay` | Lower importance with age | operator |
| `GET` | `/memories/recent` | Recent memories | operator |
| `POST` | `/memories/search` | Search | operator |
| `DELETE` | `/memories/{id}` | Delete one | operator |
| `GET` | `/memories/{id}` | One memory | operator |
<!-- route-table:end -->

## Testing

| Command | Needs | Covers |
|---|---|---|
| `go test ./...` | nothing | Unit tests, and the conformance matrix against `cmd/mock-harness` |
| `go test -tags pty_integration ./...` | `claude` and `llm-bridge-claudecode` on `PATH` | A real pty-mode session: attach, a keystroke each way, stop |
| `go test -tags convenience_events_integration ./...` | the same, signed in | A real turn, checking the `agent_state`, `usage_total` and `turn_complete` events |

The tagged tests skip when a binary is missing.

⚠️ `scripts/e2e-smoke.sh` and `scripts/e2e-claude.sh` do not set the five required settings, so the server they start refuses to run.

## Docs

`docs/` describes how parts of the server work now:

- `HARNESS-LAYER.md`: one interface over every harness
- `TOOL-ROUTING.md`: how tools and skills reach a session
- `AGENT-MANAGEMENT.md`: how an agent record becomes harness config
- `SESSION-SIGNALS.md`: questions and notices a session raises
- `SESSION-STATE-RELIABILITY.md`: how session state is worked out from events
- `PTY-MODE.md`: pty-mode sessions
- `CACHE-RULES.md`: what may and may not break prompt caching
- `CONFORMANCE-GRADING.md`: how the conformance matrix is graded
- `CC-VERIFIED.md`: Claude Code flags and behaviour, checked by hand

`docs/plans/` holds plans, some done in part and some not started: `CODEX-PARITY`, `IMPLEMENTATION-ROADMAP`, `CONTEXT-MIGRATION`, `TEAM-ORCHESTRATION`, `CLI-SURFACE`. Check a plan against the code before relying on it.
