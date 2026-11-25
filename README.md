# llm-bridge-server

The session server of the llm-bridge system, on `:8160`. It starts agent CLIs through harness wrappers, keeps a record of every session, and streams each session's events to clients as `msg.Event` over SSE. A client gets one API whichever agent runs behind a session. bridge-ui (served by dash) is its main client.

For the whole system — which repo owns what, and how the pieces talk — read the [llm-bridge README](https://github.com/kayushkin/llm-bridge#readme) first. `AGENTS.md` here explains why the server works the way it does, for anyone changing its code.

## Boundaries

```
  dash / bridge-ui · scheduler · bridge-agent · llm-bridge-tui
                    │ HTTP, SSE, WebSocket
                    ▼
          ┌──────────────────────┐   HTTP   principal-store, grant-store, kanban-store,
          │  llm-bridge-server   │ ───────► auth-store, tool-store, bundle-store,
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

**It keeps every session event** in log-store, which runs in this process as the `session_log` module in Postgres (`internal/sessionlog/logstore`; its README says how). The harness manager writes and reads it in process, the session routes answer from it, and its own routes are under `/log-store/`.

**It asks** these services over HTTP and keeps none of their data:

| Service | Asked for |
|---|---|
| principal-store | Who a caller is, and whether they are an administrator |
| grant-store | Which instances and agents a principal may use, and which tools |
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
| `LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY` | At least 32 bytes. Signs session agent tokens and session posting tokens. |
| `LLMBRIDGE_SERVICE_TOKEN` | At least 32 bytes. Internal callers send it as `X-LLM-Bridge-Service-Token`. |
| `LLMBRIDGE_PRINCIPAL_STORE_URL` | principal-store |
| `LLMBRIDGE_GRANT_STORE_URL` | grant-store |

Login has its own optional settings. `LLMBRIDGE_DEMO_LOGIN_ENABLED` (default `true`) switches demo login; set it `false` where people sign in through Entra ID. Entra ID sign-in is on when all four of `LLMBRIDGE_ENTRA_TENANT_ID` (a GUID), `LLMBRIDGE_ENTRA_CLIENT_ID`, `LLMBRIDGE_ENTRA_CLIENT_SECRET` and `LLMBRIDGE_ENTRA_REDIRECT_URL` are set; some but not all is a startup error naming the missing ones. `LLMBRIDGE_ENTRA_AUTHORITY_URL` defaults to `https://login.microsoftonline.com`.

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

Each SSE `data:` line is one `msg.Event` (`github.com/kayushkin/llm-bridge/msg`; `Event` in `@kayushkin/llm-bridge-types` for TypeScript). The stream replays the current turn on connect and honors `Last-Event-ID`.

`cmd/bridge-agent` wraps these calls: it hands one prompt to a new session with a chosen MCP bundle and prints the final answer.

## Who may call what

Every request is checked before a handler sees it (`internal/server/request_authorization.go`). The last column of the route table below names the rule for each route.

### Callers

1. **Operator**: the service token, or a principal principal-store marks `is_administrator`. May call anything. A wrong token is 401.
2. **The service token on behalf of a person**: the token plus `X-Principal-Id`. dash sends this for its logged-in user, and the request is judged as that person's. `X-Principal-Id` without the token is 401 on every route.
3. **Principal**: a login cookie naming a live login session, from Entra ID sign-in or `POST /auth/demo-login`, or a session agent token: anyone's on the store proxies, and an administrator's (principal-store's `is_administrator`) on every other route a principal may call, from this host only. A cookie request with any method but `GET`, `HEAD` or `OPTIONS` must also carry the session's CSRF token in `X-CSRF-Token` (403 `csrf_token_missing` / `csrf_token_mismatch`).
4. **A session's agent**: its session posting token (`LLM_BRIDGE_SESSION_POSTING_TOKEN`) as `Authorization: Bearer`. It shares files, offers actions and makes images in its own session, and starts and drives delegates under it: `POST /sessions` creates a session whose `manager_session_id` is the agent's session and whose principal is that session's (403 `delegate_manager_mismatch` / `delegate_principal_mismatch` for a body naming another); `POST /sessions/{id}/send`, `POST /sessions/{id}/stop` and `GET /sessions/{id}/events` reach only those delegates, 404 otherwise; `GET /instances` reads the catalog. It also **reads** sessions, read-only, on `GET /sessions`, `GET /sessions/search`, `GET /sessions/{id}` and `GET /sessions/{id}/messages`: as the internal service when its own session runs with no principal (every session on a one-operator server), otherwise as that principal exactly — its own sessions, an administrator's every one, search refused 403 `route_not_available_to_principals` to a non-administrator — and, for a session a connected server started, within that server's limits too. These read routes refuse the token through the public reverse proxy (403 `session_posting_token_not_public`). Every one of these answers 403 `delegating_session_ended` once the agent's session has ended. The token is bound to one spawn of its session, so a token from an earlier spawn is 401 `session_posting_token_from_an_earlier_spawn`. `cmd/bridge-agent` and `cmd/bridge-sessions` use it when there is no service token. Any other route answers it 401.
5. **A service**: a service principal token (`Authorization: Bearer llm-bridge-service-principal-token-v1.…`) that an administrator issued with `POST /service-principal-tokens` for a principal of principal-store's kind `service`. It acts as that service on every route, held to the route rules and to its grants like a person signed in as it, from this host only (403 `service_principal_token_not_public` through the public proxy). Revoked, it is 401 `service_principal_token_revoked`.
6. **Another llm-bridge-server**: a peer token in `X-LLM-Bridge-Peer-Token`, which this server delivered when an administrator granted that server a level of access. It acts as the peer principal the grant made and is held to the route rules like a person signed in as it; a peer is never let onto a harness callback. By https it is 409 `demo_login_enabled` while demo login is on; over a socket this server dialed out itself it is not. A request over a peer socket with no peer token is 403 unless it is a connection call.
7. **Nobody**: 401 everywhere but the routes open to anyone and the harness callbacks.

Every principal is looked up in principal-store: unknown is 400 `unknown_principal`, a group is 400 `principal_not_human`, disabled is 403 `principal_disabled`, principal-store down is 502. Answers are cached for 30 seconds, so disabling someone takes up to 30 seconds to take effect.

### The rules in the route table

| Rule | Meaning |
|---|---|
| anyone | No credential needed |
| harness callback | Called by a process the server started, or by a runner; carries no login. The handler makes whatever check it makes. ⚠️ The prehooks, sidecar, hook exec and harness proxy check nothing, so do not publish them beyond this host. |
| operator | Service token or administrator; a principal gets 403 `operator_route` |
| connected server | A connection call from a server connected to this one: the handler checks the connection's credential in `X-LLM-Bridge-Peer-Connection`, or the socket the call arrived over. Never a peer token |
| principal | Any principal. `GET /instances` lists only the instances its `can_dispatch_on` grants allow |
| principal, as itself | `POST /sessions` creates the session as the caller; naming someone else is 403 `principal_mismatch`, unless the caller holds `can_start_sessions_as` on them, when the session runs as them and the caller, recorded as its starter, may drive it while the grant stands |
| principal, own sessions only | The answer holds only the caller's sessions |
| principal, as itself, in its organization | `POST /operations` starts the operation as the caller, whatever the body says. `organization_id` must be an active principal-store group (400 `unknown_organization`, `organization_not_group`, `organization_disabled`) the caller belongs to (403 `not_a_member_of_organization`); an administrator and the service token skip the membership check |
| principal, own operations only | The answer holds only the caller's operations |
| owner | Only the principal who owns the session, signal or operation. Anyone else's, one with no principal and a missing one all answer the same 404 |
| principal or session agent | A login cookie or a session agent token; the request reaches the store as that principal |
| principal, connections it may call through only | An administrator sees every connection; anyone else only those its `can_call_through` grants name, and another is 404 |
| principal, named to the connected server | Any principal calls the connected server through the connection, named there as itself unless it is an administrator; a principal restricted to its grants, such as a service, needs `can_call_through` on the connection (403 `peer_connection_not_granted`) |
| principal, own logins only | The caller's own login sessions; an administrator reaches anyone's, and someone else's is 404 to anyone else |

A route with no rule is 403 `route_not_classified` to everyone but the service token, and `TestEveryRegisteredRouteIsClassified` fails until it has one. A fork or promoted subagent takes its parent's principal.

### Login sessions

Signing in creates a row in table `login_sessions` and sets the `llm_bridge_principal_session` cookie to a random token; the row keeps only its SHA-256. A login lasts 12 hours. An unknown token is 401 `login_session_unknown`, a revoked one `login_session_revoked`, an expired one `login_session_expired`. `POST /auth/logout` revokes the session on the server, so a copy of the cookie stops working too. `GET /auth/sessions` lists the caller's sessions, and `POST /auth/sessions/{id}/revoke {"reason":…}` ends one; an administrator may do either for anyone. Cookies signed before 2026-10-19 name no session, and their holders sign in again.

The login response and `GET /auth/principal` carry `csrf_token`. Send it as `X-CSRF-Token` on every cookie request that is not `GET`, `HEAD` or `OPTIONS`.

### Demo login

⚠️ **A stand-in for a real login, with no password.** Anyone who can reach `POST /auth/demo-login` can sign in as any active human principal by naming its id. Never publish it beyond a network you control, and switch it off with `LLMBRIDGE_DEMO_LOGIN_ENABLED=false` where people sign in through Entra ID; it then answers 404 `demo_login_disabled`.

```bash
curl -s -c jar -X POST localhost:8160/auth/demo-login -H 'Content-Type: application/json' \
  -d '{"principal_id":"principal_000001"}'
```

### Entra ID sign-in

`GET /auth/entra/start?return_to=/chat` sends the browser to Entra ID (authorization code with PKCE). `return_to` must be a path on this site. Entra sends it back to `GET /auth/entra/callback`, which redeems the code, checks the ID token's signature, issuer, audience, times, nonce and tenant, and looks up the `tid` and `oid` claims in principal-store's identity bindings. Never `sub`, never an email. A bound, active human gets a login session and lands on `return_to`. Anyone else lands there with `?login_error=<code>`, or gets a 401 when the token itself fails a check. Every attempt is a row in `login_attempts` with its outcome. An administrator finds a refused person's `tid` and `oid` with `GET /auth/login-attempts?outcome=identity_not_provisioned` and links them with `POST /admin/identity-bindings {"principal_id","provider":"entra","tenant_id","subject","note"}`. The bridge sets `linked_by` itself. `POST /admin/identity-bindings/{id}/unlink {"reason"}` unlinks a binding and revokes its live login sessions.

### Store proxies

`/kanban/<rest>` is kanban-store's `/api/<rest>` and `/kanban-store/<rest>` its `/<rest>`. kanban-store runs in this process on its Postgres module (`internal/tasktracking/kanbanstore`), and reads its caller from the request context: the verified principal, narrowed by its board grants, or the internal service (this server's service token naming nobody), unrestricted. No header names the caller, so a forged `X-Principal-Id` or kanban-store service token names nobody. `llm-bridge-server import-kanban-store -sqlite <file>` copies a standalone kanban-store's database into the module. `/grant-store/<rest>` is grant-store's `/<rest>`, in this process the same way. `/mailstack/<rest>` is mailstack's `/<rest>`, in this process on its Postgres module (`internal/mail/mailstack`, module `mail`) the same way: the internal service unrestricted, a principal narrowed to its `can_read_mail` and `can_import_mail` grants and the messages its viewable cards link, and `POST /mailstack/api/ingress/deliveries` open to a sender with an ingress credential. Gmail, Outlook and IMAP accounts reach their providers only while `mailstack.provider_connections.enabled` is on. `llm-bridge-server import-mailstack -accounts-sqlite <file> [-local-mail-sqlite <file>]` copies a standalone mailstack in. `/reference-store/<rest>` runs in this process too.

`/scheduler/<rest>` is the scheduler's `/<rest>` and `/rule-store/<rest>` rule-store's `/<rest>`, both in this process on their Postgres modules (`internal/automation/scheduler`, module `scheduler`; `internal/automation/rulestore`, module `rules`), operator only. Neither acts on its own unless a setting says so: `scheduler.runs.enabled` (off) starts the cron ticker, run now, the bus commands and event triggers, and `rule_store.reactor.enabled` (off) starts the reactor; off, their records are read and written and run now answers 503. `llm-bridge-server import-scheduler -sqlite <file> [-newest-runs-per-job <n>]` and `import-rule-store -sqlite <file>` copy the standalone files in.

`/people-store/`, `/calendar-store/` and `/journal-store/` are the connected records stores, on their Postgres modules (`internal/connectedrecords/`), with their own paths behind the prefix. Every record carries `owner_principal_id`, the principal-store id of the person who keeps it, and each store narrows every read and write to the caller's own records: another person's record is 404, as a missing one is, and a list holds only the caller's. An administrator sees every owner's records and may narrow to one with `?owner_principal_id=`. The service token acting as nobody sees every owner's in people-store and calendar-store and cannot make a record; journal-store refuses it outright (403 `journal_store_requires_a_principal`), so it answers only its owner's or an administrator's principal. `llm-bridge-server import-people-store|import-calendar-store|import-journal-store -sqlite <file> -owner <principal id>` copies a standalone store into its module, every record owned by `-owner`, which is required and must be a person principal-store holds.

`/message-store/`, `/activity-store/` and `/time-ledger-store/` are connected records stores too (`internal/connectedrecords/messagestore`, `activitystore`, `timeledgerstore`; modules `messages`, `activity`, `time_ledger`), under the same owner rule. The service token acting as nobody sees every owner's records in all three and cannot make one; an importer sends it beside the `X-Principal-Id` of the person it files for. activity-store keeps its attachments' bytes in file-store, as files owned by `activity-store` and the activity. None runs a loop of its own: the importers (`message-import-*`, `activity-import-google`, `time-ledger-import` and the rest) are separate commands that call these routes. `llm-bridge-server import-message-store|import-time-ledger-store -sqlite <file> -owner <principal id>` and `import-activity-store -sqlite <file> -attachments <directory> -owner <principal id>` copy a standalone store into its module; the activity import carries each attachment into file-store after checking its SHA-256.

### Session agent tokens

A session started as a principal gives its harness process `LLM_BRIDGE_GATEWAY_URL` and `LLM_BRIDGE_PRINCIPAL_TOKEN`, so a tool call inside the session can reach the stores as that principal:

```bash
curl -s "$LLM_BRIDGE_GATEWAY_URL/kanban/boards" -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN"
```

The token lasts 24 hours, is minted at each spawn, and works only on the two store proxies while its session exists and has not ended. Only the local transport can pass these variables, so a principal's session on an SSH or runner instance is refused at spawn. ⚠️ In deployment, kanban-store and grant-store must not be reachable from agent processes directly.

### Secrets never reach a child process

Every process the server starts gets its environment from `internal/childprocessenv`, which removes the signing key, the service token, `GRANT_STORE_SERVICE_TOKEN`, `KANBAN_STORE_SERVICE_TOKEN`, `FILE_STORE_SERVICE_TOKEN` and `LLMBRIDGE_ENTRA_CLIENT_SECRET`; a test fails on any `exec.Command` that skips it. The server also marks itself non-dumpable, so a process running as the same user cannot read its memory. Keep the unit's environment file unreadable by the user agents run as.

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
| `*` | `/log-store/` | log-store's own routes (`/log-store/api/v1/…`, `/log-store/health`), answered in this process, for callers written against the standalone service | operator |
| `GET` | `/session-events` | SSE stream of changes to the session list | principal, own sessions only |
| `GET` | `/sessions` | List sessions | principal, own sessions only |
| `POST` | `/sessions` | Create a session; `auto_start` starts it | principal, as itself |
| `GET` | `/sessions/aggregates` | Totals across sessions, answered by log-store | operator |
| `GET` | `/sessions/discover` | Import sessions the harness CLIs have on this host's disk | operator |
| `GET` | `/sessions/principals` | Who the sessions you may see were started as, with names and counts | principal, own sessions only |
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
| `GET` | `/sessions/{id}/actions` | Actions offered in the session, and who ran each and how it ended | owner |
| `POST` | `/sessions/{id}/actions` | Offer an action the person can confirm from the chat (its owner, or its own agent with LLM_BRIDGE_SESSION_POSTING_TOKEN) | owner, or the session's own agent |
| `POST` | `/sessions/{id}/actions/{action_id}/run` | Confirm an action: the server runs it (owner only, never the agent) | owner |
| `GET` | `/sessions/{id}/attach` | WebSocket onto a pty-mode session's terminal | owner |
| `GET` | `/sessions/{id}/attach-token` | The token the attach WebSocket needs | owner |
| `PUT` | `/sessions/{id}/bypass-permissions` | Old boolean form of the above | operator |
| `GET` | `/sessions/{id}/chat-links` | The session's chat links, active and ended | owner, or the session's own agent |
| `POST` | `/sessions/{id}/chat-links` | Link the session to a person writing to one of our bot accounts: what they send comes in, each turn it starts is sent back (its owner, or its own agent with LLM_BRIDGE_SESSION_POSTING_TOKEN) | owner, or the session's own agent |
| `GET` | `/sessions/{id}/chat-links/{link_id}` | One chat link and its log of messages both ways | owner, or the session's own agent |
| `POST` | `/sessions/{id}/chat-links/{link_id}/end` | End a chat link; what the person sends after is not kept | owner, or the session's own agent |
| `POST` | `/sessions/{id}/compact` | Compact the context | owner |
| `POST` | `/sessions/{id}/config` | Change model, effort, budget or disabled tools | owner |
| `GET` | `/sessions/{id}/effective-config` | Every setting the session runs with, and which layer decided it | owner |
| `GET` | `/sessions/{id}/entries/{eventId}` | One history entry with its tool input and output in full | owner |
| `GET` | `/sessions/{id}/entries/{eventId}/images/{index}` | One image a tool result carries, relayed from log-store | owner |
| `GET` | `/sessions/{id}/events` | SSE stream of `msg.Event`; replays the current turn and honors `Last-Event-ID` | owner |
| `GET` | `/sessions/{id}/files` | Files shared into the session | owner |
| `POST` | `/sessions/{id}/files` | Share a file into the session (its owner, or its own agent with LLM_BRIDGE_SESSION_POSTING_TOKEN) | owner, or the session's own agent |
| `GET` | `/sessions/{id}/files-named-by-tools` | Files on disk that the session's tool calls and their output named | owner |
| `GET` | `/sessions/{id}/files-named-by-tools/content` | One such file's current text, only for a path on that list | owner |
| `GET` | `/sessions/{id}/files-named-by-tools/image` | One such file's bytes when it is an image, only for a path on that list | owner |
| `GET` | `/sessions/{id}/files/{file_id}/content` | One shared file's bytes, relayed from file-store | owner |
| `PUT` | `/sessions/{id}/folder` | Move into a folder | operator |
| `POST` | `/sessions/{id}/fork` | Branch a new session from this one | owner |
| `GET` | `/sessions/{id}/git` | Status and diff of one of them (`?repo=`) | owner |
| `GET` | `/sessions/{id}/git/repos` | Git repos in the session's directory | owner |
| `GET` | `/sessions/{id}/hooks/pending` | Hooks waiting on a human decision | owner |
| `POST` | `/sessions/{id}/hooks/{request_id}/resolve` | Allow or deny one | owner |
| `POST` | `/sessions/{id}/images` | Make images from a prompt and reference images and share them into the session (its owner, or its own agent with LLM_BRIDGE_SESSION_POSTING_TOKEN) | owner, or the session's own agent |
| `POST` | `/sessions/{id}/interrupt` | Stop the current turn | owner |
| `POST` | `/sessions/{id}/mark-done` | Mark done and move to Archive | owner |
| `GET` | `/sessions/{id}/messages` | History, projected by log-store for reading | owner |
| `GET` | `/sessions/{id}/messages/raw` | History with nothing left out, about ten times larger | owner |
| `POST` | `/sessions/{id}/mode` | Switch between events mode and pty mode | owner |
| `PUT` | `/sessions/{id}/permission-mode` | `ask`, `auto` or `bypass` for this session | operator |
| `POST` | `/sessions/{id}/rename` | Set the title | owner |
| `POST` | `/sessions/{id}/resume` | Restart a stopped session with its history | owner |
| `POST` | `/sessions/{id}/send` | Send a message | owner |
| `PUT` | `/sessions/{id}/starter` | Record which service started a session as another principal, for a session created before starters were recorded; the service must hold `can_start_sessions_as` on that principal | operator |
| `POST` | `/sessions/{id}/stop` | End the session | owner |
| `GET` | `/sessions/{id}/tools/{tool_use_id}/snapshots` | File snapshots before and after an Edit or Write (snapshot-store) | owner |

### Signals

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/session-briefs` | Briefs across chats (`?session_id=&loose_end_state=open&updated_since=&limit=`; `session_id` repeatable) | principal, own sessions only |
| `GET` | `/sessions/{id}/brief` | One chat's brief: overall goal, current step, each turn's revision, and its loose ends | owner |
| `POST` | `/sessions/{id}/loose-ends/{loose_end_id}/answer` | Answer a loose end (`{"text"}`): sent to the session as a message quoting it | owner |
| `POST` | `/sessions/{id}/loose-ends/{loose_end_id}/dismiss` | Close a loose end without an answer | owner |
| `GET` | `/sessions/{id}/signals` | One session's signals | owner |
| `POST` | `/sessions/{id}/signals` | A session raises a notice about itself | owner |
| `GET` | `/signals` | The inbox across sessions (`?state=open&kind=&surface=&session_type=&session_id=&linked_todo_id=&limit=`; `session_type` repeatable) | principal, own sessions only |
| `POST` | `/signals/{id}/answer` | Answer a question, whether or not its session still runs | owner |
| `POST` | `/signals/{id}/resolve` | Acknowledge or dismiss | owner |

### Operations

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/operation-budgets` | Every organization's monthly limit and what it has spent this UTC month | operator |
| `DELETE` | `/operation-budgets/{organization_id}` | Remove an organization's monthly limit | operator |
| `GET` | `/operation-budgets/{organization_id}` | One organization's monthly limit and spend | operator |
| `PUT` | `/operation-budgets/{organization_id}` | Set an organization's monthly limit (`{"monthly_limit_usd":50}`) | operator |
| `GET` | `/operation-types` | The operation types this server runs, with their attempt and time limits | anyone |
| `GET` | `/operations` | Receipts, newest first (`?organization_id=&principal_id=&type=&state=&created_after=&created_before=&limit=`) | principal, own operations only |
| `POST` | `/operations` | Accept an operation intent: 202 and a new receipt, or 200 and the first one for a repeated idempotency key | principal, as itself, in its organization |
| `GET` | `/operations/{id}` | One operation's receipt | owner |
| `POST` | `/operations/{id}/cancel` | Ask an operation to stop; 409 with the receipt when it already finished | owner |
| `GET` | `/operations/{id}/children` | The receipts of an operation's children | owner |
| `GET` | `/operations/{id}/events` | SSE of an operation's events, resumable with `Last-Event-ID`, closed once it and its children finish | owner |

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
| `GET` | `/machines/stats` | Every machine with its newest health reading: CPU, memory, drives, batteries, uptime | operator |
| `DELETE` | `/machines/{id}` | Remove a machine (harness-store) | operator |
| `GET` | `/machines/{id}` | One machine (harness-store) | operator |
| `PUT` | `/machines/{id}` | Change a machine (harness-store) | operator |
| `PUT` | `/machines/{id}/computer-use` | Switch computer use on or off for a machine's sessions (`{"allowed": bool}`) | operator |
| `GET` | `/machines/{id}/disk-usage` | A runner machine's drives, its scans, its deletion in progress and its recorded deletions | operator |
| `POST` | `/machines/{id}/disk-usage/deletion-checks` | Whether an entry may be deleted, why not and what to know first; deletes nothing (`{"path"}`) | operator |
| `GET` | `/machines/{id}/disk-usage/deletions` | Every recorded deletion on a machine, newest first | operator |
| `POST` | `/machines/{id}/disk-usage/deletions` | Delete a file or folder on a runner machine for good, recorded first (`{"path","kind","confirmation_name","expected_size_bytes"}`) | operator |
| `GET` | `/machines/{id}/disk-usage/entries` | An entry of a drive's latest scan and the entries directly inside it, largest first (`?path=C:\`) | operator |
| `POST` | `/machines/{id}/disk-usage/scans` | Start a scan of one of a runner machine's drives (`{"drive":"C:"}`) | operator |
| `GET` | `/machines/{id}/stats/history` | A machine's health readings over a time range, grouped into steps | operator |

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
| `POST` | `/images` | Make images from a prompt and reference images on the instance image_generation.instance_id names, with no session; answers with the saved files | operator |
| `*` | `/images/` | Harness images | principal |
| `GET` | `/models` | Models there are credentials for | operator |
| `POST` | `/oneshot` | One model call for a model-store role, with no session; the server picks the model, the instance and the fallbacks | operator |
| `DELETE` | `/restart-drain` | Stop holding new turns and send the held messages | operator |
| `POST` | `/restart-drain` | Hold new turns until a restart, for at most maximum_seconds | operator |
| `GET` | `/restart-readiness` | Which sessions a restart would cut off, so a deploy can wait for them | operator |
| `GET` | `/server` | This server's name and whether it is holding its machine awake | principal |
| `GET` | `/settings` | Every server setting, its value and what decided it | operator |
| `PUT` | `/settings/{key}` | Change a stored setting without a restart | operator |
| `GET` | `/ui/` | The front end compiled into this server: a file of the build, or its index.html for a page | anyone |
| `GET` | `/{$}` | Redirects to /ui/ | anyone |

### Folders, labels and housekeeping

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `POST` | `/admin/archive-old` | Archive old sessions; a scheduler job calls it | operator |
| `POST` | `/admin/file-inactive` | File inactive sessions; a scheduler job calls it | operator |
| `GET` | `/folders` | List folders | operator |
| `POST` | `/folders` | Create one | operator |
| `DELETE` | `/folders/{name}` | Delete one | operator |
| `PUT` | `/folders/{name}` | Rename one | operator |
| `GET` | `/session-taxonomy` | The types and purposes a session may carry, and the reasons its state may change for | principal |
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
| `GET` | `/admin/identity-bindings` | principal-store's identity bindings, query passed through | operator |
| `POST` | `/admin/identity-bindings` | Link an Entra identity to a principal, as the calling administrator | operator |
| `POST` | `/admin/identity-bindings/{id}/unlink` | Unlink an identity and revoke its live login sessions | operator |
| `POST` | `/auth/demo-login` | Sign in as a principal by id, with no password; 404 when switched off | anyone |
| `GET` | `/auth/entra/callback` | Where Entra ID sends the browser back; verifies the ID token and starts a login session | anyone |
| `GET` | `/auth/entra/start` | Send the browser to Entra ID to sign in, coming back to return_to | anyone |
| `GET` | `/auth/login-attempts` | Sign-in attempts and how each ended, filtered by outcome | operator |
| `POST` | `/auth/logout` | Revoke the cookie's login session and clear the cookie | anyone |
| `GET` | `/auth/principal` | Who the login cookie names, and the session's CSRF token | anyone |
| `GET` | `/auth/sessions` | The caller's login sessions; an administrator names anyone's with principal_id | principal, own logins only |
| `POST` | `/auth/sessions/{id}/revoke` | End a login session, with a reason | principal, own logins only |
| `*` | `/kanban-store/` | kanban-store at its root (`/api/…`, `/health`), as the caller | principal or session agent |
| `*` | `/kanban/` | kanban-store `/api/…`, as the caller | principal or session agent |
| `*` | `/mailstack/` | mailstack `/…`, in this process, as the caller, checked against their mail grants and tickets | principal or session agent |
| `POST` | `/mailstack/api/ingress/deliveries` | Pushed mail delivery into mailstack's local accounts, with an ingress credential and no login | anyone |
| `GET` | `/self` | Who the request acts as: its principal and whether it is an administrator, or the internal service | principal |
| `GET` | `/service-principal-tokens` | Every service principal token's record, revoked or not; never a token | operator |
| `POST` | `/service-principal-tokens` | Issue a token for a service principal (principal-store kind `service`); the token is answered once and kept as its SHA-256 | operator |
| `POST` | `/service-principal-tokens/{id}/revoke` | Stop a service principal token working, with a reason | operator |

### Peers: other llm-bridge-servers

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/peer-access-levels` | The levels of access this server can give, and each level's group | operator |
| `POST` | `/peer-access-levels/{key}/provision` | Make a level's principal-store group and its grants | operator |
| `GET` | `/peer-connection-invites` | Every connection passphrase, redeemed or not; never a passphrase | operator |
| `POST` | `/peer-connection-invites` | Make a one-time passphrase another server enters to connect to this one; connecting gives no access | operator |
| `GET` | `/peer-connections` | Every connected server, whether its socket is up, and the access given and held | principal, connections it may call through only |
| `POST` | `/peer-connections` | Connect to another server by its address and passphrase, directly or by dialing out | operator |
| `GET` | `/peer-connections/{id}` | One connection, whether its socket is up, and the access given and held | principal, connections it may call through only |
| `POST` | `/peer-connections/{id}/check` | Ask the other server who the access this server holds acts as there | operator |
| `POST` | `/peer-connections/{id}/grant` | Give the other server a level of access here and deliver its token | operator |
| `POST` | `/peer-connections/{id}/messages` | Sends the other server a request, notice or status, through this server's outbox | operator |
| `*` | `/peer-connections/{id}/proxy/{rest...}` | Any request to the connected server, as this server's peer principal there and, for a caller who is not an administrator, named as that caller; the answer streams back unchanged | principal, named to the connected server |
| `POST` | `/peer-connections/{id}/remove` | End a connection: revoke the access given, drop the access held, close the socket | operator |
| `POST` | `/peer-connections/{id}/revoke-grant` | Take the other server's access away, with a reason, and tell it | operator |
| `POST` | `/peer-message-attachments` | Store a file (the raw body; ?name=, ?media_type=) to send with the next message, answered with its id, size and SHA-256 | operator |
| `GET` | `/peer-messages` | Messages to and from connected servers, newest first, by connection, direction, state and kind; open=true lists this server's requests, notices and patches still waiting for an answer | operator |
| `GET` | `/peer-messages/{id}` | One message to or from a connected server | operator |
| `POST` | `/peer-messages/{id}/approve` | Approves a connected server's request or patch (hands it to the orchestrator, or starts its session where there is none) or notice (runs the update) | operator |
| `GET` | `/peer-messages/{id}/attachments/{attachment_id}` | Download a file of a message, as application/octet-stream | operator |
| `POST` | `/peer-messages/{id}/attachments/{attachment_id}/retry` | Fetch a file of an incoming message from the other server again now | operator |
| `POST` | `/peer-messages/{id}/decline` | Declines a connected server's request or notice, telling it the reason | operator |
| `POST` | `/peer-messages/{id}/place` | Starts an approved request's or patch's session on the instance the orchestrator chose | operator |
| `GET` | `/peer-user-mappings` | Which connected server's users act as which of this server's principals; ?peer_principal_id= narrows | operator |
| `POST` | `/peer-user-mappings` | Map a connected server's user to a principal here, allowed only what both it and the peer may do | operator |
| `POST` | `/peer-user-mappings/{id}/remove` | End a mapping, with a reason | operator |
| `POST` | `/peer/access` | The other server delivers a token for access on it | connected server |
| `POST` | `/peer/access/revoked` | The other server says a token it gave no longer works | connected server |
| `POST` | `/peer/connect` | Another server connects with a one-time passphrase | anyone |
| `POST` | `/peer/connection/removed` | The other server says it removed the connection; this side removes it too | connected server |
| `POST` | `/peer/messages` | A connected server delivers a message; answered with this server's id for it | connected server |
| `GET` | `/peer/messages/{message_id}/attachments/{attachment_id}` | A connected server fetches the bytes of a file this server sent it | connected server |
| `GET` | `/peer/self` | Who the presented peer token acts as here | principal |
| `GET` | `/peer/server-name` | This server's id and name, for a connected server; the call carries the caller's | connected server |
| `GET` | `/peer/socket` | The WebSocket a server without an address keeps open to this one | connected server |

### Runners and harness callbacks

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `POST` | `/api/agent-store/seed/drift` | A runner sends an agent file it changed | harness callback |
| `GET` | `/api/agent-store/seed/manifest` | A runner's agent file seed manifest | harness callback |
| `POST` | `/api/agent-store/seed/observe` | A runner reports the agent file hashes it holds | harness callback |
| `GET` | `/api/agent-store/versions/{vid}/content` | A runner fetches one agent file version | harness callback |
| `*` | `/api/harness-proxy/{harness}/{rest...}` | Forward to a harness backend (inber, hermes) on this host. Checks no credential; nothing calls it | harness callback |
| `GET` | `/api/runner/binary` | Runner and wrapper binaries (`?os=&arch=&name=`) | harness callback |
| `POST` | `/api/runner/enroll` | Trade a passphrase for a runner token | harness callback |
| `GET` | `/api/runner/install.sh` | Runner install script | harness callback |
| `POST` | `/api/runner/seed/broadcast` | Tell every runner to re-sync agent and skill files | operator |
| `GET` | `/api/runner/ws` | A runner's WebSocket | harness callback |
| `POST` | `/api/skill-store/seed/drift` | A runner sends a skill file it changed | harness callback |
| `GET` | `/api/skill-store/seed/manifest` | A runner's skill seed manifest | harness callback |
| `POST` | `/api/skill-store/seed/observe` | A runner reports the skill file hashes it holds | harness callback |
| `GET` | `/api/skill-store/versions/{vid}/content` | A runner fetches one skill file version | harness callback |
| `POST` | `/hooks/exec/{id}` | A harness runs a registered hook (hook-store) | harness callback |
| `POST` | `/permission/cc-prehook/{bridge_id}` | Claude Code's permission check before each tool call | harness callback |
| `POST` | `/permission/codex-prehook/{bridge_id}` | The same for codex | harness callback |
| `POST` | `/permission/devin-prehook/{bridge_id}` | The same for devin | harness callback |
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

### prediction-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/prediction-store/base-rates` | How often claims of a kind came true, by `group_by` (default `category`) | operator |
| `GET` | `/prediction-store/calibration` | Brier, log score, overconfidence and Murphy decomposition of resolved predictions, by `group_by` | operator |
| `GET` | `/prediction-store/categories` | The same as `/prediction-store/vocabulary` | operator |
| `GET` | `/prediction-store/health` | `{"status":"ok","counts":{predictions,open,overdue}}` | operator |
| `DELETE` | `/prediction-store/links/{id}` | Remove one link by its number | operator |
| `GET` | `/prediction-store/predictions` | List predictions, newest first, by status, category, tag, author, provenance, outcome, link, `q`, `overdue`, dates | operator |
| `POST` | `/prediction-store/predictions` | State a claim with its resolution criteria and odds on the ladder; writes the opening estimate | operator |
| `DELETE` | `/prediction-store/predictions/{id}` | Soft delete; `?hard=true` purges it with its estimates and links | operator |
| `GET` | `/prediction-store/predictions/{id}` | One prediction with its estimates and links | operator |
| `PATCH` | `/prediction-store/predictions/{id}` | Edit the descriptive fields; never the probability, the outcome or the status | operator |
| `GET` | `/prediction-store/predictions/{id}/estimates` | Every estimate, oldest first | operator |
| `POST` | `/prediction-store/predictions/{id}/estimates` | Append a revised probability; 409 unless the prediction is open | operator |
| `GET` | `/prediction-store/predictions/{id}/links` | What the prediction is about | operator |
| `POST` | `/prediction-store/predictions/{id}/links` | Link it to a record by the owning store's id; linking twice is a no-op | operator |
| `POST` | `/prediction-store/predictions/{id}/resolve` | Record the outcome, once: `true`, `false` or `void` | operator |
| `POST` | `/prediction-store/predictions/{id}/restore` | Undo a soft delete | operator |
| `GET` | `/prediction-store/tags` | Tags on live predictions with their counts, most used first | operator |
| `GET` | `/prediction-store/vocabulary` | Categories, provenances, statuses, outcomes, `group_by` keys and the probability ladder | operator |

### tool-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `POST` | `/tool-store/harness-tools/observed` | Record the built-in tools a harness reported; registers an unknown one switched off and `unreviewed` | operator |
| `GET` | `/tool-store/health` | `{"status":"ok"}` | operator |
| `GET` | `/tool-store/instances/{id}/tools` | The tools an instance opted in to that are on | operator |
| `DELETE` | `/tool-store/instances/{id}/tools/by-name/{name}` | Opt an instance out of a tool | operator |
| `POST` | `/tool-store/instances/{id}/tools/by-name/{name}` | Opt an instance in to a tool; refused while the tool is off | operator |
| `GET` | `/tool-store/kinds` | The tool kinds (`mcp`, `cli`, `local`, `harness`), each with a one-line description | operator |
| `GET` | `/tool-store/locals` | The in-process tools compiled into this server | operator |
| `POST` | `/tool-store/provision` | MCP config for `tools`, `tool_ids` or an `instance_id`'s opt-ins, with credentials resolved from auth-store | operator |
| `GET` | `/tool-store/tools` | List tools by `kind`, `harness`, `tag`, `q`, `enabled`, `not_seen_since`, `limit` | operator |
| `POST` | `/tool-store/tools` | Create or replace a tool by name; never writes `last_seen_at` | operator |
| `GET` | `/tool-store/tools/by-name/{name}` | One tool by name | operator |
| `GET` | `/tool-store/tools/by-name/{name}/instances` | Every instance opted in to a tool | operator |
| `POST` | `/tool-store/tools/by-name/{name}/invoke` | Run a local or CLI tool in this process; 409 for a harness tool | operator |
| `GET` | `/tool-store/tools/by-name/{name}/spec` | A CLI or MCP tool's launch spec; 409 for a harness tool | operator |
| `DELETE` | `/tool-store/tools/{id}` | Delete a tool and its instance opt-ins | operator |
| `GET` | `/tool-store/tools/{id}` | One tool | operator |
| `PATCH` | `/tool-store/tools/{id}` | Change a tool's `tags`, `description` or `enabled` | operator |
| `POST` | `/tool-store/tools/{id}/disable` | Switch a tool off in every instance and session | operator |
| `POST` | `/tool-store/tools/{id}/enable` | Switch a tool on everywhere (the master switch) | operator |

### skill-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/skill-store/health` | `{"status":"ok"}` | operator |
| `GET` | `/skill-store/installations` | Every installed skill | operator |
| `POST` | `/skill-store/seed/drift` | Keep the bytes of a file a machine changed | operator |
| `GET` | `/skill-store/seed/manifest` | The installed skill files a machine should hold, with version ids | operator |
| `POST` | `/skill-store/seed/observe` | Record the hashes a machine holds | operator |
| `GET` | `/skill-store/seed/profile` | Whether a machine is seeded with installed skills | operator |
| `PUT` | `/skill-store/seed/profile` | Switch seeding on or off for a machine | operator |
| `GET` | `/skill-store/skills` | List skills by `source`, `source_id`, `q`, `limit` | operator |
| `DELETE` | `/skill-store/skills/{id}` | Delete a skill | operator |
| `GET` | `/skill-store/skills/{id}` | One skill with its SKILL.md | operator |
| `GET` | `/skill-store/skills/{id}/files` | The files beside a skill's SKILL.md | operator |
| `GET` | `/skill-store/skills/{id}/files/{path...}` | One of a skill's files | operator |
| `POST` | `/skill-store/skills/{id}/install` | Copy a skill into this host's skills directory | operator |
| `POST` | `/skill-store/skills/{id}/uninstall` | Remove an installed skill | operator |
| `GET` | `/skill-store/sources` | Every skill source | operator |
| `POST` | `/skill-store/sources` | Register a git or local source | operator |
| `DELETE` | `/skill-store/sources/{id}` | Delete a source and its skills | operator |
| `GET` | `/skill-store/sources/{id}` | One source | operator |
| `POST` | `/skill-store/sources/{id}/sync` | Clone or read the source and record each SKILL.md in it | operator |
| `GET` | `/skill-store/versions/{vid}/content` | The bytes of one skill file version | operator |

### bundle-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/bundle-store/bundles` | List bundles | operator |
| `POST` | `/bundle-store/bundles` | Create or replace a bundle by name, with member ids | operator |
| `GET` | `/bundle-store/bundles/by-name/{name}` | One bundle by name | operator |
| `DELETE` | `/bundle-store/bundles/{id}` | Delete a bundle; refused while another extends it | operator |
| `GET` | `/bundle-store/bundles/{id}` | One bundle with its members | operator |
| `POST` | `/bundle-store/bundles/{id}/disable` | Switch a bundle off | operator |
| `POST` | `/bundle-store/bundles/{id}/enable` | Switch a bundle on | operator |
| `GET` | `/bundle-store/health` | `{"status":"ok"}` | operator |
| `GET` | `/bundle-store/member-effects` | The effects a bundle member may carry (`include`, `deny`) | operator |
| `POST` | `/bundle-store/resolve` | Compose bundles by repo and task tags or `bundle_ids`: tools, skills, denials, model and effort | operator |

### noteboard, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/noteboard/api/items` | List items by `type`, `tag`, `exclude_tag`, `status`, `list_id`, `created_by`, `parent_id`, `sort`, `limit`; held and deleted items only when asked for | operator |
| `POST` | `/noteboard/api/items` | Make an item: `type` and `title` required; `hold` makes it held from the start | operator |
| `POST` | `/noteboard/api/items/batch` | The named items in the order named, `null` for one that names no live item | operator |
| `POST` | `/noteboard/api/items/query` | Of these ids, which match the filters, in what order; held items answered, gone ones in `missing_ids` | operator |
| `GET` | `/noteboard/api/items/query-options` | The sorts and limits `/noteboard/api/items/query` takes | operator |
| `POST` | `/noteboard/api/items/rerank` | Set the `rank` of each named item | operator |
| `DELETE` | `/noteboard/api/items/{id}` | Soft delete, after a revision; `?hard=true` purges the row | operator |
| `GET` | `/noteboard/api/items/{id}` | One live item, its `updated_at` as the `ETag`; `?include_deleted=true` for a deleted one | operator |
| `PATCH` | `/noteboard/api/items/{id}` | Change the fields sent, after a revision of the old state; `If-Match` makes it conditional (412 with the current item) | operator |
| `POST` | `/noteboard/api/items/{id}/hold` | Hold an item, and with it its children, out of list and search; optional `reason` | operator |
| `GET` | `/noteboard/api/items/{id}/occurrences` | When the item's recurrence rule falls due and nags between `from` and `to` | operator |
| `POST` | `/noteboard/api/items/{id}/restore` | Undo a soft delete | operator |
| `GET` | `/noteboard/api/items/{id}/revisions` | Every prior state, newest first; `limit` and `offset` page it | operator |
| `POST` | `/noteboard/api/items/{id}/unhold` | Clear a hold | operator |
| `GET` | `/noteboard/api/lists` | Every `list_id` with its count of live items | operator |
| `GET` | `/noteboard/api/search` | Full-text search of title and body for `q`, every word in each term, newest first; held items only with `include_held=true` | operator |
| `GET` | `/noteboard/api/tags` | Every tag on live items with its count, most used first | operator |
| `GET` | `/noteboard/health` | `{"status":"ok","items":N}`, live items not archived; 500 when the store cannot be read | operator |

### project-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/project-store/digest` | Every live project's rollup as plain text for an agent | operator |
| `GET` | `/project-store/goal-links` | Which goals a record serves, by `entity_type` and `entity_ref` | operator |
| `GET` | `/project-store/goals` | Every project's goals, each project's together; `state`, `project_id` | operator |
| `GET` | `/project-store/goals/{goal_id}` | One goal | operator |
| `PATCH` | `/project-store/goals/{goal_id}` | Change a goal's fields, recording each change; a state move the rules do not allow is 409 | operator |
| `GET` | `/project-store/goals/{goal_id}/changes` | Every change to the goal's fields, oldest first | operator |
| `GET` | `/project-store/goals/{goal_id}/links` | What serves the goal; `entity_type` narrows | operator |
| `POST` | `/project-store/goals/{goal_id}/links` | Link a note or a session its own store has to the goal; 400, 502 or 409 as a project link | operator |
| `DELETE` | `/project-store/goals/{goal_id}/links/{link_id}` | Remove one goal link | operator |
| `GET` | `/project-store/health` | `{"status":"ok"}` | operator |
| `GET` | `/project-store/links` | Which projects own a record, by `entity_type` and `entity_ref` | operator |
| `GET` | `/project-store/projects` | Projects by name; `q`, `kind`, `parent_id`, `include_archived` | operator |
| `POST` | `/project-store/projects` | Make a project; `name` required | operator |
| `GET` | `/project-store/projects/{id}` | One project | operator |
| `PATCH` | `/project-store/projects/{id}` | Change a project's fields, recording each change; `archived` archives or restores it | operator |
| `GET` | `/project-store/projects/{id}/activity` | What happened in the project, newest first, over `days` (default 14) | operator |
| `GET` | `/project-store/projects/{id}/changes` | Every change to the project's fields, oldest first | operator |
| `GET` | `/project-store/projects/{id}/goals` | One project's goals, parents before their children; `state` narrows, and without it every state but `dismissed` | operator |
| `POST` | `/project-store/projects/{id}/goals` | Make a goal of a project: `statement` required, `open` or `proposed`; 400 for an archived project | operator |
| `GET` | `/project-store/projects/{id}/links` | What the project owns; `entity_type` narrows | operator |
| `POST` | `/project-store/projects/{id}/links` | Link a record its own store has; 400 when it has none, 502 when it cannot answer, 409 when linked already | operator |
| `DELETE` | `/project-store/projects/{id}/links/{link_id}` | Remove one link | operator |
| `POST` | `/project-store/projects/{id}/next-steps` | Make a noteboard todo tagged `next-step` and link it to the project | operator |
| `GET` | `/project-store/projects/{id}/rollup` | Worked out on request: children, links, cards, repos, notes, suggestions and goals | operator |
| `GET` | `/project-store/projects/{id}/suggestions` | The project's suggested next steps, oldest first; `status` narrows | operator |
| `POST` | `/project-store/projects/{id}/suggestions` | Ask a model for up to five next steps, superseding the undecided ones | operator |
| `GET` | `/project-store/session-filings` | What the session filer concluded about each chat, newest first; `status`, `session_id`, `project_id`, `limit` | operator |
| `POST` | `/project-store/session-filings/run` | One filing pass over the chats changed in the window; 502 with the report when some session could not be judged, 409 while a pass runs | operator |
| `POST` | `/project-store/session-filings/{session_filing_id}/accept` | File the session of a proposed filing; 409 when already decided | operator |
| `POST` | `/project-store/session-filings/{session_filing_id}/dismiss` | Leave a proposed filing's session unfiled; 409 when already decided | operator |
| `POST` | `/project-store/suggestions/{suggestion_id}/accept` | Make a suggestion a next step; 409 when already decided | operator |
| `POST` | `/project-store/suggestions/{suggestion_id}/dismiss` | Dismiss a suggestion; 409 when already decided | operator |
| `GET` | `/project-store/vocabulary` | Kinds, stages, statuses, activity kinds and the entity types a project can own | operator |

### principal-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/principal-store/availability-reasons` | The reasons an availability answer can give | principal |
| `POST` | `/principal-store/contacts/resolve` | Turn an email address into one contact principal | operator |
| `GET` | `/principal-store/health` | `{"status":"ok","counts":{principals,humans,groups,disabled}}` | operator |
| `GET` | `/principal-store/identity-bindings` | Identity bindings, by principal, provider, tenant | operator |
| `POST` | `/principal-store/identity-bindings` | Link an identity to a principal, as the calling administrator (as `POST /admin/identity-bindings`) | operator |
| `GET` | `/principal-store/identity-bindings/resolve` | Which principal an identity signs in as | operator |
| `GET` | `/principal-store/identity-bindings/{id}` | One identity binding | operator |
| `POST` | `/principal-store/identity-bindings/{id}/unlink` | Unlink an identity and end its login sessions (as `POST /admin/identity-bindings/{id}/unlink`) | operator |
| `GET` | `/principal-store/identity-providers` | The providers an identity binding can name | principal |
| `GET` | `/principal-store/kind-definitions` | Each kind with what it may do | principal |
| `GET` | `/principal-store/kinds` | The principal kinds | principal |
| `GET` | `/principal-store/principals` | List principals by `q`, `kind`, `include_disabled`, `available_at`, page | operator |
| `POST` | `/principal-store/principals` | Create a principal | operator |
| `DELETE` | `/principal-store/principals/{group}/members/{member}` | Take a principal out of a group | operator |
| `PUT` | `/principal-store/principals/{group}/members/{member}` | Put a principal in a group | operator |
| `GET` | `/principal-store/principals/{id}` | One principal with its groups or members | operator |
| `PATCH` | `/principal-store/principals/{id}` | Change the name, email, working week or administrator flag | operator |
| `GET` | `/principal-store/principals/{id}/availability` | Whether a person is available at `at`, and why | operator |
| `POST` | `/principal-store/principals/{id}/disable` | Disable a principal | operator |
| `POST` | `/principal-store/principals/{id}/enable` | Enable a principal | operator |
| `GET` | `/principal-store/principals/{id}/groups` | The groups a principal is in | operator |
| `GET` | `/principal-store/principals/{id}/members` | A group's members | operator |
| `GET` | `/principal-store/principals/{id}/time-off` | A person's time off | operator |
| `POST` | `/principal-store/principals/{id}/time-off` | Add time off | operator |
| `DELETE` | `/principal-store/principals/{id}/time-off/{timeOffID}` | Remove time off | operator |
| `GET` | `/principal-store/weekday-codes` | The day codes a working week uses | principal |

### grant-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/grant-store/grants` | List grants, narrowed to what the caller may read | principal or session agent |
| `POST` | `/grant-store/grants` | Grant a relation on a resource, once both owners confirm them | principal or session agent |
| `GET` | `/grant-store/grants/{id}` | One grant | principal or session agent |
| `POST` | `/grant-store/grants/{id}/revoke` | Revoke a grant | principal or session agent |
| `GET` | `/grant-store/health` | `{"status":"ok","counts":…}` | principal or session agent |
| `GET` | `/grant-store/principals/{id}/effective` | A principal's own and inherited active grants | principal or session agent |
| `GET` | `/grant-store/relations` | Each relation with its resource types and whether it is enforced | principal or session agent |
| `GET` | `/grant-store/resource-types` | The resource types a grant can name | principal or session agent |

### sign-in-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/sign-in-store/discord/callback` | Finish a Discord round trip | anyone |
| `GET` | `/sign-in-store/discord/start` | Start Sign in with Discord, or linking a Discord account | anyone |
| `GET` | `/sign-in-store/health` | `{"status":"ok"}` | anyone |
| `POST` | `/sign-in-store/link-requests` | Send a sign-in link to a person's own chat; the same answer whether or not they have an account | anyone |
| `POST` | `/sign-in-store/principals/{id}/discord-messages` | Send a message to a member's one-to-one Discord chat | operator |
| `POST` | `/sign-in-store/redeem` | Spend a link's secret and open a session cookie | anyone |
| `GET` | `/sign-in-store/session` | Who the browser's `sign_in_session` cookie signs in as | anyone |
| `GET` | `/sign-in-store/sessions` | Sign-in sessions, newest first | operator |
| `POST` | `/sign-in-store/sessions/{id}/revoke` | End a sign-in session, as the calling administrator | operator |
| `GET` | `/sign-in-store/sign-in-links` | Sign-in links made, newest first | operator |
| `POST` | `/sign-in-store/sign-in-links` | Make a one-time sign-in link for a principal, as the calling administrator | operator |
| `POST` | `/sign-in-store/sign-out` | End the browser's session | anyone |

### permission-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/permission-store/audit` | Verdicts given, newest first | operator |
| `POST` | `/permission-store/evaluate` | The verdict on one tool call; writes an audit row | operator |
| `GET` | `/permission-store/healthz` | `{"status":"ok"}` | operator |
| `GET` | `/permission-store/rules` | Rules, by `scope` | operator |
| `POST` | `/permission-store/rules` | Create a rule | operator |
| `DELETE` | `/permission-store/rules/{id}` | Delete a rule | operator |
| `GET` | `/permission-store/rules/{id}` | One rule | operator |
| `PATCH` | `/permission-store/rules/{id}` | Change a rule | operator |

### model-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/model-store/api/health` | `{"ok":true}` | operator |
| `GET` | `/model-store/api/models` | Every model with its health and aliases, by priority then id | operator |
| `POST` | `/model-store/api/models/toggle` | Switch a model on or off: `{"model","enabled"}` | operator |
| `GET` | `/model-store/api/roles` | Each role's first model, each role's list, and the canonical roles | operator |
| `POST` | `/model-store/api/roles` | Set a role's first model (`model`) or its whole list (`models`) | operator |
| `GET` | `/model-store/api/roles/{role}` | A role's first model, resolved | operator |
| `GET` | `/model-store/api/roles/{role}/models` | Every model in a role's list, in the order to try them | operator |

### scheduler, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/scheduler/api/calendar/events` | Every enabled job's cron occurrences between `from` and `to` (RFC3339) | operator |
| `GET` | `/scheduler/api/event-trigger-subjects` | The bus subjects a job's event trigger may name, with each one's fields | operator |
| `GET` | `/scheduler/api/job-references/{reference}` | A job by its reference id (`scheduler_job_000076`), with `reference_id` and `last_run` | operator |
| `GET` | `/scheduler/api/jobs` | Every job, by id | operator |
| `POST` | `/scheduler/api/jobs` | Create a shell or agent job; `"enabled":false` creates it switched off. 201 | operator |
| `DELETE` | `/scheduler/api/jobs/{id}` | Delete a job; its runs stay. 204 | operator |
| `GET` | `/scheduler/api/jobs/{id}` | One job | operator |
| `PATCH` | `/scheduler/api/jobs/{id}` | Change any field; an unknown field, a bad cron expression or an unrunnable job is 400 | operator |
| `DELETE` | `/scheduler/api/jobs/{id}/hold` | Clear a job's hold | operator |
| `POST` | `/scheduler/api/jobs/{id}/hold` | Hold a job: `{"days"}` or `{"until"}` (RFC3339) | operator |
| `POST` | `/scheduler/api/jobs/{id}/run` | Run a job now; 503 while `scheduler.runs.enabled` is off, 409 for a triggered job | operator |
| `GET` | `/scheduler/api/jobs/{id}/runs` | A job's newest runs with output, newest first: `limit` (1 to 100) | operator |
| `GET` | `/scheduler/api/runs/latest` | Every job's newest run, without output | operator |
| `GET` | `/scheduler/health` | `{"status","uptime_s","started_at"}` | operator |
| `GET` | `/scheduler/settings` | The model role each job binary and agent job asks for, stored here | operator |
| `PUT` | `/scheduler/settings/{key}` | Store a model role, checked with model-store: `{"value"}` | operator |

### rule-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/rule-store/activity` | The reactor's recent activity, newest first: `after`, `kind`, `limit` | operator |
| `GET` | `/rule-store/health` | Counts of each record, and the reactor's state | operator |
| `GET` | `/rule-store/rules` | Every rule, oldest first: `state` | operator |
| `POST` | `/rule-store/rules` | Create a draft rule: `{name, description?, source, created_by}`. 201 | operator |
| `GET` | `/rule-store/rules/{id}` | A rule with its revisions and activations | operator |
| `POST` | `/rule-store/rules/{id}/activate` | Activate a revision in `shadow` or `live` mode | operator |
| `GET` | `/rule-store/rules/{id}/comparison` | Whether each run's planned effects are visible at their owners now | operator |
| `POST` | `/rule-store/rules/{id}/pause` | Pause an active rule | operator |
| `POST` | `/rule-store/rules/{id}/preview` | What a revision or definition would decide about every session held now; records nothing | operator |
| `POST` | `/rule-store/rules/{id}/retire` | Retire a rule, for good | operator |
| `POST` | `/rule-store/rules/{id}/revisions` | Add a validated, immutable revision. 201 | operator |
| `GET` | `/rule-store/runs` | Decisions, newest first: `rule_id`, `subject_id`, `since`, `limit` | operator |
| `GET` | `/rule-store/subjects/{type}/{id}` | rule-store's copy of a subject and its state episode | operator |
| `GET` | `/rule-store/summary` | Counts, the reactor's state and every rule with its run counts | operator |
| `GET` | `/rule-store/vocabulary` | Sources, condition kinds, session fields, effect kinds, modes and rule states | operator |

### budget-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/budget-store/accounts` | The accounts in the books, each with its balance and details | principal or session agent |
| `POST` | `/budget-store/accounts` | Add an account; 201 | principal or session agent |
| `GET` | `/budget-store/accounts/{id}` | One account with its balance and details | principal or session agent |
| `PATCH` | `/budget-store/accounts/{id}` | Change an account's fields; a detail block replaces the stored one | principal or session agent |
| `POST` | `/budget-store/accounts/{id}/description-markers` | Add text that names this account in other accounts' rows | principal or session agent |
| `DELETE` | `/budget-store/accounts/{id}/description-markers/{marker_id}` | Remove a description marker | principal or session agent |
| `GET` | `/budget-store/accounts/{id}/loan-payment-facts` | What statements say about a loan's payments | principal or session agent |
| `POST` | `/budget-store/accounts/{id}/loan-payment-facts` | Record what a statement says about payments, replacing one on the same date | principal or session agent |
| `GET` | `/budget-store/accounts/{id}/loan-schedule` | A loan's payments split into interest and principal, and the projection to payoff | principal or session agent |
| `POST` | `/budget-store/accounts/{id}/loan-schedule/apply` | Write the loan schedule onto the loan's payments | principal or session agent |
| `POST` | `/budget-store/accounts/{id}/opening-balance` | Make the ledger agree with a reported balance with one opening-balance transaction | principal or session agent |
| `GET` | `/budget-store/balance-assertions` | Reported balances, each with the ledger's balance and the difference | principal or session agent |
| `POST` | `/budget-store/balance-assertions` | Record a reported balance; 201 | principal or session agent |
| `GET` | `/budget-store/budget` | One `month`'s budget in the limit or envelope `view` | principal or session agent |
| `PUT` | `/budget-store/budget-assignments` | Set one category's amount for one month | principal or session agent |
| `GET` | `/budget-store/categories` | The categories in the books | principal or session agent |
| `POST` | `/budget-store/categories` | Add a category; 201 | principal or session agent |
| `GET` | `/budget-store/categories/{id}` | One category | principal or session agent |
| `GET` | `/budget-store/category-paths` | Every category's path from the top of its tree | principal or session agent |
| `GET` | `/budget-store/category-suggestions` | Category suggestions by `status`, with each transaction's facts | principal or session agent |
| `POST` | `/budget-store/category-suggestions` | Record a model's category suggestions; it writes no category | principal or session agent |
| `GET` | `/budget-store/category-suggestions/candidates` | Wholly Uncategorized transactions with no pending suggestion, for the categorizer | principal or session agent |
| `POST` | `/budget-store/category-suggestions/decisions` | A person accepting, changing or rejecting suggestions, all or nothing | principal or session agent |
| `GET` | `/budget-store/health` | `{"status":"ok"}` | principal or session agent |
| `GET` | `/budget-store/import-batches` | Imported files, newest first, with what became of their rows | principal or session agent |
| `POST` | `/budget-store/import-batches` | Import one bank file into an account, all or nothing; 201 | principal or session agent |
| `GET` | `/budget-store/import-batches/{id}` | One imported file | principal or session agent |
| `POST` | `/budget-store/import-batches/{id}/undo` | Delete what an imported file made, keeping its rows | principal or session agent |
| `GET` | `/budget-store/institutions` | The banks and issuers in the books | principal or session agent |
| `POST` | `/budget-store/institutions` | Add a bank or issuer; 201 | principal or session agent |
| `GET` | `/budget-store/interests` | The interests followed in the books, each with its rules and total | principal or session agent |
| `POST` | `/budget-store/interests` | Add an interest; 201 | principal or session agent |
| `POST` | `/budget-store/interests/match` | Run every interest's rules again for new transactions | principal or session agent |
| `GET` | `/budget-store/interests/{id}` | One interest with its rules, history sources and total | principal or session agent |
| `POST` | `/budget-store/interests/{id}/history-sources` | Point an interest at another store's record of it | principal or session agent |
| `POST` | `/budget-store/interests/{id}/rules` | Add a rule to an interest and confirm what it matches | principal or session agent |
| `GET` | `/budget-store/interests/{id}/transactions` | An interest's transactions, removed ones included | principal or session agent |
| `PUT` | `/budget-store/interests/{id}/transactions/{transaction_id}` | A person confirming or removing a transaction on an interest | principal or session agent |
| `GET` | `/budget-store/links` | Links from one of the store's records (`from_id`) to other stores' records | principal or session agent |
| `POST` | `/budget-store/links` | Link one of the store's records to another store's record by its id; 201 | principal or session agent |
| `GET` | `/budget-store/marketplace-order-lines/{id}` | One line of a marketplace order | principal or session agent |
| `PATCH` | `/budget-store/marketplace-order-lines/{id}` | Change what became of an order line | principal or session agent |
| `GET` | `/budget-store/marketplace-orders` | Marketplace orders newest first, by `transaction_id` and `unmatched` | principal or session agent |
| `POST` | `/budget-store/marketplace-orders` | Record a marketplace order and its lines, finding its charge; 201 | principal or session agent |
| `POST` | `/budget-store/marketplace-orders/match-transactions` | Look again for the charge of every order without one | principal or session agent |
| `GET` | `/budget-store/marketplace-orders/{id}` | One marketplace order with its lines | principal or session agent |
| `GET` | `/budget-store/payees` | The payees in the books | principal or session agent |
| `POST` | `/budget-store/payees` | Add a payee and the bank texts that name it; 201 | principal or session agent |
| `POST` | `/budget-store/payees/apply-rules` | Name each transaction without a payee by the payee rules, and move a wholly Uncategorized one to its payee's category | principal or session agent |
| `GET` | `/budget-store/payees/{id}` | One payee with its patterns | principal or session agent |
| `PUT` | `/budget-store/plaid/accounts/{plaid_account_id}` | Match a Plaid account to one of the owner's accounts, or unmatch it | principal or session agent |
| `GET` | `/budget-store/plaid/items` | Every bank linked through Plaid, with its accounts | principal or session agent |
| `POST` | `/budget-store/plaid/items` | Link a bank from Plaid Link's public token, keeping its access token in auth-store; 201 | principal or session agent |
| `POST` | `/budget-store/plaid/link-token` | A Plaid Link token for linking one more bank | principal or session agent |
| `POST` | `/budget-store/plaid/sync` | Sync every linked bank's transactions from its stored cursor | principal or session agent |
| `GET` | `/budget-store/recurring-bills` | Recurring bills by `status`, soonest due first | principal or session agent |
| `POST` | `/budget-store/recurring-bills` | Name a bill from a transaction's payments, with the caller's cadence; 201 | principal or session agent |
| `POST` | `/budget-store/recurring-bills/detect` | Find payments that come on a steady rhythm and refresh the known bills | principal or session agent |
| `GET` | `/budget-store/recurring-bills/totals` | Confirmed bills: expected and actual per month, six months and year | principal or session agent |
| `GET` | `/budget-store/recurring-bills/{id}` | One bill with every payment | principal or session agent |
| `PATCH` | `/budget-store/recurring-bills/{id}` | A person confirming, rejecting or renaming a bill | principal or session agent |
| `GET` | `/budget-store/reports/by-account` | The named categories and those under them, per month, category and account | principal or session agent |
| `GET` | `/budget-store/reports/monthly` | Each category's total in each month from `from` to `to` | principal or session agent |
| `GET` | `/budget-store/reports/spending` | Each category's total in one `month` | principal or session agent |
| `GET` | `/budget-store/transactions` | Transactions newest first, by `account_id`, `category_id`, `month`, `q`, `uncategorized`, `limit` and `offset` | principal or session agent |
| `POST` | `/budget-store/transactions` | Record a balanced transaction by hand; 201 | principal or session agent |
| `GET` | `/budget-store/transactions/{id}` | One transaction with its postings, source rows, category decision, orders and links | principal or session agent |
| `PATCH` | `/budget-store/transactions/{id}` | Change a transaction's status, payee or memo | principal or session agent |
| `PUT` | `/budget-store/transactions/{id}/categories` | Replace a transaction's category postings with splits summing to the same | principal or session agent |
| `POST` | `/budget-store/transactions/{id}/move-to-account` | Turn a transaction's one category posting into a posting on another of the owner's accounts | principal or session agent |
| `POST` | `/budget-store/transactions/{id}/undo-transfer-without-partner` | Put a posting the transfer matcher moved without a partner back on Uncategorized | principal or session agent |
| `POST` | `/budget-store/transfers/match` | Pair transfers between the owner's own accounts | principal or session agent |
| `GET` | `/budget-store/trips/totals` | Confirmed and suggested spending per location-store trip | principal or session agent |
| `POST` | `/budget-store/trips/{trip_ref}/suggest` | Suggest the spending dated during a trip | principal or session agent |
| `GET` | `/budget-store/trips/{trip_ref}/transactions` | The transactions linked to one trip, removed ones included | principal or session agent |
| `PUT` | `/budget-store/trips/{trip_ref}/transactions/{transaction_id}` | A person confirming, removing or adding a transaction on a trip | principal or session agent |
| `GET` | `/budget-store/vocabulary` | Every closed set of values budget-store accepts | principal or session agent |

### people-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `PUT` | `/people-store/handle-activity` | Report how much the caller talks with each handle | principal or session agent |
| `GET` | `/people-store/health` | `{"status":"ok","counts":…}`, counted over the caller's own people | principal or session agent |
| `GET` | `/people-store/identities` | Every handle of a `kind`, in the order linked | principal or session agent |
| `GET` | `/people-store/identities/resolve` | The person holding a `kind` and `value`; 404 when nobody does | principal or session agent |
| `DELETE` | `/people-store/identities/{id}` | Remove a handle, refusing it for that person | principal or session agent |
| `GET` | `/people-store/identity-kinds` | The handle kinds and how each is normalized | principal or session agent |
| `GET` | `/people-store/link-sources` | Who may link a handle, and whether each overrides refusals | principal or session agent |
| `GET` | `/people-store/persons` | The caller's people by `q`, `tag`, `principal_id`, `include_deleted`, `limit` and `offset`, by name | principal or session agent |
| `POST` | `/people-store/persons` | Make a person owned by the caller, with handles; 201 | principal or session agent |
| `DELETE` | `/people-store/persons/{id}` | Delete a person, reversibly | principal or session agent |
| `GET` | `/people-store/persons/{id}` | One person with handles and relationships; a deleted one answers with `deleted_at` | principal or session agent |
| `PATCH` | `/people-store/persons/{id}` | Change a person's name, birthday, how met, tags or principal | principal or session agent |
| `POST` | `/people-store/persons/{id}/identities` | Link a handle to a person, saying who linked it and why; 201 | principal or session agent |
| `POST` | `/people-store/persons/{id}/relationships` | Record how two of one owner's people are related; 201 | principal or session agent |
| `POST` | `/people-store/persons/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/people-store/persons/{id}/revisions` | A person as they were before each change, oldest first | principal or session agent |
| `GET` | `/people-store/refusals` | Handles refused for a person, by `person_id`, `kind` and `value` | principal or session agent |
| `GET` | `/people-store/relationship-kinds` | The relationship kinds and their inverses | principal or session agent |
| `DELETE` | `/people-store/relationships/{id}` | Remove a relationship | principal or session agent |
| `POST` | `/people-store/reviews` | Mark handles reviewed | principal or session agent |
| `POST` | `/people-store/reviews/claim` | Take handles off the review queue for 30 minutes | principal or session agent |
| `GET` | `/people-store/reviews/pending` | Handles the caller talks with that nobody has reviewed | principal or session agent |
| `GET` | `/people-store/suggestions` | Guesses about who is who, by `status`, `q`, `sort`, `limit` and `offset` | principal or session agent |
| `POST` | `/people-store/suggestions` | Propose that some handles are one person; 201, or 200 when none is new | principal or session agent |
| `GET` | `/people-store/suggestions/{id}` | One suggestion | principal or session agent |
| `POST` | `/people-store/suggestions/{id}/accept` | Accept a suggestion into a new person or `person_id` | principal or session agent |
| `POST` | `/people-store/suggestions/{id}/reject` | Reject a suggestion, refusing its handles for the people it named | principal or session agent |

### calendar-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/calendar-store/calendars` | The caller's calendars, in the order made | principal or session agent |
| `POST` | `/calendar-store/calendars` | Make a calendar owned by the caller; 201, or 200 for the caller's one with that `source` and `source_ref` | principal or session agent |
| `POST` | `/calendar-store/calendars/{id}/google-event-imports` | Import Google Calendar event resources into a calendar; 201 | principal or session agent |
| `POST` | `/calendar-store/calendars/{id}/imports` | Import an .ics file into a calendar, in one transaction; 201 | principal or session agent |
| `GET` | `/calendar-store/events` | Events by `calendar_id`, `from`, `to`, `q`, `person_id`, `limit` and `offset`, oldest first | principal or session agent |
| `GET` | `/calendar-store/events/{id}` | One event, with its raw VEVENT | principal or session agent |
| `POST` | `/calendar-store/events/{id}/people` | Link one of the event owner's people to an event; 201 | principal or session agent |
| `DELETE` | `/calendar-store/events/{id}/people/{person_id}` | Unlink a person from an event | principal or session agent |
| `GET` | `/calendar-store/events/{id}/revisions` | An event as it was before each import changed it, newest first | principal or session agent |
| `GET` | `/calendar-store/health` | `{"status":"ok","counts":…}`, counted over the caller's own calendars | principal or session agent |
| `GET` | `/calendar-store/imports` | The caller's imports, newest first | principal or session agent |
| `GET` | `/calendar-store/vocabularies` | The link sources and revision reasons | principal or session agent |

### journal-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/journal-store/entries` | The caller's entries by `q`, `kind`, `tag`, `visibility`, `about_ref`, `source`, `from`, `to`, `limit` and `offset`, newest first | principal or session agent |
| `POST` | `/journal-store/entries` | Write an entry owned by the caller; 201 | principal or session agent |
| `DELETE` | `/journal-store/entries/{id}` | Delete an entry, reversibly; 204 | principal or session agent |
| `GET` | `/journal-store/entries/{id}` | One entry with its mentions | principal or session agent |
| `PATCH` | `/journal-store/entries/{id}` | Change an entry | principal or session agent |
| `POST` | `/journal-store/entries/{id}/promote-todo` | Make a checklist `line` a noteboard todo; an administrator's own entries only | principal or session agent |
| `POST` | `/journal-store/entries/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/journal-store/entries/{id}/revisions` | An entry as it was before each change, newest first | principal or session agent |
| `GET` | `/journal-store/entry-kinds` | The entry kinds | principal or session agent |
| `GET` | `/journal-store/health` | `{"status":…,"counts":…}`, counted over the caller's own entries | principal or session agent |
| `POST` | `/journal-store/imports` | Upsert entries by `source` and `source_ref`, in one transaction; `dry_run=true` writes nothing | principal or session agent |
| `GET` | `/journal-store/mentions` | The caller's entries that mention `ref` | principal or session agent |
| `GET` | `/journal-store/published` | The caller's published entries, newest first | principal or session agent |
| `GET` | `/journal-store/published/{slug}` | One of the caller's published entries by slug | principal or session agent |
| `GET` | `/journal-store/time-precisions` | The precisions a timeline item's time takes | principal or session agent |
| `GET` | `/journal-store/timeline-items` | Items of every `timeline` named, merged in time order | principal or session agent |
| `DELETE` | `/journal-store/timeline-items/{id}` | Delete a timeline item, reversibly; 204 | principal or session agent |
| `GET` | `/journal-store/timeline-items/{id}` | One timeline item | principal or session agent |
| `PATCH` | `/journal-store/timeline-items/{id}` | Change a timeline item | principal or session agent |
| `POST` | `/journal-store/timeline-items/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/journal-store/timeline-items/{id}/revisions` | A timeline item as it was before each change, newest first | principal or session agent |
| `GET` | `/journal-store/timelines/{id}/items` | A timeline's items, oldest first | principal or session agent |
| `POST` | `/journal-store/timelines/{id}/items` | Add items to a timeline, in one transaction; 201 | principal or session agent |
| `GET` | `/journal-store/visibilities` | The visibilities | principal or session agent |

### message-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/message-store/accounts` | The caller's accounts on platforms, its own first, then its bots | principal or session agent |
| `POST` | `/message-store/accounts` | The caller's account with that platform and handle, made if new | principal or session agent |
| `GET` | `/message-store/accounts/{id}` | One account | principal or session agent |
| `GET` | `/message-store/activity` | Message counts per time bucket, by `group_by`, `interval`, `time_zone`, `person`, `after`, `before` and `involved` | principal or session agent |
| `GET` | `/message-store/conversation-days` | Conversation days by `conversation`, `journal_entry`, `person`, `from`, `to`, `involved` and `limit` | principal or session agent |
| `POST` | `/message-store/conversation-days/journal-entries` | Record which journal entry held each day's transcript | principal or session agent |
| `GET` | `/message-store/conversation-days/{id}` | One conversation day, its conversation and its messages | principal or session agent |
| `GET` | `/message-store/conversation-merges` | Merged conversations, newest first, by `merged_into` and `limit` | principal or session agent |
| `GET` | `/message-store/conversations` | The caller's conversations, most recently active first, by `account`, `person`, `platform`, `q` and `limit` | principal or session agent |
| `GET` | `/message-store/conversations/{id}` | One conversation with participants and a message summary; a merged id is a 301 to the one it merged into | principal or session agent |
| `PATCH` | `/message-store/conversations/{id}/participants/{handle}` | Name a participant's people-store person, or clear it | principal or session agent |
| `GET` | `/message-store/health` | `{"status":"ok"}` when the database answers | principal or session agent |
| `GET` | `/message-store/imports` | The caller's import runs | principal or session agent |
| `POST` | `/message-store/imports` | Start an import run owned by the caller | principal or session agent |
| `GET` | `/message-store/imports/{id}` | One import run and what it wrote | principal or session agent |
| `POST` | `/message-store/imports/{id}/batches` | Write conversations, messages, days, supersessions and merges in one transaction | principal or session agent |
| `POST` | `/message-store/imports/{id}/finish` | End an import run | principal or session agent |
| `GET` | `/message-store/messages` | The caller's messages in time order by `conversation`, `person`, `platform`, `source`, `source_ref`, `after`, `before`, `q`, `order`, `limit` and `cursor` | principal or session agent |
| `GET` | `/message-store/messages/{id}` | One message; a superseded id is a 301 to the message that holds it now | principal or session agent |
| `GET` | `/message-store/messages/{id}/revisions` | Every earlier version of a message, oldest first | principal or session agent |
| `GET` | `/message-store/summary` | The caller's conversations and messages per platform, with first and last times | principal or session agent |
| `GET` | `/message-store/superseded-messages` | Retired message rows, newest first, by `superseded_by` and `limit` | principal or session agent |
| `GET` | `/message-store/vocabulary` | Platforms, message kinds, account kinds, merge rules, day involvements, activity groupings and intervals | principal or session agent |

### activity-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/activity-store/activities` | The caller's activities by `from`, `to`, `product`, `action`, `source`, `q`, `include_deleted`, `order`, `limit` and `offset` | principal or session agent |
| `DELETE` | `/activity-store/activities/{id}` | Delete an activity, reversibly; 204 | principal or session agent |
| `GET` | `/activity-store/activities/{id}` | One activity with its sightings and attachments | principal or session agent |
| `POST` | `/activity-store/activities/{id}/purge` | Blank an activity's words and destroy its attachments, keeping the row so no import brings it back | principal or session agent |
| `POST` | `/activity-store/activities/{id}/restore` | Undo a delete; a purged activity is 409 | principal or session agent |
| `GET` | `/activity-store/activities/{id}/revisions` | An activity as it was before each delete, restore and purge, newest first | principal or session agent |
| `GET` | `/activity-store/attachments/{id}` | An attachment's bytes, kept in file-store; a purged one is 410 | principal or session agent |
| `GET` | `/activity-store/days/{date}` | One local day (`tz` required) summarized by product | principal or session agent |
| `GET` | `/activity-store/health` | `{status, counts}`, counted over the caller's own activities | principal or session agent |
| `GET` | `/activity-store/imports` | The caller's imports, newest first | principal or session agent |
| `POST` | `/activity-store/imports` | Start an import owned by the caller, or answer the caller's one with that hash | principal or session agent |
| `GET` | `/activity-store/imports/{id}` | One import | principal or session agent |
| `POST` | `/activity-store/imports/{id}/batches` | Write activities in one transaction, folding what the caller already holds | principal or session agent |
| `POST` | `/activity-store/imports/{id}/finish` | End an import | principal or session agent |
| `GET` | `/activity-store/stats` | The caller's live activities by product, source and action | principal or session agent |
| `GET` | `/activity-store/vocabularies` | Actions, action phrases, sources, import kinds and revision reasons | principal or session agent |

### time-ledger-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/time-ledger-store/health` | `{status, counts}`, counted over the caller's own entries | principal or session agent |
| `GET` | `/time-ledger-store/kinds` | The caller's kinds, by name | principal or session agent |
| `POST` | `/time-ledger-store/kinds` | Make a kind owned by the caller; 201 | principal or session agent |
| `GET` | `/time-ledger-store/kinds/{id}` | One kind | principal or session agent |
| `GET` | `/time-ledger-store/links/{link_type}` | The caller's entries' links to one record, by `ref`, `occurrence_starts_at` and `include_removed` | principal or session agent |
| `GET` | `/time-ledger-store/reviews/{id}` | One review | principal or session agent |
| `GET` | `/time-ledger-store/time-entries` | The caller's entries by `from`, `to`, `ended_before`, `parent`, `kind_id`, `outcome`, `q`, `needs_review`, `include_deleted`, `limit` and `offset`, earliest first | principal or session agent |
| `POST` | `/time-ledger-store/time-entries` | Make an entry owned by the caller, with kinds and links, in one transaction; 201 | principal or session agent |
| `DELETE` | `/time-ledger-store/time-entries/{id}` | Delete an entry, reversibly, keeping its links; 204 | principal or session agent |
| `GET` | `/time-ledger-store/time-entries/{id}` | One entry with its links, children and reviews | principal or session agent |
| `PATCH` | `/time-ledger-store/time-entries/{id}` | Change an entry's title, span, place or parent, by who may | principal or session agent |
| `DELETE` | `/time-ledger-store/time-entries/{id}/kinds/{kind_id}` | Take a kind off an entry; 204 | principal or session agent |
| `PUT` | `/time-ledger-store/time-entries/{id}/kinds/{kind_id}` | Give an entry a kind | principal or session agent |
| `DELETE` | `/time-ledger-store/time-entries/{id}/links/{link_type}` | Take a link off an entry, keeping it as removed; 204 | principal or session agent |
| `POST` | `/time-ledger-store/time-entries/{id}/links/{link_type}` | Link a record of another store to an entry; 201 | principal or session agent |
| `GET` | `/time-ledger-store/time-entries/{id}/reviews` | An entry's reviews, newest first | principal or session agent |
| `POST` | `/time-ledger-store/time-entries/{id}/reviews` | Judge whether an entry happened; 201 | principal or session agent |
| `GET` | `/time-ledger-store/time-entries/{id}/revisions` | An entry as it was before each change, newest first | principal or session agent |
| `GET` | `/time-ledger-store/vocabularies` | Link types, bears-on values, actor kinds and outcomes | principal or session agent |

### photo-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/photo-store/date-sources` | The ways a photo's date is known, strongest first | principal or session agent |
| `GET` | `/photo-store/face-group-statuses` | The statuses a face group may hold | principal or session agent |
| `GET` | `/photo-store/face-groups` | The caller's face groups by `status`, `person_id`, `limit` and `offset`, the most photos first | principal or session agent |
| `POST` | `/photo-store/face-groups` | Make an open group of one owner's faces; 201 | principal or session agent |
| `GET` | `/photo-store/face-groups/{id}` | One face group with every face in it | principal or session agent |
| `POST` | `/photo-store/face-groups/{id}/decision` | A person saying who a group is, confirming or rejecting its faces as that person | principal or session agent |
| `POST` | `/photo-store/face-groups/{id}/guesses` | Record who a model or the context thinks a group is; 201 | principal or session agent |
| `POST` | `/photo-store/face-groups/{id}/members` | Add faces of the group's owner to an open group | principal or session agent |
| `POST` | `/photo-store/face-scans` | Record what one face model found on one of the caller's photos; 201 | principal or session agent |
| `GET` | `/photo-store/face-scans/pending` | Ids of the caller's photos a `model` has not scanned, oldest first | principal or session agent |
| `GET` | `/photo-store/faces` | Faces on the caller's live photos in id order, by `photo_id`, `model`, `after`, `include_embeddings` and `limit` | principal or session agent |
| `GET` | `/photo-store/faces/{id}/image` | One face cut from its photo's thumbnail, as a JPEG | principal or session agent |
| `GET` | `/photo-store/health` | `{"status":"ok","counts":…}`, counted over the caller's own photos | principal or session agent |
| `POST` | `/photo-store/imports` | Upsert photos owned by the caller by the original's sha256, thumbnails kept in file-store; description and tags are never touched | principal or session agent |
| `GET` | `/photo-store/people-counts` | How many of the caller's photos each person is confirmed and suggested on | principal or session agent |
| `GET` | `/photo-store/photo-people` | Who is on which of the caller's photos, by `photo_id`, `person_id`, `status`, `limit` and `offset` | principal or session agent |
| `POST` | `/photo-store/photo-people` | Propose or confirm a person of the photo owner's people on a photo; only ever moves a link upward | principal or session agent |
| `PATCH` | `/photo-store/photo-people/{id}` | A person's decision on who is on a photo | principal or session agent |
| `GET` | `/photo-store/photo-person-statuses` | The statuses a person on a photo may hold | principal or session agent |
| `GET` | `/photo-store/photos` | The caller's live photos by `from`, `to`, `folder`, `q`, `tag`, `person`, `described`, `has_gps`, `sha256`, `exclude_tag`, `limit` and `offset`, oldest taken first | principal or session agent |
| `DELETE` | `/photo-store/photos/{id}` | Delete a photo, reversibly; 204 | principal or session agent |
| `GET` | `/photo-store/photos/{id}` | One photo; a deleted one only with `include_deleted=true` | principal or session agent |
| `PATCH` | `/photo-store/photos/{id}` | Change a photo's description, tags or date | principal or session agent |
| `GET` | `/photo-store/photos/{id}/faces` | The faces found on one photo | principal or session agent |
| `POST` | `/photo-store/photos/{id}/purge` | Remove a picture that must not be kept: destroy its thumbnail and blank its description everywhere | principal or session agent |
| `POST` | `/photo-store/photos/{id}/restore` | Undo a delete; a purged photo is 409 | principal or session agent |
| `GET` | `/photo-store/photos/{id}/revisions` | A photo as it was before each change, newest first | principal or session agent |
| `GET` | `/photo-store/photos/{id}/thumbnail` | The photo's JPEG thumbnail, from file-store; a purged photo is 410 | principal or session agent |
| `GET` | `/photo-store/sightings` | Every place an import found each of the caller's photos, by `source_machine`, `limit` and `offset` | principal or session agent |
| `GET` | `/photo-store/tags` | Every tag on the caller's live photos, with how many carry it | principal or session agent |
| `GET` | `/photo-store/taken-activity` | The caller's photos counted per day, week, month or year in `time_zone`, with sample ids | principal or session agent |

### location-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/location-store/calendar-event-links` | The caller's links from calendar events to visits, flights and journeys | principal or session agent |
| `POST` | `/location-store/calendar-event-links` | Link a calendar event to one of the caller's records by hand; 201 | principal or session agent |
| `DELETE` | `/location-store/calendar-event-links/{id}` | Delete a calendar event link | principal or session agent |
| `GET` | `/location-store/calendar-event-links/{id}` | One calendar event link | principal or session agent |
| `GET` | `/location-store/calendar-series-places` | What each recurring calendar series came to in the caller's records | principal or session agent |
| `GET` | `/location-store/calendar-series-places/{id}` | One series place | principal or session agent |
| `POST` | `/location-store/calendar/match` | Link the calendar events sent to one owner's visits, flights and journeys | principal or session agent |
| `GET` | `/location-store/health` | `{"status":"ok","counts":…}`, counted over the caller's own records | principal or session agent |
| `GET` | `/location-store/home-periods` | The caller's home periods, oldest first | principal or session agent |
| `GET` | `/location-store/home-periods/{id}` | One home period | principal or session agent |
| `GET` | `/location-store/imports` | The caller's imports, newest first | principal or session agent |
| `POST` | `/location-store/imports` | Start an import owned by the caller, or answer the caller's one with that hash | principal or session agent |
| `GET` | `/location-store/imports/{id}` | One import | principal or session agent |
| `POST` | `/location-store/imports/{id}/batches` | Add a batch of records to an import, in one transaction | principal or session agent |
| `POST` | `/location-store/imports/{id}/finish` | Finish an import, applying a snapshot's span | principal or session agent |
| `GET` | `/location-store/journeys` | The caller's journeys | principal or session agent |
| `POST` | `/location-store/journeys/group` | Group one owner's movements into home periods and journeys | principal or session agent |
| `GET` | `/location-store/journeys/{id}` | One journey with its movements | principal or session agent |
| `POST` | `/location-store/localities/fill` | Name the town of every empty locality in one owner's records, offline, from GeoNames | principal or session agent |
| `POST` | `/location-store/phone-points/segment` | Make one owner's live phone points into visits and movements | principal or session agent |
| `GET` | `/location-store/place-suggestions` | The caller's place name suggestions by `status` and `place_id` | principal or session agent |
| `POST` | `/location-store/place-suggestions/proposals` | Propose names for one owner's unnamed places | principal or session agent |
| `GET` | `/location-store/place-suggestions/{id}` | One place name suggestion | principal or session agent |
| `POST` | `/location-store/place-suggestions/{id}/accept` | Accept a suggested name, naming the place | principal or session agent |
| `POST` | `/location-store/place-suggestions/{id}/reject` | Reject a suggested name | principal or session agent |
| `GET` | `/location-store/places` | The caller's places by `q`, `kind`, `tag` and `google_place_id`, oldest first | principal or session agent |
| `POST` | `/location-store/places` | Make a place owned by the caller; 201 | principal or session agent |
| `DELETE` | `/location-store/places/{id}` | Delete a place, reversibly | principal or session agent |
| `GET` | `/location-store/places/{id}` | One place; a deleted one only with `include_deleted=true` | principal or session agent |
| `PATCH` | `/location-store/places/{id}` | Change a place's name, address, description or tags | principal or session agent |
| `POST` | `/location-store/places/{id}/merge` | Merge this place into another of the same owner's (`into`), moving its visits, trips and links | principal or session agent |
| `POST` | `/location-store/places/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/location-store/places/{id}/revisions` | A place as it was before each change | principal or session agent |
| `GET` | `/location-store/points` | The caller's raw location points by `from`, `to`, `source` and `source_ref`, earliest first | principal or session agent |
| `POST` | `/location-store/points/live` | A phone's fixes as it makes them, owned by the caller | principal or session agent |
| `GET` | `/location-store/timeline` | The caller's visits and trips overlapping `from` to `to`, in start order | principal or session agent |
| `GET` | `/location-store/trips` | The caller's trips by `from`, `to`, `mode`, `parent_trip_id`, `source`, `tag` and the rest of CONTRACT.md's filters, earliest first | principal or session agent |
| `POST` | `/location-store/trips` | Make a trip owned by the caller; 201 | principal or session agent |
| `DELETE` | `/location-store/trips/{id}` | Delete a trip, reversibly | principal or session agent |
| `GET` | `/location-store/trips/{id}` | One trip | principal or session agent |
| `PATCH` | `/location-store/trips/{id}` | Change a trip's parent, places, mode, title, description or tags | principal or session agent |
| `POST` | `/location-store/trips/{id}/children` | Make trips of the same owner part of this one, all or none | principal or session agent |
| `GET` | `/location-store/trips/{id}/path` | The points a trip went through, in time order | principal or session agent |
| `POST` | `/location-store/trips/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/location-store/trips/{id}/revisions` | A trip as it was before each change | principal or session agent |
| `GET` | `/location-store/unnamed-places` | The caller's places with no name, the most visited first | principal or session agent |
| `GET` | `/location-store/visits` | The caller's visits by `from`, `to`, `place_id`, `source` and `tag`, earliest first | principal or session agent |
| `POST` | `/location-store/visits` | Make a visit owned by the caller; 201 | principal or session agent |
| `DELETE` | `/location-store/visits/{id}` | Delete a visit, reversibly | principal or session agent |
| `GET` | `/location-store/visits/{id}` | One visit | principal or session agent |
| `PATCH` | `/location-store/visits/{id}` | Change a visit's place, description or tags | principal or session agent |
| `POST` | `/location-store/visits/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/location-store/visits/{id}/revisions` | A visit as it was before each change | principal or session agent |
| `GET` | `/location-store/vocabularies` | The kinds, sources, modes and reasons location-store knows | principal or session agent |

### possession-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/possession-store/appliances` | The caller's possessions of kind appliance, by the possession filters | principal or session agent |
| `GET` | `/possession-store/appliances/{id}` | The possession a appliance record (`appliance_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/appliances/{id}/image` | The cover of the possession a appliance record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/art-and-decor` | The caller's possessions of kind art and decor, by the possession filters | principal or session agent |
| `GET` | `/possession-store/art-and-decor/{id}` | The possession a art and decor record (`art_and_decor_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/art-and-decor/{id}/image` | The cover of the possession a art and decor record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/audio-devices` | The caller's possessions of kind audio device, by the possession filters | principal or session agent |
| `GET` | `/possession-store/audio-devices/{id}` | The possession a audio device record (`audio_device_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/audio-devices/{id}/image` | The cover of the possession a audio device record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/bags-and-cases` | The caller's possessions of kind bag or case, by the possession filters | principal or session agent |
| `GET` | `/possession-store/bags-and-cases/{id}` | The possession a bag or case record (`bag_or_case_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/bags-and-cases/{id}/image` | The cover of the possession a bag or case record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/collectibles` | The caller's possessions of kind collectible, by the possession filters | principal or session agent |
| `GET` | `/possession-store/collectibles/{id}` | The possession a collectible record (`collectible_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/collectibles/{id}/image` | The cover of the possession a collectible record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/computer-parts` | The caller's possessions of kind computer part, by the possession filters | principal or session agent |
| `GET` | `/possession-store/computer-parts/{id}` | The possession a computer part record (`computer_part_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/computer-parts/{id}/image` | The cover of the possession a computer part record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/computers` | The caller's possessions of kind computer, by the possession filters | principal or session agent |
| `GET` | `/possession-store/computers/{id}` | The possession a computer record (`computer_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/computers/{id}/image` | The cover of the possession a computer record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/electronic-accessories` | The caller's possessions of kind electronic accessory, by the possession filters | principal or session agent |
| `GET` | `/possession-store/electronic-accessories/{id}` | The possession a electronic accessory record (`electronic_accessory_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/electronic-accessories/{id}/image` | The cover of the possession a electronic accessory record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/eyewear` | The caller's possessions of kind eyewear, by the possession filters | principal or session agent |
| `GET` | `/possession-store/eyewear/{id}` | The possession a eyewear record (`eyewear_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/eyewear/{id}/image` | The cover of the possession a eyewear record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/fitness-equipment` | The caller's possessions of kind fitness equipment, by the possession filters | principal or session agent |
| `GET` | `/possession-store/fitness-equipment/{id}` | The possession a fitness equipment record (`fitness_equipment_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/fitness-equipment/{id}/image` | The cover of the possession a fitness equipment record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/furniture` | The caller's possessions of kind furniture, by the possession filters | principal or session agent |
| `GET` | `/possession-store/furniture/{id}` | The possession a furniture record (`furniture_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/furniture/{id}/image` | The cover of the possession a furniture record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/garments` | The caller's possessions of kind garment, by the possession filters | principal or session agent |
| `GET` | `/possession-store/garments/{id}` | The possession a garment record (`garment_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/garments/{id}/image` | The cover of the possession a garment record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/harness-machines/{machine_id}/computer` | The caller's computer a harness-store machine is | principal or session agent |
| `GET` | `/possession-store/health` | `{"status":"ok","counts":…}`, counted over the caller's own records | principal or session agent |
| `GET` | `/possession-store/home-textiles` | The caller's possessions of kind home textile, by the possession filters | principal or session agent |
| `GET` | `/possession-store/home-textiles/{id}` | The possession a home textile record (`home_textile_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/home-textiles/{id}/image` | The cover of the possession a home textile record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/household-tools` | The caller's possessions of kind household tool, by the possession filters | principal or session agent |
| `GET` | `/possession-store/household-tools/{id}` | The possession a household tool record (`household_tool_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/household-tools/{id}/image` | The cover of the possession a household tool record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/jewelry` | The caller's possessions of kind jewelry, by the possession filters | principal or session agent |
| `GET` | `/possession-store/jewelry/{id}` | The possession a jewelry record (`jewelry_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/jewelry/{id}/image` | The cover of the possession a jewelry record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/kinds` | The kinds a possession may have, each with its fields and id prefix | principal or session agent |
| `POST` | `/possession-store/marketplace-order-lines/match` | Both readers match one owner's open order lines to possessions | principal or session agent |
| `POST` | `/possession-store/marketplace-orders/import-ebay-mail` | Read one owner's eBay order mail with the first reader and record each order in budget-store | principal or session agent |
| `GET` | `/possession-store/media` | The caller's possessions of kind media, by the possession filters | principal or session agent |
| `GET` | `/possession-store/media/{id}` | The possession a media record (`media_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/media/{id}/image` | The cover of the possession a media record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/music-accessories` | The caller's possessions of kind music accessory, by the possession filters | principal or session agent |
| `GET` | `/possession-store/music-accessories/{id}` | The possession a music accessory record (`music_accessory_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/music-accessories/{id}/image` | The cover of the possession a music accessory record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/musical-instruments` | The caller's possessions of kind musical instrument, by the possession filters | principal or session agent |
| `GET` | `/possession-store/musical-instruments/{id}` | The possession a musical instrument record (`musical_instrument_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/musical-instruments/{id}/image` | The cover of the possession a musical instrument record belongs to, as a JPEG | principal or session agent |
| `PUT` | `/possession-store/originals/{sha256}` | Keep an original's bytes in file-store for the owner the request acts for; answers where photo-store should say it is | principal or session agent |
| `POST` | `/possession-store/outfit-covers/read-wearer-boxes` | Read the wearer box of every cover photo of one owner's outfits that has none | principal or session agent |
| `GET` | `/possession-store/outfit-looks` | The caller's looks by `worn_on` and `outfit_id` | principal or session agent |
| `POST` | `/possession-store/outfit-looks/read` | Both readers read each day's photos of one owner into looks and slots | principal or session agent |
| `GET` | `/possession-store/outfit-looks/{id}` | One look with its photos and slots | principal or session agent |
| `GET` | `/possession-store/outfit-merges` | The caller's outfit merges, by `outfit_id`, newest first | principal or session agent |
| `GET` | `/possession-store/outfit-merges/{id}` | One outfit merge and every row it moved | principal or session agent |
| `POST` | `/possession-store/outfit-merges/{id}/undo` | Undo an outfit merge | principal or session agent |
| `POST` | `/possession-store/outfit-pieces/rematch` | Match one owner's stuck outfit pieces by picture with both readers | principal or session agent |
| `GET` | `/possession-store/outfit-rebuilds/{id}` | One rebuild of outfits from looks | principal or session agent |
| `POST` | `/possession-store/outfit-rebuilds/{id}/undo` | Undo a rebuild | principal or session agent |
| `POST` | `/possession-store/outfit-slots/identify` | Both readers sort one owner's slots into pieces and link each to its best guess | principal or session agent |
| `GET` | `/possession-store/outfit-slots/{id}` | One slot with its boxes and candidates | principal or session agent |
| `GET` | `/possession-store/outfit-slots/{id}/image` | One slot cut from its photo, as a JPEG | principal or session agent |
| `GET` | `/possession-store/outfit-tags` | Every tag on the caller's live outfits, the most used first | principal or session agent |
| `GET` | `/possession-store/outfits` | The caller's outfits by `status`, `possession_id`, `favorite`, `tag`, `limit` and `offset`, with their covers | principal or session agent |
| `POST` | `/possession-store/outfits` | Make an outfit owned by the caller of its wearables; 201 | principal or session agent |
| `POST` | `/possession-store/outfits/consolidate` | Both readers look for one owner's outfits recorded twice, combining what both see as one | principal or session agent |
| `POST` | `/possession-store/outfits/rebuild-from-looks` | Place every look of one owner into outfits again, undoably | principal or session agent |
| `DELETE` | `/possession-store/outfits/{id}` | Delete an outfit; 204 | principal or session agent |
| `GET` | `/possession-store/outfits/{id}` | One outfit with its cover | principal or session agent |
| `PATCH` | `/possession-store/outfits/{id}` | Change an outfit, its pieces, favorite or tags | principal or session agent |
| `POST` | `/possession-store/outfits/{id}/combine` | Combine a duplicate outfit into another of the same owner's, undoably | principal or session agent |
| `PUT` | `/possession-store/outfits/{id}/cover` | Pick an outfit's cover photo, or take the pick away | principal or session agent |
| `GET` | `/possession-store/outfits/{id}/image` | An outfit's cover cut to the wearer, as a JPEG | principal or session agent |
| `POST` | `/possession-store/outfits/{id}/wearings` | Record a day an outfit was worn; 201 | principal or session agent |
| `GET` | `/possession-store/owner-questions` | The caller's questions, open ones by default, by `photo_review_id`, `purpose`, `include_answered` | principal or session agent |
| `POST` | `/possession-store/owner-questions/triage-outfit-doubts` | Sort one owner's outfit questions into decisions and details | principal or session agent |
| `GET` | `/possession-store/owner-questions/{id}` | One question with its choices | principal or session agent |
| `POST` | `/possession-store/owner-questions/{id}/answer` | Answer a question by a choice or in words, applying what it decides | principal or session agent |
| `GET` | `/possession-store/owner-questions/{id}/image` | The picture a question asks about, as a JPEG | principal or session agent |
| `GET` | `/possession-store/personal-care` | The caller's possessions of kind personal care, by the possession filters | principal or session agent |
| `GET` | `/possession-store/personal-care/{id}` | The possession a personal care record (`personal_care_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/personal-care/{id}/image` | The cover of the possession a personal care record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/photo-reviews` | The caller's photo reviews by `status`, newest first | principal or session agent |
| `POST` | `/possession-store/photo-reviews` | Queue one of the caller's photos for review; 201, or 200 with the review already queued | principal or session agent |
| `GET` | `/possession-store/photo-reviews/{id}` | One review with its readings, sightings and questions | principal or session agent |
| `POST` | `/possession-store/photo-reviews/{id}/retry` | Queue a failed review again | principal or session agent |
| `GET` | `/possession-store/possession-merges` | The caller's possession merges, by `possession_id`, newest first | principal or session agent |
| `GET` | `/possession-store/possession-merges/{id}` | One possession merge and every row it moved | principal or session agent |
| `POST` | `/possession-store/possession-merges/{id}/undo` | Undo a possession merge | principal or session agent |
| `GET` | `/possession-store/possessions` | The caller's possessions by `q`, `status`, `kind`, `place_id`, `include_deleted`, `limit` and `offset`, newest first | principal or session agent |
| `POST` | `/possession-store/possessions` | Make a possession owned by the caller, with at most one kind and its photos; 201 | principal or session agent |
| `POST` | `/possession-store/possessions/classify-kindless` | Give one owner's possessions with no kind one, by the first reader; asks when unsure | principal or session agent |
| `DELETE` | `/possession-store/possessions/{id}` | Delete a possession, reversibly; 204 | principal or session agent |
| `GET` | `/possession-store/possessions/{id}` | One possession with its kind, photos, outfits and product | principal or session agent |
| `PATCH` | `/possession-store/possessions/{id}` | Change a possession's fields or give it a kind | principal or session agent |
| `GET` | `/possession-store/possessions/{id}/image` | A possession's cover cut from its photo, as a JPEG | principal or session agent |
| `PUT` | `/possession-store/possessions/{id}/kind` | Move a possession to another kind | principal or session agent |
| `POST` | `/possession-store/possessions/{id}/merge` | Merge a duplicate possession into another of the same owner's, undoably | principal or session agent |
| `POST` | `/possession-store/possessions/{id}/photos` | Link a photo of the owner's to a possession, with its box | principal or session agent |
| `DELETE` | `/possession-store/possessions/{id}/photos/{photo_id}` | Unlink a photo from a possession | principal or session agent |
| `POST` | `/possession-store/possessions/{id}/restore` | Undo a delete | principal or session agent |
| `GET` | `/possession-store/possessions/{id}/revisions` | A possession's rows as they were before each change, newest first | principal or session agent |
| `GET` | `/possession-store/products` | The caller's products by `q`, newest first | principal or session agent |
| `POST` | `/possession-store/products` | Make a product owned by the caller from its listing in article-store; 201 | principal or session agent |
| `GET` | `/possession-store/products/{id}` | One product with its possessions | principal or session agent |
| `PATCH` | `/possession-store/products/{id}` | Change a product, reading its listing again when it moves | principal or session agent |
| `PATCH` | `/possession-store/purchase-lines/{id}` | Say what a purchase line became; 204 | principal or session agent |
| `GET` | `/possession-store/purchases` | The caller's purchases by `marketplace`, newest first | principal or session agent |
| `POST` | `/possession-store/purchases` | Record a purchase owned by the caller, line by line; 201 | principal or session agent |
| `DELETE` | `/possession-store/purchases/{id}` | Delete a purchase, withdrawing its questions | principal or session agent |
| `GET` | `/possession-store/purchases/{id}` | One purchase with its lines | principal or session agent |
| `POST` | `/possession-store/sightings/record-agreed` | Record the sightings of one owner both readers agreed on that earlier reviews held back | principal or session agent |
| `GET` | `/possession-store/sightings/{id}` | One sighting | principal or session agent |
| `GET` | `/possession-store/sightings/{id}/image` | One sighting cut from its photo, as a JPEG | principal or session agent |
| `GET` | `/possession-store/sports-gear` | The caller's possessions of kind sports gear, by the possession filters | principal or session agent |
| `GET` | `/possession-store/sports-gear/{id}` | The possession a sports gear record (`sports_gear_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/sports-gear/{id}/image` | The cover of the possession a sports gear record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/tabletop-games` | The caller's possessions of kind tabletop game, by the possession filters | principal or session agent |
| `GET` | `/possession-store/tabletop-games/{id}` | The possession a tabletop game record (`tabletop_game_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/tabletop-games/{id}/image` | The cover of the possession a tabletop game record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/vehicles` | The caller's possessions of kind vehicle, by the possession filters | principal or session agent |
| `GET` | `/possession-store/vehicles/{id}` | The possession a vehicle record (`vehicle_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/vehicles/{id}/image` | The cover of the possession a vehicle record belongs to, as a JPEG | principal or session agent |
| `POST` | `/possession-store/vehicles/{id}/odometer-readings` | Record a vehicle's odometer; 201 | principal or session agent |
| `GET` | `/possession-store/vocabulary` | Every vocabulary a field takes its words from | principal or session agent |
| `GET` | `/possession-store/watches` | The caller's possessions of kind watch, by the possession filters | principal or session agent |
| `GET` | `/possession-store/watches/{id}` | The possession a watch record (`watch_…`) belongs to | principal or session agent |
| `GET` | `/possession-store/watches/{id}/image` | The cover of the possession a watch record belongs to, as a JPEG | principal or session agent |
| `GET` | `/possession-store/wearable-groups` | The caller's wearable groups with their members by status | principal or session agent |
| `PUT` | `/possession-store/wearable-groups/{group}` | Close or open a wearable group, or say how many pieces it holds | principal or session agent |
| `POST` | `/possession-store/wearables/check-duplicates` | Both readers look for one owner's wearables recorded twice, merging what both see as one | principal or session agent |
| `POST` | `/possession-store/wearables/check-links` | Both readers check one owner's wearables' photo links, unlinking what both doubt | principal or session agent |

### repo-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/repo-store/by-path` | One repo by `path` | operator |
| `GET` | `/repo-store/deployments` | One repo's deploys (`repo_id`, `limit`), newest first | operator |
| `GET` | `/repo-store/deployments/latest` | The newest deploy of every repo | operator |
| `POST` | `/repo-store/detect` | Detect the tags of a `path`, storing nothing | operator |
| `GET` | `/repo-store/health` | `{"status":"ok"}` | operator |
| `GET` | `/repo-store/pull-requests` | Pull requests by `repo_id`, `state` (`open`, `closed`, `merged`, `all`) and `limit` | operator |
| `GET` | `/repo-store/repos` | List repos by `tag`, `q`, `provenance` (`ours`, `vendored`, `unstated`) and `limit`, by name | operator |
| `POST` | `/repo-store/repos` | Make or refresh a repo by name, its tags detected from `path`; 201 when new | operator |
| `GET` | `/repo-store/repos/by-mention/{mention}` | One repo by a `repo:<name>` mention | operator |
| `GET` | `/repo-store/repos/by-name/{name}` | One repo by name | operator |
| `DELETE` | `/repo-store/repos/{id}` | Delete a repo, its deploys and pull requests | operator |
| `GET` | `/repo-store/repos/{id}` | One repo, with `github_url` and `effective_tags` | operator |
| `POST` | `/repo-store/repos/{id}/deployments` | Append a deploy to the ledger, as deploy-gate does | operator |
| `POST` | `/repo-store/repos/{id}/detect` | Detect its tags from its path again | operator |
| `PUT` | `/repo-store/repos/{id}/identity` | Rename it and move its path and remote, keeping its id; 409 when another repo holds the name or path | operator |
| `PUT` | `/repo-store/repos/{id}/provenance` | Say who authors it: `ours`, `vendored` or `""` | operator |
| `POST` | `/repo-store/repos/{id}/pull-requests/sync` | Read the repo's pull requests from GitHub now; 503 when this server cannot run gh | operator |

### work-graph-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/work-graph-store/activity` | The branches sessions touched in the last `hours`; `format=text` for an agent | operator |
| `GET` | `/work-graph-store/commits/{mention...}` | One commit by a `<repo>@<sha>` mention, with its repo and maker | operator |
| `GET` | `/work-graph-store/health` | `{"status":"ok"}` | operator |
| `GET` | `/work-graph-store/ref-updates` | Ref updates by `repo_store_id`, `session_id`, `ref`, `since_unix_nano` and `limit`, newest first | operator |
| `GET` | `/work-graph-store/repos/{repo_store_id}/graph` | A repo's commit graph with each commit's maker and each branch's sessions | operator |
| `GET` | `/work-graph-store/session-repos` | How many refs each session moved in each repo since `since_unix_nano` | operator |

### file-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/file-store/files` | Files by `owner_service`, `owner_ref`, `sha256`, `include_deleted`, `limit` and `offset`, oldest first | operator |
| `POST` | `/file-store/files` | Keep the body as a file of `owner_service` and `owner_ref`, named `filename`; 201 and the record | operator |
| `DELETE` | `/file-store/files/{id}` | Delete, reversibly; `hard=true` purges the row and, with the last row naming them, the bytes | operator |
| `GET` | `/file-store/files/{id}` | One file's record; `include_deleted=true` for a deleted one | operator |
| `GET` | `/file-store/files/{id}/content` | The bytes, as a download unless `inline=true` and the type is on the inline list; ranges work | operator |
| `POST` | `/file-store/files/{id}/restore` | Undo a delete | operator |
| `GET` | `/file-store/health` | `{"status":"ok","files":…,"blobs":…,"blob_bytes":…}` | operator |
| `GET` | `/file-store/limits` | The size limit and the inline content types in force | operator |

### reference-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/reference-store/attachments/{id}` | A picture of the record, when the caller may see it | principal or session agent |
| `GET` | `/reference-store/health` | `{"status":"ok"}` | anyone |
| `POST` | `/reference-store/resolve` | What each of `ids` (at most 128) names, narrowed to what the caller may see | principal or session agent |
| `GET` | `/reference-store/search` | Records matching `q` in every mentionable type the caller may see whole | principal or session agent |
| `GET` | `/reference-store/types` | Every reference type: id patterns, owning service, label and summary fields, page, access | anyone |
| `GET` | `/reference-store/types/{type}` | One reference type | anyone |
| `GET` | `/reference-store/vocabulary` | The summary formats, the access rules and the grant names the types use | anyone |

### usage-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/usage-store/api/usage` | Token totals per agent, orchestrator and model over the last `day`, `week` and `month`, from model-store's old daily usage rows | operator |
| `GET` | `/usage-store/api/usage/limits` | The latest limits snapshot of `anthropic` and `codex`, each with `stale_after` | operator |
| `POST` | `/usage-store/api/usage/limits/refresh` | Read `?provider=anthropic|codex` now and keep the snapshot | operator |
| `POST` | `/usage-store/api/usage/limits/watch` | The usage page is open: read limits on the watched interval for a while. 204 | operator |
| `GET` | `/usage-store/api/usage/limits/{provider}` | One provider's latest limits snapshot; 404 when it has none | operator |
| `GET` | `/usage-store/api/usage/limits/{provider}/history` | One provider's snapshot rows, oldest first: `window`, `since`, `until`, `limit` | operator |
| `GET` | `/usage-store/api/usage/spend/keys` | Each account's API keys with their 24h, 7d and 30d spend, top-ups and remaining credit | operator |
| `GET` | `/usage-store/api/usage/spend/keys/{provider}/{api_key_id}/raw` | The provider's last answer about one key, as it came | operator |
| `POST` | `/usage-store/api/usage/spend/refresh` | Read `?provider=anthropic` per-key spend now | operator |
| `GET` | `/usage-store/api/usage/spend/topups` | A provider's top-ups, earliest first: `?provider=` | operator |
| `POST` | `/usage-store/api/usage/spend/topups` | Record credit bought: `{"provider","amount_usd","occurred_at"|"occurred_at_str","note"}` | operator |
| `DELETE` | `/usage-store/api/usage/spend/topups/{id}` | Delete one top-up. 204 | operator |
| `GET` | `/usage-store/health` | `{"status":"ok"}` | operator |

### eval-store, mounted in this process

| Method | Route | What it does | Who may call |
|---|---|---|---|
| `GET` | `/eval-store/cases/{id}` | One case with its prompts and original answer | operator |
| `PATCH` | `/eval-store/cases/{id}` | Replace a case's `expected_answer` | operator |
| `GET` | `/eval-store/health` | Counts of each record | operator |
| `GET` | `/eval-store/runs` | Runs, newest first: `?suite_id=` | operator |
| `POST` | `/eval-store/runs` | Create a run, freezing its cases and model-store's prices | operator |
| `DELETE` | `/eval-store/runs/{id}` | Delete a run with its answers, verdicts and checks; 409 while it is going | operator |
| `GET` | `/eval-store/runs/{id}` | One run and its progress | operator |
| `PATCH` | `/eval-store/runs/{id}` | Change a run's `prediction_id` or `note` | operator |
| `GET` | `/eval-store/runs/{id}/answers` | A run's answers; bodies only with `include_bodies=true` | operator |
| `GET` | `/eval-store/runs/{id}/checks` | A run's check results | operator |
| `POST` | `/eval-store/runs/{id}/checks` | Run the suite's checker over a run's answers again | operator |
| `GET` | `/eval-store/runs/{id}/cost-projection` | What each model would cost a week, and what reaches `target_usd` | operator |
| `GET` | `/eval-store/runs/{id}/report` | A run's report: `format=json|markdown`, `calls_per_week` | operator |
| `POST` | `/eval-store/runs/{id}/resume` | Make what a run is missing, and with `retry_failed` what failed (202); 503 while runs are off | operator |
| `POST` | `/eval-store/runs/{id}/start` | Start a created run (202); 503 while `eval_store.runs.enabled` is off | operator |
| `POST` | `/eval-store/runs/{id}/stop` | Finish the calls in flight, then stop | operator |
| `GET` | `/eval-store/runs/{id}/verdicts` | A run's verdicts; bodies and reasons only with `include_bodies=true` | operator |
| `GET` | `/eval-store/suites` | Every suite | operator |
| `POST` | `/eval-store/suites` | Create a suite | operator |
| `DELETE` | `/eval-store/suites/{id}` | Delete a suite; 409 while it has runs | operator |
| `GET` | `/eval-store/suites/{id}` | One suite | operator |
| `PATCH` | `/eval-store/suites/{id}` | Change the suite fields named | operator |
| `GET` | `/eval-store/suites/{id}/cases` | A suite's cases without their prompts or answers | operator |
| `POST` | `/eval-store/suites/{id}/cases` | Add one case | operator |
| `POST` | `/eval-store/suites/{id}/cases/import-operations` | Import succeeded llm.completion operations as cases | operator |
| `GET` | `/eval-store/vocabulary` | Run statuses, source kinds, checkers, selection modes and answer origins | operator |
<!-- route-table:end -->

## Testing

| Command | Needs | Covers |
|---|---|---|
| `go test ./...` | nothing | Unit tests, and the conformance matrix against `cmd/mock-harness` |
| `go test -tags pty_integration ./...` | `claude` and `llm-bridge-claudecode` on `PATH` | A real pty-mode session: attach, a keystroke each way, stop |
| `go test -tags convenience_events_integration ./...` | the same, signed in | A real turn, checking the `agent_state`, `usage_total` and `turn_complete` events |
| `go test -tags harness_live -run 'TestHarness(ModeSwitch\|ExternalCLI)Live' -v -timeout 30m ./internal/server/` | `claude`, `codex` and both wrappers on `PATH`, signed in; `~/.config/model-store/store.db` | Per harness: events → pty → events keeps the conversation and model, and the dash's stream and session row follow the pty turn; with the external activity watcher running, a turn run with the CLI outside the bridge, and a conversation started there, reach the dash |

`TestHarnessCLIUpdateJobLive` (same tag) checks scheduler job 107, which runs `scripts/update-harness-clis.sh` every night to keep both CLIs at npm's latest version: the job is on, runs that script daily, and its last run passed.

The tagged tests skip when a binary is missing. The `harness_live` tests spend real model calls on the `efficient` role's model for each harness's provider, read from a copy of production's model registry. They clear the `CLAUDE*`, `CODEX_*` and `LLM_BRIDGE_*` variables an agent session sets, since a harness child that inherits them believes it is nested and Claude Code stops writing its session file.

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
- `CC-VERIFIED.md`: Claude Code flags and behavior, checked by hand

`docs/plans/` holds plans, some done in part and some not started: `CODEX-PARITY`, `IMPLEMENTATION-ROADMAP`, `CONTEXT-MIGRATION`, `TEAM-ORCHESTRATION`, `CLI-SURFACE`. Check a plan against the code before relying on it.
