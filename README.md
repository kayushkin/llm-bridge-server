# llm-bridge-server

The session server and HTTP gateway of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. It starts harness wrappers as child processes, keeps each session's record, and streams every session's `msg.Event` output to clients over SSE, so a client gets one API whichever agent CLI runs behind the session. It is the one backend behind bridge-ui.

```
  bridge-ui, dash, scheduler, bridge-agent, …
                  │  HTTP / SSE / WebSocket
                  ▼
        ┌────────────────────┐        principal-store  (who is calling)
        │ llm-bridge-server  │ ─────► grant-store      (what they may run)
        │       :8160        │        kanban-store     (cards, boards)
        └────────────────────┘        log-store        (event history)
          │ stdin/stdout NDJSON, a pty, SSH or a runner WebSocket
          ▼
  harness wrapper (llm-bridge-claudecode, -codex, -hermes, …)
          │ spawns or connects to
          ▼
  the agent itself (claude, codex, a hermes server, …)
```

Each harness wrapper is a separate binary. The wrapper alone speaks the agent's native protocol; the server sees only `msg.Event`.

## Running it

### Build

`go.mod` points ten `replace` directives at sibling checkouts (`../llm-bridge`, `../agent-store`, …), so the repo builds only next to them. On this host they already sit in `~/repos`. On a fresh machine, `./scripts/bootstrap.sh` clones them.

```bash
go build -o llm-bridge ./cmd/llm-bridge-server
```

### Settings the server will not start without

| Variable | What it is |
|---|---|
| `LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY` | At least 32 bytes. Signs login cookies and session agent tokens. |
| `LLMBRIDGE_SERVICE_TOKEN` | At least 32 bytes. An internal caller sends it as `X-LLM-Bridge-Service-Token`. |
| `LLMBRIDGE_PRINCIPAL_STORE_URL` | principal-store, which says who each caller is. |
| `LLMBRIDGE_KANBAN_STORE_URL` | kanban-store, behind `/kanban/`. |
| `LLMBRIDGE_GRANT_STORE_URL` | grant-store, behind `/grant-store/`. |

model-store's database is also required. The other embedded stores (agent, memory, harness, hook, snapshot) are optional: one that will not open is logged and its routes are not mounted.

Every other setting is declared once, in `internal/config/settings.go`, with its default and what it changes. `GET /settings` lists each value in force and what decided it. The server refuses to start on a value that does not parse, or on an `LLMBRIDGE_` variable nothing declares.

### Deploy

```bash
./deploy.sh
```

Run it from the main clone on `main`. It passes `~/bin/deploy-gate`, builds, installs `/usr/local/bin/llm-bridge` and restarts the system unit `llm-bridge.service`. It runs itself in a separate systemd unit so that stopping the service does not stop the deploy, and logs to `~/.cache/llm-bridge-deploy.log`. ⚠️ A deploy ends every live bridge session, including the one that ran it.

### Mint a runner enrollment passphrase

```bash
llm-bridge mint-enroll -ttl 15m
```

It prints a single-use passphrase and stores only its hash in harness-store. A runner on another machine trades the passphrase for a lasting token at `POST /api/runner/enroll`.

### Start a session

A session runs on an enabled *instance* of its harness, so enroll a machine and an instance first (`POST /machines`, then `POST /instances` with `name`, `harness_type` and `machine_id`). Without one, `POST /sessions` answers 503.

```bash
H="X-LLM-Bridge-Service-Token: $LLMBRIDGE_SERVICE_TOKEN"

curl -s -X POST localhost:8160/sessions -H "$H" -H 'Content-Type: application/json' \
  -d '{"harness":"claude_code","auto_start":true}'           # returns {"id": …}

curl -s -X POST localhost:8160/sessions/<id>/send -H "$H" -H 'Content-Type: application/json' \
  -d '{"message":"Fix the tests"}'

curl -sN localhost:8160/sessions/<id>/events -H "$H"         # SSE stream of msg.Event
```

A browser signs in with `POST /auth/demo-login` instead and sends the cookie it gets back; see [Who may call what](#who-may-call-what).

`cmd/bridge-agent` wraps these calls: it hands one prompt to a new session carrying a chosen MCP bundle and prints the agent's final answer.

### Read events

In Go, each SSE `data:` line is one `msg.Event` from `github.com/kayushkin/llm-bridge/msg`; switch on `event.Type` (`msg.EventResult`, `msg.EventToolCall`, `msg.EventApproval`, …). In TypeScript the same type is `Event` from `@kayushkin/llm-bridge-types`. The stream replays the current turn on connect and honours `Last-Event-ID`.

## API

Every route below also needs the caller to be allowed on it; see [Who may call what](#who-may-call-what). A route marked *(harness-store)*, *(hook-store)* or *(snapshot-store)* exists only when that store opened.

### Sessions

| Method | Route | What it does |
|---|---|---|
| `GET` | `/sessions` | List sessions |
| `GET` / `POST` | `/sessions/summary` | The chat sidebar's list, projected small. `POST` takes the id lists in a body, since a long query string makes nginx drop the whole HTTP/2 connection |
| `GET` | `/sessions/recent-bundle` | Warm-up bundle of recent sessions for the chat page |
| `GET` / `POST` | `/sessions/validators` | Cheap staleness check for a client's cached sessions |
| `GET` | `/session-events` | SSE stream of changes to the session list |
| `GET` | `/sessions/search` | Full-text search, answered by log-store |
| `GET` | `/sessions/aggregates` | Totals across sessions, answered by log-store |
| `GET` | `/sessions/discover` | Sessions the harness CLIs have on disk |
| `POST` | `/sessions` | Create a session; `auto_start` starts it |
| `GET` | `/sessions/{id}` | One session |
| `GET` | `/sessions/{id}/events` | SSE stream of the session's events |
| `GET` | `/sessions/{id}/attach` | WebSocket onto a pty-mode session's terminal |
| `GET` | `/sessions/{id}/attach-token` | Token for the attach WebSocket |
| `GET` | `/sessions/{id}/messages` | History, projected by log-store for reading |
| `GET` | `/sessions/{id}/messages/raw` | History with nothing left out, about ten times larger |
| `GET` | `/sessions/{id}/entries/{eventId}` | One history entry with its tool input and output in full |
| `POST` | `/sessions/{id}/send` | Send a message |
| `POST` | `/sessions/{id}/interrupt` | Stop the current turn |
| `POST` | `/sessions/{id}/resume` | Restart a stopped session with its history |
| `POST` | `/sessions/{id}/stop` | End the session |
| `POST` | `/sessions/{id}/mode` | Switch between events mode and pty mode |
| `POST` | `/sessions/{id}/compact` | Compact the context |
| `POST` | `/sessions/{id}/fork` | Branch a new session from this one |
| `POST` | `/sessions/{id}/rename` | Set the title |
| `POST` | `/sessions/{id}/auto-rename` | Have a model write the title |
| `POST` | `/sessions/{id}/config` | Change model, effort, budget or disabled tools |
| `POST` | `/sessions/{id}/mark-done` | Mark the session done |
| `PUT` | `/sessions/{id}/folder` | Move it into a folder |
| `PUT` | `/sessions/{id}/permission-mode` | `ask`, `auto` or `bypass` for this session |
| `PUT` | `/sessions/{id}/bypass-permissions` | Old boolean form of the above |
| `GET` | `/sessions/{id}/git/repos` | Git repos found in the session's directory |
| `GET` | `/sessions/{id}/git` | Status and diff of one of them (`?repo=`) |
| `GET` | `/sessions/{id}/effective-config` | Every setting the session runs with, and which layer decided it |
| `GET` | `/effective-config` | The same for a session not yet created (`?harness=&instance_id=&principal_id=&board_id=&card_id=…`) |
| `GET` | `/sessions/{id}/hooks/pending` | Hooks waiting on a human decision |
| `POST` | `/sessions/{id}/hooks/{request_id}/resolve` | Allow or deny one |
| `GET` | `/sessions/{id}/tools/{tool_use_id}/snapshots` | File snapshots before and after an Edit or Write *(snapshot-store)* |
| `GET` | `/snapshots/blob/{sha}` | One snapshot's content *(snapshot-store)* |

### Signals

A signal is anything a session raises for a human: a question to answer or a notice to acknowledge. `SESSION-SIGNALS.md` has the design.

| Method | Route | What it does |
|---|---|---|
| `GET` | `/signals` | The inbox across sessions (`?state=open`) |
| `GET` / `POST` | `/sessions/{id}/signals` | One session's signals; `POST` raises a notice |
| `POST` | `/signals/{id}/answer` | Answer a question, whether or not its session still runs |
| `POST` | `/signals/{id}/resolve` | Acknowledge or dismiss |

### Machines, instances and credentials

| Method | Route | What it does |
|---|---|---|
| `GET` `POST` | `/machines` | List or create machines *(harness-store)* |
| `GET` `PUT` `DELETE` | `/machines/{id}` | One machine *(harness-store)* |
| `GET` `POST` | `/instances` | List or create instances *(harness-store)* |
| `GET` `PUT` `DELETE` | `/instances/{id}` | One instance *(harness-store)* |
| `GET` | `/instances/{id}/status` | Live sessions and credential state *(harness-store)* |
| `GET` | `/instances/{id}/sessions` | Its sessions *(harness-store)* |
| `POST` | `/instances/{id}/oneshot` | One model call on the instance, with no session *(harness-store)* |
| `GET` `POST` | `/instances/{id}/credentials` | Bound credentials; bind one *(harness-store)* |
| `DELETE` | `/instances/{id}/credentials/{cred_id}` | Unbind one *(harness-store)* |
| `GET` `POST` | `/credentials` | List (keys masked) or add credentials |
| `DELETE` | `/credentials/{id}` | Delete one |

### Hooks

Hooks the bridge wires into a harness: an event and a matcher that run a shell command, scoped to a session, an instance or everything.

| Method | Route | What it does |
|---|---|---|
| `GET` `POST` | `/hooks` | List or create *(hook-store)* |
| `GET` `PATCH` `DELETE` | `/hooks/{id}` | One hook *(hook-store)* |
| `GET` | `/hook-options` | Which harnesses take hooks, their events and scopes *(hook-store)* |
| `POST` | `/hooks/exec/{id}` | Run a hook; a harness calls this *(hook-store)* |

### Harnesses, models and settings

| Method | Route | What it does |
|---|---|---|
| `GET` | `/health` | Health, harnesses present, session counts |
| `GET` | `/harnesses` | Each harness's name, label, image, capabilities and hook events |
| `GET` | `/harnesses/{name}/capabilities` | One harness's capabilities |
| `GET` | `/harnesses/{name}/agents` | Its named agents |
| `GET` | `/images/…` | Harness images |
| `GET` | `/models` | Models there are credentials for |
| `GET` `PUT` | `/bridge-prefs` | Stored preferences: per-harness defaults, default principal, … |
| `POST` | `/bridge/permission-mode` | Default permission mode for new sessions |
| `POST` | `/bridge/bypass-permissions` | Old boolean form of the above |
| `GET` | `/settings` | Every server setting, its value and what decided it |
| `PUT` | `/settings/{key}` | Change a stored setting without a restart |
| `GET` | `/conformance` | Latest capability matrix across harnesses |
| `POST` | `/conformance/run` | Start a new run |

### Folders and session labels

| Method | Route | What it does |
|---|---|---|
| `GET` `POST` | `/folders` | List or create folders |
| `PUT` `DELETE` | `/folders/{name}` | Rename or delete one |
| `GET` | `/source-folders` | Which folder each session `source` files into |
| `PUT` `DELETE` | `/source-folders/{source}` | Set or clear one |
| `GET` | `/session-taxonomy` | The words sessions may be labelled with |
| `GET` | `/session-taxonomy/report` | Sessions whose labels disagree with them |
| `POST` | `/admin/file-inactive` | File inactive sessions; a scheduler job calls it |
| `POST` | `/admin/archive-old` | Archive old sessions; a scheduler job calls it |

### Services page

| Method | Route | What it does |
|---|---|---|
| `GET` | `/services` | Each service healthcheck watches, its status, processes and open SQLite files |
| `GET` | `/services/databases/schema?path=` | Tables of one such file |
| `GET` | `/services/databases/rows?path=&table=&filter=col:op:value` | Newest rows of one table |

Only a file some watched service holds open right now can be read; any other path is 404. Files open read-only, and a column whose name marks it as a credential comes back masked.

### Runners and harness callbacks

| Method | Route | What it does |
|---|---|---|
| `GET` | `/api/runner/ws` | A runner's WebSocket; bearer token checked against the machine |
| `POST` | `/api/runner/enroll` | Trade a passphrase for a runner token |
| `GET` | `/api/runner/install.sh` | Runner install script |
| `GET` | `/api/runner/binary?os=&arch=&name=` | Runner and wrapper binaries |
| `POST` | `/api/runner/seed/broadcast` | Tell every runner to re-sync agent and skill files |
| `*` | `/api/agent-store/…`, `/api/skill-store/…` | Runners read agent-store and skill-store through these |
| `*` | `/api/harness-proxy/{harness}/…` | Forwards to a harness backend (inber, hermes) on this host. **Checks no credential**; nothing calls it today. Todo `f4e5e1ef-f622-49a4-826a-51450319da08` |
| `POST` | `/permission/cc-prehook/{bridge_id}` | Claude Code's permission check before each tool call |
| `POST` | `/permission/codex-prehook/{bridge_id}` | The same for codex |
| `POST` | `/sidecar/event/{bridge_id}` | Events from a pty-mode session's sidecar |

### Login and store proxies

| Method | Route | What it does |
|---|---|---|
| `POST` | `/auth/demo-login` | Sign in as a principal by id |
| `GET` | `/auth/principal` | Who the cookie names |
| `POST` | `/auth/logout` | Clear the cookie |
| `*` | `/kanban/…` | kanban-store `/api/…`, as the caller |
| `*` | `/grant-store/…` | grant-store `/…`, as the caller |

When agent-store and memory-store open, their own routes are mounted too, among them `/prompt-sections`, the source of every rendered prompt file on this host.

## Who may call what

`AGENTS.md` has the reasons behind each rule; this is the reference.

### Callers

A request is authorized before any handler sees it (`internal/server/request_authorization.go`). The server knows four kinds of caller, checked in this order:

1. **The internal service**: `X-LLM-Bridge-Service-Token` matches. It may call anything. A wrong token is 401.
2. **The internal service on behalf of a person**: the token plus `X-Principal-Id`. dash sends this for its logged-in user, and the request is treated as that person's. `X-Principal-Id` without the token is 401 on every route.
3. **A principal**: a login cookie from `POST /auth/demo-login`, or, on the two store proxies only, a session agent token.
4. **Nobody**: 401 everywhere except the open routes and the harness callbacks.

Every principal is looked up in principal-store first: unknown is 400 `unknown_principal`, a group is 400 `principal_not_human`, disabled is 403 `principal_disabled`, and principal-store down is 502. A principal marked `is_administrator` passes every per-session and operator check. Answers are cached for 30 seconds, so disabling someone takes up to 30 seconds to bite.

### Routes

Each route pattern has a rule in `routeAccessRules`. **A pattern with no rule is 403 `route_not_classified`** to everyone but the service token, and `TestEveryRegisteredRouteIsClassified` fails until the rule exists.

- **Open to everyone**: `GET /health` and the three `/auth/…` routes.
- **Harness callbacks**: the prehooks, the sidecar, `POST /hooks/exec/{id}`, `POST /sessions/{id}/auto-rename`, the `/api/runner/…` routes and the three proxies under `/api/`. These come from processes the server started or from runners, and carry no login. ⚠️ The prehooks, sidecar, hook exec and harness proxy check no credential, so do not publish them beyond this host.
- **For a principal who is not an administrator**:
  - `POST /sessions` creates the session as the caller; a body naming someone else is 403 `principal_mismatch`.
  - A route naming one session or signal reaches only the caller's own. Anyone else's, one with no principal and a missing one all answer the same 404.
  - Session lists, the summary, validators, `GET /signals` and `/session-events` show only the caller's sessions.
  - `GET /sessions/search`, `GET /sessions/aggregates` and `GET /snapshots/blob/{sha}` are 403, since they cannot be narrowed to one person.
  - The catalogs (`/harnesses…`, `/images/`, `/session-taxonomy`, `GET /agents…`) read as they are. `GET /instances` lists only instances the caller's `can_dispatch_on` grants allow.
  - Everything else is 403 `operator_route`.

A fork or a promoted subagent takes its parent's principal.

### Demo login

⚠️ **This is a stand-in for a real login, and it has no password.** Anyone who can reach `POST /auth/demo-login` can sign in as any active human principal by naming its id. Never publish it beyond a network you control.

```bash
curl -s -c jar -X POST localhost:8160/auth/demo-login -H 'Content-Type: application/json' \
  -d '{"principal_id":"principal_000001"}'
```

It sets the `llm_bridge_principal_session` cookie: the principal id and a 12-hour expiry, signed with HMAC-SHA256. The cookie holds no server state, so a copy taken before logout works until it expires.

### The store proxies

`/kanban/<rest>` goes to kanban-store `/api/<rest>` and `/grant-store/<rest>` to grant-store `/<rest>`, both through `serveStoreProxyAsPrincipal`. Each needs a cookie or a session agent token. It deletes the caller's `X-Principal-Id`, store tokens, service token, `Authorization` header and cookie, then sets `X-Principal-Id` to the verified principal. The service token alone is 403 `store_proxy_requires_a_principal`.

⚠️ **kanban-store believes `X-Principal-Id`**, so users must reach it only through this proxy.

### Session agent tokens

A session started as a principal gives its harness process two variables:

| Variable | Value |
|---|---|
| `LLM_BRIDGE_GATEWAY_URL` | This server's base URL |
| `LLM_BRIDGE_PRINCIPAL_TOKEN` | The principal and session, signed, valid 24 hours, minted at each spawn |

A tool call inside the session uses them to reach the stores as that principal:

```bash
curl -s "$LLM_BRIDGE_GATEWAY_URL/kanban/boards" -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN"
```

The token works only on the two store proxies, and only while its session exists, belongs to that principal and has not ended. Only the local transport can pass these variables, so a principal's session on an SSH or runner instance is refused at spawn.

⚠️ This holds only for agents that go through the gateway. In deployment, kanban-store and grant-store must not be reachable from agent processes.

### Secrets never reach a child process

Every process the server starts gets its environment from `internal/childprocessenv`, which removes each variable in `config.SecretEnvironmentVariableNames`: the signing key, the service token, `GRANT_STORE_SERVICE_TOKEN` and `KANBAN_STORE_SERVICE_TOKEN`. A test fails on any `exec.Command` that skips it. The server also marks itself non-dumpable at start, so a process running as the same user cannot read its memory or `/proc/<pid>/environ`. That does not protect the environment file the unit loads; keep it unreadable by the user agents run as.

## Testing

| Command | Needs | Covers |
|---|---|---|
| `go test ./...` | nothing | Unit tests, and the conformance matrix against `cmd/mock-harness` |
| `go test -tags pty_integration ./...` | `claude` and `llm-bridge-claudecode` on `PATH` | A real pty-mode session: attach, a keystroke each way, stop |
| `go test -tags convenience_events_integration ./...` | the same, signed in | A real turn, checking the `agent_state`, `usage_total` and `turn_complete` events |

The tagged tests skip when a binary is missing.

⚠️ `scripts/e2e-smoke.sh` and `scripts/e2e-claude.sh` predate the required settings above and do not set them, so the server they start will refuse to run until they do.

## Design notes

Longer write-ups at the root of this repo:

- `HARNESS-LAYER.md`: how one interface covers every harness
- `TOOL-ROUTING.md`: how tools and skills reach a session
- `AGENT-MANAGEMENT.md`: how an agent record becomes harness config
- `SESSION-SIGNALS.md`: questions and notices a session raises
- `SESSION-STATE-RELIABILITY.md`: how session state is worked out from events
- `PTY-MODE.md`: pty-mode sessions
- `CACHE-RULES.md`: what may and may not break prompt caching
- `CONFORMANCE-GRADING.md`: how the conformance matrix is graded
