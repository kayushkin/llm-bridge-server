# Harness Layer Abstraction

## The promise

bridge-server speaks one language: canonical agent and tool definitions, plus a handle to "this harness for this session." It does **not** know that CC reads `CLAUDE.md`, that Codex reads `AGENTS.md`, that inber takes a `system_prompt` field, that some harnesses have a native subagent dispatcher and others don't. All of that is below the line.

The harness bridge (`llm-bridge-claudecode`, `llm-bridge-codex`, etc.) is where harness-specific filesystem state, flag construction, native-vs-custom tool routing, and protocol translation live. By the time data crosses the line into bridge-server, it's harness-shape-free.

```
┌─────────────── bridge-server ────────────────┐
│  knows: agents, tools, skills, sessions      │
│  doesn't know: CLAUDE.md, --append-flags,    │
│                ~/.claude/agents/, etc.       │
└──────────────────┬───────────────────────────┘
                   │ canonical types: AgentDef, ToolRef, ...
                   │ via HarnessBridge interface
┌──────────────────┴───────────────────────────┐
│  llm-bridge-claudecode    llm-bridge-codex   │
│  llm-bridge-inber         llm-bridge-...     │
│  knows: per-harness file paths, flags,       │
│         subagent dispatch mechanism,         │
│         what tools are native vs. inject     │
└──────────────────────────────────────────────┘
```

## The interface

In `llm-bridge` (the canonical lib), define the contract every harness bridge implements:

```go
package bridge

// HarnessBridge is the per-harness adapter that prepares an agent
// session for execution and tears it down. Lives in each
// llm-bridge-<harness> wrapper.
type HarnessBridge interface {
    // EnsureAgent reconciles the agent's static state with what
    // the harness expects. Idempotent. Called when an agent is
    // created or its definition changes — NOT on every session.
    EnsureAgent(ctx context.Context, agent AgentDef) error

    // PrepareSession resolves the per-spawn details needed to start
    // a session for this agent. Does not write per-session files.
    PrepareSession(ctx context.Context, sess SessionRef) (SpawnSpec, error)

    // CleanupAgent removes any harness-side artifacts for an agent
    // that's being deleted. Idempotent.
    CleanupAgent(ctx context.Context, agentID string) error
}

// AgentDef is the canonical agent shape, harness-agnostic.
type AgentDef struct {
    ID          string
    Slug        string
    Identity    string          // long-form system prompt body
    Tools       []ToolEnrollment
    Skills      []SkillEnrollment
    Subagents   []AgentDef      // recursively, for subagent dispatchers
    Extras      []NamedSection  // freeform per-agent instructions
    Updated     time.Time       // for staleness checks
}

// SpawnSpec is what bridge-server uses to actually start the harness.
// Pure data — no filesystem side effects implied.
type SpawnSpec struct {
    Args        []string          // additional CLI args to pass
    Env         map[string]string // additional env vars
    CWD         string            // working directory (usually the user's repo)
    BundleHash  string            // SHA256 of the materialized identity bundle, for cache observability
}
```

Two-phase split is deliberate:

- **`EnsureAgent`** runs when the agent definition changes (creation, edit, tool/skill enrollment change). It does the filesystem materialization — for CC, writing `~/.claude/agents/<slug>.md`, updating `~/.claude/settings.json` if needed. It runs *rarely* — once per agent, not once per session.
- **`PrepareSession`** runs on every session start. Returns flags + env + CWD. No filesystem writes here. CWD is the user's actual working directory (their repo); we don't manufacture session workspaces.

This split is what eliminates per-session-workspace overhead. The agent's state lives at known per-agent paths; sessions just pick the right flags to point at it.

## What lives where, per harness

### `llm-bridge-claudecode` — verified 2026-05-09

CC's `--agents <json>` flag (verified in `--input-format stream-json`) accepts inline subagent definitions per session. Eliminates per-agent filesystem state for subagents.

`EnsureAgent`:

1. **Subagents — no-op.** Subagents are passed via `--agents` JSON at session spawn, not materialized as files. Nothing to write.
2. **MCP config** (only if the agent has tool-store entries with `type=mcp`): write `~/.claude/mcp-servers/<agent_slug>.json`, register as tracked_file with scope=`mcp-config`. Per-agent, not per-session.
3. **CLI tool / skill injection text**: pre-compute and cache on the agent_harness row (or compute at PrepareSession time — cheap either way).

`PrepareSession`:

```go
// Build subagent JSON map from agent.Subagents (recursively)
agentsJSON := buildAgentsJSON(agent)  // {"<sub_slug>": {description, prompt, tools, ...}, ...}

return SpawnSpec{
    Args: []string{
        "--input-format", "stream-json",
        "--output-format", "stream-json",
        "--append-system-prompt", composedIdentity,   // top-level identity + CLI tool/skill injection
        "--allowed-tools", strings.Join(allowedTools, ","),
        "--agents", agentsJSON,
        // optional: "--mcp-config", "~/.claude/mcp-servers/<agent_slug>.json"
        // optional: "--model", agent.Model
    },
    CWD:        request.CWD, // user's repo, NOT a manufactured workspace
    BundleHash: sha256(composedIdentity + agentsJSON + sortedTools + sortedSkills),
}
```

`CleanupAgent`: remove the per-agent MCP config file (if any). No subagent files exist to clean.

The user's repo CLAUDE.md (if any) is read by CC automatically from CWD — we don't touch it. That's project-level shared truth, owned by the human. Same for the user's `~/.claude/CLAUDE.md` (their personal preferences).

**Why `--append-system-prompt` instead of `--system-prompt`:** verified 2026-05-09 (see `CC-VERIFIED.md`) — `--system-prompt` does NOT suppress auto-loaded CLAUDE.md. The replace-vs-append choice is purely about CC's built-in identity prelude. Appending keeps the helpful prelude *and* lets the user's project + global CLAUDE.md load alongside our composed identity. Default to append.

**Why not `--bare`:** `--bare` is API-key-only, incompatible with subscription auth.

### `llm-bridge-codex`

`EnsureAgent`:
1. Codex doesn't have native subagents (verify against current Codex version). Subagents become **custom tools** the harness bridge registers — see Subagent routing below.
2. Identity + injected tools/skills accumulate into a single string for `--system-prompt`-equivalent.

`PrepareSession`: passes the composed identity via the JSON-RPC field on `codex app-server` initialize.

`CleanupAgent`: nothing if no per-agent files; otherwise the agent-store equivalent.

### `llm-bridge-inber`

`EnsureAgent`: probably no-op. Inber's runtime reads agent state directly from agent-store.

`PrepareSession`: returns a SpawnSpec with the agent ID baked into env; inber-side code reads agent-store.

### Other harness bridges

Same pattern. The contract is small enough that even a scaffold (Aider, Goose, Cline) can implement a no-op `EnsureAgent` plus a minimal `PrepareSession` that just sets `--system <identity>` and call it good.

## Subagent routing — the key abstraction

`AgentDef.Subagents` is the canonical "this agent can dispatch these other agents." Every harness has to make that real somehow:

| Harness | Implementation |
|---|---|
| Claude Code | For subagents whose preferred harness == `claudecode`: build a JSON map and pass via `--agents <json>` at session spawn (verified 2026-05-09 in stream-json mode). The model uses CC's native `Task`/`Agent` tool with `subagent_type: "<slug>"`. For cross-harness subagents (preferred harness != `claudecode`): describe `bridge agent ask <slug> "<task>"` in the appended system prompt — same pattern as every other non-native CLI tool. Bash invocation, no MCP, no special registration. `bridge agent ask` calls back to bridge-server which spawns a fresh session on the subagent's preferred harness, returns the final output as stdout. Zero filesystem state for inline subagents. |
| Codex | Codex has no native inline-subagent mechanism (verify against current Codex version). All subagents — same-harness or cross-harness — are dispatched via the `bridge agent ask <slug>` CLI described in the appended system prompt and invoked through Codex's native `shell` tool. Same CLI-in-prompt pattern as everything else; no MCP, no custom tool registration. |
| inber | Inber-runtime parents dispatch subagents through their own internal mechanism (inber knows about agent-store agents natively). For cross-harness delegation from inber, use `bridge agent ask <slug>` — same CLI as other harnesses. |
| Hermes / API harnesses | Same as Codex pattern: custom tool that calls back through bridge-server. |

Routing rule: **prefer native dispatchers when they exist.** CC's `Task` is well-trained, dispatching through it produces better behavior than a CLI-in-prompt path. For harnesses without a native equivalent (Codex, etc.), describe `bridge agent ask <slug>` in the appended system prompt and let the model invoke it via the harness's shell/Bash tool — same CLI-described-in-prompt pattern as every other non-native capability.

Bridge-server doesn't know which path was taken. It just provides `AgentDef.Subagents` to the harness bridge and lets it figure out the rest.

When Codex's custom tool dispatches back to bridge-server, that's a normal session-create call — bridge-server resolves the subagent's AgentDef, picks the right harness for *that* subagent (could be a different one than the parent — e.g. parent on Codex sub, subagent on inber API), runs it, returns. Subagent recursion works out of the box because the canonical types compose.

## Session lifecycle, end to end

```
[Agent created / edited]
    ├── agent-store: record updated
    └── HarnessBridge.EnsureAgent for each registered harness
        └── per-agent files reconciled, tracked_files drift settles

[session.create(agentID, harness)]
    ├── bridge-server: resolve AgentDef from agent-store
    ├── bridge-server: HarnessBridge.PrepareSession(sessionRef)
    │   └── returns SpawnSpec (args, env, CWD, BundleHash)
    ├── bridge-server: log session.start with BundleHash
    └── bridge-server: spawn the harness wrapper binary with SpawnSpec

[session running]
    └── normal stream-json / JSON-RPC flow per harness
        - tool calls handled by harness or custom-tool callbacks
        - subagent dispatches handled per-harness routing
        - mid-session edits to agent-store: noted, take effect next session
          (rare-edit case — accepted cache miss budget)

[session.end]
    ├── bridge-server: close session, persist final state
    └── HarnessBridge keeps per-agent files; nothing per-session to clean

[Agent deleted]
    └── HarnessBridge.CleanupAgent
        └── per-agent files removed
```

## What this gives us

- **bridge-server stays harness-clean.** Adding a new harness is purely an `llm-bridge-<harness>` repo concern — bridge-server doesn't grow.
- **No per-session filesystem ceremony.** No workspace directories to manage, no agent confusion about CWDs, no symlinks.
- **Single source of truth for every per-agent fact.** agent-store rows → `EnsureAgent` materializes harness-side files → tracked_files keeps them honest. /files UI keeps working unchanged.
- **Subagent surface uses the harness's best path.** CC users get `Task` (well-trained, well-cached). Codex users get `bridge agent ask` described in prompt, invoked via Codex's shell tool. Both look like `AgentDef.Subagents` upstream.
- **Cache observability.** BundleHash logged per session-start; comparing across sessions tells you exactly what changed when caching regresses.

## What we explicitly don't do

- Materialize per-session CLAUDE.md / AGENTS.md / similar files. Per-agent only.
- Manipulate the user's repo (no CLAUDE.md writes inside their repo dir).
- Hot-reload agent state into running sessions. Mid-session agent-store edits show up next session.
- Try to suppress globally-cached files like `~/.claude/CLAUDE.md` (user's own preferences). They co-exist with our injected content.
- Re-implement CC subagents in the canonical layer. CC's native mechanism is fine; we just feed it.

## Open questions

1. ~~**Subagent file naming under CC.**~~ Resolved 2026-05-09. CC's `--agents <json>` flag means there are no subagent files; the JSON map key is the canonical name.
2. ~~**Cross-harness subagent dispatch.**~~ Resolved 2026-05-10 — allowed by default, no special tool registration. Renderer logic: if subagent's preferred harness == parent's harness, inline via the harness's native dispatcher (CC's `Task`/`--agents`, etc.). If different, describe `bridge agent ask <slug> "<task>"` in the appended system prompt — Bash invocation, follows the standard CLI-described-in-prompt pattern from `TOOL-ROUTING.md`. `bridge agent ask` calls back to bridge-server to spawn a fresh session on the subagent's preferred harness. Default per subagent: use its own configured harness; fall back to parent's harness if unset. No MCP, no per-session tool registration, no user-facing config needed for common cases.
3. **EnsureAgent triggering for the artifacts that DO need files.** Now narrower in scope (MCP config files, per-agent `--settings` JSON). Two options: (a) bridge-server calls EnsureAgent on every harness when an agent-store row changes (push), (b) harness bridges subscribe to agent-store change events (pull). (a) is simpler if there are few harness bridges; (b) is better for many. Default (a) for now.
4. ~~**`--agents` JSON `@file` syntax.**~~ Verified 2026-05-09 — does NOT exist. Inline JSON only. Mitigation if argv size becomes an issue: filter by `agent_harness.subagent_allow` first; fall back to `~/.claude/agents/<slug>.md` (CC auto-discovers these from disk) for the overflow.
5. ~~**`--system-prompt` vs `--append-system-prompt` interaction with auto-loaded CLAUDE.md.**~~ Verified 2026-05-09 — `--system-prompt` does NOT suppress auto-loaded CLAUDE.md. The replace-vs-append choice is purely about CC's built-in identity prelude. Default to `--append-system-prompt` (keeps CC's helpful prelude).
6. ~~**Permission/settings per-agent vs. per-user.**~~ Resolved — `--settings <path>` accepts a per-session JSON file (verified). Materialize a per-agent settings JSON via `EnsureAgent`, register as tracked_file, pass via flag at session spawn.
