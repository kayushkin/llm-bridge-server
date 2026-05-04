# Retrospective: CC permission gating via embedded MCP

## What we built

The first implementation of the gating system in `llm-bridge-claudecode`
used CC's `--permission-prompt-tool` mechanism. Each harness instance ran
an embedded HTTP MCP server (`bridge_perm`, exposing one tool
`approval_prompt`) on a free `127.0.0.1` port. We passed CC two flags:

```
--mcp-config /tmp/bridge-perm-mcp-<rand>.json
--permission-prompt-tool mcp__bridge_perm__approval_prompt
```

The MCP config registered our server as `bridge_perm` with `type: "http"`.
On every tool call CC made, it invoked our `approval_prompt` over HTTP
JSON-RPC. The MCP routed the request through `permission_client.go` to
`permission-store /evaluate` and translated the verdict back into the
shape CC expects.

Code lived in two files:

- `permission_mcp.go` — the embedded HTTP server, JSON-RPC dispatch,
  response shaping, session tracking, GET-stream handling
- `permission_client.go` — the HTTP client to `permission-store`

Outcome semantics:

- `allow` → respond immediately with `{behavior: "allow"}`
- `deny` → respond with `{behavior: "deny", message}`
- `ask` → park the call, emit `awaiting_resolution` HookEvent,
  wait for `resolve_hook` JSON-RPC from bridge-server,
  push response to CC

## Bugs hit, in chronological order

We shipped the MCP path on 2026-05-01 and spent most of 2026-05-04
debugging it. Eight distinct bugs surfaced; each was real, each was
fixed, and each masked the next. In order:

### 1. Async dispatch deadlock (commit `b92038e`)

The harness has a single goroutine reading stdin and dispatching
JSON-RPC. When `handleMessage` runs it calls `drainUntilResult` which
loops on CC's stdout until a result event. While that loop is running
the dispatcher can't read new requests. So when CC parked on
`approval_prompt` and the user clicked Allow:

1. bridge-server wrote `resolve_hook` to harness stdin (succeeded —
   OS buffered)
2. bridge-server returned `200` (it never waits for ack)
3. harness was inside `drainUntilResult`, never read the bytes
4. CC stayed parked → `drainUntilResult` looped forever → deadlock

Fix: dispatch known-safe methods (`resolve_hook`, `set_bypass_permissions`,
`set_model`, `control`, `config:*`) on a goroutine.

### 2. `MCP_TOOL_TIMEOUT` defaulted to 60s (commit `e35c7da`)

CC's MCP TS SDK reads `process.env.MCP_TOOL_TIMEOUT` and uses it as
the per-tool-call timeout. Default is 60s. Parked permission calls
got cancelled after a minute and CC fell back to its
no-permission-prompt behavior.

Fix: set `MCP_TOOL_TIMEOUT` and `MCP_TIMEOUT` on every CC spawn.

### 3. Hardcoded `protocolVersion` rejected by handshake (commit `a10544b`)

Our `initialize` reply hardcoded `protocolVersion: "2025-06-18"`.
CC 2.1.123 sends `protocolVersion: "2025-11-25"` and treats a
downgraded reply as a failure — `bridge_perm` landed in
`mcp_servers` as `{"status": "failed"}` and CC fell back to its
default no-permission-prompt behavior. **Every Allow click was a
no-op for several days because of this single mismatch — the
preceding fixes were correct but their effects were invisible.**

Fix: explicit version negotiation. `supportedProtocolVersions =
["2025-11-25", "2025-06-18", "2025-03-26"]` — the revisions whose
JSON-RPC shapes our minimal server is wire-compatible with. Newest
matching version returned, otherwise our newest as a fallback. Six
unit tests added to enforce.

### 4. `MCP_TOOL_TIMEOUT=9999999999` overflowed JS `setTimeout` (commit `dd96cf2`)

The fix in (2) used `9999999999ms` (≈115 days) thinking "effectively
unlimited". Node's `setTimeout` clamps any delay larger than
`2^31 - 1 = 2147483647ms` (≈24.8 days) to `1ms`. So our "unlimited"
timeout was actually 1ms, CC's connect probe fired at 30ms with the
log message:

```
Connection timeout triggered after 30ms (limit: 9999999999ms)
HTTP Connection failed: connection timed out after 9999999999ms
```

`bridge_perm` once again landed as `failed` — bug 3 was fixed but
this one immediately took its place.

Fix: cap to `2147483647`.

### 5. HTTP transport idle timeout at 60s (commit `b590719`, `e6dca61`)

Even with `MCP_TOOL_TIMEOUT` honored, CC's HTTP MCP transport
closes the request body after ~60s of stream silence. The parked
POST got killed at 60s and CC retried the same tool call. Audit
showed three back-to-back Bash evaluate calls 60-65s apart — same
prompt, three retries, then "aborted_tools".

Fix attempt 1 (`b590719`): SSE keepalive on the POST. Send
`notifications/progress` events every 25s while parked. Worked but
was aesthetically unpleasant — keepalives every 25s for an overnight
wait.

Fix attempt 2 (`e6dca61`): the spec-blessed `202 Accepted` + GET
SSE stream pattern. CC opens a long-lived GET to the MCP endpoint
right after init; we respond `202` to parked POSTs and push the
response via the GET stream when it arrives. Eliminates keepalives
entirely.

### 6. Goroutine canceled by `r.Context()` (commit `49b2fd0`)

The `202` handler spawned a goroutine that selected on the resolve
channel OR `r.Context().Done()`. Go's HTTP server cancels the
request context the moment the handler returns — i.e. immediately
after `WriteHeader(202)`. So the goroutine fired
`<-r.Context().Done()` instantly and **deleted the pending entry
before any user click could land**. Every Allow click came back as
`"stale"` — the user saw the same "no response" symptom.

Fix: drop the context arg; goroutine blocks indefinitely on `<-ch`,
relying on `PermissionMCP.Shutdown` to drain pending channels at
teardown.

### 7. `progressToken` type-coerced from number to string

Fallback keepalive code converted numeric tokens to strings via
`fmt.Sprintf("%v", v)`. CC's MCP SDK validates response types
against the original request type via Zod — a number-where-string
or string-where-number trips ZodError. Two ZodErrors surfaced in
CC's terminal-reason error before "aborted_tools".

Fix: hold the token as `json.RawMessage` so the original JSON
type round-trips byte-exact. Same commit as 8.

### 8. Strict GET handler rejected CC's race-ahead stream

After (5) we required `Mcp-Session-Id` on the GET, expecting CC to
send it. CC's SDK opens the GET stream eagerly — sometimes BEFORE
the init response containing the session id has reached it. Strict
check 400'd that GET, CC didn't retry, no GET stream registered,
the 202 path silently fell through to keepalive (which itself had
its own bugs above).

Fix: lenient GET handler — accept any GET, bind to whatever id
(or empty) the client sent. Posts that arrive later under the same
id find the same stream slot.

## Lessons learned about CC's MCP transport

Things that surprised us, ranked by how much pain they caused:

1. **The MCP TS SDK's HTTP transport has multiple independent
   timeouts**: a connect-probe timer (~30ms hard floor irrespective
   of `MCP_TIMEOUT`), `MCP_TIMEOUT` for connect, `MCP_TOOL_TIMEOUT`
   for tools/call, and a transport-level body-idle timeout (~60s)
   we never found a config knob for. They interact in non-obvious
   ways.

2. **Node's `setTimeout` silently clamps `>2^31-1` to `1`**, with no
   error or warning. Any "effectively unlimited" duration above
   2147483647ms gets you "effectively zero". This bit us once on
   `MCP_TOOL_TIMEOUT` and would have bit again if we'd been less
   careful with the 202+GET path's resolve channel.

3. **Protocol version negotiation is strict, not best-effort.** The
   client (CC) refuses connections where the server returns a
   version it doesn't support. Our minimal server's wire format is
   stable across all 2025 revisions, but we still had to declare a
   `supportedVersions` list to participate honestly.

4. **`Mcp-Session-Id` is best-effort on the client side.** The SDK
   opens the GET SSE stream eagerly enough that it can race the
   init response. Server code must accept GETs without a session
   id and bind them lazily.

5. **Zod validation is unforgiving on response shape.** Subtle type
   coercion (`progressToken: 42` → `"42"`) trips schema validation
   that surfaces as a generic `ZodError` deep in CC's terminal log,
   far from the actual cause.

6. **`r.Context()` in Go HTTP handlers cancels on response-write,
   not on connection-close.** Spawning a goroutine that outlives
   the handler must NOT use `r.Context()` for cancellation —
   that's the path to immediate-cancel and lost work.

7. **CC's hook system (`--settings hooks.PreToolUse`) is independent
   from its MCP system (`--permission-prompt-tool`).** They have
   different output schemas, different timeout behaviors, and
   different validation rules. We were running both simultaneously
   (snapshots via hooks, approvals via MCP) and the snapshot path
   broke in its own way (empty-body response with
   `Content-Type: application/json` failed CC's hook JSON validator,
   surfaced to the model as "validation error from a hook").

## Why we're switching to PreToolUse hooks

Now that the MCP path works, the question is whether to keep it. The
honest answer is no — for these reasons:

### 1. Cross-harness uniformity

The MCP `--permission-prompt-tool` mechanism is CC-specific. Other
harnesses (codex, gemini, hermes, openclaw, …) don't have it. They
mostly do have some "before tool runs" hook concept. If we route
all gating through "the harness's tool-use hook, whatever its
shape", we get one mental model for `permission-store` integration
across the whole ecosystem.

### 2. We already use PreToolUse hooks for snapshots

`bridge-server` already configures CC PreToolUse/PostToolUse hooks
via `--settings` for the file-snapshot system. Approvals via the
same surface unifies those two concerns into one infrastructure
(`/hooks/exec`-shaped HTTP handlers) instead of two
(`/hooks/exec` AND an embedded MCP server).

### 3. The "hard" features are no harder via hooks

- **Allow / deny**: CC's PreToolUse output supports
  `hookSpecificOutput.permissionDecision: "allow" | "deny" | "ask"`.
- **Reason / message**: `permissionDecisionReason` field.
- **Long human wait**: the hook command is `sh -c <cmd>`. If the
  command is a curl that blocks until bridge-server returns, CC just
  waits for the subprocess. No keepalives, no SSE, no version
  negotiation, no session ids — just an HTTP request that takes a
  long time.
- **Concurrent calls**: each tool call spawns its own subprocess.
  One sleeping curl per pending approval. Negligible overhead.

### 4. Smaller surface area

Switching deletes:

- `permission_mcp.go` (~600 lines of HTTP server + SSE + 202+GET
  state)
- `permission_client.go` (the HTTP client wrapper)
- `MCP_TOOL_TIMEOUT` / `MCP_TIMEOUT` env-var management
- The `set_bypass_permissions` JSON-RPC pathway
- The `resolve_hook` JSON-RPC pathway
- ~100 lines of harness state (sessions map, bypass atomic, etc.)

And adds:

- One bridge-server endpoint that proxies `permission-store/evaluate`
  and parks asks
- One additional entry in `buildClaudeCodeSettings` to register the
  PreToolUse hook

Net code change is significantly negative. Net behavior change is
zero.

### 5. Bugs we'd never have hit

The eight bugs above were almost entirely *transport* bugs at the
MCP layer. None of them would have existed with the hook path:

- No session ids, no race
- No protocol version negotiation
- No SDK-internal Zod validation on our notifications
- No JS `setTimeout` overflow
- No transport-level idle timeouts (subprocess just blocks)
- No two-mechanism context cancellation race

The transport surface is the curl, which we already operate.

## What we keep

The redesign that got us here is sound and we keep all of it:

- `permission-store` service (`:8304`) as the single source of
  truth for rules, evaluation, audit.
- `permission-store` schema and `/evaluate` endpoint shape.
- The Bash splitter (security boundary, fuzz-tested).
- Seed rules.
- bridge-prefs `bypass_permissions` flag.
- `bridge-ui`'s `PendingPermissionsBanner`, the rules editor at
  `/permissions`, the test tab.
- The `awaiting_resolution`/`completed` HookEvent shape and the
  `/hooks/pending` + `/hooks/{id}/resolve` endpoints — these are
  agnostic to whether the harness path is MCP or PreToolUse.

The switch is at the transport layer between bridge-server and
the harness, not in the gating system itself.
