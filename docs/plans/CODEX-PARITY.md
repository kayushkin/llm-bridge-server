# Codex Parity Plan

How to make a codex session indistinguishable from a Claude Code session from the bridge-ui's perspective — same permission-mode toggle, same hook surface, same tool-store integration, same parked-asks banner — by mapping each CC piece onto codex's primitives, plus surfacing codex-specific extras that CC doesn't have.

Companion to `HARNESS-LAYER.md` and `TOOL-ROUTING.md`. References [Codex Hooks docs](https://developers.openai.com/codex/hooks) and [Codex Agent Approvals & Security](https://developers.openai.com/codex/agent-approvals-security).

---

## 1. How the CC permission stack works today

The CC flow is the reference architecture we're targeting parity with. Every codex piece below is judged against this.

### 1.1 PreToolUse prehook — the universal gate

CC calls a tool → its `--settings` JSON has a `PreToolUse` HTTP hook → CC POSTs to `/permission/cc-prehook/{bridge_session_id}` with:

```json
{
  "session_id": "...",
  "tool_name": "Bash",
  "tool_input": { "command": "..." },
  "tool_use_id": "..."
}
```

`handleCCPermissionPrehook` (`permission_prehook.go:42`) makes a decision in this order:

1. **Live permission_mode short-circuit** (`permission_prehook.go:77-87`)
   - `bypass` → return `allow` immediately
   - `auto` + tool in `isAutoModeSafeTool` whitelist (Read/Glob/Grep/Edit/Write/TodoWrite, lines 126-146) → return `allow`
   - Otherwise fall through

2. **Permission-store `/evaluate`** (`permission_prehook.go:94-114`) — rule engine matches tool name + input pattern → outcome `allow|deny|ask`

3. **Park-for-human on `ask`** (`permission_prehook.go:112` → `parkedAsks.parkPrehook`, `parked_asks.go:26-77`)
   - Bridge mints a `request_id`, broadcasts `HookEvent{phase: "awaiting_resolution"}` over SSE
   - The handler blocks on a channel keyed by `(session_id, request_id)`
   - bridge-ui's `PendingPermissionsBanner` (`bridge-ui/.../PendingPermissionsBanner.tsx:17`) shows the prompt
   - User clicks Allow/Deny/Always — POST `/sessions/{id}/hooks/{request_id}/resolve` delivers to the parked channel
   - Optional `updatedInput` field replaces tool input (used heavily for `AskUserQuestion` answer-merge)

Response shape returned to CC:
```json
{ "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "allow" | "deny" | "ask",
    "permissionDecisionReason": "...",
    "updatedInput": { ... }    // optional
}}
```

### 1.2 Settings injection — how the hook URL gets into CC

At session-start (`startOnInstance` → `injectHookSettings`, `hook_settings.go:47`):

1. `buildClaudeCodeSettings` (line 110) synthesizes a JSON blob
2. Prepends the bridge's permission gate as the first PreToolUse hook (type=`http`, URL=`/permission/cc-prehook/...`, timeout=86400s)
3. Appends user-registered hooks from `hookStore` matching scope (global + instance + session), each rendered as a curl command to `/hooks/exec/{id}`
4. Marshals the blob into `sess.HarnessConfig["settings"]`; `buildStartParams` (`harness/process.go:93`) passes it to the harness via `--settings`

User overrides win: if `HarnessConfig["settings"]` is already set, the injector bails (`hook_settings.go:65-68`).

### 1.3 Tool-store provisioning — how MCP servers get into CC

At session-start (`injectMCPConfig`, `tool_provision.go:33`):

1. Reads `HarnessConfig["tool_store_tools"]` (a JSON `[]string` of tool names) — opt-in, no-op if absent
2. POSTs `{"tools": [...]}` to `$TOOL_STORE_URL/provision`
3. Tool-store returns MCP server config JSON with env vars **resolved from auth-store**
4. Bridge writes to a tmpfile in `/tmp/llm-bridge-mcp/`, rewrites `HarnessConfig["mcp_config"] = <tmpfile_path>`, removes `tool_store_tools`
5. CC consumes via `--mcp-config <path>`

**Tool-store is permission-agnostic** — it only resolves MCP server configs. Permission gating happens 100% at the prehook layer. This is important: the same tool-store API can serve codex without any changes.

### 1.4 Permission-mode toggle (UI ↔ server)

bridge-ui has `SessionPermissionMode.tsx` (a dropdown with Ask/Auto/Bypass) in the chat header. PUT `/api/bridge/sessions/{id}/permission-mode` updates `HarnessConfig["permission_mode"]`. The prehook reads the value **live on every tool call** (`permission_mode.go:75-94`), so mid-session toggles take effect on the next tool call without restarting the session.

Modes available per session are filtered by `HarnessInfo.supported_permission_modes` — codex currently declares `{ask, auto, bypass}` in `health.go:109-111` even though the harness binary doesn't implement them all uniformly.

### 1.5 Hook-store vs permission-store

| Store | Purpose | Used by |
|---|---|---|
| **hook-store** | Registry of user-registered hooks (PreToolUse, PostToolUse, …) for observation/instrumentation. Scoped global/instance/session. | `buildClaudeCodeSettings` enumerates applicable hooks → embeds curl-to-`/hooks/exec/{id}` in settings. |
| **permission-store** | Rule engine: condition (tool + regex) → outcome (allow/deny/ask). Scoped global/instance/bridge. | Prehook calls `/evaluate` per tool call. UI `BridgePermissions.tsx` exposes Rules/Audit/Test tabs. |

These are **complementary, not redundant.** Permission-store is the gating brain; hook-store is the side-effect/observation library.

### 1.6 The canonical sequence on one CC tool call

1. CC fires PreToolUse HTTP hook → POST bridge `/permission/cc-prehook/{id}` with payload
2. Bridge reads live `permission_mode` from session HarnessConfig
3. If `bypass` → allow. If `auto` + whitelisted tool → allow. Otherwise call permission-store `/evaluate`
4. If outcome=`ask` → park, broadcast SSE, banner shows in UI, user clicks → resolve → unblock
5. Bridge returns `hookSpecificOutput{permissionDecision, updatedInput?}` to CC
6. CC runs (or refuses) the tool with the possibly-modified input

---

## 2. Codex's primitives — what we have to work with

### 2.1 Hook events available in codex 0.130+ (stable)

| Event | Fires | Block via | CC equivalent? |
|---|---|---|---|
| `SessionStart` | Session init/resume/clear | `additionalContext` injection | No direct equivalent |
| `PreToolUse` | Before Bash / apply_patch / MCP exec | `permissionDecision: "deny"` (or legacy `decision: "block"`) | **Direct match** |
| `PermissionRequest` | When tool needs approval escalation | `decision: { behavior: "allow" \| "deny", message }` | **Codex-only** |
| `PostToolUse` | After tool completes (incl. failures) | `decision: "block"` injects feedback (can't undo side-effects) | No direct equivalent |
| `UserPromptSubmit` | User submits a prompt | `additionalContext` injection or `decision: "block"` to reject | No direct equivalent |
| `Stop` | Turn finishes | `decision: "block"` forces continuation | No direct equivalent |

**Critical caveats** (current codex 0.130 stable):
- `PreToolUse` only fires for `Bash`, `unified_exec`, `apply_patch`, and `mcp_tool_call` ([#20204](https://github.com/openai/codex/issues/20204)). Edits, writes, web-search, image-gen don't fire it yet.
- `PreToolUse` parses but **fails open** on `updatedInput`, `additionalContext`, `continue: false`, `stopReason`, `suppressOutput` — we cannot modify tool input via PreToolUse the way CC supports.
- `PermissionRequest` **fails closed** on `updatedInput`, `updatedPermissions`, `interrupt` — those are reserved.
- Open regression [#21639](https://github.com/openai/codex/issues/21639) — hooks may not fire reliably in 0.129+. May be fixed in 0.130 stable; needs end-to-end verification through `codex app-server`.

### 2.2 Approval policy + sandbox mode

Codex has **two orthogonal axes**, not a single permission-mode enum:

| Sandbox | Effect |
|---|---|
| `read-only` | Filesystem reads only; no writes, no network |
| `workspace-write` | Read + edit within workspace + `--add-dir` paths; no network unless approved |
| `danger-full-access` | No restrictions |

| Approval policy | Effect |
|---|---|
| `untrusted` | Allow known-safe reads; escalate to user on any mutating command |
| `on-request` | Model decides when to escalate to user |
| `never` | No approval prompts ever; failures returned to model |
| `granular` | Per-category interactive prompts (sandbox, rules, MCP, permissions, skills) |

The CC `--permission-mode {ask, auto, bypass}` knob maps onto the **product of these two axes**:

| CC mode | Closest codex pair |
|---|---|
| `ask` | `approval=on-request` + `sandbox=workspace-write` (codex's `--full-auto` is the alias) |
| `auto` | `approval=never` + `sandbox=workspace-write` |
| `bypass` | `approval=never` + `sandbox=danger-full-access` (codex's `--dangerously-bypass-approvals-and-sandbox`) |

**Important:** the current llm-bridge-codex bridge already auto-approves every `*ApprovalRequest` over the JSON-RPC stream (`handler.go`). That means **bridge-side gating already overrides codex's approval policy** — the prehook is the real fence regardless of what `approvalPolicy` codex thinks it has. We should still pass the policy through (some codex internal paths inspect it), but the bridge prehook is authoritative.

### 2.3 Codex-specific surface CC doesn't have

| Feature | Stage | What it gives us |
|---|---|---|
| `PermissionRequest` hook | stable | A second, narrower gate that only fires on approval escalations. Cleaner than rolling all gating into PreToolUse. |
| `SessionStart` hook with `additionalContext` | stable | Inject context (project rules, agent identity, recent decisions) into every new session without modifying system prompt. CC has `--append-system-prompt` but it's set-once. |
| `UserPromptSubmit` hook with `additionalContext` | stable | Inject context into every user message (e.g. "current branch is X, ticket is Y"). |
| `Stop` hook with continuation | stable | Force codex to keep going after it stops (e.g. "validate the change before declaring done"). CC has no equivalent. |
| `--output-schema <file>` | stable | Constrain the final response to a JSON Schema. CC has `--strict-output-schema` only in some versions. |
| `--search` web search | stable | Native web search tool. Already translated to `EventToolCall(web_search)` in `translate.go`. |
| Plugins | stable | Codex's plugin distribution mechanism. Distinct from CC's `--plugin-dir`. Could expose via bridge-ui Plugins page. |
| Skills (`skill_mcp_dependency_install`) | stable | Codex skills auto-install MCP deps. We currently don't surface these. |
| Memories | under dev | Codex's memories. Bridge has its own `memory-store`; question is whether to bridge or keep separate. |

---

## 3. Mapping plan: CC piece → codex piece

### 3.1 PreToolUse prehook → codex `PreToolUse` + `PermissionRequest`

Two hooks, both routed to bridge-server:

| Bridge-server endpoint | Codex hook | Purpose |
|---|---|---|
| `POST /permission/codex-prehook/{bridge_session_id}` | `PreToolUse` | Preventive gate. Returns `permissionDecision: allow\|deny`. |
| `POST /permission/codex-approval/{bridge_session_id}` | `PermissionRequest` | Approval-escalation gate. Returns `decision: { behavior: allow\|deny, message }`. |

Reuse the existing gating logic from `handleCCPermissionPrehook`. The only difference is payload shape on input and output. Concrete sketch:

```go
// permission_codex_prehook.go (new)
type codexPrehookPayload struct {
    SessionID     string          `json:"session_id"`
    HookEventName string          `json:"hook_event_name"`  // "PreToolUse" or "PermissionRequest"
    ToolName      string          `json:"tool_name"`
    ToolInput     json.RawMessage `json:"tool_input"`
    ToolUseID     string          `json:"tool_use_id,omitempty"`
    TurnID        string          `json:"turn_id,omitempty"`
}

// PreToolUse response
{ "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "deny",
    "permissionDecisionReason": "..."
}}

// PermissionRequest response
{ "hookSpecificOutput": {
    "hookEventName": "PermissionRequest",
    "decision": { "behavior": "deny", "message": "..." }
}}
```

The decision pipeline (mode short-circuit → permission-store → park) is harness-agnostic; refactor it into a `decidePermission(sess, tool, input) (Outcome, error)` helper used by both CC and codex handlers.

**Caveat:** `updatedInput` doesn't work in codex's PreToolUse today. If we want to keep `AskUserQuestion`-style answer-merge for codex, we have two options:
- Wait for codex to land `updatedInput` (currently "fails open")
- Use codex's `additionalContext` on `UserPromptSubmit` to inject the answer as a follow-up message — different UX but functionally equivalent

Recommendation: ship without `updatedInput` for now; flag follow-ups in bridge-ui as "answer routed via context-injection on next prompt" for codex sessions.

### 3.2 Settings injection → codex `[hooks]` block via `-c` overrides

bridge-server adds a codex branch to `injectHookSettings`:

1. `buildCodexHooksConfig(sess)` synthesizes a JSON tree analogous to `buildClaudeCodeSettings`:
   ```json
   {
     "PreToolUse": [{
       "matcher": ".*",
       "hooks": [{
         "type": "command",
         "command": "curl -sfS --max-time 86400 -X POST http://localhost:8160/permission/codex-prehook/<bridge_id> --data-binary @-",
         "timeout": 86400
       }]
     }],
     "PermissionRequest": [{
       "hooks": [{
         "type": "command",
         "command": "curl -sfS --max-time 86400 -X POST http://localhost:8160/permission/codex-approval/<bridge_id> --data-binary @-",
         "timeout": 86400
       }]
     }]
   }
   ```
   plus any user-registered codex hooks from hook-store.

2. Writes the blob into `HarnessConfig["codex_hooks"]`.

3. llm-bridge-codex reads `codex_hooks` on `start`, translates each top-level key to one `-c` argument (TOML inline syntax) on the `codex app-server` spawn at `appserver.go:72`:
   ```
   -c 'hooks.PreToolUse=[{matcher=".*",hooks=[{type="command",command="...",timeout=86400}]}]'
   ```
   Or — simpler — write the JSON to a tmpfile and set `CODEX_HOME` to a per-session tmpdir with symlinked `auth.json` and a one-line `config.toml` pointing the `[hooks]` block at the JSON. The `-c` approach is cleaner; pick after a smoke test.

**No native `type: "http"` hook in codex yet** — we wrap the bridge endpoint as a curl `type: "command"`. Same pattern CC used pre-native-HTTP.

### 3.3 Tool-store provisioning → codex `[mcp_servers]` TOML

Extend `injectMCPConfig` (`tool_provision.go:33`) to branch on `HarnessCodex`:

1. Same `tool_store_tools` → `/provision` → MCP config JSON flow
2. Translate JSON to TOML inline-tables, e.g.:
   ```toml
   [mcp_servers.noteboard]
   command = "python3"
   args = ["-m", "noteboard_mcp"]
   env = { NOTEBOARD_URL = "http://localhost:8191" }
   ```
3. Pass as `-c` overrides on the `codex app-server` spawn, OR write to `CODEX_HOME/config.toml`. The MCP config tree may be large enough that `CODEX_HOME` is cleaner; spike both.

Tool-store needs zero changes — its `/provision` output is generic MCP JSON. The translation layer lives in bridge-server.

### 3.4 Permission-mode toggle → codex sandbox + approval pair

`PUT /sessions/{id}/permission-mode` already exists and writes `HarnessConfig["permission_mode"]`. For codex:

| User picks | Bridge writes to HarnessConfig | Sent to codex via |
|---|---|---|
| `ask` | `permission_mode=ask`, `approval_mode=on-request`, `sandbox=workspace-write` | `-c approval_policy="on-request"`, `-c sandbox_mode="workspace-write"` |
| `auto` | `permission_mode=auto`, `approval_mode=never`, `sandbox=workspace-write` | same flags |
| `bypass` | `permission_mode=bypass`, `approval_mode=never`, `sandbox=danger-full-access` | same flags |

**Live mid-session toggle:** codex doesn't accept `approval_policy` change after thread/start (per current `codex.go`). Two paths:
- Keep mid-session toggle bridge-side: prehook reads `permission_mode` live (already works), codex sees a static config but the prehook decides. The bridge auto-approves all codex `*ApprovalRequest` regardless of policy. This is the **easier path** and matches how the bridge already behaves.
- Add a `thread/setApprovalPolicy` JSON-RPC method to codex (upstream feature request). Not blocking.

**Recommendation:** option 1. The bridge's prehook IS the permission boundary; the codex-side sandbox/approval pair is set once at thread/start as a defense-in-depth floor.

### 3.5 Don't use `--dangerously-bypass-approvals-and-sandbox`

Per the user's instruction. That flag is documented to "skip all confirmation prompts" — strong signal it bypasses the hook layer too (consistent with smoke-test results in this session where the flag was set and hooks didn't fire).

For `bypass` mode, use the explicit pair `approval=never` + `sandbox=danger-full-access` instead. This keeps hooks active (the bridge can still observe and override) while granting unrestricted execution.

### 3.6 bridge-ui surface changes

Reusable as-is (harness-agnostic):
- `BridgePermissions.tsx` (Rules/Audit/Test) — permission-store is shared
- `BridgeTools.tsx` (Tools page) — tool-store is shared
- `PendingPermissionsBanner.tsx` — `HookEvent` shape is generic
- `SessionPermissionMode.tsx` — already filters modes by `HarnessInfo.supported_permission_modes`

Needs a codex branch or new component:
- **`BridgeHooks.tsx`** (if it exists; check) — must surface PostToolUse / UserPromptSubmit / Stop / SessionStart as additional event types. Currently CC-centric.
- **`BridgeSettings.tsx`** harness defaults — codex needs entries for `effort`, `sandbox_mode` default, `approval_mode` default, plus capability flags (`model`, `effort`, `tools`, `budget`) registered in `health.go`.
- **System-prompt modal** — codex's `SessionStart.additionalContext` is the analog; surface it as "session preamble" alongside the existing system-prompt display.

### 3.7 Codex-specific UI extensions worth shipping

Net-new surfaces for codex sessions:

| UI element | Backed by |
|---|---|
| **"Stop-hook" toggle** ("Force continuation if model declares done with X incomplete") | codex `Stop` hook + a server-side continuation policy |
| **"Prompt preamble" textarea** ("Always inject this on every prompt") | `UserPromptSubmit` hook + `additionalContext` |
| **"Session preamble" textarea** ("Always inject this on session start") | `SessionStart` hook + `additionalContext` |
| **Output-schema picker** ("Enforce JSON shape on final response") | `--output-schema <file>` |
| **Sandbox/approval advanced override** (when user wants to step outside the {ask/auto/bypass} preset) | direct `sandbox` + `approval` pair |
| **PermissionRequest log** (separate from Audit) | new endpoint listing decisions made on `PermissionRequest` events specifically |

Defer until after parity — these are "codex superpowers" worth their own design sprint.

---

## 4. Implementation roadmap

### P1 — Server-side codex permission stack (≈ 1 day)

1. Refactor `decidePermission(sess, tool, input)` out of `handleCCPermissionPrehook` (`permission_prehook.go`)
2. New `handleCodexPermissionPrehook` (`/permission/codex-prehook/{id}`) and `handleCodexApprovalRequest` (`/permission/codex-approval/{id}`) using the shared helper
3. Wire routes in `server.go`

### P2 — Settings + MCP injection for codex (≈ 1 day)

4. `buildCodexHooksConfig(sess)` in `hook_settings.go`
5. Branch `injectHookSettings` on `HarnessCodex` — write JSON tree into `HarnessConfig["codex_hooks"]`
6. Branch `injectMCPConfig` on `HarnessCodex` — translate MCP JSON to TOML inline tables, write into `HarnessConfig["codex_mcp_config"]`

### P3 — llm-bridge-codex bridge updates (≈ 1 day)

7. Parse `codex_hooks` + `codex_mcp_config` from start params
8. Translate to `-c` overrides on `codex app-server` spawn at `appserver.go:72`
9. Map session permission_mode → `approval_policy` + `sandbox_mode` `-c` overrides (drop bypass-flag use)
10. Smoke test end-to-end through `codex app-server` JSON-RPC (NOT `codex exec` — the bridge uses app-server)

### P4 — Permission-mode UX + capability registration (≈ ½ day)

11. Update `health.go` HarnessInfo for codex: capability list `{model, effort, tools, budget}`, supported_permission_modes `{ask, auto, bypass}` (already there but verify)
12. Re-test `SessionPermissionMode.tsx` with a live codex session

### P5 — Verification + docs (≈ ½ day)

13. End-to-end smoke: codex session → tool call → prehook fires → permission-store rule matches → ask → banner → resolve → tool runs
14. Update `README.md`, `HARNESS-LAYER.md`, this doc with empirical findings
15. File codex issues we hit (regression #21639 if it bites, hook coverage gaps on non-bash tools)

### P6 — Codex-specific extras (deferred)

16. Stop / UserPromptSubmit / SessionStart hook events surfaced in bridge-ui
17. Output-schema picker
18. PermissionRequest audit log

---

## 5. Open questions to resolve before writing P1 code

- **Hook regression severity**: does `PreToolUse` fire in 0.130 stable through `codex app-server`? Need a real bridge-driven smoke test before committing to P1.
- **`-c` vs `CODEX_HOME` for config injection**: spike both, pick the one that survives multi-MB MCP configs and JSON-with-quotes hooks without escaping pain.
- **`updatedInput` substitute**: can we use `additionalContext` on `PreToolUse` for in-band answer-merge once codex stops failing-open on it, or do we need `UserPromptSubmit` as the carrier?
- **`PermissionRequest` vs `PreToolUse`**: should we route ALL gating through PreToolUse, or use PermissionRequest as a finer gate for escalation-only? PreToolUse is more universal; PermissionRequest is more semantically correct but currently fails-closed on richer return shapes.

---

## 6. Codex permission knob inventory

Every knob codex exposes that touches permissions, sandboxing, network, filesystem access, or approval flow. Each row shows where it's settable, whether it can change mid-session, and what we should do with it given that bridge-side permission-store + prehook is the actual gate.

### 6.1 The two big knobs (the only ones that vary live)

| Knob | Type / values | Settable at | Mid-session changeable? | Recommended bridge use |
|---|---|---|---|---|
| **`approval_policy`** / `--ask-for-approval -a` / `approvalPolicy` (thread+turn) | `untrusted` \| `on-request` \| `never` \| `{granular={...}}` | config.toml, `-c`, CLI flag, `thread/start.approvalPolicy`, **`turn/start.approvalPolicy`** | **YES — per-turn**, via app-server JSON-RPC. The bridge already does this (`codex.go:142`, `handler.go:519`). | Hard-pin to **`never`** in 99% of sessions. The bridge auto-approves every `*ApprovalRequest` notification anyway (`handler.go`), so any non-`never` value just wastes round-trips. The exception is `ask` mode (see §7.5). |
| **`sandbox_mode`** / `--sandbox -s` / `sandboxPolicy` (thread+turn) | `read-only` \| `workspace-write` \| `danger-full-access` | config.toml, `-c`, CLI flag, `thread/start.sandbox` (string), **`turn/start.sandboxPolicy`** (tagged enum: `readOnly`/`workspaceWrite`/`dangerFullAccess`) | **YES — per-turn**, via app-server JSON-RPC. Bridge already does this. | Default to **`workspace-write`** because hooks fire and edits work. Bump to `danger-full-access` only on explicit user opt-in for bypass mode. Avoid `read-only` unless the user explicitly wants it — too restrictive for most tasks. |

These two are the only knobs codex's protocol exposes per-turn. Everything else below is process-start-only.

### 6.2 Process-start-only knobs (lockable, but require `app-server` respawn to change)

| Knob | Settable at | What it does | Recommended bridge use |
|---|---|---|---|
| `sandbox_workspace_write.writable_roots` (TOML, `-c`) | startup | Extra paths writable in workspace-write mode | Pass session workdir + any `add_dirs` the bridge already collects |
| `sandbox_workspace_write.network_access` (bool) | startup | Outbound network in workspace-write | **Default `true`** — most tasks need network; the prehook can still block specific tool calls |
| `sandbox_workspace_write.exclude_slash_tmp` (bool) | startup | Removes `/tmp` from writable roots | Leave default (false). `/tmp` is commonly used |
| `sandbox_workspace_write.exclude_tmpdir_env_var` (bool) | startup | Removes `$TMPDIR` | Leave default |
| `shell_environment_policy.inherit` (`all`/`core`/`none`) | startup, `-c` | Which env vars bleed into subprocess | **`core`** (defaults). Keep secret-stripping defaults |
| `shell_environment_policy.exclude` (`[]string` globs) | startup | Strip patterns after defaults | Use to strip bridge-internal env vars |
| `shell_environment_policy.include_only` (`[]string`) | startup | Whitelist (overrides exclude) | Don't set — too easy to break |
| `shell_environment_policy.set` (`map[k]v`) | startup | Force-set vars per subprocess | Use to inject `BRIDGE_SESSION_ID`, etc. |
| `default_permissions` (profile name) | startup, `-c` | Named permission profile (`:read-only`, `:workspace`, `:danger-no-sandbox`) | Skip. We control via `sandbox_mode` + writable_roots directly |
| `permissions.<name>.filesystem.*` (per-path R/W/N) | startup (TOML only) | Granular path-level policy | Skip for v1. Possibly expose later for "deny read access to ~/.ssh"-style rules |
| `permissions.<name>.network.{enabled,mode,domains,...}` | startup (TOML only) | Per-profile network policy (managed proxy, SOCKS5, domain allowlist) | Skip for v1. Powerful — domain allowlists could be a "codex superpower" UI later |
| `approvals_reviewer` (`user`/`auto_review`) | startup, `-c` | Who reviews escalations under interactive policies | Leave default (`user`) — we auto-approve regardless |
| `projects.<path>.trust_level` (`trusted`/`untrusted`) | TOML only | Whether `.codex/` config in the path is loaded (project-local hooks!) | **Must be `trusted`** for the session workdir if we ever rely on project-local `.codex/`. Bridge writes this into the per-session `CODEX_HOME/config.toml`. |

### 6.3 Approval-policy granular sub-mode (advanced)

If `approval_policy = { granular = { … } }` is set, these sub-knobs surface or hide individual prompt classes (all bool, startup-only):

| Sub-knob | What it controls |
|---|---|
| `granular.sandbox_approval` | Sandbox-escalation prompts |
| `granular.rules` | execpolicy/`.rules` matches |
| `granular.mcp_elicitations` | MCP elicitation prompts |
| `granular.request_permissions` | `request_permissions` tool prompts |
| `granular.skill_approval` | Skill-script approval prompts |

**Skip granular mode entirely.** The bridge prehook covers all of these uniformly. Pinning `approval_policy = never` is simpler than orchestrating five toggles.

### 6.4 CLI-only flags (no config.toml equivalent)

| Flag | Subcommands | Use? |
|---|---|---|
| `--dangerously-bypass-approvals-and-sandbox` / `--yolo` | all | **NO.** Likely bypasses hooks (per docs: "skip all confirmation prompts and execute commands without sandboxing"). Use the explicit `never + danger-full-access` pair instead so hooks stay alive. |
| `--add-dir DIR` | all | YES, pass user-extra workspace paths through. Codex's `sandbox_workspace_write.writable_roots` is the config equivalent and we can pass via `-c` instead — pick one and be consistent. |
| `--cd DIR` / `-C` | all | YES — set the session workdir |
| `--search` | all | YES — enables `web_search` tool. Bridge translates `webSearch` events already (`translate.go`) |
| `--full-auto` | `codex exec` only | NO — deprecated alias for `--sandbox workspace-write` + `--ask-for-approval on-request`. Set the pair explicitly |
| `--ignore-rules` | `codex exec` | NO — we want execpolicy rule files honored as a defense-in-depth |
| `--skip-git-repo-check` | `codex exec` | N/A — `app-server` doesn't have this |
| `--ignore-user-config` | `codex exec` | N/A — we use `CODEX_HOME` for isolation instead |
| `--ephemeral` | `codex exec` | N/A — `app-server` thread persistence is independent |

### 6.5 Where the actual prompt fires (codex vs bridge)

Two ways for "ask the human" to happen:

**Option A — keep codex on `never`, route asks through the bridge prehook only**
- Codex never escalates; bridge prehook is the universal gate
- Permission-store rules with `outcome=ask` (or `always_ask` mode) trigger the parked-asks banner
- Simplest, mirrors CC behavior

**Option B — let codex escalate via `on-request`, intercept via `PermissionRequest` hook**
- Codex's PreToolUse fires our hook → bridge decides
- Codex's PermissionRequest fires a SECOND hook on actual escalations
- Slightly more accurate ("ask only when codex thinks it's risky") but adds a second pathway

**Recommendation: Option A.** Permission-store rules + canonical mode are the source of truth. Codex's own escalation logic adds noise without adding signal — we'd just be filtering our own rules through codex's heuristics. Pin `approval_policy = never`, let the prehook decide.

### 6.6 The bypass case

For `permission_mode = bypass` we want **maximum permissiveness while keeping hooks observable**. The explicit pair `approval_policy = never` + `sandbox_mode = danger-full-access` gives us this. Do NOT use `--dangerously-bypass-approvals-and-sandbox` — that flag drops the hook layer entirely.

### 6.7 The mid-session change mechanism

Per-turn changes already work end-to-end:

1. User toggles permission mode in bridge-ui → PUT `/sessions/{id}/permission-mode` → updates `HarnessConfig["permission_mode"]`
2. Bridge-server emits a `control{subtype=set_permission_mode, payload={mode}}` JSON-RPC call to llm-bridge-codex
3. `HandleSetPermissionMode` (`handler.go:445`) mutates `b.cfg.ApprovalMode` — **bug today: this expects codex-vocab (`never`/etc.), not canonical (`ask`/`auto`/`bypass`)**. Needs to route through `applyCanonicalPermissionMode` when input is canonical.
4. Next `turn/start` (`handler.go:519`) sends the updated `ApprovalPolicy` + `SandboxPolicy` per the new mode
5. Codex respects the new policy from that turn onward

**Gaps to close for true mid-session toggle:**
- Fix `HandleSetPermissionMode` to translate canonical modes (one-line: call `applyCanonicalPermissionMode` if `isCanonicalPermissionMode`)
- Ensure bridge-server emits the `control` call on permission-mode PUT (verify in `permission_mode.go` handlers — may already work, may not)
- Sandbox mode also needs a control path: today only `ApprovalMode` is mutable mid-session in the bridge. Add `set_sandbox_mode` or fold both into the canonical mode change

### 6.8 Summary — what to pin, what to vary

| Knob | Pin or vary | Value(s) |
|---|---|---|
| `approval_policy` | **vary per-turn** | Always `never` (bridge prehook is the gate). `on-request` only if we adopt §6.5 option B |
| `sandbox_mode` | **vary per-turn** | `workspace-write` default; `read-only` on Plan/Read modes; `danger-full-access` on Allow All mode |
| `sandbox_workspace_write.network_access` | **vary per-respawn** | Driven by the `Disable Network` toggle (boolean side-axis, not part of the mode enum) |
| `sandbox_workspace_write.writable_roots` | pin at start | session workdir + user `add_dirs` |
| `sandbox_workspace_write.network_access` | pin at start | `true` |
| `shell_environment_policy.inherit` | pin at start | `core` |
| `projects.<workdir>.trust_level` | pin at start | `trusted` (in per-session `CODEX_HOME/config.toml`) |
| `default_permissions` | don't set | — |
| `permissions.*` granular table | don't set | — |
| `approval_policy.granular.*` | don't set | — |
| Hooks `[hooks]` block | pin at start | bridge prehook + permission-approval hook URLs |
| `--dangerously-bypass-approvals-and-sandbox` flag | **never use** | — |

The mental model: **codex is configured to be maximally permissive within a workspace, hooks are how the bridge sees+gates everything, permission-store is the actual brain.** Codex's sandbox is the floor (defense-in-depth) not the ceiling.

### 6.9 Host escape hatch: `CODEX_DISABLE_SANDBOX`

Codex's `workspace-write` and `read-only` sandbox modes are implemented with **bubblewrap**. On hosts where bwrap can't initialize a user namespace with loopback networking, every codex tool call fails at sandbox setup with:

```
bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted
```

Common triggers:
- Ubuntu 24.04+ with restricted user namespaces / AppArmor blocking unprivileged bwrap
- Kernels without `CAP_NET_ADMIN` available in unprivileged user namespaces
- Containerized hosts that drop `NET_ADMIN`

**Critically, the failure happens BEFORE the model dispatches the tool**, so the bridge prehook never gets the chance to gate it. The user sees "auto-deny" with no banner because no permission check ever ran.

**Workaround: `CODEX_DISABLE_SANDBOX=1`** (read by the codex bridge in `config.go`). When set, the bridge pins every session's `SandboxPolicy` to `danger-full-access` regardless of canonical mode (Plan/Read/Auto/Rules/Ask All/Block All all run sandbox-less at the codex layer). Implementation: last step of `applyStartConfig` in `handler.go` — runs after canonical mapping AND Custom-mode override so nothing can undo it.

**This is not a security regression.** The bridge prehook + permission-store remain the security gate:
- `Plan`/`Read` modes still deny non-whitelisted tools at the prehook BEFORE the call runs (bridge-side, not sandbox-side)
- `Block All` still denies everything at the prehook
- `Ask All` still parks every call for human approval
- `Rules` still consults permission-store rules

What's lost: defense-in-depth at the codex sandbox layer. If the bridge prehook is bypassed somehow (e.g. codex's PreToolUse doesn't fire for a tool — see [issue #20204](https://github.com/openai/codex/issues/20204)), the codex sandbox would no longer catch unsafe operations. Acceptable trade-off when the sandbox can't function at all on the host.

**Set via systemd drop-in in `llm-bridge-server`'s `deploy.sh`:**

```ini
[Service]
Environment=CODEX_DISABLE_SANDBOX=1
```

Remove the line once the host's user-namespace / `CAP_NET_ADMIN` config supports unprivileged bwrap loopback. The codex bridge code path stays — it's a permanent escape hatch for any future host with the same issue.

---

## 7. Expanding the permission-mode enum + UI

Today the canonical permission mode is a 3-state enum (`ask`/`auto`/`bypass`) collapsed from each harness's native model. That worked when CC was the only harness, but it's lossy:

- **CC has more modes than we expose.** CC's native `permission-mode` enum includes `plan` (read-only planning, no execution) which the bridge never surfaces.
- **Codex has two orthogonal axes** (approval × sandbox = 12 possible combinations) flattened into 3 buckets, losing useful intermediates like "read-only with network" or "workspace-write but no network."
- **Future harnesses (aider, goose, hermes, …)** will each bring their own subset.

### 7.1 The expansion: more canonical modes, gated by harness capability

Add to the canonical enum so each useful combination has a name. Each harness declares which subset it supports via `HarnessInfo.supported_permission_modes`; UI filters the dropdown accordingly.

| UI Label | Wire value | What the bridge prehook does | Default? | CC mapping | Codex mapping (approval / sandbox) |
|---|---|---|---|---|---|
| Block All | `block_all` *(new)* | Deny every tool call; reason is "blocked by user." Agent sees the deny in the tool result and continues — it can reason about the block, switch approaches, or ask the human | — | `default` (bridge denies before CC gets the call) | `never` / `workspace-write` |
| Plan | `plan` *(new)* | Allow planning whitelist (Read/Glob/Grep/TodoWrite); deny everything else | — | `plan` | `never` / `read-only` |
| Read | `read` *(new)* | Allow read tools (Read/Glob/Grep/safe Bash); deny Write/Edit/destructive Bash | — | `acceptEdits` + Write/Edit deny-list | `never` / `read-only` |
| Ask All | `ask_all` *(new)* | Skip permission-store entirely; park EVERY tool call for human approval | — | `default` (bridge parks before CC gets the call) | `never` / `workspace-write` |
| **Rules** | `ask` *(existing — relabeled)* | Call permission-store `/evaluate`; rules decide allow/deny/ask | **✓** | `default` | `never` / `workspace-write` |
| Auto Rules | `auto` *(existing — relabeled)* | Whitelist (Read/Glob/Grep/Edit/Write/TodoWrite) → allow; else permission-store | — | `acceptEdits` | `never` / `workspace-write` |
| Allow All | `bypass` *(existing — relabeled)* | Allow everything; permission-store skipped entirely | — | `bypassPermissions` | `never` / `danger-full-access` |
| Custom… | `custom` *(new)* | User sets raw approval/sandbox; bridge prehook still applies rules | — | raw fields | raw fields |

**`Rules` is the default** for new sessions. `Auto Rules` and `Allow All` are always available so legacy sessions and unknown harnesses don't break.

### 7.1.1 The "Disable Network" toggle (separate axis)

Network gating is a separate concern from tool gating — `Plan + no network` and `Auto Rules + no network` are both valid. So it's a side checkbox, not a mode:

```
Mode: [Rules ▼]
☐ Disable network access     ← codex-only initially; greyed out on CC
```

- Wire field: `HarnessConfig["disable_network"]` *(bool, default false)*
- Codex mapping: `sandbox_workspace_write.network_access = false` (via `-c` override on app-server spawn)
- CC mapping: currently no clean equivalent. Phase 1: greyed out for CC. Phase 2: implement as a hook-store rule that denies any tool call matching network-egress patterns. Until then, the checkbox is harness-gated.

### 7.1.2 Mode semantics — why each one exists

- **Block All** — pause the agent without ending the session. Agent sees each tool call get denied, can reason about it (ask the user, switch tactics, stop). Useful for "I want to take over typing" or "freeze while I review."
- **Plan** — investigation-only mode. Agent reads code, drafts todos, thinks out loud. Can't run Bash or write files. CC's native `plan` mode.
- **Read** — slightly less restrictive than Plan: read-only `Bash` (`ls`, `cat`, `git log`) is allowed alongside Read/Glob/Grep, but Write/Edit/destructive Bash is denied. Useful for code review sessions.
- **Ask All** — paranoia mode. Skip the rule engine; prompt every single tool call regardless of prior `allow` rules. Useful for high-stakes work, demos, or shadow-auditing.
- **Rules** *(default)* — rules-based. Permission-store evaluates every call; allow/deny/ask per rule. Users teach the rule engine over time ("always allow `git status`") and get asked less.
- **Auto Rules** — lighter touch. Common safe tools (Read/Glob/Grep/Edit/Write/TodoWrite) bypass rules and auto-allow; everything else falls through to permission-store.
- **Allow All** — unrestricted. Skip rules entirely; useful when you trust the agent fully or are running externally sandboxed.
- **Custom** — power-user escape hatch. Raw approval/sandbox knobs without us pre-baking a preset.

Plus the orthogonal **Disable Network** toggle.

### 7.2 What's already in place vs what changes

**Already in place:**
- `msg.PermissionMode` constants (`ask`/`auto`/`bypass`) in `~/repos/llm-bridge/msg`
- `HarnessInfo.supported_permission_modes` registry in `health.go:104-112`
- `SessionPermissionMode.tsx` filters the dropdown by `supported_permission_modes`
- `PUT /sessions/{id}/permission-mode` writes to `HarnessConfig["permission_mode"]`
- Mid-session emission via `control{subtype=set_permission_mode}` JSON-RPC call to harness
- Codex bridge's `applyCanonicalPermissionMode` translates canonical → codex vocab at start time

**Changes needed:**

1. **llm-bridge msg package** — add new enum values to `msg.PermissionMode*` constants. Backwards-compat: existing CC/codex bridges silently ignore unknown modes (they fall through `applyStartConfig` default branch today).

2. **bridge-server `health.go`** — extend each harness's `supported_permission_modes` list:
   ```go
   case msg.HarnessClaudeCode:
       return []msg.PermissionMode{
           msg.PermissionModePlan, msg.PermissionModeReadOnly,
           msg.PermissionModeAsk, msg.PermissionModeAuto,
           msg.PermissionModeBypass, msg.PermissionModeCustom,
       }
   case msg.HarnessCodex:
       return []msg.PermissionMode{
           msg.PermissionModePlan, msg.PermissionModeReadOnly,
           msg.PermissionModeAsk, msg.PermissionModeAuto,
           msg.PermissionModeAirgapped, msg.PermissionModeBypass,
           msg.PermissionModeCustom,
       }
   case msg.HarnessJig: return []msg.PermissionMode{...} // declare what jig supports
   default:
       return []msg.PermissionMode{msg.PermissionModeAsk, msg.PermissionModeAuto, msg.PermissionModeBypass}
   ```

3. **bridge-server `permission_prehook.go`** — extend the short-circuit table at `permission_prehook.go:77-87` with the new modes:
   - `block_all` → deny every tool call with reason "Tool blocked by user (Block All mode)." Agent receives the deny in its tool result and continues its turn — model can reason about the block, ask the human, or stop on its own. No park, no rule consult.
   - `plan` → allow only the planning whitelist (Read/Glob/Grep/TodoWrite); deny everything else with reason "Plan mode: only planning tools permitted." No rule consult on either branch.
   - `read` → allow read tools + safe Bash (`ls`/`cat`/`git log`-style; needs a safe-bash heuristic); deny Write/Edit/destructive Bash. No rule consult.
   - `ask_all` → **skip permission-store entirely**, park every tool call directly. Deliberate bypass of the rule engine.
   - `custom` → fall through to permission-store (rules apply); raw approval/sandbox knobs are harness-side concerns
   - `ask` *(default)* → unchanged: call permission-store `/evaluate`, rules decide
   - `auto` → unchanged: safe-tool whitelist, else permission-store
   - `bypass` → unchanged: allow immediately

   `disable_network` is **not** read by the prehook — it's a sandbox-layer concern. The prehook gates tool *invocation*; network access is enforced at the codex sandbox or via a hook-store rule for CC.

4. **Each harness bridge** — extend `applyCanonicalPermissionMode` with new cases:
   - CC bridge: `plan` → `plan`, `read_only` → `acceptEdits` + disable Write/Edit tools, `airgapped` → `acceptEdits` + network-block hook
   - Codex bridge: as per mapping table above
   - Default for unknown modes: leave config alone (codex falls through to its TOML defaults)

5. **bridge-ui** —
   - `SessionPermissionMode.tsx` already filters by `supported_permission_modes`; extend the option-rendering switch with the new labels + one-line subtitles (see §7.1.2 for descriptions)
   - **NEW: "Disable network" checkbox** alongside the mode dropdown. Disabled (greyed out) when the harness doesn't expose the capability — drive off a new `HarnessInfo.supports_disable_network` bool. Posts to a new endpoint `PUT /sessions/{id}/disable-network` or piggybacks on the same `permission-mode` PUT with a `disable_network` field.
   - **NEW: "Custom" mode panel** — when `custom` is selected, show two controls (approval_policy enum, sandbox_mode enum) gated by what the harness exposes. Persist to `HarnessConfig["permission_mode_custom"]` as a struct `{approval, sandbox}`. The Disable Network checkbox above still applies (don't duplicate it inside the custom panel).
   - **Dropdown ordering** (restrictive → permissive, divider before special):
     ```
     Block All
     Plan
     Read
     Ask All
     Rules (Default)  ★
     Auto Rules
     Allow All
     ─────────────
     Custom…
     ```

### 7.3 Why not just expose codex's two axes directly?

It's tempting to drop the canonical enum entirely and let the UI expose codex's `approval × sandbox` pair raw. Why we shouldn't:

- **The prehook layer needs a single canonical mode** to short-circuit on. `plan`/`bypass`/`read_only` are semantic, `(approval=never, sandbox=read-only)` is a configuration. The prehook should branch on intent, not on a specific harness's config.
- **Other harnesses don't fit the two-axis model.** Aider has `--yes` flag (one knob). Hermes has nothing. CC has its 4-state enum. A canonical "intent" enum survives harness diversity; a codex-specific pair doesn't.
- **Cross-harness rules in permission-store rely on canonical modes.** A rule like "always ask before running rm -rf" should fire identically across CC and codex; that means the rule engine reads canonical mode, not native config.

The two-axis exposure DOES live on, but inside `custom` mode — power users get raw control without inverting the canonical model.

### 7.4 Per-turn vs per-session UX

Codex's per-turn capability (`turn/start.approvalPolicy` + `turn/start.sandboxPolicy`) means we technically could let the user change mode per individual tool call. But that's bad UX — the user picks a mode for the session and it sticks until they change it. The "per-turn" capability is purely an implementation detail: it's how mid-session changes propagate without restarting codex.

**One concrete UX pattern worth considering for later:** "temporary elevation" — user hits a button to bump bypass mode for the next N seconds or next 1 tool call, then auto-revert. Powerful for "let me just run this one risky command." Defer until parity is shipped.

### 7.5 Implementation order (revised P4 from §4)

P4 expands from a half-day to ~1 day:

11a. **llm-bridge msg** — add the new `PermissionMode*` constants (1 commit, separate PR)
11b. **bridge-server `health.go`** — declare per-harness support
11c. **bridge-server `permission_prehook.go`** — short-circuit table extension
11d. **Each harness bridge** — `applyCanonicalPermissionMode` cases (one PR per harness)
11e. **bridge-ui `SessionPermissionMode.tsx`** — labels + descriptions
11f. **bridge-ui `CustomPermissionPanel.tsx`** — new component for `custom` mode

Ship 11a → 11b → 11c → 11e in one cycle (UI works with fallthrough mappings even before harness PRs land). Then 11d per harness. 11f is optional polish.

---

## 8. Doc references

- [Codex Hooks](https://developers.openai.com/codex/hooks) — event list, payload + output JSON shapes
- [Codex Agent Approvals & Security](https://developers.openai.com/codex/agent-approvals-security) — sandbox + approval policy reference
- [Codex Changelog](https://developers.openai.com/codex/changelog) — 0.128-0.130 ship notes
- [Issue #21639](https://github.com/openai/codex/issues/21639) — PreToolUse/SessionStart regression (open)
- [Issue #20204](https://github.com/openai/codex/issues/20204) — uneven PreToolUse coverage across tool handlers (open)
- Internal: `permission_prehook.go`, `hook_settings.go`, `tool_provision.go`, `permission_mode.go`, `parked_asks.go`, `hooks_resolve.go`
- Internal: `bridge-ui/.../BridgePermissions.tsx`, `BridgeTools.tsx`, `SessionPermissionMode.tsx`, `PendingPermissionsBanner.tsx`
- Internal: `llm-bridge-codex/handler.go`, `codex.go`, `appserver.go`
