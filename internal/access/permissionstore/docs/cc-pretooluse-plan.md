# Plan: switch CC permission gating from MCP to PreToolUse hook

Status: **shipped**. All six migration steps below have landed. Measured against
`main` in each repo on 2026-08-21:

| step | evidence |
|---|---|
| 1-2. bridge-server prehook endpoint + registered `PreToolUse` hook | `llm-bridge-server/internal/server/permission_prehook.go`; the settings builder writes the gate's URL at `internal/server/hook_settings.go:211` |
| 3-4. claudecode drops `--permission-prompt-tool` and the MCP code | `llm-bridge-claudecode/process.go:83` says the flag is no longer wired, and none of `ensurePermissionMCP`, `SetBypassPermissionsParams`, `ResolveHookParams`, `PermissionMCP`, `handleResolveHook` or `handleSetBypassPermissions` is declared anywhere in that repo |
| 5-6. fallback dropped, bridge-ui cleaned | `llm-bridge-claudecode/DESIGN.md:543` states the end state: no `bridge_perm` MCP, no `resolve_hook` JSON-RPC method, no `--permission-prompt-tool` / `--mcp-config` for permissions |

⚠️ **The header carried no status at all until 2026-08-21**, so a document whose
last three sections are *"Migration steps, in shippable order"*, *"Open questions to
resolve before step 1"* and *"Estimated effort"* read as work waiting to start.
Everything below is kept as written and is now a record of what was done, not a
proposal: the names in **Code to remove** are named in order to be deleted, and they
are deleted.

This document plans the migration described in `cc-mcp-retrospective.md`.
The approval flow moves from the embedded `bridge_perm` MCP to a
PreToolUse hook configured via `--settings`. `permission-store` and
`bridge-ui` are unchanged; the change is at the transport between
`bridge-server` and CC.

## Architecture after the switch

```
                                                       ┌────────────────────┐
                                                       │  permission-store  │
                                                       │      (:8304)       │
                                                       └─────────▲──────────┘
                                                                 │ POST /evaluate
                                                                 │
  ┌───────────────────┐   curl POST    ┌────────────────────────┴───────────┐
  │   Claude Code     │ ─────────────► │  bridge-server                     │
  │  (PreToolUse hook │ ◄───────────── │  POST /permission/cc-prehook/{id}  │
  │   subprocess)     │   JSON resp    │                                    │
  └───────────────────┘                │  parks "ask" until resolve         │
                                       │  delivers via channel              │
                                       └────────▲───────────────────────────┘
                                                │
                                       POST /sessions/{id}/hooks/
                                       {request_id}/resolve  (existing)
                                                │
                                       ┌────────┴───────┐
                                       │   bridge-ui    │
                                       │ banner click   │
                                       └────────────────┘
```

CC fires the PreToolUse hook for every tool call. The hook is a curl
that POSTs to `bridge-server`. Bridge-server consults
`permission-store`, returns allow/deny immediately, or parks the curl
on a Go channel until `bridge-ui` resolves it via the existing
`/sessions/{id}/hooks/{request_id}/resolve` endpoint.

## Hook shape on the wire

CC sends this stdin JSON to a PreToolUse hook command:

```json
{
  "session_id": "<CC session UUID>",
  "transcript_path": "/path/to/conv.jsonl",
  "cwd": "/working/dir",
  "hook_event_name": "PreToolUse",
  "tool_name": "Bash",
  "tool_input": { "command": "...", "description": "..." }
}
```

The hook command we register:

```
curl -sfS -X POST -H 'Content-Type: application/json' \
     --data-binary @- \
     http://localhost:8160/permission/cc-prehook/<bridge_id>
```

Bridge-server's response (per CC's PreToolUse hook contract):

```json
{
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "allow" | "deny" | "ask",
    "permissionDecisionReason": "<message>"
  }
}
```

`allow` lets CC run the tool. `deny` blocks it and surfaces the reason
to the model. `ask` would normally show CC's interactive UI — we don't
use that; we always resolve to allow or deny ourselves before
returning, so the response is one of those two.

For long parks, the curl subprocess simply waits for the HTTP response.
No keepalives, no SSE, no session protocol — bridge-server holds the
HTTP connection open until the user resolves.

## bridge-server changes

### New endpoint: `POST /permission/cc-prehook/{bridge_id}`

```go
func (s *Server) handleCCPermissionPrehook(w http.ResponseWriter, r *http.Request) {
    bridgeID := r.PathValue("bridge_id")
    body, _ := io.ReadAll(r.Body)

    var hookPayload struct {
        ToolName  string          `json:"tool_name"`
        ToolInput json.RawMessage `json:"tool_input"`
    }
    json.Unmarshal(body, &hookPayload)

    // Bypass short-circuit: live-toggleable via bridge-prefs.
    if s.bridgePrefs.get().BypassPermissions {
        writeAllow(w, "bypass-mode")
        return
    }

    // Evaluate.
    resp, err := s.permClient.Evaluate(EvalRequest{
        BridgeID: bridgeID,
        Tool:     hookPayload.ToolName,
        Input:    hookPayload.ToolInput,
    })
    if err != nil {
        // permission-store unreachable → ask outcome (same as engine default)
        writeAsk(w, "permission-store unreachable: "+err.Error())
        return
    }

    switch resp.Outcome {
    case "allow":
        writeAllow(w, resp.Message)
    case "deny":
        writeDeny(w, resp.Message)
    case "ask":
        // Park: emit awaiting_resolution event, register parked request,
        // block until resolve_hook arrives via the resolve endpoint or
        // the request context cancels.
        decision := s.parkPermissionAsk(r.Context(), bridgeID, hookPayload.ToolName, hookPayload.ToolInput)
        switch decision.behavior {
        case "allow":
            writeAllow(w, decision.message)
        default:
            writeDeny(w, decision.message)
        }
    }
}
```

Helper writers shape the JSON exactly as CC expects:

```go
func writeAllow(w http.ResponseWriter, reason string) {
    writeHookDecision(w, "allow", reason)
}
func writeDeny(w http.ResponseWriter, reason string) {
    writeHookDecision(w, "deny", reason)
}
func writeHookDecision(w http.ResponseWriter, decision, reason string) {
    writeJSON(w, map[string]any{
        "hookSpecificOutput": map[string]any{
            "hookEventName":            "PreToolUse",
            "permissionDecision":       decision,
            "permissionDecisionReason": reason,
        },
    })
}
```

### Parked-request map

Replaces the per-harness `pendingHooks` infrastructure. Lives in
bridge-server because that's where both the parked HTTP handler and
the resolve endpoint can reach it.

```go
type parkedAskState struct {
    mu      sync.Mutex
    parked  map[string]map[string]chan permissionDecision // bridgeID → reqID → ch
}

// On park: register, emit awaiting_resolution HookEvent (existing
// SSE broadcast), block on channel receive.
// On resolve: lookup by (bridgeID, reqID), send decision on channel.
// On context cancel (CC died, page reload, etc.): drop entry, log.
```

The existing `awaiting_resolution`/`completed` HookEvent broadcast
infrastructure stays unchanged — bridge-ui's banner already listens
for those.

### `handleResolveHook` rewrite

Currently forwards `resolve_hook` JSON-RPC to the harness. New shape:

```go
func (s *Server) handleResolveHook(w, r) {
    // ... parse bridgeID, requestID, body ...
    if !s.parkedAsks.deliver(bridgeID, requestID, decision) {
        // No matching parked request — emit completed-with-stale anyway
        // so bridge-ui banner clears, mirroring the harness's old behavior.
        s.broadcastStaleResolution(bridgeID, requestID, decision)
    }
    writeJSON(w, map[string]string{"status": "resolved"})
}
```

### `buildClaudeCodeSettings` addition

The current PreToolUse and PostToolUse arrays are filled from the
hook-store. We add one synthetic entry at the front of `PreToolUse`
that routes ALL tools (`matcher: "*"`) through our permission endpoint:

```go
permEntry := map[string]any{
    "matcher": "*",
    "hooks": []map[string]any{
        {"type": "command", "command": fmt.Sprintf(
            "curl -sfS -X POST -H 'Content-Type: application/json' --data-binary @- %s/permission/cc-prehook/%s",
            base, sess.BridgeID,
        )},
    },
}
```

Order matters: the permission hook fires first, blocks/allows/denies,
then the existing snapshot hook runs (only if the permission hook
allowed).

### bridge-prefs bypass behavior

No change to the public behavior of the toggle. Internally:

- bypass=true is checked at the top of `handleCCPermissionPrehook`
  and short-circuits to allow.
- The harness no longer needs `set_bypass_permissions` — bridge-server
  reads its own prefs on every request.
- `--permission-mode` flag is no longer needed at all (we don't use
  CC's interactive permission flow). We can drop the
  `bypass: 'bypassPermissions' : 'default'` logic in BridgeChat
  entirely — every session always launches in `default` (or whatever
  CC's no-flag default is) since the PreToolUse hook handles the gate.

## Claude-code harness changes

### Files to delete

- `permission_mcp.go` — entire embedded MCP server
- `permission_mcp_test.go`
- `permission_client.go` — HTTP client to permission-store

### Code to remove from `handler.go`

- `permissionMCP *PermissionMCP` field
- `permissionCfgPath string` field
- `pendingBypass *bool` field
- `ensurePermissionMCP` function (~80 lines)
- `SetBypassPermissionsParams` type
- `case "set_bypass_permissions"` in `HandleRequest`
- `case "set_bypass_permissions"` in `handleConfig`
- `handleSetBypassPermissions` function
- `ResolveHookParams` type
- `case "resolve_hook"` in `HandleRequest`
- `handleResolveHook` function
- The two `extraArgs` lines that add `--mcp-config` and
  `--permission-prompt-tool` to spawn args (lines ~559-562)
- Permission MCP shutdown branch in `Shutdown`

### Code to remove from `process.go`

- `MCP_TOOL_TIMEOUT` / `MCP_TIMEOUT` env var setting (no MCP, no
  reason to override)

### Code to remove from `main.go`

- `set_bypass_permissions`, `resolve_hook`, `control` no longer need
  the async dispatch fast-path — we kept it for those two methods
  specifically. Without them the async-method map can shrink.

### Net change

About 700 lines of code deleted, ~50 added. The bypass-mode atomic,
the session map, the GET stream handling, the version negotiation,
the SSE keepalive — all gone.

## bridge-ui changes

Almost none. The data layer (`pendingHooks`, `resolveHook`) talks
to `/sessions/{id}/hooks/pending` and `/hooks/{id}/resolve` — those
endpoints stay. Internally those endpoints now consult the bridge-
server's parked-asks map instead of forwarding to the harness, but
the wire shape is unchanged.

The bypass toggle in `dash/settings` keeps the same UX. Internally,
the toggle no longer needs to broadcast `set_bypass_permissions`
to harnesses; it just updates bridge-prefs and bridge-server reads
that on every request. We can either keep `POST /bridge/bypass-permissions`
as a thin "update prefs" endpoint or drop it and have the UI write
directly to `PUT /bridge-prefs`. Either way, the bridge-prefs key
`bypass_permissions` stays.

`BridgeChat.tsx`: the `permission_mode` harness_config field is no
longer used — every CC session can run in `default` mode (or no
mode flag at all). Drop the conditional.

## Migration steps, in shippable order

Each step ships independently and leaves a working system.

### Step 1: bridge-server — new prehook endpoint, parked-asks map

- Add `parkedAsks` to `Server` struct
- Add `handleCCPermissionPrehook` — fully functional, but not yet
  registered as a hook in `--settings`
- Modify `handleResolveHook` to deliver via parkedAsks (after
  attempting the existing harness-JSONRPC path, for backwards-compat
  during the transition)
- Add `permClient` if not already in bridge-server (currently
  permission-store is only called from claudecode harness)
- Tests: unit-test `parkedAsks` deliver/timeout/double-resolve paths
- Deploy and verify: send a curl directly to the new endpoint with
  a fake hook payload, confirm allow/deny/ask behavior

### Step 2: bridge-server — register PreToolUse permission hook

- Modify `buildClaudeCodeSettings` to prepend the permission hook
  entry to the `PreToolUse` array
- Deploy. The MCP path is still active (CC uses both — MCP for
  approval_prompt calls, hook for our new endpoint).
- This means every tool call now consults permission-store TWICE:
  once via the MCP path, once via the hook path. Both should
  return the same answer; if they disagree something's wrong.
  Audit log shows duplicate evaluate calls — that's expected during
  transition.

### Step 3: claudecode harness — remove `--permission-prompt-tool`

- Stop passing `--permission-prompt-tool` and `--mcp-config` for
  bridge_perm
- CC stops calling our MCP entirely; it relies on the PreToolUse
  hook
- The MCP code is still in the binary but no longer wired
- Verify: end-to-end approval flow works through hooks; audit log
  no longer shows duplicate evaluations
- bypass toggle: replace harness-side atomic broadcast with
  bridge-server reading bridge-prefs

### Step 4: claudecode harness — delete the MCP code

- Pure deletion: `permission_mcp.go`, `permission_client.go`,
  `permission_mcp_test.go`
- Strip handler.go fields and methods
- Strip process.go env vars
- Verify the harness still builds and the test suite passes

### Step 5: bridge-server — drop the JSON-RPC fallback in handleResolveHook

- Once step 4 lands and no harness still expects `resolve_hook`
  JSON-RPC, drop that path
- handleResolveHook is now exclusively parked-asks delivery

### Step 6: bridge-ui cleanup

- Drop the `permission_mode` field from `BridgeChat` harness_config
- Drop `POST /bridge/bypass-permissions` if we choose to consolidate
  (or keep as a convenience that wraps the prefs PUT)
- The pending-hooks data layer and banner stay exactly as-is

## Open questions to resolve before step 1

1. **Hook command timeout.** CC has a default hook command timeout
   (60s I think; need to confirm). The parked curl needs to live
   longer. Two options:
   - Configure the timeout via `--settings`
     `hooks.PreToolUse[].timeout` field (if it exists)
   - Configure via env (`CLAUDE_CODE_HOOK_TIMEOUT_MS` if it exists)
   - Use a wrapper script that re-invokes curl with a long backoff
     if the response hasn't come in
   We need to verify the actual mechanism with a probe before
   shipping step 2.

2. **`tool_use_id`.** CC's hook payload includes `tool_use_id` (the
   id CC mints for this specific tool call). Should we use that as
   the `request_id` we pass to bridge-ui, or mint our own and
   correlate? The MCP path minted a fresh `request_id`. Easier to
   keep that pattern; `tool_use_id` is just additional context for
   the audit log.

3. **Multiple tool calls in flight.** A single turn can have multiple
   parallel tool calls. Each fires its own PreToolUse hook → its
   own curl subprocess → its own parked request in our map.
   bridge-ui's banner needs to handle N pending hooks for the same
   session — already does, just want to confirm.

4. **Hook ordering with the snapshot hook.** Both fire on Edit|Write.
   We want permission first, snapshot second. Need to confirm CC
   runs hooks in the array order they appear in `--settings`, not
   in parallel.

5. **Migration order safety.** During step 2 (both paths active)
   we'll see double evaluations. Is the duplicate audit logging
   acceptable for a few hours? Probably yes — same outcome both
   times, easy to filter. Worth flagging in the commit message so
   anyone reading audit logs understands.

## Estimated effort

- Step 1: ~3h (new endpoint, parked-asks map, tests)
- Step 2: ~30min (one config change + deploy)
- Step 3: ~1h (remove flags, swap bypass plumbing)
- Step 4: ~1h (delete MCP code)
- Step 5: ~30min (cleanup)
- Step 6: ~1h (bridge-ui cleanup)

Total: ~7h, spread across multiple deploys with verification at each
boundary.

## Verification checklist

After each step, confirm:

- bypass toggle in dash/settings works (live-flips correctly)
- a tool call outside the seed allowlist shows the banner in chat
- clicking Allow unblocks CC and the tool runs
- clicking Deny surfaces a reason to the model
- "Always allow" creates a global rule and the next identical call
  doesn't prompt
- audit log records every evaluation
- existing snapshot hooks still fire on Edit|Write
- no orphaned parked requests after harness restart
- tests pass: `permission-store`, `llm-bridge-claudecode`,
  `bridge-ui`, `dash`
