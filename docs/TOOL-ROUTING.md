# Tool Routing Across Harnesses

## Why this matters

A capability the agent needs (read a file, search memories, run a shell command) can be served by:

1. A **native** tool of the harness — the model was trained on it, calls it well, output is structured.
2. An **MCP** tool registered with the harness — first-class `tool_use` semantics, slightly less training weight than native, but reliable.
3. A **CLI** tool described in the system prompt and called via the harness's Bash-equivalent — real `tool_use` (Bash), reliability gated by how clearly we describe the surface.
4. **Not available** — model has to refuse or improvise.

Reliability and quality drop as you go down the list. Token efficiency drops too: native tools have terse, model-friendly schemas; MCP tools are similar; CLI tools cost prompt tokens to describe.

**Routing rule, in order:**

1. If the harness has a native tool for this capability → use the native tool, do not register a duplicate.
2. Else if a registered MCP tool covers it → register with the harness.
3. Else if a CLI tool exists on PATH (or is provisionable) → describe it in the system prompt / CLAUDE.md and let the model call it via Bash.
4. Else → omit.

The whole point: don't make the model use `cat` via Bash when the harness has a `Read` tool that's been trained into it.

## Native tool catalog

Cataloged by harness. Capabilities are normalized to inber-internal names so the routing layer can match. Marked ★ when the native version is materially better than the CLI fallback (training reliability, structured output, or both).

### Claude Code
| Capability | Native tool | Notes |
|---|---|---|
| read_file | `Read` ★ | Handles binary, images, PDFs natively. Always prefer over `cat`. |
| write_file | `Write` ★ | Creates parent dirs, atomic. Prefer over shell redirect. |
| edit_file | `Edit` ★ | Diff-aware, validates exact match. Far better than `sed`. |
| edit_notebook | `NotebookEdit` ★ | Jupyter-aware. No CLI fallback that approaches this. |
| run_shell | `Bash` ★ | The CLI escape hatch itself. |
| glob_files | `Glob` ★ | Faster than shell-equivalent. |
| grep_content | `Grep` ★ | Built on ripgrep, structured output. |
| web_fetch | `WebFetch` ★ | URL → markdown, with caching. CLI alternatives are weaker. |
| web_search | `WebSearch` ★ | Hosted search. No good CLI fallback. |
| subagent | `Task` | Useful for delegation but expensive — leave to user agent prefs. |
| todo_track | `TodoWrite` | In-session todo list. Often duplicative of noteboard; default off. |
| plan_mode | `ExitPlanMode` | Plan-mode-specific. Default off unless plan mode is in play. |

### Codex
| Capability | Native tool | Notes |
|---|---|---|
| read_file | `shell` (cat) | Codex's native fs ops route through `shell`. CLI is the surface. |
| write_file | `apply_patch` ★ | Patch-based; structured. Prefer over `shell`-redirect. |
| edit_file | `apply_patch` ★ | Same. |
| run_shell | `shell` ★ | Native. |
| ...others | varies by Codex version | Check `codex --help`; this catalog needs a verification pass. |

Codex catalog is incomplete here — verifying the actual tool surface against a live Codex install is a follow-up.

### ForgeCode
| Capability | Native tool | Notes |
|---|---|---|
| (TBD) | (TBD) | Forge's tool surface depends on its model config. Treat as opaque until we audit. |

### Gemini CLI / Cline / Aider / Roo / Goose
- Scaffolds today (per CLAUDE.md). Catalog when each goes from scaffold to functional.

### inber (own runtime)
- Tools are defined by inber, not the harness — the catalog lives in inber/tools/, not here.
- Routing is moot: inber owns the tool surface entirely.

## Capability registry

Defined as a stable enum bridge-server uses for routing. Each tool-store entry declares which capability it implements (or "custom" for inber-specific things with no native equivalent).

```
capabilities/
  read_file
  write_file
  edit_file
  edit_notebook
  run_shell
  glob_files
  grep_content
  web_fetch
  web_search
  subagent
  todo_track
  plan_mode

  # inber-specific (no native equivalent on any harness)
  memory_search
  memory_save
  agent_delegate
  bus_publish
  bus_tail
  notes_read
  notes_write
  kanban_op
  tools_list      # meta: list registered tools
  tools_run       # meta: invoke a registered tool by name
```

Anything in the second block routes to CLI/MCP regardless of harness — no harness has these natively, and that's the whole reason inber's CLI surface exists.

## Discovery — how the harness server knows what's available

The "harness server" here = the bridge that wraps a specific harness (`llm-bridge-claudecode`, `llm-bridge-codex`, …). At session start it does:

1. **Native catalog**: look up its own static catalog (committed in code, this doc is the source).
2. **MCP catalog**: query `tool-store` for entries with `type=mcp` flagged available for this agent.
3. **CLI catalog**: query `tool-store` for entries with `type=cli`. For each, verify its `bin` resolves on PATH (or via `which`) inside the runner's environment. Skip ones that don't resolve.
4. **Resolve conflicts**: for each capability, walk the rule above (native > MCP > CLI). Drop duplicates.
5. **Render**:
   - Native tools → already known to the harness, no action.
   - MCP tools → emit harness-specific MCP config (CC: `.mcp.json`, Codex: equivalent).
   - CLI tools → emit a `## Available CLI tools` section in CLAUDE.md (or AGENTS.md for Codex), with one line per tool: `\`<bin> <usage hint>\` — <description>`.
6. **Permissions**: allowlist the bins of the CLI tools in `.claude/settings.json` (CC) or the equivalent.

## Skills routing

Skills (SKILL.md entries in skill-store) follow the same `native > MCP > CLI/inject > omit` pattern, with one twist: CC has *native skill discovery* that auto-loads everything in `~/.claude/skills/` and surfaces them as slash commands. So routing is mostly about whether to surface, not how.

| Harness | Native skill mechanism | Routing |
|---|---|---|
| Claude Code | Auto-discovers `~/.claude/skills/*/SKILL.md`. Surfaces as `/<skill-name>` slash commands. Verified 2026-05-09 — init event lists all installed skills regardless of agent. | Use native discovery. For per-agent filtering, CC has `--disable-slash-commands` (nuclear) — finer-grained per-skill enable/disable would need investigation. |
| Codex / inber / API harnesses | None native | Concatenate enrolled SKILL.md content into the agent's system prompt at compose time. Skill enrollment per agent comes from skill-store. |
| Cursor / Aider / scaffold harnesses | Varies | Default to inject; revisit when each goes from scaffold to functional. |

### What "enroll" means for skills

skill-store already has a registry of installed skills. Per-agent enrollment (which skills *this* agent can see) lives in agent-store as `agent_skills` (parallels `agent_harness_tools`). The renderer pulls the enrolled set, then routes per-harness:

- CC: enrollment is mostly informational. Native discovery wins. If we want hard filtering, inject "Skills available to you: X, Y, Z" in the appended system prompt and rely on the model honoring it (soft filter, fine for most cases).
- Non-CC: enrollment is load-bearing. Each enrolled skill's SKILL.md content gets included in the rendered identity. Renderer can include the full body or just a summary, depending on size.

### Cache implications for skills

Skill content is stable across sessions for the same agent. As long as enrollment doesn't change, skills sit in the cached prefix. Adding/removing a skill from an agent's enrollment is a cache-bust event by definition — same as adding/removing a tool. Treat with the same care.

### Skill content size

Skills can be large (full how-to docs). For non-CC harnesses where we inject SKILL.md content, the size adds up fast. Two mitigations:

1. **Inject SKILL.md headers / front matter only** by default — name, description, when-to-use. Model can request the full body via a CLI tool (`bridge skills get <name> --json`).
2. **Per-agent enrollment** — only inject what this agent actually needs.

Both stack. Header-only injection plus tight enrollment keeps the prefix lean while still letting the model self-discover when a skill applies.

## Why CLI-described-in-prompt is sufficient (when the alternative is nothing)

We had a concern that prompt-described tools regress reliability vs. real `tool_use`. That concern stands — *for pseudo-tools the model would call directly*. It does **not** apply to this design, because:

- The actual tool the model calls is **Bash** (real `tool_use` block).
- The prompt only describes *what to type* into Bash.
- The model is heavily trained on driving CLIs from Bash — it does this every day in Claude Code workflows. There's no out-of-distribution gap.

The CLI being described in the prompt is no different from the model being told "you can use `git`" and then deciding to run `git status`. It's not a synthetic protocol; it's just documentation.

## Why this doc lives in bridge-server, not the individual harness wrappers

- The routing rule is cross-harness. Putting it in any one wrapper would duplicate.
- The native catalog is shared truth — multiple consumers (UI, conformance tests, agent config validation) need it.
- The wrappers stay thin — their job is to translate the harness's protocol, not to make policy decisions.

A future change might extract this into a small `internal/toolrouting/` package consumed by both the wrappers and bridge-server's session-start code. For now, this doc is the source of truth.

## End-to-end setup walkthrough

What happens when you set up a tool or a skill and an agent ends up using it.

### Adding a new tool

```
1. Register in tool-store
   curl -X POST http://localhost:8302/tools \
     -d '{"name": "my-tool", "type": "cli", "bin": "my-tool", "description": "..."}'
   # type can be: cli, mcp, native (rare — only when implementing in tool-store itself)

2. Enroll for an agent (via /agents UI or curl)
   POST /agents/{id}/tools { "harness_id": "claudecode", "tool_name": "my-tool" }
   # Writes a row to agent_harness_tools.

3. Next session.create for that agent on that harness:
   - AgentReconciler.PrepareSession calls render.Registry["claudecode"]
   - Renderer pulls the agent's enrolled tools from tool-store
   - Routes per the table above:
     • If type=native and CC has it natively → adds to --allowed-tools
     • If type=mcp → emits to per-agent ~/.claude/mcp-servers/<agent>.json, passed via --mcp-config
     • If type=cli → adds a "## Available CLI tools" line to --append-system-prompt
   - Spawns CC with the assembled flags

4. Model invokes:
   • Native → tool_use block, dispatched by CC
   • MCP → tool_use block, dispatched to MCP server
   • CLI → Bash tool_use block calling the command directly
```

### Adding a new skill

```
1. Register in skill-store (or it's auto-ingested from a known repo)
   curl -X POST http://localhost:8301/skills \
     -d '{"id": "my-skill", "name": "...", "description": "...", "body": "<SKILL.md content>"}'

2. Enroll for an agent
   POST /agents/{id}/skills { "skill_id": "my-skill" }
   # Writes a row to agent_skills.

3. Next session.create:
   - For Claude Code:
     • CC auto-discovers from ~/.claude/skills/*/SKILL.md (skill-store seeds these via the existing tracked_files / runner-seed mechanism)
     • Skills surface as /<skill-name> slash commands automatically
     • Per-agent filtering not currently fine-grained; full discovery
   - For non-CC harnesses:
     • Renderer pulls enrolled skill metadata only (name + description)
     • Adds a "## Skills available to you" block in --append-system-prompt
     • Model can grab full SKILL.md body via `bridge skills get <name> --json` (CLI surface) when applicable

4. Cache implication: skill enrollment changes are session-bust events. Treat with the same care as tool enrollment.
```

### Adding a new agent

```
1. Create the agent record (agent-store API or /agents UI)
   POST /agents { "slug": "researcher", "display_name": "Researcher", ... }
   POST /agents/{id}/nature { "kind": "identity", "content": "You research..." }

2. Pick a harness
   POST /agents/{id}/harness/claudecode { "model": "sonnet", "subagent_allow": ["*"] }

3. Enroll tools and skills (steps above)

4. Optionally add subagents
   POST /agents { "slug": "explorer", "parent_agent_id": <researcher.id>, ... }

5. Done. session.create against this agent works automatically:
   - bridge-server resolves AgentDef from agent-store
   - AgentReconciler.PrepareSession composes the SpawnSpec
   - Spawns CC with --append-system-prompt, --agents (subagents), --allowed-tools, --mcp-config, --settings
```

### Mid-session edits

Out of policy. Edits via UI or CLI take effect on the *next* session. The model can use `bridge agent identity append` or similar to mutate state mid-session — those mutations write to agent-store but don't reload the running session's prefix. See `CACHE-RULES.md`.

## Implementation outline

Phase 1 — catalog as data:
- `internal/toolrouting/catalog.go` — static native catalog per harness, derived from this doc.
- `internal/toolrouting/capabilities.go` — capability enum.
- `internal/toolrouting/route.go` — `Route(agent, harness, available) → []Tool` returning the deduped, prioritized tool set.

Phase 2 — wire into session start:
- `internal/harness/manifests.go` calls `toolrouting.Route` to get the tool set.
- `internal/server/tool_provision.go` renders the CLAUDE.md/MCP/settings outputs from the routed tool set.

Phase 3 — verification:
- Conformance test per harness: spawn it, ask it to use each capability, assert it routed to the expected tool (native vs. CLI).
- Catalog drift detector: parse `claude --help` (and equivalents) at CI time, fail if the static catalog disagrees.

## Open questions

1. **Tool description tokens.** The CLI block in CLAUDE.md is uncached on first turn but cached after. With ~20 CLI tools at ~30 tokens each, that's ~600 tokens of stable prefix. Acceptable. Watch this number as the surface grows.
2. **Per-agent tool subsets.** Some agents shouldn't get certain tools (e.g. memory agents don't need WebSearch). Filter via agent-store `runtime.tools.exclude` after routing.
3. **Capability override.** Should an agent be able to *force* CLI even when a native tool exists (e.g. for instrumentation)? Probably yes via an explicit `runtime.tools.force_cli: [capability]` list. Default to native.
4. **Codex catalog verification.** Static catalog is incomplete here. Needs an audit pass against a live Codex.
